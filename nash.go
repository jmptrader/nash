package nash

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/NeowayLabs/nash/ast"
	"github.com/NeowayLabs/nash/errors"
	"github.com/NeowayLabs/nash/parser"
)

type (
	// Env is the environment map of lists
	Env map[string]*Obj
	Var Env
	Fns map[string]*Shell
	Bns Fns

	// Shell is the core data structure.
	Shell struct {
		name        string
		debug       bool
		lambdas     uint
		logf        LogFn
		nashdPath   string
		dotDir      string
		isFn        bool
		currentFile string // current file being executed or imported

		interrupted bool
		looping     bool

		stdin  io.Reader
		stdout io.Writer
		stderr io.Writer

		argNames []string
		env      Env
		vars     Var
		fns      Fns
		binds    Fns

		root   *ast.Tree
		parent *Shell

		repr string // string representation

		*sync.Mutex
	}

	errIgnore struct {
		*errors.NashError
	}

	errInterrupted struct {
		*errors.NashError
	}
)

func newErrIgnore(format string, arg ...interface{}) error {
	e := &errIgnore{
		NashError: errors.NewError(format, arg...),
	}

	return e
}

func (e *errIgnore) Ignore() bool { return true }

func newErrInterrupted(format string, arg ...interface{}) error {
	return &errInterrupted{
		NashError: errors.NewError(format, arg...),
	}
}

func (e *errInterrupted) Interrupted() bool { return true }

const (
	logNS     = "nash.Shell"
	defPrompt = "\033[31mλ>\033[0m "
)

// NewShell creates a new shell object
func NewShell() (*Shell, error) {
	sh := &Shell{
		name:      "parent scope",
		isFn:      false,
		logf:      NewLog(logNS, false),
		nashdPath: nashdAutoDiscover(),
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		stdin:     os.Stdin,
		env:       make(Env),
		vars:      make(Var),
		fns:       make(Fns),
		binds:     make(Fns),
		argNames:  make([]string, 0, 16),
		Mutex:     &sync.Mutex{},
	}

	err := sh.setup()

	if err != nil {
		return nil, err
	}

	sh.setupSignals()

	return sh, nil
}

// NewSubShell creates a nash.Shell that inherits the parent shell stdin,
// stdout, stderr and mutex lock.
// Every variable and function lookup is done first in the subshell and then, if
// not found, in the parent shell recursively.
func NewSubShell(name string, parent *Shell) (*Shell, error) {
	if parent == nil {
		return nil, errors.NewError("A sub Shell requires a parent shell")
	}

	sh := &Shell{
		name:      name,
		isFn:      true,
		parent:    parent,
		logf:      NewLog(logNS, false),
		nashdPath: nashdAutoDiscover(),
		stdout:    parent.Stdout(),
		stderr:    parent.Stderr(),
		stdin:     parent.Stdin(),
		env:       make(Env),
		vars:      make(Var),
		fns:       make(Fns),
		binds:     make(Fns),
		argNames:  make([]string, 0, 16),
		Mutex:     parent.Mutex,
	}

	return sh, nil
}

// initEnv creates a new environment from old one
func (sh *Shell) initEnv() error {
	processEnv := os.Environ()

	argv := NewListObj(os.Args)

	sh.Setenv("argv", argv)
	sh.Setvar("argv", argv)

	for _, penv := range processEnv {
		var value *Obj
		p := strings.Split(penv, "=")

		if len(p) == 2 {
			value = NewStrObj(p[1])

			sh.Setvar(p[0], value)
			sh.Setenv(p[0], value)
		}
	}

	pidVal := NewStrObj(strconv.Itoa(os.Getpid()))

	sh.Setenv("PID", pidVal)
	sh.Setvar("PID", pidVal)

	if _, ok := sh.Getenv("SHELL"); !ok {
		shellVal := NewStrObj(nashdAutoDiscover())
		sh.Setenv("SHELL", shellVal)
		sh.Setvar("SHELL", shellVal)
	}

	cwd, err := os.Getwd()

	if err != nil {
		return err
	}

	cwdObj := NewStrObj(cwd)
	sh.Setenv("PWD", cwdObj)
	sh.Setvar("PWD", cwdObj)

	return nil
}

// Reset internal state
func (sh *Shell) Reset() {
	sh.fns = make(Fns)
	sh.vars = make(Var)
	sh.env = make(Env)
	sh.binds = make(Fns)
}

// SetDebug enable/disable debug in the shell
func (sh *Shell) SetDebug(d bool) {
	sh.debug = d
	sh.logf = NewLog(logNS, d)
}

