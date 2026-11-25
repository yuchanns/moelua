// Command moelua is the standalone pure-Go Lua 5.5.1 interpreter CLI,
// implementing PUC-Rio lua.c flags (-e, -l, -i, -v, -W, -E, --, -),
// LUA_INIT_5_5/LUA_INIT, arg table, interactive REPL, and SIGINT handling.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/yuchanns/moelua/vm"
)

const luaReleaseBanner = "Lua 5.5.1  Copyright (C) 1994-2025 Lua.org, PUC-Rio"

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
				if i >= len(argv) {
					f.needArg = "-e"
					return f
				}
			}
		case 'l':
			if len(arg) == 2 {
				i++
				if i >= len(argv) {
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
		select {
		case <-done:
			return
		case <-sigCh:
			// Match lua.c:laction: restore default SIGINT disposition on the first signal
			// so a second SIGINT terminates the process even if Lua caught the first via pcall.
			signal.Reset(os.Interrupt)
			intrFlag.Store(true)
			select {
			case <-sigCh:
				os.Exit(130)
			default:
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

const cliRunnerSrc = `
local _xpcall, _getmt, _rawget, _tb, _type, _tostring = xpcall, debug.getmetatable, rawget, debug.traceback, type, tostring
local function invokeToString(tm, err)
	local r = tm(err)
	return r
end
local function msgh(err)
	local t = _type(err)
	if t == "string" or t == "number" then
		return _tb(_tostring(err), 2)
	end
	local mt = _getmt(err)
	local tm = _type(mt) == "table" and _rawget(mt, "__tostring") or nil
	if tm ~= nil then
		local r = invokeToString(tm, err)
		local rt = _type(r)
		if rt == "string" or rt == "number" then
			return _tb(_tostring(r), 2)
		end
	end
	return _tb("(error object is a " .. t .. " value)", 2)
end
return function(fn, ...)
	return _xpcall(fn, msgh, ...)
end
`

type cliState struct {
	s      *vm.State
	runner vm.Value
}

func newCLIState(noEnv bool) *cliState {
	var s *vm.State
	if noEnv {
		s = vm.NewStateNoEnv()
	} else {
		s = vm.NewState()
	}
	fn, err := s.LoadChunk([]byte(cliRunnerSrc), "=(cli)")
	if err != nil {
		panic(err)
	}
	res, err := s.Call(fn)
	if err != nil || len(res) == 0 {
		panic(fmt.Sprintf("cli runner init failed: %v", err))
	}
	return &cliState{s: s, runner: res[0]}
}

func (c *cliState) doCall(progName string, fn vm.Value, args ...vm.Value) ([]vm.Value, bool) {
	disarm := armInterruptHandler(c.s)
	defer disarm()
	callArgs := make([]vm.Value, 0, 1+len(args))
	callArgs = append(callArgs, fn)
	callArgs = append(callArgs, args...)
	res, err := c.s.Call(c.runner, callArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(err))
		return nil, false
	}
	if len(res) > 0 && res[0].IsFalsy() {
		errMsg := ""
		if len(res) > 1 {
			if res[1].IsString() {
				errMsg = res[1].AsString().S
			} else {
				errMsg = vm.FormatValue(res[1])
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, errMsg)
		return nil, false
	}
	if len(res) > 1 {
		return res[1:], true
	}
	return nil, true
}

func (c *cliState) doChunkString(progName, chunkName, src string) bool {
	fn, err := c.s.LoadChunk([]byte(src), chunkName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(err))
		return false
	}
	_, ok := c.doCall(progName, fn)
	return ok
}

func (c *cliState) doChunkFile(progName, path string, literalFile, passArgs bool) bool {
	var data []byte
	var err error
	chunkName := "=stdin"
	if path == "-" && !literalFile {
		data, err = c.s.ReadAllStdin()
	} else {
		chunkName = "@" + path
		data, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot open %s\n", progName, err.Error())
		return false
	}
	fn, err := c.s.LoadChunk(data, chunkName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(err))
		return false
	}
	var args []vm.Value
	if passArgs {
		argVal, gerr := c.s.GetGlobalMM("arg")
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(gerr))
			return false
		}
		if !argVal.IsTable() {
			fmt.Fprintf(os.Stderr, "%s: 'arg' is not a table\n", progName)
			return false
		}
		n, lerr := c.s.TableLen(argVal)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(lerr))
			return false
		}
		if n > 1_000_000 {
			fmt.Fprintf(os.Stderr, "%s: too many arguments to script\n", progName)
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
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(gerr))
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
		fmt.Fprintf(os.Stderr, "%s: %s\n", progName, c.s.FormatError(serr))
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
	if v.IsString() {
		return v.AsString().S
	}
	tsFn, err := s.GetGlobalMM("tostring")
	if err == nil && tsFn.IsFunction() {
		res, err := s.Call(tsFn, v)
		if err == nil && len(res) > 0 && res[0].IsString() {
			return res[0].AsString().S
		}
	}
	return dflt
}

func isIncompleteError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasSuffix(err.Error(), "near <eof>")
}

func hasLeadingLocalKeyword(line string) bool {
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, "local") {
		return false
	}
	if len(trimmed) == len("local") {
		return true
	}
	r := rune(trimmed[len("local")])
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

func (c *cliState) doREPL(progName string, noEnv bool) {
	s := c.s
	if !noEnv {
		if rl, ok := os.LookupEnv("LUA_READLINELIB"); ok && rl != "" {
			fmt.Fprintf(os.Stderr, "%s: warning: unable to load readline library '%s'\n", progName, rl)
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

		// Try "return <line>" first for interactive expression printing.
		fn, lerr := s.LoadChunk([]byte("return "+line), "=stdin")
		if lerr != nil {
			// Fall back to statement mode with multi-line continuation on <eof>.
			buf := line
			if hasLeadingLocalKeyword(line) {
				fmt.Fprintf(os.Stderr, "%s: warning: local variable in interactive mode is not kept across chunks\n", progName)
			}
			for {
				fn, lerr = s.LoadChunk([]byte(buf), "=stdin")
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
			fmt.Fprintf(os.Stderr, "%s: %s\n", progName, s.FormatError(lerr))
			continue
		}
		res, ok := c.doCall(progName, fn)
		if !ok {
			continue
		}
		if len(res) > 0 {
			printFn, gerr := s.GetGlobalMM("print")
			if gerr != nil {
				fmt.Fprintf(os.Stderr, "%s: error calling 'print' (%s)\n", progName, s.FormatError(gerr))
				continue
			}
			if _, perr := s.Call(printFn, res...); perr != nil {
				fmt.Fprintf(os.Stderr, "%s: error calling 'print' (%s)\n", progName, s.FormatError(perr))
			}
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
		c.doREPL(progName, flags.hasNoE)
	} else if flags.script == 0 && !flags.hasE && !flags.hasV {
		if isStdinTerminal() {
			fmt.Fprintln(os.Stdout, luaReleaseBanner)
			c.doREPL(progName, flags.hasNoE)
		} else {
			if !c.doChunkFile(progName, "-", false, false) {
				s.Close()
				os.Exit(1)
			}
		}
	}

	s.Close()
}
