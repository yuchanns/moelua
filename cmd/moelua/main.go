// Command moelua is the standalone pure-Go Lua 5.5.1 interpreter CLI,
// implementing PUC-Rio lua.c flags (-e, -l, -i, -v, -W, -E, --, -),
// LUA_INIT_5_5/LUA_INIT, arg table, interactive REPL, and SIGINT handling.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"

	"github.com/yuchanns/moelua/vm"
)

const luaReleaseBanner = "Lua 5.5.1  Copyright (C) 1994-2026 Lua.org, PUC-Rio"

type cliFlags struct {
	hasE    bool
	hasI    bool
	hasV    bool
	hasNoE  bool // -E
	script  int  // index in os.Args of first script/non-option arg (0 if none)
	badOpt  string
	needArg string
}

func collectArgs(argv []string) cliFlags {
	var f cliFlags
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") {
			f.script = i
			return f
		}
		if arg == "--" {
			if i+1 < len(argv) {
				f.script = i + 1
			}
			return f
		}
		if arg == "-" {
			f.script = i
			return f
		}
		switch arg[1] {
		case 'E':
			if len(arg) > 2 {
				f.badOpt = arg
				return f
			}
			f.hasNoE = true
		case 'W':
			if len(arg) > 2 {
				f.badOpt = arg
				return f
			}
		case 'i':
			if len(arg) > 2 {
				f.badOpt = arg
				return f
			}
			f.hasI = true
			f.hasV = true
		case 'v':
			if len(arg) > 2 {
				f.badOpt = arg
				return f
			}
			f.hasV = true
		case 'e':
			f.hasE = true
			if len(arg) == 2 {
				i++
				// lua.c:collectargs rejects a missing separated argument or one that is another option.
				if i >= len(argv) || strings.HasPrefix(argv[i], "-") {
					f.needArg = "-e"
					return f
				}
			}
		case 'l':
			if len(arg) == 2 {
				i++
				// lua.c:collectargs rejects a missing separated argument or one that is another option.
				if i >= len(argv) || strings.HasPrefix(argv[i], "-") {
					f.needArg = "-l"
					return f
				}
			}
		default:
			f.badOpt = arg
			return f
		}
	}
	return f
}

func printUsage(progName, badOpt, needArg string) {
	if needArg != "" {
		fmt.Fprintf(os.Stderr, "%s: '%s' needs argument\n", progName, needArg)
	} else if badOpt != "" {
		fmt.Fprintf(os.Stderr, "%s: unrecognized option '%s'\n", progName, badOpt)
	}
	fmt.Fprintf(os.Stderr, "usage: %s [options] [script [args]]\n"+
		"Available options are:\n"+
		"  -e stat   execute string 'stat'\n"+
		"  -i        enter interactive mode after executing 'script'\n"+
		"  -l mod    require library 'mod' into global 'mod'\n"+
		"  -l g=mod  require library 'mod' into global 'g'\n"+
		"  -v        show version information\n"+
		"  -E        ignore environment variables\n"+
		"  -W        turn warnings on\n"+
		"  --        stop handling options\n"+
		"  -         stop handling options and execute stdin\n", progName)
}

func createArgTable(s *vm.State, argv []string, script int) {
	narg := max(0, len(argv)-(script+1))
	argTbl := s.NewTable(narg, script+1)
	for i := 0; i < len(argv); i++ {
		argTbl.RawSetInt(s, int64(i-script), vm.StringValue(s.Intern(argv[i])))
	}
	s.SetGlobal("arg", vm.TableValue(argTbl))
}

var intrFlag atomic.Bool

func armInterruptHandler(s *vm.State) func() {
	intrFlag.Store(false)
	sigCh := make(chan os.Signal, 2)
	done := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt)
	s.SetInterruptFlag(&intrFlag)
	go func() {
		gotFirst := false
		for {
			select {
			case <-done:
				return
			case <-sigCh:
				// While os.execute runs a child process, ignore SIGINT in the parent
				// (matching POSIX system(3) / lua.c) so Ctrl-C only interrupts the child.
				if s.InChildExec() {
					continue
				}
				// Match lua.c:laction: first SIGINT interrupts Lua; second SIGINT terminates
				// immediately (even if Lua caught the first via pcall, or if POSIX '&'
				// started this process with SIGINT=SIG_IGN where signal.Reset would restore SIG_IGN).
				if gotFirst {
					os.Exit(130)
				}
				gotFirst = true
				intrFlag.Store(true)
			}
		}
	}()
	return func() {
		s.SetInterruptFlag(nil)
		signal.Stop(sigCh)
		close(done)
		intrFlag.Store(false)
	}
}