func (sh *Shell) SetName(a string) {
	sh.name = a
}

func (sh *Shell) Name() string { return sh.name }

func (sh *Shell) SetParent(a *Shell) {
	sh.parent = a
}

func (sh *Shell) Environ() Env {
	if sh.parent != nil {
		// Note(i4k): stack overflow, refactor this!
		return sh.parent.Environ()
	}

	return sh.env
}

func (sh *Shell) Getenv(name string) (*Obj, bool) {
	if sh.parent != nil {
		return sh.parent.Getenv(name)
	}

	value, ok := sh.env[name]
	return value, ok
}

func (sh *Shell) Setenv(name string, value *Obj) {
	if sh.parent != nil {
		sh.parent.Setenv(name, value)
		return
	}

	sh.env[name] = value

	os.Setenv(name, value.String())
}

func (sh *Shell) SetEnviron(env Env) {
	sh.env = env
}

func (sh *Shell) GetVar(name string) (*Obj, bool) {
	if value, ok := sh.vars[name]; ok {
		return value, ok
	}

	if sh.parent != nil {
		return sh.parent.GetVar(name)
	}

	return nil, false
}

func (sh *Shell) GetFn(name string) (*Shell, bool) {
	sh.logf("Looking for function '%s' on shell '%s'\n", name, sh.name)

	if fn, ok := sh.fns[name]; ok {
		return fn, ok
	}

	if sh.parent != nil {
		return sh.parent.GetFn(name)
	}

	return nil, false
}

func (sh *Shell) Setbindfn(name string, value *Shell) {
	sh.binds[name] = value
}

func (sh *Shell) Getbindfn(cmdName string) (*Shell, bool) {
	if fn, ok := sh.binds[cmdName]; ok {
		return fn, true
	}

	if sh.parent != nil {
		return sh.parent.Getbindfn(cmdName)
	}

	return nil, false
}

func (sh *Shell) Setvar(name string, value *Obj) {
	sh.vars[name] = value
}

func (sh *Shell) IsFn() bool { return sh.isFn }

func (sh *Shell) SetIsFn(b bool) { sh.isFn = b }

// Prompt returns the environment prompt or the default one
func (sh *Shell) Prompt() string {
	value, ok := sh.Getenv("PROMPT")

	if ok {
		return value.String()
	}

	return "<no prompt> "
}

// SetNashdPath sets an alternativa path to nashd
func (sh *Shell) SetNashdPath(path string) {
	sh.nashdPath = path
}

func (sh *Shell) SetDotDir(path string) {
	sh.dotDir = path

	obj := NewStrObj(sh.dotDir)

	sh.Setenv("NASHPATH", obj)
	sh.Setvar("NASHPATH", obj)
}

func (sh *Shell) DotDir() string {
	return sh.dotDir
}

// SetStdin sets the stdin for commands
func (sh *Shell) SetStdin(in io.Reader) {
	sh.stdin = in
}

// SetStdout sets stdout for commands
func (sh *Shell) SetStdout(out io.Writer) {
	sh.stdout = out
}

// SetStderr sets stderr for commands
func (sh *Shell) SetStderr(err io.Writer) {
	sh.stderr = err
}

func (sh *Shell) Stdout() io.Writer { return sh.stdout }
func (sh *Shell) Stderr() io.Writer { return sh.stderr }
func (sh *Shell) Stdin() io.Reader  { return sh.stdin }

func (sh *Shell) AddArgName(name string) {
	sh.argNames = append(sh.argNames, name)
}

func (sh *Shell) SetTree(t *ast.Tree) {
	sh.root = t
}

func (sh *Shell) Tree() *ast.Tree { return sh.root }

func (sh *Shell) SetRepr(a string) {
	sh.repr = a
}

func (sh *Shell) String() string {
	if sh.repr != "" {
		return sh.repr
	}

	var out bytes.Buffer

	sh.dump(&out)

	return string(out.Bytes())
}

func (sh *Shell) setup() error {
	err := sh.initEnv()

	if err != nil {
		return err
	}

	if sh.env["PROMPT"] == nil {
		pobj := NewStrObj(defPrompt)
		sh.Setenv("PROMPT", pobj)
		sh.Setvar("PROMPT", pobj)
	}

	return nil
}

func (sh *Shell) setupSignals() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGCHLD)

	go func() {
		for {
			sig := <-sigs

			switch sig {
			case syscall.SIGINT:
				sh.Lock()

				// TODO(i4k): Review implementation when interrupted inside
				// function loops
				if sh.looping {
					sh.setIntr(true)
				}

				sh.Unlock()
			case syscall.SIGCHLD:
				// dont need reaping because we dont have job control yet
				// every command is wait'ed.
			default:
				fmt.Printf("%s\n", sig)
			}
		}
	}()
}

