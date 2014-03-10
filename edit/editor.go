// Package edit implements a full-righteature line editor.
package edit

import (
	"fmt"
	"os"
	"strings"

	"github.com/xiaq/elvish/edit/tty"
	"github.com/xiaq/elvish/eval"
	"github.com/xiaq/elvish/parse"
	"github.com/xiaq/elvish/util"
)

var LackEOL = "\033[7m\u23ce\033[m\n"

type bufferMode int

const (
	modeInsert bufferMode = iota
	modeCommand
	modeCompletion
	modeNavigation
	modeHistory
)

type editorState struct {
	// States used during ReadLine.
	tokens                []parse.Item
	prompt, rprompt, line string
	dot                   int
	tips                  []string
	mode                  bufferMode
	completion            *completion
	completionLines       int
	navigation            *navigation
	history               *historyState
}

type historyState struct {
	items         []string
	current       int
	saved, prefix string
}

// Editor keeps the status of the line editor.
type Editor struct {
	savedTermios *tty.Termios
	file         *os.File
	writer       *writer
	reader       *reader
	ev           *eval.Evaluator
	sigch        <-chan os.Signal
	editorState
}

// LineRead is the result of ReadLine. Exactly one member is non-zero, making
// it effectively a tagged union.
type LineRead struct {
	Line string
	EOF  bool
	Err  error
}

func (hs *historyState) append(line string) {
	hs.items = append(hs.items, line)
}

func (hs *historyState) prev() bool {
	for i := hs.current - 1; i >= 0; i-- {
		if strings.HasPrefix(hs.items[i], hs.prefix) {
			hs.current = i
			return true
		}
	}
	return false
}

func (hs *historyState) next() bool {
	for i := hs.current + 1; i < len(hs.items); i++ {
		if strings.HasPrefix(hs.items[i], hs.prefix) {
			hs.current = i
			return true
		}
	}
	return false
}

// New creates an Editor.
func New(file *os.File, tr *util.TimedReader, ev *eval.Evaluator, sigch <-chan os.Signal) *Editor {
	return &Editor{
		// savedTermios: term.Copy(),
		file:   file,
		writer: newWriter(file),
		reader: newReader(tr),
		ev:     ev,
		sigch:  sigch,
		editorState: editorState{
			history: &historyState{},
		},
	}
}

func (ed *Editor) beep() {
}

func (ed *Editor) pushTip(more string) {
	ed.tips = append(ed.tips, more)
}

func (ed *Editor) refresh() error {
	// Re-lex the line, unless we are in modeCompletion
	if ed.mode != modeCompletion {
		ed.tokens = nil
		hl := Highlight("<interactive code>", ed.line, ed.ev)
		for token := range hl {
			ed.tokens = append(ed.tokens, token)
		}
	}
	return ed.writer.refresh(&ed.editorState)
}

// TODO Allow modifiable keybindings.
var keyBindings = map[bufferMode]map[Key]string{
	modeCommand: map[Key]string{
		Key{'i', 0}:    "start-insert",
		Key{'h', 0}:    "move-dot-left",
		Key{'l', 0}:    "move-dot-right",
		Key{'D', 0}:    "kill-line-right",
		DefaultBinding: "default-command",
	},
	modeInsert: map[Key]string{
		Key{'[', Ctrl}:    "start-command",
		Key{'U', Ctrl}:    "kill-line-left",
		Key{'K', Ctrl}:    "kill-line-right",
		Key{Backspace, 0}: "kill-rune-left",
		Key{Delete, 0}:    "kill-rune-right",
		Key{Left, 0}:      "move-dot-left",
		Key{Right, 0}:     "move-dot-right",
		Key{Up, 0}:        "move-dot-up",
		Key{Down, 0}:      "move-dot-down",
		Key{Enter, Alt}:   "insert-key",
		Key{Enter, 0}:     "return-line",
		Key{'D', Ctrl}:    "return-eof",
		Key{Tab, 0}:       "start-completion",
		Key{PageUp, 0}:    "start-history",
		Key{'N', Ctrl}:    "start-navigation",
		DefaultBinding:    "default-insert",
	},
	modeCompletion: map[Key]string{
		Key{'[', Ctrl}: "cancel-completion",
		Key{Up, 0}:     "select-cand-up",
		Key{Down, 0}:   "select-cand-down",
		Key{Left, 0}:   "select-cand-left",
		Key{Right, 0}:  "select-cand-right",
		Key{Tab, 0}:    "cycle-cand-right",
		DefaultBinding: "default-completion",
	},
	modeNavigation: map[Key]string{
		Key{Up, 0}:     "select-nav-up",
		Key{Down, 0}:   "select-nav-down",
		Key{Left, 0}:   "ascend-nav",
		Key{Right, 0}:  "descend-nav",
		DefaultBinding: "default-navigation",
	},
	modeHistory: map[Key]string{
		Key{'[', Ctrl}:   "cancel-history",
		Key{PageUp, 0}:   "select-history-prev",
		Key{PageDown, 0}: "select-history-next",
		DefaultBinding:   "default-history",
	},
}