func lMessage(progName, msg string) {
	if progName != "" {
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s\n", msg)
	}
}

func luaToStringErr(s *vm.State, err error) string {
	var le *vm.LuaError
	if errors.As(err, &le) {
		if le.Val.IsString() || le.Val.IsNumber() {
			return s.FormatValue(le.Val)
		}
		return "(null)"
	}
	return err.Error()
}

type cliState struct {
	s    *vm.State
	msgh vm.Value
}

func newCLIState(noEnv bool) *cliState {
	var s *vm.State
	if noEnv {
		s = vm.NewStateNoEnv()
	} else {
		s = vm.NewState()
	}
	// Match luaL_newstate (lauxlib.c:1201): warnings start ON so LUA_INIT warnings are emitted;
	// runArgs turns them off after handleLuaInit unless -W is passed.
	s.SetWarnings(true)
	// Match lua.c:754: stand-alone interpreter switches GC to generational mode at startup.
	s.SetGCGenerational()

	// Native message handler matching lua.c:142-154 (msghandler) without pushing a Lua wrapper frame.
	var msghNF *vm.NativeFunc
	msghNF = &vm.NativeFunc{
		Name: "msghandler",
		Fn: func(s *vm.State, args []vm.Value) ([]vm.Value, error) {
			errVal := vm.NilValue()
			if len(args) > 0 {
				errVal = args[0]
			}
			var msg string
			if errVal.IsString() || errVal.IsNumber() {
				msg = s.FormatValue(errVal)
			} else {
				if mt := s.GetMetatableOf(errVal); mt != nil {
					tm := mt.RawGetStr(s.Intern("__tostring"))
					if !tm.IsNil() {
						r, err := s.Call(tm, errVal)
						if err != nil {
							return nil, err
						}
						if len(r) > 0 && r[0].IsString() {
							return []vm.Value{r[0]}, nil
						}
					}
				}
				msg = fmt.Sprintf("(error object is a %s value)", errVal.TypeName())
			}
			tb := s.TracebackFromNative(msg, msghNF)
			return []vm.Value{vm.StringValue(s.Intern(tb))}, nil
		},
	}
	return &cliState{s: s, msgh: vm.NativeValue(msghNF)}
}

func (c *cliState) doCall(progName string, fn vm.Value, args ...vm.Value) ([]vm.Value, bool) {
	disarm := armInterruptHandler(c.s)
	defer disarm()
	res, errVal, ok, abortErr := c.s.CallProtected(fn, c.msgh, args...)
	if abortErr != nil {
		lMessage(progName, c.s.FormatError(abortErr))
		return nil, false
	}
	if !ok {
		msg := "(error message not a string)"
		if errVal.IsString() || errVal.IsNumber() {
			msg = c.s.FormatValue(errVal)
		}
		lMessage(progName, msg)
		return nil, false
	}
	return res, true
}

func (c *cliState) doChunkString(progName, chunkName, src string) bool {
	// Match lua.c:215 (dostring): luaL_loadbufferx(..., "t") with no BOM/# stripping and text-only mode.
	fn, err := c.s.LoadTextChunk([]byte(src), chunkName)
	if err != nil {
		lMessage(progName, c.s.FormatError(err))
		return false
	}
	_, ok := c.doCall(progName, fn)
	return ok
}