func (sh *Shell) TriggerCTRLC() error {
	p, err := os.FindProcess(os.Getpid())

	if err != nil {
		return err
	}

	return p.Signal(syscall.SIGINT)
}

// setIntr *do not lock*. You must do it yourself!
func (sh *Shell) setIntr(b bool) {
	if sh.parent != nil {
		sh.parent.setIntr(b)
		return
	}

	sh.interrupted = b
}

// getIntr returns true if nash was interrupted by CTRL-C
func (sh *Shell) getIntr() bool {
	if sh.parent != nil {
		return sh.parent.getIntr()
	}

	return sh.interrupted
}

func (sh *Shell) executeConcat(path *ast.Arg) (string, error) {
	var pathStr string

	concat := path.Concat()

	for i := 0; i < len(concat); i++ {
		part := concat[i]

		if part.IsConcat() {
			return "", errors.NewError("Nested concat is not allowed")
		}

		if part.IsVariable() {
			partValues, err := sh.evalVariable(part)

			if err != nil {
				return "", err
			}

			if partValues.Type() == ListType {
				return "", fmt.Errorf("Concat of list variables is not allowed: %s = %v", part.Value(), partValues)
			} else if partValues.Type() != StringType {
				return "", fmt.Errorf("Invalid concat element: %v", partValues)
			}

			pathStr += partValues.Str()
		} else if part.IsQuoted() || part.IsUnquoted() {
			pathStr += part.Value()
		} else if part.IsList() {
			return "", errors.NewError("Concat of lists is not allowed: %+v", part.List())
		}
	}

	return pathStr, nil
}

func (sh *Shell) Execute() (*Obj, error) {
	if sh.root != nil {
		return sh.ExecuteTree(sh.root)
	}

	return nil, nil
}

// ExecuteString executes the commands specified by string content
func (sh *Shell) ExecuteString(path, content string) error {
	parser := parser.NewParser(path, content)

	tr, err := parser.Parse()

	if err != nil {
		return err
	}

	_, err = sh.ExecuteTree(tr)
	return err
}

// Execute the nash file at given path
func (sh *Shell) ExecuteFile(path string) error {
	bkCurFile := sh.currentFile

	content, err := ioutil.ReadFile(path)

	if err != nil {
		return err
	}

	sh.currentFile = path

	defer func() {
		sh.currentFile = bkCurFile
	}()

	return sh.ExecuteString(path, string(content))
}

func (sh *Shell) executeNode(node ast.Node, builtin bool) (*Obj, error) {
	var (
		obj *Obj
		err error
	)

	sh.logf("Executing node: %v\n", node)

	switch node.Type() {
	case ast.NodeBuiltin:
		err = sh.executeBuiltin(node.(*ast.BuiltinNode))
	case ast.NodeImport:
		err = sh.executeImport(node.(*ast.ImportNode))
	case ast.NodeShowEnv:
		err = sh.executeShowEnv(node.(*ast.ShowEnvNode))
	case ast.NodeComment:
		// ignore
	case ast.NodeSetAssignment:
		err = sh.executeSetAssignment(node.(*ast.SetAssignmentNode))
	case ast.NodeAssignment:
		err = sh.executeAssignment(node.(*ast.AssignmentNode))
	case ast.NodeCmdAssignment:
		err = sh.executeCmdAssignment(node.(*ast.CmdAssignmentNode))
	case ast.NodeCommand:
		err = sh.executeCommand(node.(*ast.CommandNode))
	case ast.NodePipe:
		err = sh.executePipe(node.(*ast.PipeNode))
	case ast.NodeRfork:
		err = sh.executeRfork(node.(*ast.RforkNode))
	case ast.NodeCd:
		err = sh.executeCd(node.(*ast.CdNode), builtin)
	case ast.NodeIf:
		err = sh.executeIf(node.(*ast.IfNode))
	case ast.NodeFnDecl:
		err = sh.executeFnDecl(node.(*ast.FnDeclNode))
	case ast.NodeFnInv:
		// invocation ignoring output
		_, err = sh.executeFnInv(node.(*ast.FnInvNode))
	case ast.NodeFor:
		err = sh.executeFor(node.(*ast.ForNode))
	case ast.NodeBindFn:
		err = sh.executeBindFn(node.(*ast.BindFnNode))
	case ast.NodeDump:
		err = sh.executeDump(node.(*ast.DumpNode))
	case ast.NodeReturn:
		if sh.IsFn() {
			obj, err = sh.executeReturn(node.(*ast.ReturnNode))
		} else {
			err = errors.NewError("Unexpected return outside of function declaration.")
		}
	default:
		// should never get here
		return nil, errors.NewError("invalid node: %v.", node.Type())
	}

	return obj, err
}

