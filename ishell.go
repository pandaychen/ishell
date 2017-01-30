// Package ishell implements an interactive shell.
package ishell

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/flynn/go-shlex"
	"gopkg.in/readline.v1"
)

const (
	defaultPrompt      = ">>> "
	defaultMultiPrompt = "... "
)

var (
	errNoHandler          = errors.New("no handler registered for input")
	errNoInterruptHandler = errors.New("No interrupt handler")
)

// Shell is an interactive cli shell.
type Shell struct {
	functions       map[string]Func
	rootCmd         *Cmd
	generic         Func
	interrupt       Func
	interruptCount  int
	reader          *shellReader
	writer          io.Writer
	active          bool
	activeMutex     sync.RWMutex
	ignoreCase      bool
	customCompleter bool
	haltChan        chan struct{}
	historyFile     string
	contextValues   map[string]interface{}
	autoHelp        bool
	Actions
}

// New creates a new shell with default settings. Uses standard output and default prompt ">> ".
func New() *Shell {
	rl, err := readline.New(defaultPrompt)
	if err != nil {
		log.Println("Shell or operating system not supported.")
		log.Fatal(err)
	}
	shell := &Shell{
		rootCmd:   &Cmd{},
		functions: make(map[string]Func),
		reader: &shellReader{
			scanner:     rl,
			prompt:      defaultPrompt,
			multiPrompt: defaultMultiPrompt,
			showPrompt:  true,
			buf:         &bytes.Buffer{},
			completer:   readline.NewPrefixCompleter(),
		},
		writer:   os.Stdout,
		haltChan: make(chan struct{}),
		autoHelp: true,
	}
	shell.Actions = &shellActionsImpl{Shell: shell}
	addDefaultFuncs(shell)
	return shell
}

// Start starts the shell. It reads inputs from standard input and calls registered functions
// accordingly. This function blocks until the shell is stopped.
func (s *Shell) Start() {
	s.start()
}

func (s *Shell) start() {
	if s.Active() {
		return
	}
	if !s.customCompleter {
		s.initCompleters()
	}
	s.activeMutex.Lock()
	s.active = true
	s.activeMutex.Unlock()

shell:
	for s.Active() {
		var line []string
		var err error
		read := make(chan struct{})
		go func() {
			line, err = s.read()
			read <- struct{}{}
		}()
		select {
		case <-read:
			break
		case <-s.haltChan:
			continue shell
		}
		if err == io.EOF {
			fmt.Println("EOF")
			break
		} else if err != nil && err != readline.ErrInterrupt {
			s.Println("Error:", err)
		}

		if err == readline.ErrInterrupt {
			// interrupt received
			err = handleInterrupt(s, line)
		} else {
			// reset interrupt counter
			s.interruptCount = 0

			// normal flow
			if len(line) == 0 {
				// no input line
				continue
			}

			err = handleInput(s, line)
		}
		if err != nil {
			s.Println("Error:", err)
		}
	}
}

// Active tells if the shell is active. i.e. Start is previously called.
func (s *Shell) Active() bool {
	s.activeMutex.RLock()
	defer s.activeMutex.RUnlock()
	return s.active
}

func handleInput(s *Shell, line []string) error {
	handled, err := s.handleCommand(line)
	if handled || err != nil {
		return err
	}

	// Generic handler
	if s.generic == nil {
		return errNoHandler
	}
	c := newContext(s, nil, line)
	s.generic(c)
	return c.err
}

func handleInterrupt(s *Shell, line []string) error {
	if s.interrupt == nil {
		return errNoInterruptHandler
	}
	c := newContext(s, nil, line)
	s.interrupt(c)
	return c.err
}

func (s *Shell) handleCommand(str []string) (bool, error) {
	if s.ignoreCase {
		for i := range str {
			str[i] = strings.ToLower(str[i])
		}
	}
	cmd, args := s.rootCmd.FindCmd(str)
	if cmd == nil {
		return false, nil
	}
	// trigger help if auto help is true
	if s.autoHelp && len(args) == 1 && args[0] == "help" {
		s.Println(cmd.HelpText())
		return true, nil
	}
	c := newContext(s, cmd, args)
	cmd.Func(c)
	return true, c.err
}

func (s *Shell) readLine() (line string, err error) {
	consumer := make(chan lineString)
	defer close(consumer)
	go s.reader.readLine(consumer)
	ls := <-consumer
	return ls.line, ls.err
}