func (c *cliState) doChunkFile(progName, path string, literalFile, passArgs bool) bool {
	var fn vm.Value
	var err error
	if path == "-" && !literalFile {
		fn, err = c.s.LoadChunkReader(c.s.StdinReader(), "=stdin")
	} else {
		f, oerr := os.Open(path)
		if oerr != nil {
			reason := oerr.Error()
			var pe *os.PathError
			if errors.As(oerr, &pe) {
				reason = pe.Err.Error()
			}
			lMessage(progName, fmt.Sprintf("cannot open %s: %s", path, reason))
			return false
		}
		if info, serr := f.Stat(); serr == nil && info.IsDir() {
			_ = f.Close()
			lMessage(progName, fmt.Sprintf("cannot read %s", path))
			return false
		}
		fn, err = c.s.LoadChunkReader(f, "@"+path)
		_ = f.Close()
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				lMessage(progName, fmt.Sprintf("cannot read %s: %s", path, pe.Err.Error()))
				return false
			}
		}
	}
	if err != nil {
		lMessage(progName, c.s.FormatError(err))
		return false
	}
	var args []vm.Value
	if passArgs {
		argVal, gerr := c.s.GetGlobalMM("arg")
		if gerr != nil {
			lMessage(progName, c.s.FormatError(gerr))
			return false
		}
		if !argVal.IsTable() {
			lMessage(progName, "'arg' is not a table")
			return false
		}
		n, lerr := c.s.TableLen(argVal)
		if lerr != nil {
			lMessage(progName, c.s.FormatError(lerr))
			return false
		}
		if n > 1_000_000 {
			lMessage(progName, "too many arguments to script")
			return false
		}
		if n > 0 {
			argTbl := argVal.AsTable()
			args = make([]vm.Value, n)
			for i := int64(1); i <= n; i++ {
				args[i-1] = argTbl.RawGetInt(i)
			}
		}
	}
	_, ok := c.doCall(progName, fn, args...)
	return ok
}

func (c *cliState) doLibrary(progName, spec string) bool {
	globName := spec
	modName := spec
	if eq := strings.IndexByte(spec, '='); eq >= 0 {
		globName = spec[:eq]
		modName = spec[eq+1:]
	} else if dash := strings.IndexByte(globName, '-'); dash >= 0 {
		// Lua 5.5 lua.c:dolibrary: strip version suffix after '-' from implicit global name.
		globName = globName[:dash]
	}
	reqFn, gerr := c.s.GetGlobalMM("require")
	if gerr != nil {
		lMessage(progName, c.s.FormatError(gerr))
		return false
	}
	res, ok := c.doCall(progName, reqFn, vm.StringValue(c.s.Intern(modName)))
	if !ok {
		return false
	}
	val := vm.NilValue()
	if len(res) > 0 {
		val = res[0]
	}
	if serr := c.s.SetGlobalMM(globName, val); serr != nil {
		lMessage(progName, c.s.FormatError(serr))
		return false
	}
	return true
}

func (c *cliState) handleLuaInit(progName string) bool {
	name := "LUA_INIT_5_5"
	val, ok := os.LookupEnv(name)
	if !ok {
		name = "LUA_INIT"
		val, ok = os.LookupEnv(name)
	}
	if !ok || val == "" {
		return true
	}
	if strings.HasPrefix(val, "@") {
		return c.doChunkFile(progName, val[1:], true, false)
	}
	return c.doChunkString(progName, "="+name, val)
}

func (c *cliState) runArgs(progName string, argv []string, n int) bool {
	// Match lua.c:runargs: reset warnings off after LUA_INIT before processing CLI options.
	c.s.SetWarnings(false)
	for i := 1; i < n; i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if arg == "--" || arg == "-" {
			break
		}
		switch arg[1] {
		case 'W':
			c.s.SetWarnings(true)
		case 'e':
			chunk := arg[2:]
			if chunk == "" {
				i++
				chunk = argv[i]
			}
			if !c.doChunkString(progName, "=(command line)", chunk) {
				return false
			}
		case 'l':
			spec := arg[2:]
			if spec == "" {
				i++
				spec = argv[i]
			}
			if !c.doLibrary(progName, spec) {
				return false
			}
		}
	}
	return true
}

func getPrompt(s *vm.State, firstLine bool) string {
	name := "_PROMPT2"
	dflt := ">> "
	if firstLine {
		name = "_PROMPT"
		dflt = "> "
	}
	v, err := s.GetGlobalMM(name)
	if err != nil || v.IsNil() {
		return dflt
	}
	// Match lua.c:558: use luaL_tolstring (ToStringValue) directly without consulting _G.tostring.
	if p, serr := s.ToStringValue(v); serr == nil {
		return p
	}
	return dflt
}

func isIncompleteError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasSuffix(err.Error(), "near <eof>")
}