// ExecuteTree evaluates the given tree
func (sh *Shell) ExecuteTree(tr *ast.Tree) (*Obj, error) {
	if tr == nil || tr.Root == nil {
		return nil, errors.NewError("nothing parsed")
	}

	root := tr.Root

	for _, node := range root.Nodes {
		obj, err := sh.executeNode(node, false)

		if err != nil {
			type IgnoreError interface {
				Ignore() bool
			}

			if errIgnore, ok := err.(IgnoreError); ok && errIgnore.Ignore() {
				continue
			}

			type InterruptedError interface {
				Interrupted() bool
			}

			if errInterrupted, ok := err.(InterruptedError); ok && errInterrupted.Interrupted() {
				return obj, err
			}

			return nil, err
		}

		if node.Type() == ast.NodeReturn {
			return obj, nil
		}
	}

	return nil, nil
}

func (sh *Shell) executeReturn(n *ast.ReturnNode) (*Obj, error) {
	if n.Return() == nil {
		return nil, nil
	}

	return sh.evalArg(n.Return())
}

func (sh *Shell) executeBuiltin(node *ast.BuiltinNode) error {
	// cd and for does not return data
	_, err := sh.executeNode(node.Stmt(), true)
	return err
}

func (sh *Shell) executeImport(node *ast.ImportNode) error {
	arg := node.Path()

	obj, err := sh.evalArg(arg)

	if err != nil {
		return err
	}

	if obj.Type() != StringType {
		return errors.NewError("Invalid type on import argument: %s", obj.Type())
	}

	fname := obj.Str()

	sh.logf("Importing '%s'", fname)

	if len(fname) > 0 && fname[0] == '/' {
		return sh.ExecuteFile(fname)
	}

	tries := make([]string, 0, 4)
	tries = append(tries, fname)

	if sh.currentFile != "" {
		tries = append(tries, path.Dir(sh.currentFile)+"/"+fname)
	}

	tries = append(tries, sh.dotDir+"/"+fname)
	tries = append(tries, sh.dotDir+"/lib/"+fname)

	sh.logf("Trying %q\n", tries)

	for _, path := range tries {
		d, err := os.Stat(path)

		if err != nil {
			continue
		}

		if m := d.Mode(); !m.IsDir() {
			return sh.ExecuteFile(path)
		}
	}

	return errors.NewError("Failed to import path '%s'. The locations below have been tried:\n \"%s\"",
		fname,
		strings.Join(tries, `", "`))
}

func (sh *Shell) executeShowEnv(node *ast.ShowEnvNode) error {
	envVars := buildenv(sh.Environ())
	for _, e := range envVars {
		fmt.Fprintf(sh.stdout, "%s\n", e)
	}

	return nil
}

func (sh *Shell) executePipe(pipe *ast.PipeNode) error {
	var err error
	nodeCommands := pipe.Commands()

	if len(nodeCommands) <= 1 {
		return errors.NewError("Pipe requires at least two commands.")
	}

	cmds := make([]*Command, len(nodeCommands))

	// Create all commands
	for i := 0; i < len(nodeCommands); i++ {
		nodeCmd := nodeCommands[i]
		cmd, err := NewCommand(nodeCmd.Name(), sh)

		if err != nil {
			return err
		}

		err = cmd.SetArgs(nodeCmd.Args())

		if err != nil {
			return err
		}

		cmd.SetPassDone(false)

		if i < (len(nodeCommands) - 1) {
			err = cmd.SetRedirects(nodeCmd.Redirects())

			if err != nil {
				return err
			}
		}

		cmds[i] = cmd
	}

	last := len(nodeCommands) - 1

	// Setup the commands. Pointing the stdin of next command to stdout of previous.
	// Except the last one
	for i, cmd := range cmds[:last] {
		//cmd.SetFDMap(0, sh.stdin)
		//cmd.SetFDMap(1, sh.stdout)
		//cmd.SetFDMap(2, sh.stderr)

		// connect commands
		if cmds[i+1].Stdin != os.Stdin {
			return errors.NewError("Stdin redirected")
		}

		if cmd.Stdout != os.Stdout {
			return errors.NewError("Stdout redirected")
		}

		cmds[i+1].Stdin = nil
		cmd.Stdout = nil

		if cmds[i+1].Stdin, err = cmd.StdoutPipe(); err != nil {
			return err
		}

		cmd.stdoutDone <- true
		cmd.stdinDone <- true
		cmd.stderrDone <- true
	}

	cmds[last].stdinDone <- true

	if sh.stdout != os.Stdout {
		cmds[last].Stdout = nil
		stdout, err := cmds[last].StdoutPipe()

		if err != nil {
			return err
		}

		go func() {
			io.Copy(sh.stdout, stdout)
			cmds[last].stdoutDone <- true
		}()
	} else {
		cmds[last].Stdout = sh.stdout
		cmds[last].stdoutDone <- true
	}

	if sh.stderr != os.Stderr {
		cmds[last].Stderr = nil
		stderr, err := cmds[last].StderrPipe()

		if err != nil {
			return err
		}

		go func() {
			io.Copy(sh.stderr, stderr)
			cmds[last].stderrDone <- true
		}()
	} else {
		cmds[last].Stderr = sh.stderr
		cmds[last].stderrDone <- true
	}

	for _, cmd := range cmds {
		err := cmd.Start()

		if err != nil {
			return err
		}
	}

	for _, cmd := range cmds {
		err := cmd.Wait()

		if err != nil {
			return err
		}
	}

	return nil
}