func init() {
	for _, kb := range keyBindings {
		for _, name := range kb {
			if leBuiltins[name] == nil {
				panic("bad keyBindings table: no editor builtin named " + name)
			}
		}
	}
}

// acceptCompletion accepts currently selected completion candidate.
func (ed *Editor) acceptCompletion() {
	c := ed.completion
	if 0 <= c.current && c.current < len(c.candidates) {
		accepted := c.candidates[c.current].text
		ed.line = ed.line[:c.start] + accepted + ed.line[c.end:]
		ed.dot += len(accepted) - (c.end - c.start)
	}
	ed.completion = nil
	ed.mode = modeInsert
}

// acceptHistory accepts currently history.
func (ed *Editor) acceptHistory() {
	ed.line = ed.history.items[ed.history.current]
	ed.dot = len(ed.line)
}

func SetupTerminal(file *os.File) (*tty.Termios, error) {
	fd := int(file.Fd())
	term, err := tty.NewTermiosFromFd(fd)
	if err != nil {
		return nil, fmt.Errorf("can't get terminal attribute: %s", err)
	}

	savedTermios := term.Copy()

	term.SetIcanon(false)
	term.SetEcho(false)
	term.SetMin(1)
	term.SetTime(0)

	err = term.ApplyToFd(fd)
	if err != nil {
		return nil, fmt.Errorf("can't set up terminal attribute: %s", err)
	}

	// Set autowrap off
	file.WriteString("\033[?7l")

	err = tty.FlushInput(fd)
	if err != nil {
		return nil, fmt.Errorf("can't flush input: %s", err)
	}

	return savedTermios, nil
}

func CleanupTerminal(file *os.File, savedTermios *tty.Termios) error {
	// Set autowrap on
	file.WriteString("\033[?7h")
	fd := int(file.Fd())
	return savedTermios.ApplyToFd(fd)
}

// startsReadLine prepares the terminal for the editor.
func (ed *Editor) startReadLine() error {
	savedTermios, err := SetupTerminal(ed.file)
	if err != nil {
		return err
	}
	ed.savedTermios = savedTermios

	// Query cursor location
	ed.file.WriteString("\033[6n")
	// BUG(xiaq): In Editor.startReadLine, there is a race condition when user
	// input sneaked in between WriteString and readCPR
	x, _, err := ed.reader.readCPR()
	if err != nil {
		return err
	}

	if x != 1 {
		ed.file.WriteString(LackEOL)
	}

	return nil
}

// finishReadLine puts the terminal in a state suitable for other programs to
// use.
func (ed *Editor) finishReadLine(lr *LineRead) {
	if lr.EOF == false && lr.Err == nil {
		ed.history.append(lr.Line)
	}

	ed.tips = nil
	ed.mode = modeInsert
	ed.completion = nil
	ed.dot = len(ed.line)
	// TODO Perhaps make it optional to NOT clear the rprompt
	ed.rprompt = ""
	ed.refresh() // XXX(xiaq): Ignore possible error
	ed.file.WriteString("\n")

	err := CleanupTerminal(ed.file, ed.savedTermios)

	if err != nil {
		// BUG(xiaq): Error in Editor.finishReadLine may override earlier error
		*lr = LineRead{Err: fmt.Errorf("can't restore terminal attribute: %s", err)}
	}
	ed.savedTermios = nil
}

// ReadLine reads a line interactively.
// TODO(xiaq): ReadLine currently just ignores all signals.
func (ed *Editor) ReadLine(prompt, rprompt func() string) (lr LineRead) {
	err := ed.startReadLine()
	if err != nil {
		return LineRead{Err: err}
	}
	defer ed.finishReadLine(&lr)

	ed.line = ""
	ed.mode = modeInsert
	ed.tips = nil
	ed.completion = nil
	ed.dot = 0
	ed.writer.oldBuf.cells = nil

	for {
		ed.prompt = prompt()
		ed.rprompt = rprompt()
		err := ed.refresh()
		if err != nil {
			return LineRead{Err: err}
		}

		ed.tips = nil

		k, err := ed.reader.readKey()
		if err != nil {
			ed.pushTip(err.Error())
			continue
		}

	lookupKey:
		keyBinding, ok := keyBindings[ed.mode]
		if !ok {
			ed.pushTip("No binding for current mode")
			continue
		}

		name, bound := keyBinding[k]
		if !bound {
			name = keyBinding[DefaultBinding]
		}
		ret := leBuiltins[name](ed, k)
		if ret == nil {
			continue
		}
		switch ret.action {
		case noAction:
			continue
		case changeMode:
			ed.mode = ret.newMode
			continue
		case changeModeAndReprocess:
			ed.mode = ret.newMode
			goto lookupKey
		case exitReadLine:
			return ret.readLineReturn
		}
	}
}