// hasLeadingLocalKeyword matches lua.c:621-630 (checklocal): skip space/tab, match "local"
// followed by space, tab, or end-of-line.
func hasLeadingLocalKeyword(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "local") {
		return false
	}
	if len(trimmed) == len("local") {
		return true
	}
	next := trimmed[len("local")]
	return next == ' ' || next == '\t'
}

func (c *cliState) doREPL(noEnv bool) {
	s := c.s
	if !noEnv {
		if rl, ok := os.LookupEnv("LUA_READLINELIB"); ok && rl != "" {
			// Match lua.c:531-533: emitted via lua_warning (visible only when warnings are on).
			s.EmitWarning(fmt.Sprintf("unable to load readline library '%s'", rl))
		}
	}
	for {
		fmt.Fprint(os.Stdout, getPrompt(s, true))
		line, err := s.ReadStdinLine()
		if err != nil && line == "" {
			fmt.Fprintln(os.Stdout)
			return
		}
		line = strings.TrimRight(line, "\r\n")

		// Match lua.c:611-612 (addreturn): try "return <line>;" in text-only mode first.
		fn, lerr := s.LoadTextChunk([]byte("return "+line+";"), "=stdin")
		if lerr != nil {
			// Fall back to statement mode with multi-line continuation on <eof>.
			buf := line
			if hasLeadingLocalKeyword(line) {
				fmt.Fprintln(os.Stderr, "warning: locals do not survive across lines in interactive mode")
			}
			for {
				fn, lerr = s.LoadTextChunk([]byte(buf), "=stdin")
				if lerr == nil || !isIncompleteError(lerr) {
					break
				}
				fmt.Fprint(os.Stdout, getPrompt(s, false))
				more, rerr := s.ReadStdinLine()
				if rerr != nil && more == "" {
					break
				}
				buf += "\n" + strings.TrimRight(more, "\r\n")
			}
		}
		if lerr != nil {
			// Match lua.c:701: progname = NULL inside doREPL (no progname prefix on REPL errors).
			lMessage("", s.FormatError(lerr))
			continue
		}
		res, ok := c.doCall("", fn)
		if !ok {
			continue
		}
		if len(res) > 0 {
			func() {
				refs := make([]int, len(res))
				for i, v := range res {
					refs[i] = s.Ref(v)
				}
				defer func() {
					for _, id := range refs {
						s.Unref(id)
					}
				}()
				printFn, gerr := s.GetGlobalMM("print")
				if gerr != nil {
					lMessage("", fmt.Sprintf("error calling 'print' (%s)", luaToStringErr(s, gerr)))
					return
				}
				if _, perr := s.Call(printFn, res...); perr != nil {
					lMessage("", fmt.Sprintf("error calling 'print' (%s)", luaToStringErr(s, perr)))
				}
			}()
		}
	}
}

func main() {
	argv := os.Args
	progName := "moelua"
	if len(argv) > 0 && argv[0] != "" {
		progName = argv[0]
	}

	flags := collectArgs(argv)
	if flags.badOpt != "" || flags.needArg != "" {
		printUsage(progName, flags.badOpt, flags.needArg)
		os.Exit(1)
	}

	c := newCLIState(flags.hasNoE)
	s := c.s

	if flags.hasV {
		fmt.Fprintln(os.Stdout, luaReleaseBanner)
	}

	createArgTable(s, argv, flags.script)

	if !flags.hasNoE {
		if !c.handleLuaInit(progName) {
			s.Close()
			os.Exit(1)
		}
	}

	optEnd := flags.script
	if optEnd == 0 {
		optEnd = len(argv)
	}
	if !c.runArgs(progName, argv, optEnd) {
		s.Close()
		os.Exit(1)
	}

	if flags.script > 0 {
		literalFile := flags.script > 1 && argv[flags.script-1] == "--"
		if !c.doChunkFile(progName, argv[flags.script], literalFile, true) {
			s.Close()
			os.Exit(1)
		}
	}

	if flags.hasI {
		c.doREPL(flags.hasNoE)
	} else if flags.script == 0 && !flags.hasE && !flags.hasV {
		if isStdinTerminal() {
			fmt.Fprintln(os.Stdout, luaReleaseBanner)
			c.doREPL(flags.hasNoE)
		} else {
			// Match lua.c:770: piped stdin with no script reports errors but exits 0.
			_ = c.doChunkFile(progName, "-", false, false)
		}
	}

	s.Close()
}