func (sh *Shell) executeCommand(c *ast.CommandNode) error {
	var (
		ignoreError bool
		status      = 127
	)

	cmdName := c.Name()

	sh.logf("Executing: %s\n", c.Name())

	if len(cmdName) > 1 && cmdName[0] == '-' {
		ignoreError = true
		cmdName = cmdName[1:]

		sh.logf("Ignoring error\n")
	}

	cmd, err := NewCommand(cmdName, sh)

	if err != nil {
		type NotFound interface {
			NotFound() bool
		}

		sh.logf("Command fails: %s", err.Error())

		if errNotFound, ok := err.(NotFound); ok && errNotFound.NotFound() {
			if fn, ok := sh.Getbindfn(cmdName); ok {
				sh.logf("Executing bind %s", cmdName)

				if len(c.Args()) > len(fn.argNames) {
					err = errors.NewError("Too much arguments for"+
						" function '%s'. It expects %d args, but given %d. Arguments: %q",
						fn.name,
						len(fn.argNames),
						len(c.Args()), c.Args())
					goto cmdError
				}

				for i := 0 + len(c.Args()); i < len(fn.argNames); i++ {
					c.SetArgs(append(c.Args(), ast.NewArg(0, ast.ArgQuoted)))
				}

				_, err = sh.executeFn(fn, c.Args())

				if err != nil {
					goto cmdError
				}

				return nil
			}
		}

		goto cmdError
	}

	err = cmd.SetArgs(c.Args())

	if err != nil {
		goto cmdError
	}

	cmd.SetFDMap(0, sh.stdin)
	cmd.SetFDMap(1, sh.stdout)
	cmd.SetFDMap(2, sh.stderr)

	err = cmd.SetRedirects(c.Redirects())

	if err != nil {
		goto cmdError
	}

	defer cmd.CloseNetDescriptors()

	err = cmd.Start()

	if err != nil {
		goto cmdError
	}

	err = cmd.Wait()

	if err != nil {
		goto cmdError
	}

	sh.Setvar("status", NewStrObj("0"))

	return nil

cmdError:
	if exiterr, ok := err.(*exec.ExitError); ok {
		if statusObj, ok := exiterr.Sys().(syscall.WaitStatus); ok {
			status = statusObj.ExitStatus()
		}
	}

	sh.Setvar("status", NewStrObj(strconv.Itoa(status)))

	if ignoreError {
		return newErrIgnore(err.Error())
	}

	return err
}

func (sh *Shell) evalVariable(a *ast.Arg) (*Obj, error) {
	var (
		v  *Obj
		ok bool
	)

	if a.ArgType() != ast.ArgVariable {
		return nil, errors.NewError("Invalid eval of non variable argument: %s", a)
	}

	varName := a.Value()

	if v, ok = sh.GetVar(varName[1:]); !ok {
		return nil, fmt.Errorf("Variable %s not set", varName)
	}

	if a.Index() != nil {
		if v.Type() != ListType {
			return nil, errors.NewError("Invalid indexing of non-list variable: %s", v.Type())
		}

		var (
			indexNum int
			err      error
		)

		idxArg := a.Index()

		if idxArg.ArgType() == ast.ArgNumber {
			indexNum, err = strconv.Atoi(idxArg.Value())

			if err != nil {
				return nil, err
			}
		} else if idxArg.ArgType() == ast.ArgVariable {
			idxObj, err := sh.evalVariable(idxArg)

			if err != nil {
				return nil, err
			}

			if idxObj.Type() != StringType {
				return nil, errors.NewError("Invalid object type on index value: %s", idxObj.Type())
			}

			idxVal := idxObj.Str()
			indexNum, err = strconv.Atoi(idxVal)

			if err != nil {
				return nil, err
			}
		}

		values := v.List()

		if indexNum < 0 || indexNum >= len(values) {
			return nil, errors.NewError("Index out of bounds. len(%s) == %d, but given %d", varName, len(values), indexNum)
		}

		value := values[indexNum]
		return NewStrObj(value), nil
	}

	return v, nil
}