func (s *Shell) read() ([]string, error) {
	heredoc := false
	eof := ""
	// heredoc multiline
	lines, err := s.readMultiLinesFunc(func(line string) bool {
		if !heredoc {
			if strings.Contains(line, "<<") {
				s := strings.SplitN(line, "<<", 2)
				if eof = strings.TrimSpace(s[1]); eof != "" {
					heredoc = true
					return true
				}
			}
		} else {
			return line != eof
		}
		return strings.HasSuffix(strings.TrimSpace(line), "\\")
	})

	if heredoc {
		s := strings.SplitN(lines, "<<", 2)
		args, err1 := shlex.Split(s[0])

		arg := strings.TrimSuffix(strings.SplitN(s[1], "\n", 2)[1], eof)
		args = append(args, arg)
		if err1 != nil {
			return args, err1
		}
		return args, err
	}

	lines = strings.Replace(lines, "\\\n", " \n", -1)

	args, err1 := shlex.Split(lines)
	if err1 != nil {
		return args, err1
	}

	return args, err
}

func (s *Shell) readMultiLinesFunc(f func(string) bool) (string, error) {
	lines := bytes.NewBufferString("")
	currentLine := 0
	var err error
	for {
		if currentLine == 1 {
			// from second line, enable next line prompt.
			s.reader.setMultiMode(true)
		}
		var line string
		line, err = s.readLine()
		fmt.Fprint(lines, line)
		if !f(line) || err != nil {
			break
		}
		fmt.Fprintln(lines)
		currentLine++
	}
	if currentLine > 0 {
		// if more than one line is read
		// revert to standard prompt.
		s.reader.setMultiMode(false)
	}
	return lines.String(), err
}

func (s *Shell) initCompleters() error {
	return s.setCompleter(addCompleters(s.rootCmd, nil))
}

func (s *Shell) setCompleter(completer readline.AutoCompleter) error {
	var err error
	// close current scanner and rebuild it with
	// command in autocomplete
	s.reader.scanner.Close()
	config := s.reader.scanner.Config
	config.AutoComplete = completer
	s.reader.scanner, err = readline.NewEx(config)
	if err != nil {
		log.Fatal(err)
	}
	return nil
}

// CustomCompleter allows use of custom implementation of readline.Autocompleter
func (s *Shell) CustomCompleter(completer readline.AutoCompleter) error {
	s.customCompleter = true
	return s.setCompleter(completer)
}

func addCompleters(root *Cmd, parent readline.PrefixCompleterInterface) readline.AutoCompleter {
	var pcItems []readline.PrefixCompleterInterface
	for _, cmd := range root.children {
		pcItem := readline.PcItem(cmd.Name)
		addCompleters(cmd, pcItem)
		pcItems = append(pcItems, pcItem)
	}
	// recursion base
	if parent == nil {
		parent = readline.NewPrefixCompleter(pcItems...)
	}
	parent.SetChildren(pcItems)
	return parent
}

// AddCmd adds a new command handler.
// This only adds top level commands.
func (s *Shell) AddCmd(cmd *Cmd) {
	s.rootCmd.AddCmd(cmd)
}

// DeleteCmd deletes a top level command.
func (s *Shell) DeleteCmd(name string) {
	s.rootCmd.DeleteCmd(name)
}

// NotFound adds a generic function for all inputs.
// It is called if the shell input could not be handled by any of the
// added commands.
func (s *Shell) NotFound(f Func) {
	s.generic = f
}

// AutoHelp sets if ishell should trigger help message if
// a command's arg is "help". Defaults to true.
// This can be set to false for more control on how help is
// displayed.
func (s *Shell) AutoHelp(enable bool) {
	s.autoHelp = enable
}

// Interrupt adds a function to handle keyboard interrupt (Ctrl-c).
func (s *Shell) Interrupt(f Func) {
	s.interrupt = f
}

// SetHistoryPath sets where readlines history file location. Use an empty
// string to disable history file. It is empty by default.
func (s *Shell) SetHistoryPath(path string) error {
	var err error

	// Using scanner.SetHistoryPath doesn't initialize things properly and
	// history file is never written. Simpler to just create a new readline
	// Instance.
	s.reader.scanner.Close()
	config := s.reader.scanner.Config
	config.HistoryFile = path
	s.reader.scanner, err = readline.NewEx(config)
	return err
}

// SetHomeHistoryPath is a convenience method that sets the history path with a
// $HOME prepended path.
func (s *Shell) SetHomeHistoryPath(path string) {
	home := os.Getenv("HOME")
	abspath := filepath.Join(home, path)
	s.SetHistoryPath(abspath)
}

// SetOut sets the writer to write outputs to.
func (s *Shell) SetOut(writer io.Writer) {
	s.writer = writer
}

// IgnoreCase specifies whether commands should not be case sensitive.
// Defaults to false i.e. commands are case sensitive.
// If true, commands must be registered in lower cases. e.g. shell.Register("cmd", ...)
func (s *Shell) IgnoreCase(ignore bool) {
	s.ignoreCase = ignore
}

func newContext(s *Shell, cmd *Cmd, args []string) *Context {
	if cmd == nil {
		cmd = &Cmd{}
	}
	return &Context{
		Actions: s.Actions,
		values:  s.contextValues,
		Args:    args,
		Cmd:     *cmd,
	}
}