func (sh *Shell) evalArg(arg *ast.Arg) (*Obj, error) {
	if arg.IsQuoted() || arg.IsUnquoted() {
		return NewStrObj(arg.Value()), nil
	} else if arg.IsConcat() {
		argVal, err := sh.executeConcat(arg)

		if err != nil {
			return nil, err
		}

		return NewStrObj(argVal), nil
	} else if arg.IsVariable() {
		obj, err := sh.evalVariable(arg)

		if err != nil {
			return nil, err
		}

		return obj, nil
	} else if arg.IsList() {
		argList := arg.List()
		values := make([]string, 0, len(argList))

		for _, arg := range argList {
			obj, err := sh.evalArg(arg)

			if err != nil {
				return nil, err
			}

			if obj.Type() != StringType {
				return nil, errors.NewError("Nested lists are not supported")
			}

			values = append(values, obj.Str())
		}

		return NewListObj(values), nil
	}

	return nil, errors.NewError("Invalid argument type: %+v", arg)
}

func (sh *Shell) executeSetAssignment(v *ast.SetAssignmentNode) error {
	var (
		varValue *Obj
		ok       bool
	)

	varName := v.Identifier()

	if varValue, ok = sh.GetVar(varName); !ok {
		return fmt.Errorf("Variable '%s' not set", varName)
	}

	sh.Setenv(varName, varValue)

	return nil
}

func (sh *Shell) concatElements(elem *ast.Arg) (string, error) {
	value := ""

	concat := elem.Concat()
	for i := 0; i < len(concat); i++ {
		ec := concat[i]

		obj, err := sh.evalArg(ec)

		if err != nil {
			return "", err
		}

		if obj.Type() != StringType {
			return "", errors.NewError("Impossible to concat elements of type %s", obj.Type())
		}

		value = value + obj.String()
	}

	return value, nil
}

func (sh *Shell) executeCmdAssignment(v *ast.CmdAssignmentNode) error {
	var (
		varOut bytes.Buffer
		err    error
	)

	bkStdout := sh.stdout

	sh.SetStdout(&varOut)

	defer sh.SetStdout(bkStdout)

	assign := v.Command()

	switch assign.Type() {
	case ast.NodeCommand:
		err = sh.executeCommand(assign.(*ast.CommandNode))
	case ast.NodePipe:
		err = sh.executePipe(assign.(*ast.PipeNode))
	case ast.NodeFnInv:
		fnValues, err := sh.executeFnInv(assign.(*ast.FnInvNode))

		if err != nil {
			return err
		}

		sh.Setvar(v.Name(), fnValues)
		return nil
	default:
		err = errors.NewError("Unexpected node in assignment: %s", assign.String())
	}

	if err != nil {
		return err
	}

	var strelems []string

	outStr := string(varOut.Bytes())

	if ifs, ok := sh.GetVar("IFS"); ok && ifs.Type() == ListType {
		strelems = strings.FieldsFunc(outStr, func(r rune) bool {
			for _, delim := range ifs.List() {
				if len(delim) > 0 && rune(delim[0]) == r {
					return true
				}
			}

			return false
		})

		sh.Setvar(v.Name(), NewListObj(strelems))
	} else {
		sh.Setvar(v.Name(), NewStrObj(outStr))
	}

	return nil
}

func (sh *Shell) executeAssignment(v *ast.AssignmentNode) error {
	var err error

	obj, err := sh.evalArg(v.Value())

	if err != nil {
		return err
	}

	sh.Setvar(v.Identifier(), obj)
	return nil
}

func (sh *Shell) executeBuiltinCd(cd *ast.CdNode) error {
	var (
		pathlist []string
		pathStr  string
	)

	path := cd.Dir()

	if path == nil {
		pathobj, ok := sh.Getenv("HOME")

		if !ok {
			return errors.NewError("Nash don't know where to cd. No variable $HOME set")
		}

		if pathobj.Type() != StringType {
			return fmt.Errorf("Invalid $HOME value: %v", pathlist)
		}

		pathStr = pathobj.Str()
	} else {
		obj, err := sh.evalArg(path)

		if err != nil {
			return err
		}

		if obj.Type() != StringType {
			return errors.NewError("HOME variable has invalid type: %s", obj.Type())
		}

		pathStr = obj.Str()
	}

	err := os.Chdir(pathStr)

	if err != nil {
		return err
	}

	pwd, ok := sh.GetVar("PWD")

	if !ok {
		return fmt.Errorf("Variable $PWD is not set")
	}

	cpwd := NewStrObj(pathStr)

	sh.Setvar("OLDPWD", pwd)
	sh.Setvar("PWD", cpwd)
	sh.Setenv("OLDPWD", pwd)
	sh.Setenv("PWD", cpwd)

	return nil
}

func (sh *Shell) executeCd(cd *ast.CdNode, builtin bool) error {
	var (
		cdAlias  *Shell
		hasAlias bool
	)

	if cdAlias, hasAlias = sh.Getbindfn("cd"); !hasAlias || builtin {
		return sh.executeBuiltinCd(cd)
	}

	path := cd.Dir()

	args := make([]*ast.Arg, 0, 1)

	if path != nil {
		args = append(args, path)
	} else {
		// empty arg
		args = append(args, ast.NewArg(0, ast.ArgQuoted))
	}

	_, err := sh.executeFn(cdAlias, args)
	return err
}

func (sh *Shell) evalIfArguments(n *ast.IfNode) (string, string, error) {
	lvalue := n.Lvalue()
	rvalue := n.Rvalue()

	lobj, err := sh.evalArg(lvalue)

	if err != nil {
		return "", "", err
	}

	robj, err := sh.evalArg(rvalue)

	if err != nil {
		return "", "", err
	}

	if lobj.Type() != StringType {
		return "", "", errors.NewError("lvalue is not comparable.")
	}

	if robj.Type() != StringType {
		return "", "", errors.NewError("rvalue is not comparable")
	}

	return lobj.Str(), robj.Str(), nil
}

func (sh *Shell) executeIfEqual(n *ast.IfNode) error {
	lstr, rstr, err := sh.evalIfArguments(n)

	if err != nil {
		return err
	}

	if lstr == rstr {
		_, err = sh.ExecuteTree(n.IfTree())
		return err
	} else if n.ElseTree() != nil {
		_, err = sh.ExecuteTree(n.ElseTree())
		return err
	}

	return nil
}

func (sh *Shell) executeIfNotEqual(n *ast.IfNode) error {
	lstr, rstr, err := sh.evalIfArguments(n)

	if err != nil {
		return err
	}

	if lstr != rstr {
		_, err = sh.ExecuteTree(n.IfTree())
		return err
	} else if n.ElseTree() != nil {
		_, err = sh.ExecuteTree(n.ElseTree())
		return err
	}

	return nil
}

func (sh *Shell) executeFn(fn *Shell, args []*ast.Arg) (*Obj, error) {
	if len(fn.argNames) != len(args) {
		return nil, errors.NewError("Wrong number of arguments for function %s. Expected %d but found %d",
			fn.name, len(fn.argNames), len(args))
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		argName := fn.argNames[i]

		obj, err := sh.evalArg(arg)

		if err != nil {
			return nil, err
		}

		fn.Setvar(argName, obj)
	}

	return fn.Execute()
}

func (sh *Shell) executeFnInv(n *ast.FnInvNode) (*Obj, error) {
	fnName := n.Name()

	if len(fnName) > 0 && fnName[0] == '$' {
		argVar := ast.NewArg(n.Position(), ast.ArgVariable)
		argVar.SetString(fnName)

		obj, err := sh.evalVariable(argVar)

		if err != nil {
			return nil, err
		}

		if obj.Type() != FnType {
			return nil, errors.NewError("Variable '%s' isnt a function.", fnName)
		}

		return sh.executeFn(obj.Fn(), n.Args())
	}

	if fn, ok := sh.GetFn(n.Name()); ok {
		return sh.executeFn(fn, n.Args())
	}

	return nil, errors.NewError("no such function '%s'", fnName)
}

func (sh *Shell) executeInfLoop(tr *ast.Tree) error {
	var err error

	for {
		_, err = sh.ExecuteTree(tr)

		type interruptedError interface {
			Interrupted() bool
		}

		if errInterrupted, ok := err.(interruptedError); ok && errInterrupted.Interrupted() {
			break
		}

		sh.Lock()

		if sh.getIntr() {
			sh.setIntr(false)

			if err != nil {
				err = newErrInterrupted(err.Error())
			} else {
				err = newErrInterrupted("loop interrupted")
			}
		}

		sh.Unlock()

		if err != nil {
			break
		}
	}

	return err
}

func (sh *Shell) executeFor(n *ast.ForNode) error {
	sh.Lock()
	sh.looping = true
	sh.Unlock()

	defer func() {
		sh.Lock()
		defer sh.Unlock()

		sh.looping = false
	}()

	if n.InVar() == "" {
		return sh.executeInfLoop(n.Tree())
	}

	id := n.Identifier()
	inVar := n.InVar()

	argVar := ast.NewArg(n.Position(), ast.ArgVariable)
	argVar.SetString(inVar)

	obj, err := sh.evalVariable(argVar)

	if err != nil {
		return err
	}

	if obj.Type() != ListType {
		return errors.NewError("Invalid variable type in for range: %s", obj.Type())
	}

	for _, val := range obj.List() {
		sh.Setvar(id, NewStrObj(val))

		_, err = sh.ExecuteTree(n.Tree())

		type interruptedError interface {
			Interrupted() bool
		}

		if errInterrupted, ok := err.(interruptedError); ok && errInterrupted.Interrupted() {
			return err
		}

		sh.Lock()

		if sh.getIntr() {
			sh.setIntr(false)
			sh.Unlock()

			if err != nil {
				return newErrInterrupted(err.Error())
			}

			return newErrInterrupted("loop interrupted")
		}

		sh.Unlock()

		if err != nil {
			return err
		}
	}

	return nil
}

func (sh *Shell) executeFnDecl(n *ast.FnDeclNode) error {
	fn, err := NewSubShell(n.Name(), sh)

	if err != nil {
		return err
	}

	fn.SetDebug(sh.debug)
	fn.SetStdout(sh.stdout)
	fn.SetStderr(sh.stderr)
	fn.SetStdin(sh.stdin)
	fn.SetRepr(n.String())
	fn.SetDotDir(sh.dotDir)

	args := n.Args()

	for i := 0; i < len(args); i++ {
		arg := args[i]

		fn.AddArgName(arg)
	}

	fn.SetTree(n.Tree())

	fnName := n.Name()

	if fnName == "" {
		fnName = fmt.Sprintf("lambda %d", int(sh.lambdas))
		sh.lambdas++
	}

	sh.fns[fnName] = fn

	sh.Setvar(fnName, NewFnObj(fn))
	sh.logf("Function %s declared on '%s'", fnName, sh.name)

	return nil
}

func (sh *Shell) dumpVar(file io.Writer) {
	for n, v := range sh.vars {
		printVar(file, n, v)
	}
}

func (sh *Shell) dumpEnv(file io.Writer) {
	for n := range sh.env {
		printEnv(file, n)
	}
}

func (sh *Shell) dumpFns(file io.Writer) {
	for _, f := range sh.fns {
		fmt.Fprintf(file, "%s\n\n", f.String())
	}
}

func (sh *Shell) dump(out io.Writer) {
	sh.dumpVar(out)
	sh.dumpEnv(out)
	sh.dumpFns(out)
}

func (sh *Shell) executeDump(n *ast.DumpNode) error {
	var (
		err  error
		file io.Writer
		obj  *Obj
	)

	fnameArg := n.Filename()

	if fnameArg == nil {
		file = sh.stdout
		goto execDump
	}

	obj, err = sh.evalArg(fnameArg)

	if err != nil {
		return err
	}

	if obj.Type() != StringType {
		return errors.NewError("dump does not support argument of type %s", obj.Type())
	}

	file, err = os.OpenFile(obj.Str(), os.O_CREATE|os.O_RDWR, 0644)

	if err != nil {
		return err
	}

execDump:
	sh.dump(file)

	return nil
}

func (sh *Shell) executeBindFn(n *ast.BindFnNode) error {
	if fn, ok := sh.GetFn(n.Name()); ok {
		sh.Setbindfn(n.CmdName(), fn)
	} else {
		return errors.NewError("No such function '%s'", n.Name())
	}

	return nil
}

func (sh *Shell) executeIf(n *ast.IfNode) error {
	op := n.Op()

	if op == "==" {
		return sh.executeIfEqual(n)
	} else if op == "!=" {
		return sh.executeIfNotEqual(n)
	}

	return fmt.Errorf("Invalid operation '%s'.", op)
}

func nashdAutoDiscover() string {
	path, err := os.Readlink("/proc/self/exe")

	if err != nil {
		path = os.Args[0]

		if _, err := os.Stat(path); err != nil {
			return ""
		}
	}

	return path
}
