package vm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"weak"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/internal/luastdlib"
)

const (
	bufModeNo   uint8 = 0
	bufModeFull uint8 = 1
	bufModeLine uint8 = 2
)

// fileInner holds the OS/buffer handles separately from luaFile so runtime.AddCleanup
// can safely flush buffered writes and close the fd when a dropped handle is GC'd
// without retaining a reference to the outer luaFile.
type fileInner struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	cmd    *exec.Cmd
	closed bool
	isStd  bool
}

func (fi *fileInner) flush() error {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.closed || fi.w == nil {
		return nil
	}
	return fi.w.Flush()
}

func (fi *fileInner) isClosed() bool {
	fi.mu.Lock()
	c := fi.closed
	fi.mu.Unlock()
	return c
}

func (fi *fileInner) flushAndClose() error {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.closed || fi.isStd || fi.f == nil {
		return nil
	}
	fi.closed = true
	var ferr error
	if fi.w != nil {
		ferr = fi.w.Flush()
		fi.w = nil
	}
	err := fi.f.Close()
	if err == nil {
		err = ferr
	}
	if fi.cmd != nil {
		werr := fi.cmd.Wait()
		if err == nil {
			err = werr
		}
	}
	return err
}

func cleanupFileInner(fi *fileInner) {
	_ = fi.flushAndClose()
}

type luaFile struct {
	f       *os.File
	r       *bufio.Reader
	w       *bufio.Writer
	inner   *fileInner
	bufMode uint8
	cmd     *exec.Cmd
	closed  bool
	isStd   bool
}

func (lf *luaFile) memBytes() int64 {
	if lf == nil || lf.closed {
		return 0
	}
	n := int64(64)
	if lf.w != nil {
		n += int64(lf.w.Size())
	}
	if lf.r != nil {
		n += int64(lf.r.Size())
	}
	return n
}

func (lf *luaFile) reader() *bufio.Reader {
	if lf.r == nil {
		lf.r = bufio.NewReader(lf.f)
	}
	return lf.r
}

func (lf *luaFile) flushWriter() error {
	if lf.inner != nil {
		lf.inner.mu.Lock()
		defer lf.inner.mu.Unlock()
		if lf.inner.closed {
			return nil
		}
		if lf.w != nil {
			err := lf.w.Flush()
			if err != nil {
				// Clear sticky bufio.Writer error so subsequent writes/close match C stdio
				// (e.g., files.lua:474-479 writing to /dev/full after a failed flush).
				lf.w.Reset(lf.f)
			}
			return err
		}
		return nil
	}
	if lf.w != nil {
		err := lf.w.Flush()
		if err != nil {
			lf.w.Reset(lf.f)
		}
		return err
	}
	return nil
}

func (lf *luaFile) closeFile() error {
	if lf.closed {
		return fmt.Errorf("attempt to use a closed file")
	}
	if lf.isStd {
		return fmt.Errorf("cannot close standard file")
	}
	lf.closed = true
	var err error
	if lf.inner != nil {
		lf.inner.mu.Lock()
		lf.inner.w = lf.w
		lf.inner.mu.Unlock()
		err = lf.inner.flushAndClose()
	} else {
		ferr := lf.flushWriter()
		err = lf.f.Close()
		if err == nil {
			err = ferr
		}
		if lf.cmd != nil {
			werr := lf.cmd.Wait()
			if err == nil {
				err = werr
			}
		}
	}
	lf.w = nil
	lf.r = nil
	return err
}

func (s *State) initIOOSPackageLibs() {
	s.initBaseExtra()
	s.initIOLib()
	s.initOSLib()
	s.initPackageLib()
}

func (s *State) initBaseExtra() {
	s.registerFunc(s.env, "warn", intrNone, nativeWarn)
	s.registerFunc(s.env, "load", intrNone, nativeLoad)
	s.registerFunc(s.env, "loadfile", intrNone, nativeLoadFile)
	s.registerFunc(s.env, "dofile", intrDoFile, nativeDoFile)
}

func (s *State) emitWarning(msg string) {
	if !s.warnOn {
		return
	}
	_, _ = fmt.Fprintf(s.errOut, "Lua warning: %s\n", msg)
}

func nativeWarn(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'warn' (string expected)")
	}
	first := args[0].AsString().S
	if len(args) == 1 && strings.HasPrefix(first, "@") {
		switch first {
		case "@on":
			s.warnOn = true
		case "@off":
			s.warnOn = false
		}
		return nil, nil
	}
	var sb strings.Builder
	for _, a := range args {
		if !a.IsString() && !a.IsNumber() {
			return nil, fmt.Errorf("bad argument to 'warn' (string expected)")
		}
		sb.WriteString(FormatValue(a))
	}
	s.emitWarning(sb.String())
	return nil, nil
}

func (s *State) loadChunkBytes(data []byte, chunkName, mode string, env Value, hasEnv bool) (Value, error) {
	isBin := len(data) > 0 && data[0] == 0x1b
	if isBin {
		if !strings.ContainsRune(mode, 'b') {
			return NilValue(), fmt.Errorf("attempt to load a binary chunk (mode is '%s')", mode)
		}
		p, err := bytecode.Undump(data)
		if err != nil {
			return NilValue(), err
		}
		cl, err := s.Load(p)
		if err != nil {
			return NilValue(), err
		}
		if hasEnv && len(cl.upvals) > 0 {
			cl.upvals[0] = &Upvalue{closed: env}
		}
		return ClosureValue(cl), nil
	}
	if !strings.ContainsRune(mode, 't') {
		return NilValue(), fmt.Errorf("attempt to load a text chunk (mode is '%s')", mode)
	}
	if s.compilerFn == nil {
		return NilValue(), fmt.Errorf("text compiler not configured")
	}
	p, err := s.compilerFn(chunkName, string(data))
	if err != nil {
		return NilValue(), err
	}
	cl, err := s.Load(p)
	if err != nil {
		return NilValue(), err
	}
	if hasEnv && len(cl.upvals) > 0 {
		cl.upvals[0] = &Upvalue{closed: env}
	}
	return ClosureValue(cl), nil
}

func nativeLoad(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'load'")
	}
	var data []byte
	chunkName := "=(load)"
	if args[0].IsString() || args[0].IsNumber() {
		str := FormatValue(args[0])
		data = []byte(str)
		chunkName = str
	} else if args[0].IsFunction() {
		var buf bytes.Buffer
		var readerBytes int64
		defer func() {
			s.gc.tempBytes -= readerBytes
		}()
		for {
			res, err := s.Call(args[0])
			if err != nil {
				if isUncatchableErr(err) {
					return nil, err
				}
				return []Value{NilValue(), errorToLuaValue(s, err)}, nil
			}
			if len(res) == 0 || res[0].IsNil() {
				break
			}
			if !res[0].IsString() && !res[0].IsNumber() {
				return []Value{NilValue(), StringValue(s.intern.Intern("reader function must return a string"))}, nil
			}
			piece := FormatValue(res[0])
			if piece == "" {
				break
			}
			pieceLen := int64(len(piece))
			if err := s.checkMem(pieceLen); err != nil {
				return []Value{NilValue(), errorToLuaValue(s, err)}, nil
			}
			if readerBytes+pieceLen > (1 << 30) {
				return []Value{NilValue(), StringValue(s.intern.Intern("not enough memory"))}, nil
			}
			buf.WriteString(piece)
			readerBytes += pieceLen
			s.gc.tempBytes += pieceLen
		}
		data = buf.Bytes()
	} else {
		return nil, fmt.Errorf("bad argument #1 to 'load' (string or function expected)")
	}
	if len(args) >= 2 && !args[1].IsNil() {
		chunkName = FormatValue(args[1])
	}
	mode := "bt"
	if len(args) >= 3 && !args[2].IsNil() {
		if !args[2].IsString() {
			return nil, fmt.Errorf("bad argument #3 to 'load' (string expected)")
		}
		mode = args[2].AsString().S
		// Lua 5.5 rejects fixed-buffer mode 'B' from Lua scripts (lbaselib.c:342-349).
		if strings.ContainsRune(mode, 'B') {
			return nil, fmt.Errorf("bad argument #3 to 'load' (invalid mode)")
		}
	}
	hasEnv := len(args) >= 4
	env := NilValue()
	if hasEnv {
		env = args[3]
	}
	fn, err := s.loadChunkBytes(data, chunkName, mode, env, hasEnv)
	if err != nil {
		return []Value{NilValue(), StringValue(s.intern.Intern(err.Error()))}, nil
	}
	return []Value{fn}, nil
}

// LoadChunk loads a text or binary Lua 5.5.1 chunk from raw bytes, stripping UTF-8 BOM
// and an initial '#' comment line per Lua 5.5 lauxlib.c:luaL_loadfilex.
func (s *State) LoadChunk(data []byte, chunkName string) (Value, error) {
	data = stripBOMAndShebang(data)
	return s.loadChunkBytes(data, chunkName, "bt", NilValue(), false)
}

func stripBOMAndShebang(data []byte) []byte {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if len(data) > 0 && data[0] == '#' {
		if nl := bytes.IndexByte(data, '\n'); nl < 0 {
			return nil
		} else if nl+1 < len(data) && data[nl+1] == 0x1b {
			// Binary chunk immediately following "#...\n"
			return data[nl+1:]
		} else {
			// Text chunk: keep the '\n' so line numbers stay accurate (line 2 stays line 2)
			return data[nl:]
		}
	}
	return data
}

func nativeLoadFile(s *State, args []Value) ([]Value, error) {
	var data []byte
	var err error
	chunkName := "=stdin"
	if len(args) == 0 || args[0].IsNil() {
		data, err = s.ReadAllStdin()
	} else {
		fname := FormatValue(args[0])
		chunkName = "@" + fname
		data, err = os.ReadFile(fname)
	}
	if err != nil {
		var le *LuaError
		if errors.As(err, &le) {
			return []Value{NilValue(), le.Val}, nil
		}
		return []Value{NilValue(), StringValue(s.intern.Intern(fmt.Sprintf("cannot open %s", err.Error())))}, nil
	}
	// Strip UTF-8 BOM and initial '#' comment line per Lua 5.5 lauxlib.c:luaL_loadfilex (files.lua:571,595).
	data = stripBOMAndShebang(data)
	mode := "bt"
	if len(args) >= 2 && !args[1].IsNil() {
		mode = FormatValue(args[1])
	}
	hasEnv := len(args) >= 3
	env := NilValue()
	if hasEnv {
		env = args[2]
	}
	fn, err := s.loadChunkBytes(data, chunkName, mode, env, hasEnv)
	if err != nil {
		return []Value{NilValue(), StringValue(s.intern.Intern(err.Error()))}, nil
	}
	return []Value{fn}, nil
}

func nativeDoFile(s *State, args []Value) ([]Value, error) {
	res, err := nativeLoadFile(s, args)
	if err != nil {
		return nil, err
	}
	if res[0].IsNil() {
		return nil, &LuaError{Val: res[1]}
	}
	return s.Call(res[0])
}

// --- io library ---

func (s *State) initIOLib() {
	fileMT := s.NewTable(0, 12)
	fileMethods := s.NewTable(0, 10)
	s.registerFunc(fileMethods, "read", intrNone, nativeFileRead)
	s.registerFunc(fileMethods, "write", intrNone, nativeFileWrite)
	s.registerFunc(fileMethods, "close", intrNone, nativeFileClose)
	s.registerFunc(fileMethods, "flush", intrNone, nativeFileFlush)
	s.registerFunc(fileMethods, "seek", intrNone, nativeFileSeek)
	s.registerFunc(fileMethods, "lines", intrNone, nativeFileLines)
	s.registerFunc(fileMethods, "setvbuf", intrNone, nativeFileSetvbuf)

	fileMT.RawSetStr(s, s.intern.tms[TMIndex], TableValue(fileMethods))
	fileMT.RawSetStr(s, s.intern.Intern("__name"), StringValue(s.intern.Intern("FILE*")))
	s.registerFunc(fileMT, "__tostring", intrNone, nativeFileToString)
	s.registerFunc(fileMT, "__close", intrNone, nativeFileGCClose)
	s.registerFunc(fileMT, "__gc", intrNone, nativeFileGCClose)
	s.fileMeta = fileMT

	stdinUD := s.newFileUD(&luaFile{f: os.Stdin, isStd: true})
	stdoutUD := s.newFileUD(&luaFile{f: os.Stdout, isStd: true})
	stderrUD := s.newFileUD(&luaFile{f: os.Stderr, isStd: true})
	s.stdinInput = stdinUD
	s.defaultInput = stdinUD
	s.defaultOutput = stdoutUD

	ioTbl := s.NewTable(0, 16)
	ioTbl.RawSetStr(s, s.intern.Intern("stdin"), stdinUD)
	ioTbl.RawSetStr(s, s.intern.Intern("stdout"), stdoutUD)
	ioTbl.RawSetStr(s, s.intern.Intern("stderr"), stderrUD)
	s.registerFunc(ioTbl, "open", intrNone, nativeIOOpen)
	s.registerFunc(ioTbl, "close", intrNone, nativeIOClose)
	s.registerFunc(ioTbl, "input", intrNone, nativeIOInput)
	s.registerFunc(ioTbl, "output", intrNone, nativeIOOutput)
	s.registerFunc(ioTbl, "read", intrNone, nativeIORead)
	s.registerFunc(ioTbl, "write", intrNone, nativeIOWrite)
	s.registerFunc(ioTbl, "flush", intrNone, nativeIOFlush)
	s.registerFunc(ioTbl, "lines", intrNone, nativeIOLines)
	s.registerFunc(ioTbl, "tmpfile", intrNone, nativeIOTmpfile)
	s.registerFunc(ioTbl, "type", intrNone, nativeIOType)
	s.registerFunc(ioTbl, "popen", intrNone, nativeIOPopen)
	s.SetGlobal("io", TableValue(ioTbl))
}

func (s *State) newFileUD(lf *luaFile) Value {
	u := &Userdata{Data: lf, Meta: s.fileMeta}
	v := UserdataValue(u)
	if !lf.isStd {
		fi := &fileInner{f: lf.f, w: lf.w, cmd: lf.cmd}
		lf.inner = fi
		runtime.AddCleanup(lf, cleanupFileInner, fi)
		s.openFiles = append(s.openFiles, fi)
		// Track weakly so Go GC + runtime.AddCleanup automatically reclaims discarded
		// descriptors in Tier 0, while explicit collectgarbage()/Close() still runs __gc.
		s.gc.weakFiles = append(s.gc.weakFiles, gcWeakFileEntry{u: weak.Make(u), fi: fi})
		if len(s.gc.weakFiles)&63 == 0 {
			s.compactWeakFiles()
			s.compactOpenFiles()
		}
	}
	return v
}

func (s *State) tryNewFileUD(lf *luaFile) (Value, error) {
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(64 + lf.memBytes()); err != nil {
			if !lf.isStd {
				if lf.f != nil {
					_ = lf.f.Close()
				}
				if lf.cmd != nil {
					_ = lf.cmd.Wait()
				}
				lf.w = nil
				lf.r = nil
			}
			return NilValue(), err
		}
	}
	return s.newFileUD(lf), nil
}

func (s *State) compactWeakFiles() {
	if len(s.gc.weakFiles) == 0 {
		return
	}
	kept := s.gc.weakFiles[:0]
	for _, wf := range s.gc.weakFiles {
		u := wf.u.Value()
		if u == nil {
			if wf.fi != nil {
				_ = wf.fi.flushAndClose()
			}
			continue
		}
		if u.gcInFnz || u.gcFnzed {
			continue
		}
		kept = append(kept, wf)
	}
	clear(s.gc.weakFiles[len(kept):])
	s.gc.weakFiles = kept
}

func (s *State) compactOpenFiles() {
	if len(s.openFiles) == 0 {
		return
	}
	kept := s.openFiles[:0]
	for _, fi := range s.openFiles {
		if !fi.isClosed() {
			kept = append(kept, fi)
		}
	}
	clear(s.openFiles[len(kept):])
	s.openFiles = kept
}

func (s *State) flushOpenFiles() {
	for i := len(s.openFiles) - 1; i >= 0; i-- {
		_ = s.openFiles[i].flush()
	}
}

func (s *State) closeOpenFiles() {
	for i := len(s.openFiles) - 1; i >= 0; i-- {
		_ = s.openFiles[i].flushAndClose()
	}
	s.openFiles = nil
}

func asLuaFile(v Value) (*luaFile, bool) {
	if !v.IsUserdata() {
		return nil, false
	}
	lf, ok := v.AsUserdata().Data.(*luaFile)
	return lf, ok
}

func ioFail(s *State, err error) []Value {
	code := int64(1)
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		code = int64(errno)
	}
	return []Value{NilValue(), StringValue(s.intern.Intern(err.Error())), IntValue(code)}
}

func isValidIOMode(mode string) bool {
	if mode == "" {
		return false
	}
	if mode[0] != 'r' && mode[0] != 'w' && mode[0] != 'a' {
		return false
	}
	rest := mode[1:]
	if len(rest) > 0 && rest[0] == '+' {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == 'b' {
		rest = rest[1:]
	}
	return len(rest) == 0
}

func openFileWithMode(filename, mode string) (*luaFile, error) {
	m := strings.TrimSuffix(mode, "b")
	var flag int
	switch m {
	case "r":
		flag = os.O_RDONLY
	case "w":
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	case "a":
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	case "r+":
		flag = os.O_RDWR
	case "w+":
		flag = os.O_RDWR | os.O_CREATE | os.O_TRUNC
	case "a+":
		flag = os.O_RDWR | os.O_CREATE | os.O_APPEND
	default:
		return nil, fmt.Errorf("invalid mode '%s'", mode)
	}
	f, err := os.OpenFile(filename, flag, 0o666)
	if err != nil {
		return nil, err
	}
	lf := &luaFile{f: f}
	if m != "r" {
		// Match C stdio default full buffering (_IOFBF) for writable files.
		lf.bufMode = bufModeFull
		lf.w = bufio.NewWriterSize(f, 4096)
	}
	return lf, nil
}

func nativeIOOpen(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'io.open' (string expected)")
	}
	mode := "r"
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'open' (string expected)")
		}
		mode = FormatValue(args[1])
	}
	if !isValidIOMode(mode) {
		return nil, fmt.Errorf("bad argument #2 to 'open' (invalid mode)")
	}
	lf, err := openFileWithMode(args[0].AsString().S, mode)
	if err != nil {
		return ioFail(s, err), nil
	}
	ud, err := s.tryNewFileUD(lf)
	if err != nil {
		return nil, err
	}
	return []Value{ud}, nil
}

func nativeIOClose(s *State, args []Value) ([]Value, error) {
	target := s.defaultOutput
	if len(args) >= 1 && !args[0].IsNil() {
		target = args[0]
	}
	return nativeFileClose(s, []Value{target})
}

func nativeIOInput(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		return []Value{s.defaultInput}, nil
	}
	if args[0].IsString() {
		lf, err := openFileWithMode(args[0].AsString().S, "r")
		if err != nil {
			return nil, fmt.Errorf("cannot open file '%s' (%v)", args[0].AsString().S, err)
		}
		ud, err := s.tryNewFileUD(lf)
		if err != nil {
			return nil, err
		}
		s.defaultInput = ud
		return []Value{s.defaultInput}, nil
	}
	if _, ok := asLuaFile(args[0]); !ok {
		return nil, fmt.Errorf("bad argument #1 to 'io.input' (FILE* expected, got %s)", s.objTypeName(args[0]))
	}
	s.defaultInput = args[0]
	return []Value{s.defaultInput}, nil
}

func nativeIOOutput(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		return []Value{s.defaultOutput}, nil
	}
	if args[0].IsString() {
		lf, err := openFileWithMode(args[0].AsString().S, "w")
		if err != nil {
			return nil, fmt.Errorf("cannot open file '%s' (%v)", args[0].AsString().S, err)
		}
		ud, err := s.tryNewFileUD(lf)
		if err != nil {
			return nil, err
		}
		s.defaultOutput = ud
		return []Value{s.defaultOutput}, nil
	}
	if _, ok := asLuaFile(args[0]); !ok {
		return nil, fmt.Errorf("bad argument #1 to 'io.output' (FILE* expected)")
	}
	s.defaultOutput = args[0]
	return []Value{s.defaultOutput}, nil
}

func nativeIORead(s *State, args []Value) ([]Value, error) {
	if lf, ok := asLuaFile(s.defaultInput); !ok || lf.closed {
		return nil, fmt.Errorf("standard input file is closed")
	}
	all := make([]Value, 1+len(args))
	all[0] = s.defaultInput
	copy(all[1:], args)
	return nativeFileRead(s, all)
}

func nativeIOWrite(s *State, args []Value) ([]Value, error) {
	if lf, ok := asLuaFile(s.defaultOutput); !ok || lf.closed {
		return nil, fmt.Errorf("standard output file is closed")
	}
	all := make([]Value, 1+len(args))
	all[0] = s.defaultOutput
	copy(all[1:], args)
	return nativeFileWrite(s, all)
}

func nativeIOFlush(s *State, _ []Value) ([]Value, error) {
	return nativeFileFlush(s, []Value{s.defaultOutput})
}

func nativeIOTmpfile(s *State, _ []Value) ([]Value, error) {
	f, err := os.CreateTemp("", "moelua-tmp-*")
	if err != nil {
		return ioFail(s, err), nil
	}
	_ = os.Remove(f.Name())
	ud, err := s.tryNewFileUD(&luaFile{f: f})
	if err != nil {
		return nil, err
	}
	return []Value{ud}, nil
}

func nativeIOType(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'io.type'")
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return []Value{NilValue()}, nil
	}
	if lf.closed {
		return []Value{StringValue(s.intern.Intern("closed file"))}, nil
	}
	return []Value{StringValue(s.intern.Intern("file"))}, nil
}

func shellCommand(cmdStr string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", cmdStr)
	}
	return exec.Command("/bin/sh", "-c", cmdStr)
}

func (s *State) inheritedStdStreams(cmd *exec.Cmd) {
	cmd.Stdin = os.Stdin
	if s.out != nil {
		cmd.Stdout = s.out
	} else {
		cmd.Stdout = os.Stdout
	}
	if s.errOut != nil {
		cmd.Stderr = s.errOut
	} else {
		cmd.Stderr = os.Stderr
	}
}

func execResult(s *State, err error) []Value {
	if err == nil {
		return []Value{TrueValue(), StringValue(s.intern.Intern("exit")), IntValue(0)}
	}
	if ee, ok := err.(*exec.ExitError); ok {
		kind, code := execExitInfo(ee)
		return []Value{NilValue(), StringValue(s.intern.Intern(kind)), IntValue(code)}
	}
	return []Value{NilValue(), StringValue(s.intern.Intern("exit")), IntValue(1)}
}

func nativeIOPopen(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'io.popen' (string expected)")
	}
	mode := "r"
	if len(args) >= 2 && !args[1].IsNil() {
		mode = FormatValue(args[1])
	}
	cmd := shellCommand(args[0].AsString().S)
	s.inheritedStdStreams(cmd)
	var f *os.File
	var err error
	if mode == "w" {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return ioFail(s, perr), nil
		}
		cmd.Stdin = pr
		if err = cmd.Start(); err != nil {
			_ = pr.Close()
			_ = pw.Close()
			return ioFail(s, err), nil
		}
		_ = pr.Close()
		f = pw
	} else {
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return ioFail(s, perr), nil
		}
		cmd.Stdout = pw
		if err = cmd.Start(); err != nil {
			_ = pr.Close()
			_ = pw.Close()
			return ioFail(s, err), nil
		}
		_ = pw.Close()
		f = pr
	}
	ud, err := s.tryNewFileUD(&luaFile{f: f, cmd: cmd})
	if err != nil {
		return nil, err
	}
	return []Value{ud}, nil
}

func nativeFileToString(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to '__tostring' (FILE* expected)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return []Value{StringValue(s.intern.Intern(FormatValue(args[0])))}, nil
	}
	if lf.closed {
		return []Value{StringValue(s.intern.Intern("file (closed)"))}, nil
	}
	return []Value{StringValue(s.intern.Intern(fmt.Sprintf("file (%p)", lf)))}, nil
}

func nativeFileClose(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'close' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'close' (FILE* expected)")
	}
	if lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	freed := lf.memBytes()
	isPipe := lf.cmd != nil
	err := lf.closeFile()
	if (s.memLimit > 0 || s.gc.tier1) && freed > 0 {
		if s.gc.allocBytes > freed {
			s.gc.allocBytes -= freed
		} else {
			s.gc.allocBytes = 0
		}
	}
	if isPipe {
		return execResult(s, err), nil
	}
	if err != nil {
		return ioFail(s, err), nil
	}
	return []Value{TrueValue()}, nil
}

func nativeFileGCClose(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to '__gc' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to '__gc' (FILE* expected, got %s)", s.objTypeName(args[0]))
	}
	if !lf.closed && !lf.isStd {
		freed := lf.memBytes()
		_ = lf.closeFile()
		if (s.memLimit > 0 || s.gc.tier1) && freed > 0 {
			if s.gc.allocBytes > freed {
				s.gc.allocBytes -= freed
			} else {
				s.gc.allocBytes = 0
			}
		}
	}
	return nil, nil
}

func nativeFileFlush(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'flush' (FILE* expected)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	if err := lf.flushWriter(); err != nil {
		return ioFail(s, err), nil
	}
	_ = lf.f.Sync()
	return []Value{TrueValue()}, nil
}

func nativeFileSetvbuf(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'setvbuf' (FILE* expected)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	if len(args) < 2 || !args[1].IsString() {
		return nil, fmt.Errorf("bad argument #2 to 'setvbuf' (string expected)")
	}
	if err := lf.flushWriter(); err != nil {
		return ioFail(s, err), nil
	}
	sz := int64(4096)
	if len(args) >= 3 && !args[2].IsNil() {
		if v, ok := args[2].CoerceInt(); ok && v > 0 {
			sz = v
		}
	}
	const maxSetVBufSize = int64(1 << 28)
	oldSz := int64(0)
	if lf.w != nil {
		oldSz = int64(lf.w.Size())
	}
	mode := args[1].AsString().S
	switch mode {
	case "no":
		lf.bufMode = bufModeNo
		lf.w = nil
		if (s.memLimit > 0 || s.gc.tier1) && oldSz > 0 {
			if s.gc.allocBytes > oldSz {
				s.gc.allocBytes -= oldSz
			} else {
				s.gc.allocBytes = 0
			}
		}
	case "full", "line":
		if sz > maxSetVBufSize {
			if s.memLimit > 0 {
				return nil, &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
			}
			return nil, fmt.Errorf("bad argument #3 to 'setvbuf' (buffer size too large)")
		}
		delta := sz - oldSz
		if delta > 0 {
			if err := s.checkMemAndAccount(delta); err != nil {
				return nil, err
			}
		}
		if mode == "full" {
			lf.bufMode = bufModeFull
		} else {
			lf.bufMode = bufModeLine
		}
		lf.w = nil
		lf.w = bufio.NewWriterSize(lf.f, int(sz))
		if (s.memLimit > 0 || s.gc.tier1) && delta < 0 {
			freed := -delta
			if s.gc.allocBytes > freed {
				s.gc.allocBytes -= freed
			} else {
				s.gc.allocBytes = 0
			}
		}
	default:
		return nil, fmt.Errorf("bad argument #2 to 'setvbuf' (invalid option '%s')", mode)
	}
	if lf.inner != nil {
		lf.inner.mu.Lock()
		lf.inner.w = lf.w
		lf.inner.mu.Unlock()
	}
	return []Value{TrueValue()}, nil
}

func nativeFileSeek(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'seek' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	if err := lf.flushWriter(); err != nil {
		return ioFail(s, err), nil
	}
	whence := io.SeekCurrent
	offset := int64(0)
	if len(args) >= 2 && !args[1].IsNil() {
		switch FormatValue(args[1]) {
		case "set":
			whence = io.SeekStart
		case "cur":
			whence = io.SeekCurrent
		case "end":
			whence = io.SeekEnd
		default:
			return nil, fmt.Errorf("bad argument #1 to 'seek' (invalid option)")
		}
	}
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		offset, ok = args[2].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'seek' (number expected)")
		}
	}
	seekOffset := offset
	if lf.r != nil && whence == io.SeekCurrent {
		// Adjust underlying file position for buffered reader before seeking.
		seekOffset -= int64(lf.r.Buffered())
	}
	pos, err := lf.f.Seek(seekOffset, whence)
	if err != nil {
		return ioFail(s, err), nil
	}
	// Only discard buffered input after the underlying seek succeeds.
	lf.r = nil
	return []Value{IntValue(pos)}, nil
}

// ReadStdinLine reads one line from the State's original io.stdin reader (used by REPLs
// so interactive command reading shares buffering with io.stdin / default stdin reads
// without being redirected when Lua code calls io.input(file)).
func (s *State) ReadStdinLine() (string, error) {
	if lf, ok := asLuaFile(s.stdinInput); ok && !lf.closed && lf.f != nil {
		return lf.reader().ReadString('\n')
	}
	return "", io.EOF
}

// ReadAllStdin reads all remaining bytes from the State's original io.stdin reader
// so stdin scripts (`moelua -` / piped stdin) consume any bytes buffered by earlier
// `-e` / `LUA_INIT` stdin reads instead of bypassing them.
func (s *State) ReadAllStdin() ([]byte, error) {
	if lf, ok := asLuaFile(s.stdinInput); ok && !lf.closed && lf.f != nil {
		if s.memLimit <= 0 {
			return io.ReadAll(lf.reader())
		}
		r := lf.reader()
		var buf bytes.Buffer
		var readBytes int64
		defer func() {
			s.gc.tempBytes -= readBytes
		}()
		var tmp [4096]byte
		for {
			n, err := r.Read(tmp[:])
			if n > 0 {
				if merr := s.checkMem(int64(n)); merr != nil {
					return nil, merr
				}
				buf.Write(tmp[:n])
				readBytes += int64(n)
				s.gc.tempBytes += int64(n)
			}
			if err == io.EOF {
				return buf.Bytes(), nil
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

// ioReadErr wraps an OS-level I/O error (distinct from bad-argument format errors).
type ioReadErr struct{ err error }

func (e ioReadErr) Error() string { return e.err.Error() }

func readBoundedLine(s *State, r *bufio.Reader, keepNL bool) (string, bool, error) {
	var buf bytes.Buffer
	for {
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			if merr := s.checkMem(int64(buf.Len()) + int64(len(frag)) + 64); merr != nil {
				return "", false, merr
			}
			buf.Write(frag)
		}
		if err == nil {
			line := buf.String()
			if !keepNL {
				line = strings.TrimSuffix(line, "\n")
			}
			s.gc.allocBytes += int64(len(line)) + 64
			return line, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if buf.Len() == 0 {
				return "", false, nil
			}
			line := buf.String()
			if !keepNL {
				line = strings.TrimSuffix(line, "\n")
			}
			s.gc.allocBytes += int64(len(line)) + 64
			return line, true, nil
		}
		return "", false, ioReadErr{err}
	}
}

func readBoundedAll(s *State, r *bufio.Reader) (string, error) {
	var buf bytes.Buffer
	var chunk [4096]byte
	for {
		n, err := r.Read(chunk[:])
		if n > 0 {
			if merr := s.checkMem(int64(buf.Len()) + int64(n) + 64); merr != nil {
				return "", merr
			}
			buf.Write(chunk[:n])
		}
		if err == io.EOF {
			res := buf.String()
			s.gc.allocBytes += int64(len(res)) + 64
			return res, nil
		}
		if err != nil {
			return "", ioReadErr{err}
		}
	}
}

func readOneItem(s *State, lf *luaFile, fmtArg Value) (Value, bool, error) {
	r := lf.reader()
	if n, ok := fmtArg.ToInt(); ok {
		if n == 0 {
			_, err := r.Peek(1)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return NilValue(), false, nil
				}
				return NilValue(), false, ioReadErr{err}
			}
			return StringValue(s.intern.Intern("")), true, nil
		}
		if n < 0 {
			return NilValue(), false, fmt.Errorf("invalid read count")
		}
		const maxReadBytes = int64(1 << 30)
		if n > maxReadBytes {
			if s.memLimit > 0 {
				return NilValue(), false, &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
			}
			return NilValue(), false, fmt.Errorf("resulting string too large")
		}
		if s.memLimit > 0 {
			if err := s.checkMem(n + 64); err != nil {
				return NilValue(), false, err
			}
		}
		if n <= 65536 {
			buf := make([]byte, n)
			got, err := io.ReadFull(r, buf)
			if got == 0 && err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return NilValue(), false, nil
				}
				return NilValue(), false, ioReadErr{err}
			}
			if s.memLimit > 0 || s.gc.tier1 {
				s.gc.allocBytes += int64(got) + 64
			}
			return StringValue(s.intern.Intern(string(buf[:got]))), true, nil
		}
		var out bytes.Buffer
		var readBytes int64
		defer func() {
			s.gc.tempBytes -= readBytes
		}()
		var chunk [32768]byte
		rem := n
		for rem > 0 {
			want := min(rem, int64(len(chunk)))
			got, err := r.Read(chunk[:want])
			if got > 0 {
				if merr := s.checkMem(int64(got)); merr != nil {
					return NilValue(), false, merr
				}
				out.Write(chunk[:got])
				readBytes += int64(got)
				s.gc.tempBytes += int64(got)
				rem -= int64(got)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				if out.Len() == 0 {
					return NilValue(), false, ioReadErr{err}
				}
				break
			}
		}
		if out.Len() == 0 {
			return NilValue(), false, nil
		}
		if s.memLimit > 0 || s.gc.tier1 {
			s.gc.allocBytes += int64(out.Len()) + 64
		}
		return StringValue(s.intern.Intern(out.String())), true, nil
	}
	spec := strings.TrimPrefix(FormatValue(fmtArg), "*")
	if len(spec) == 0 {
		return NilValue(), false, fmt.Errorf("invalid format")
	}
	switch spec[0] {
	case 'l', 'L':
		if s.memLimit > 0 {
			line, ok, err := readBoundedLine(s, r, spec[0] == 'L')
			if err != nil || !ok {
				return NilValue(), false, err
			}
			return StringValue(s.intern.Intern(line)), true, nil
		}
		line, err := r.ReadString('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				return NilValue(), false, nil
			}
			return NilValue(), false, ioReadErr{err}
		}
		if spec[0] == 'l' {
			line = strings.TrimSuffix(line, "\n")
		}
		return StringValue(s.intern.Intern(line)), true, nil
	case 'a':
		if s.memLimit > 0 {
			all, err := readBoundedAll(s, r)
			if err != nil {
				return NilValue(), false, err
			}
			return StringValue(s.intern.Intern(all)), true, nil
		}
		all, err := io.ReadAll(r)
		if err != nil {
			return NilValue(), false, ioReadErr{err}
		}
		return StringValue(s.intern.Intern(string(all))), true, nil
	case 'n':
		return readLuaNumber(s, r)
	default:
		return NilValue(), false, fmt.Errorf("invalid format '%s'", spec)
	}
}

// readLuaNumber implements exact PUC-Rio Lua 5.5 liolib.c:read_number state machine.
func readLuaNumber(s *State, r *bufio.Reader) (Value, bool, error) {
	// Skip leading whitespace.
	var cur byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return NilValue(), false, nil
		}
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' && b != '\f' && b != '\v' {
			cur = b
			break
		}
	}
	const maxNumLen = 200
	var buf [maxNumLen]byte
	n := 0
	eof := false
	nextChar := func() {
		b, err := r.ReadByte()
		if err != nil {
			eof = true
			cur = 0
		} else {
			cur = b
		}
	}
	test2 := func(set string) bool {
		if eof {
			return false
		}
		for i := 0; i < len(set); i++ {
			if cur == set[i] {
				if n >= maxNumLen {
					return false
				}
				buf[n] = cur
				n++
				nextChar()
				return true
			}
		}
		return false
	}
	isHexDigit := func(c byte) bool {
		return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	readDigits := func(hex bool) int {
		cnt := 0
		for !eof && ((hex && isHexDigit(cur)) || (!hex && cur >= '0' && cur <= '9')) {
			if n >= maxNumLen {
				return -1 // overflow
			}
			buf[n] = cur
			n++
			cnt++
			nextChar()
		}
		return cnt
	}

	test2("+-")
	hex := false
	count := 0
	if test2("0") {
		if test2("xX") {
			hex = true
		} else {
			count = 1
		}
	}
	d1 := readDigits(hex)
	if d1 < 0 {
		if !eof {
			_ = r.UnreadByte()
		}
		return NilValue(), false, nil
	}
	count += d1
	if test2(".") {
		d2 := readDigits(hex)
		if d2 < 0 {
			if !eof {
				_ = r.UnreadByte()
			}
			return NilValue(), false, nil
		}
		count += d2
	}
	if count > 0 {
		expSet := "eE"
		if hex {
			expSet = "pP"
		}
		if test2(expSet) {
			test2("+-")
			if d3 := readDigits(false); d3 <= 0 {
				if !eof {
					_ = r.UnreadByte()
				}
				return NilValue(), false, nil
			}
		}
	}
	if !eof {
		_ = r.UnreadByte()
	}
	if count == 0 {
		return NilValue(), false, nil
	}
	str := string(buf[:n])
	if iv, ok := luastdlib.StrToInt(str); ok {
		return IntValue(iv), true, nil
	}
	if fv, ok := luastdlib.StrToFloat(str); ok {
		return FloatValue(fv), true, nil
	}
	return NilValue(), false, nil
}

func nativeFileRead(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'read' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	if lf.w != nil && lf.w.Buffered() > 0 {
		_ = lf.flushWriter()
	}
	if lf.r == nil && (s.memLimit > 0 || s.gc.tier1) {
		if err := s.checkMemAndAccount(4096); err != nil {
			return nil, err
		}
		lf.r = bufio.NewReader(lf.f)
	}
	fmts := args[1:]
	if len(fmts) == 0 {
		v, _, err := readOneItem(s, lf, StringValue(s.intern.Intern("l")))
		if err != nil {
			if re, ok := err.(ioReadErr); ok {
				return ioFail(s, re.err), nil
			}
			return nil, err
		}
		return []Value{v}, nil
	}
	out := make([]Value, 0, len(fmts))
	for _, f := range fmts {
		v, ok, err := readOneItem(s, lf, f)
		if err != nil {
			if re, ok := err.(ioReadErr); ok {
				return ioFail(s, re.err), nil
			}
			return nil, err
		}
		out = append(out, v)
		if !ok {
			break
		}
	}
	return out, nil
}

func nativeFileWrite(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'write' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	var totalBytes int64
	for _, a := range args[1:] {
		if !a.IsString() && !a.IsNumber() {
			return nil, fmt.Errorf("bad argument to 'write' (string expected)")
		}
		str := FormatValue(a)
		if lf.w != nil {
			n, err := lf.w.WriteString(str)
			totalBytes += int64(n)
			if err != nil {
				written := max(int64(0), totalBytes-int64(lf.w.Buffered()))
				lf.w.Reset(lf.f)
				return append(ioFail(s, err), IntValue(written)), nil
			}
			if lf.bufMode == bufModeLine && strings.ContainsRune(str, '\n') {
				if err := lf.w.Flush(); err != nil {
					written := max(int64(0), totalBytes-int64(lf.w.Buffered()))
					lf.w.Reset(lf.f)
					return append(ioFail(s, err), IntValue(written)), nil
				}
			}
		} else {
			n, err := lf.f.WriteString(str)
			totalBytes += int64(n)
			if err != nil {
				return append(ioFail(s, err), IntValue(totalBytes)), nil
			}
		}
	}
	return []Value{args[0]}, nil
}

func makeLinesIter(s *State, fileVal Value, fmts []Value, autoClose bool) []Value {
	savedFmts := append([]Value(nil), fmts...)
	iter := &NativeFunc{
		Name:        "io.lines_iter",
		Collectable: true,
		gcChild:     fileVal,
		Fn: func(s *State, _ []Value) ([]Value, error) {
			lf, ok := asLuaFile(fileVal)
			if !ok || lf.closed {
				return nil, fmt.Errorf("file is already closed")
			}
			callArgs := make([]Value, 1+len(savedFmts))
			callArgs[0] = fileVal
			copy(callArgs[1:], savedFmts)
			res, err := nativeFileRead(s, callArgs)
			if err != nil {
				return nil, err
			}
			if len(res) >= 2 && res[0].IsNil() && res[1].IsString() {
				// Propagate I/O error from f:lines() iterator (files.lua:386)!
				return nil, fmt.Errorf("%s", res[1].AsString().S)
			}
			if len(res) == 0 || res[0].IsNil() {
				if autoClose && !lf.closed && !lf.isStd {
					_ = lf.closeFile()
				}
				return []Value{NilValue()}, nil
			}
			return res, nil
		},
	}
	closeVar := NilValue()
	if autoClose {
		closeVar = fileVal
	}
	return []Value{NativeValue(iter), NilValue(), NilValue(), closeVar}
}

func nativeFileLines(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'lines' (FILE* expected, got no value)")
	}
	lf, ok := asLuaFile(args[0])
	if !ok || lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	if len(args)-1 > 250 {
		return nil, fmt.Errorf("too many arguments")
	}
	return makeLinesIter(s, args[0], args[1:], false), nil
}

func nativeIOLines(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		var fmts []Value
		if len(args) > 1 {
			fmts = args[1:]
		}
		if len(fmts) > 250 {
			return nil, fmt.Errorf("too many arguments")
		}
		return makeLinesIter(s, s.defaultInput, fmts, false), nil
	}
	if len(args)-1 > 250 {
		return nil, fmt.Errorf("too many arguments")
	}
	lf, err := openFileWithMode(FormatValue(args[0]), "r")
	if err != nil {
		return nil, fmt.Errorf("cannot open file '%s' (%v)", FormatValue(args[0]), err)
	}
	ud, err := s.tryNewFileUD(lf)
	if err != nil {
		return nil, err
	}
	return makeLinesIter(s, ud, args[1:], true), nil
}

// --- os library ---

func (s *State) initOSLib() {
	osTbl := s.NewTable(0, 12)
	s.registerFunc(osTbl, "clock", intrNone, nativeOSClock)
	s.registerFunc(osTbl, "time", intrNone, nativeOSTime)
	s.registerFunc(osTbl, "difftime", intrNone, nativeOSDiffTime)
	s.registerFunc(osTbl, "date", intrNone, nativeOSDate)
	s.registerFunc(osTbl, "getenv", intrNone, nativeOSGetEnv)
	s.registerFunc(osTbl, "remove", intrNone, nativeOSRemove)
	s.registerFunc(osTbl, "rename", intrNone, nativeOSRename)
	s.registerFunc(osTbl, "tmpname", intrNone, nativeOSTmpName)
	s.registerFunc(osTbl, "setlocale", intrNone, nativeOSSetLocale)
	s.registerFunc(osTbl, "execute", intrNone, nativeOSExecute)
	s.registerFunc(osTbl, "exit", intrNone, nativeOSExit)
	s.SetGlobal("os", TableValue(osTbl))
}

func nativeOSTime(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		return []Value{IntValue(time.Now().Unix())}, nil
	}
	if !args[0].IsTable() {
		return nil, fmt.Errorf("bad argument #1 to 'os.time' (table expected)")
	}
	tblVal := args[0]
	t := tblVal.AsTable()
	readField := func(name string) (Value, error) {
		k := s.intern.Intern(name)
		v := t.RawGetStr(k)
		if v.IsNil() && t.meta != nil {
			return s.finishGet(tblVal, StringValue(k))
		}
		return v, nil
	}
	getField := func(name string, def int, req bool, delta int64) (int, error) {
		v, err := readField(name)
		if err != nil {
			return 0, err
		}
		if v.IsNil() {
			if req {
				return 0, fmt.Errorf("field '%s' missing in date table", name)
			}
			return def, nil
		}
		iv, ok := v.CoerceInt()
		if !ok {
			return 0, fmt.Errorf("field '%s' is not an integer", name)
		}
		if iv < int64(math.MinInt32)+delta || iv > int64(math.MaxInt32)+delta {
			return 0, fmt.Errorf("field '%s' is out-of-bound", name)
		}
		return int(iv), nil
	}
	year, err := getField("year", 0, true, 1900)
	if err != nil {
		return nil, err
	}
	month, err := getField("month", 0, true, 1)
	if err != nil {
		return nil, err
	}
	day, err := getField("day", 0, true, 0)
	if err != nil {
		return nil, err
	}
	hour, err := getField("hour", 12, false, 0)
	if err != nil {
		return nil, err
	}
	min, err := getField("min", 0, false, 0)
	if err != nil {
		return nil, err
	}
	sec, err := getField("sec", 0, false, 0)
	if err != nil {
		return nil, err
	}
	isdstVal, err := readField("isdst")
	if err != nil {
		return nil, err
	}
	tm := resolveLocalDateTime(year, month, day, hour, min, sec, !isdstVal.IsNil(), !isdstVal.IsFalsy())
	// Normalize fields back into table per Lua 5.5 loslib.c:setallfields (honoring __newindex).
	normFields := [9]struct {
		name string
		val  Value
	}{
		{"year", IntValue(int64(tm.Year()))},
		{"month", IntValue(int64(tm.Month()))},
		{"day", IntValue(int64(tm.Day()))},
		{"hour", IntValue(int64(tm.Hour()))},
		{"min", IntValue(int64(tm.Minute()))},
		{"sec", IntValue(int64(tm.Second()))},
		{"yday", IntValue(int64(tm.YearDay()))},
		{"wday", IntValue(int64(tm.Weekday()) + 1)},
		{"isdst", BoolValue(tm.IsDST())},
	}
	for _, nf := range normFields {
		if err := s.tableSetStr(tblVal, s.intern.Intern(nf.name), nf.val); err != nil {
			return nil, err
		}
	}
	return []Value{IntValue(tm.Unix())}, nil
}

// resolveLocalDateTime mirrors POSIX mktime's tm_isdst handling in pure Go:
// when hasDST is false (isdst == nil), time.Date auto-detects; when hasDST is true,
// the wall-clock fields are interpreted using the nearest local zone offset whose
// DST status matches wantDST (disambiguating autumn folds and normalizing mismatches).
func resolveLocalDateTime(year, month, day, hour, min, sec int, hasDST, wantDST bool) time.Time {
	base := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	if !hasDST {
		return base
	}
	off, ok := findLocalOffsetForDST(base, wantDST)
	if !ok {
		return base
	}
	wallUTC := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
	return time.Unix(wallUTC.Unix()-int64(off), 0).In(time.Local)
}

func findLocalOffsetForDST(base time.Time, wantDST bool) (int, bool) {
	if base.IsDST() == wantDST {
		_, off := base.Zone()
		return off, true
	}
	start, end := base.ZoneBounds()
	var fwdOff, bwdOff int
	var fwdDist, bwdDist time.Duration
	fwdOK, bwdOK := false, false

	cur := end
	for range 2 {
		if cur.IsZero() {
			break
		}
		probe := cur.In(time.Local)
		if probe.IsDST() == wantDST {
			_, fwdOff = probe.Zone()
			fwdDist = probe.Sub(base)
			if fwdDist < 0 {
				fwdDist = -fwdDist
			}
			fwdOK = true
			break
		}
		_, cur = probe.ZoneBounds()
	}

	cur = start
	for range 2 {
		if cur.IsZero() {
			break
		}
		probe := cur.Add(-time.Second).In(time.Local)
		if probe.IsDST() == wantDST {
			_, bwdOff = probe.Zone()
			bwdDist = base.Sub(probe)
			if bwdDist < 0 {
				bwdDist = -bwdDist
			}
			bwdOK = true
			break
		}
		cur, _ = probe.ZoneBounds()
	}

	if fwdOK && bwdOK {
		if fwdDist <= bwdDist {
			return fwdOff, true
		}
		return bwdOff, true
	}
	if fwdOK {
		return fwdOff, true
	}
	if bwdOK {
		return bwdOff, true
	}
	return 0, false
}

func nativeOSDiffTime(_ *State, args []Value) ([]Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("bad argument #1 to 'difftime' (number expected, got no value)")
	}
	t2, ok := args[0].CoerceFloat()
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'difftime' (number expected, got %s)", args[0].TypeName())
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("bad argument #2 to 'difftime' (number expected, got no value)")
	}
	t1, ok := args[1].CoerceFloat()
	if !ok {
		return nil, fmt.Errorf("bad argument #2 to 'difftime' (number expected, got %s)", args[1].TypeName())
	}
	return []Value{FloatValue(t2 - t1)}, nil
}

func nativeOSDate(s *State, args []Value) ([]Value, error) {
	fmtStr := "%c"
	if len(args) >= 1 && !args[0].IsNil() {
		fmtStr = FormatValue(args[0])
	}
	tm := time.Now()
	if len(args) >= 2 && !args[1].IsNil() {
		sec, ok := args[1].CoerceInt()
		if !ok {
			return nil, fmt.Errorf("bad argument #2 to 'os.date'")
		}
		tm = time.Unix(sec, 0)
	}
	if strings.HasPrefix(fmtStr, "!") {
		tm = tm.UTC()
		fmtStr = fmtStr[1:]
	}
	if fmtStr == "*t" {
		tbl := s.NewTable(0, 10)
		tbl.RawSetStr(s, s.intern.Intern("year"), IntValue(int64(tm.Year())))
		tbl.RawSetStr(s, s.intern.Intern("month"), IntValue(int64(tm.Month())))
		tbl.RawSetStr(s, s.intern.Intern("day"), IntValue(int64(tm.Day())))
		tbl.RawSetStr(s, s.intern.Intern("hour"), IntValue(int64(tm.Hour())))
		tbl.RawSetStr(s, s.intern.Intern("min"), IntValue(int64(tm.Minute())))
		tbl.RawSetStr(s, s.intern.Intern("sec"), IntValue(int64(tm.Second())))
		tbl.RawSetStr(s, s.intern.Intern("wday"), IntValue(int64(tm.Weekday())+1))
		tbl.RawSetStr(s, s.intern.Intern("yday"), IntValue(int64(tm.YearDay())))
		tbl.RawSetStr(s, s.intern.Intern("isdst"), BoolValue(tm.IsDST()))
		return []Value{TableValue(tbl)}, nil
	}
	var sb strings.Builder
	for i := 0; i < len(fmtStr); i++ {
		if fmtStr[i] != '%' {
			sb.WriteByte(fmtStr[i])
			continue
		}
		i++
		if i >= len(fmtStr) {
			return nil, fmt.Errorf("invalid conversion specifier '%%'")
		}
		switch fmtStr[i] {
		case '%':
			sb.WriteByte('%')
		case 'Y':
			sb.WriteString(fmt.Sprintf("%04d", tm.Year()))
		case 'y':
			sb.WriteString(fmt.Sprintf("%02d", tm.Year()%100))
		case 'C':
			sb.WriteString(fmt.Sprintf("%02d", tm.Year()/100))
		case 'm':
			sb.WriteString(fmt.Sprintf("%02d", tm.Month()))
		case 'd':
			sb.WriteString(fmt.Sprintf("%02d", tm.Day()))
		case 'e':
			sb.WriteString(fmt.Sprintf("%2d", tm.Day()))
		case 'H':
			sb.WriteString(fmt.Sprintf("%02d", tm.Hour()))
		case 'I':
			h12 := tm.Hour() % 12
			if h12 == 0 {
				h12 = 12
			}
			sb.WriteString(fmt.Sprintf("%02d", h12))
		case 'p':
			if tm.Hour() < 12 {
				sb.WriteString("AM")
			} else {
				sb.WriteString("PM")
			}
		case 'M':
			sb.WriteString(fmt.Sprintf("%02d", tm.Minute()))
		case 'S':
			sb.WriteString(fmt.Sprintf("%02d", tm.Second()))
		case 'a':
			sb.WriteString(tm.Format("Mon"))
		case 'A':
			sb.WriteString(tm.Format("Monday"))
		case 'b', 'h':
			sb.WriteString(tm.Format("Jan"))
		case 'B':
			sb.WriteString(tm.Format("January"))
		case 'c':
			sb.WriteString(tm.Format("Mon Jan _2 15:04:05 2006"))
		case 'D', 'x':
			sb.WriteString(tm.Format("01/02/06"))
		case 'F':
			sb.WriteString(fmt.Sprintf("%04d-%02d-%02d", tm.Year(), tm.Month(), tm.Day()))
		case 'r':
			sb.WriteString(tm.Format("03:04:05 PM"))
		case 'R':
			sb.WriteString(fmt.Sprintf("%02d:%02d", tm.Hour(), tm.Minute()))
		case 'T', 'X':
			sb.WriteString(tm.Format("15:04:05"))
		case 'j':
			sb.WriteString(fmt.Sprintf("%03d", tm.YearDay()))
		case 'w':
			sb.WriteString(fmt.Sprintf("%d", tm.Weekday()))
		case 'u':
			u := int(tm.Weekday())
			if u == 0 {
				u = 7
			}
			sb.WriteString(fmt.Sprintf("%d", u))
		case 'U':
			sb.WriteString(fmt.Sprintf("%02d", (tm.YearDay()-1+7-int(tm.Weekday()))/7))
		case 'W':
			monWday := (int(tm.Weekday()) + 6) % 7
			sb.WriteString(fmt.Sprintf("%02d", (tm.YearDay()-1+7-monWday)/7))
		case 'g':
			isoY, _ := tm.ISOWeek()
			sb.WriteString(fmt.Sprintf("%02d", isoY%100))
		case 'G':
			isoY, _ := tm.ISOWeek()
			sb.WriteString(fmt.Sprintf("%04d", isoY))
		case 'V':
			_, isoW := tm.ISOWeek()
			sb.WriteString(fmt.Sprintf("%02d", isoW))
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'Z':
			zone, _ := tm.Zone()
			sb.WriteString(zone)
		case 'z':
			sb.WriteString(tm.Format("-0700"))
		default:
			return nil, fmt.Errorf("invalid conversion specifier '%%%c'", fmtStr[i])
		}
	}
	return []Value{StringValue(s.intern.Intern(sb.String()))}, nil
}

func nativeOSGetEnv(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'os.getenv'")
	}
	v, ok := os.LookupEnv(args[0].AsString().S)
	if !ok {
		return []Value{NilValue()}, nil
	}
	return []Value{StringValue(s.intern.Intern(v))}, nil
}

func nativeOSRemove(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || !args[0].IsString() {
		return nil, fmt.Errorf("bad argument #1 to 'os.remove'")
	}
	if err := os.Remove(args[0].AsString().S); err != nil {
		return ioFail(s, err), nil
	}
	return []Value{TrueValue()}, nil
}

func nativeOSRename(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 || !args[0].IsString() || !args[1].IsString() {
		return nil, fmt.Errorf("bad arguments to 'os.rename'")
	}
	if err := os.Rename(args[0].AsString().S, args[1].AsString().S); err != nil {
		return ioFail(s, err), nil
	}
	return []Value{TrueValue()}, nil
}

func nativeOSTmpName(s *State, _ []Value) ([]Value, error) {
	f, err := os.CreateTemp("", "moelua_name_*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	// On POSIX systems, Lua 5.5's os.tmpname (mkstemp + close) keeps the created
	// file present to reserve the pathname until the caller removes it.
	if runtime.GOOS == "windows" {
		_ = os.Remove(name)
	}
	return []Value{StringValue(s.intern.Intern(name))}, nil
}

func nativeOSSetLocale(s *State, args []Value) ([]Value, error) {
	if len(args) >= 1 && args[0].IsString() {
		loc := args[0].AsString().S
		if loc == "C" || loc == "" || loc == "POSIX" {
			return []Value{StringValue(s.intern.Intern("C"))}, nil
		}
		return []Value{NilValue()}, nil
	}
	return []Value{StringValue(s.intern.Intern("C"))}, nil
}

func nativeOSExecute(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		return []Value{TrueValue()}, nil
	}
	cmd := shellCommand(FormatValue(args[0]))
	s.inheritedStdStreams(cmd)
	return execResult(s, cmd.Run()), nil
}

func nativeOSExit(s *State, args []Value) ([]Value, error) {
	code := 0
	if len(args) >= 1 && !args[0].IsNil() {
		if args[0].IsFalse() {
			code = 1
		} else if i, ok := args[0].CoerceInt(); ok {
			code = int(i)
		}
	}
	s.flushOpenFiles()
	if len(args) >= 2 && !args[1].IsFalsy() {
		s.Close()
	}
	os.Exit(code)
	return nil, nil
}

// --- package library & require ---

func (s *State) initPackageLib() {
	pkg := s.NewTable(0, 10)
	loaded := s.NewTable(0, 16)
	preload := s.NewTable(0, 8)
	searchers := s.NewTable(4, 0)

	// Pre-populate standard libraries in package.loaded
	for _, name := range []string{"_G", "math", "table", "string", "os", "io", "coroutine", "debug", "utf8"} {
		if v := s.GetGlobal(name); !v.IsNil() {
			loaded.RawSetStr(s, s.intern.Intern(name), v)
		}
	}
	loaded.RawSetStr(s, s.intern.Intern("package"), TableValue(pkg))

	pkg.RawSetStr(s, s.intern.Intern("loaded"), TableValue(loaded))
	pkg.RawSetStr(s, s.intern.Intern("preload"), TableValue(preload))
	pkg.RawSetStr(s, s.intern.Intern("path"), StringValue(s.intern.Intern(defaultLuaPath)))
	pkg.RawSetStr(s, s.intern.Intern("cpath"), StringValue(s.intern.Intern(defaultLuaCPath)))
	pkg.RawSetStr(s, s.intern.Intern("config"), StringValue(s.intern.Intern("/\n;\n?\n!\n-\n")))

	// Stateless searchers & require (safe to share across States via frozen stdlib template!).
	searchers.RawSetInt(s, 1, NativeValue(&NativeFunc{Name: "searcher_preload", Fn: nativeSearcherPreload}))
	searchers.RawSetInt(s, 2, NativeValue(&NativeFunc{Name: "searcher_lua", Fn: nativeSearcherLua}))
	searchers.RawSetInt(s, 3, NativeValue(&NativeFunc{Name: "searcher_c", Fn: nativeSearcherC}))
	pkg.RawSetStr(s, s.intern.Intern("searchers"), TableValue(searchers))
	s.registerFunc(pkg, "searchpath", intrNone, nativePackageSearchPath)

	s.pkgTbl = pkg
	s.loadedTbl = loaded
	s.preloadTbl = preload
	s.SetGlobal("package", TableValue(pkg))
	s.registry.RawSetStr(s, s.intern.Intern("_LOADED"), TableValue(loaded))
	s.registry.RawSetStr(s, s.intern.Intern("_PRELOAD"), TableValue(preload))

	s.registerFunc(s.env, "require", intrNone, nativeRequire)
}

func nativeSearcherPreload(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_preload' (string expected)")
	}
	name := FormatValue(args[0])
	v := s.preloadTbl.RawGetStr(s.intern.Intern(name))
	if v.IsNil() {
		return []Value{StringValue(s.intern.Intern(fmt.Sprintf("no field package.preload['%s']", name)))}, nil
	}
	return []Value{v, StringValue(s.intern.Intern(":preload:"))}, nil
}

func nativeSearcherLua(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_lua' (string expected)")
	}
	name := FormatValue(args[0])
	pathVal := s.pkgTbl.RawGetStr(s.intern.Intern("path"))
	if !pathVal.IsString() {
		return nil, fmt.Errorf("'package.path' must be a string")
	}
	fname, errmsg := searchPath(name, pathVal.AsString().S, ".", string(filepath.Separator))
	if fname == "" {
		return []Value{StringValue(s.intern.Intern(errmsg))}, nil
	}
	res, err := nativeLoadFile(s, []Value{StringValue(s.intern.Intern(fname))})
	if err != nil {
		return nil, err
	}
	if res[0].IsNil() {
		return nil, fmt.Errorf("error loading module '%s' from file '%s':\n\t%s", name, fname, FormatValue(res[1]))
	}
	return []Value{res[0], StringValue(s.intern.Intern(fname))}, nil
}

func nativeSearcherC(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_C' (string expected)")
	}
	name := FormatValue(args[0])
	cpathVal := s.pkgTbl.RawGetStr(s.intern.Intern("cpath"))
	if !cpathVal.IsString() {
		return nil, fmt.Errorf("'package.cpath' must be a string")
	}
	_, errmsg := searchPath(name, cpathVal.AsString().S, ".", string(filepath.Separator))
	return []Value{StringValue(s.intern.Intern(errmsg))}, nil
}

func nativeRequire(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'require' (string expected)")
	}
	name := FormatValue(args[0])
	key := s.intern.Intern(name)
	if existing := s.loadedTbl.RawGetStr(key); !existing.IsFalsy() {
		return []Value{existing}, nil
	}
	sv := s.pkgTbl.RawGetStr(s.intern.Intern("searchers"))
	if !sv.IsTable() {
		return nil, fmt.Errorf("'package.searchers' must be a table")
	}
	st := sv.AsTable()
	// Root searchers table (and later loader + extra) on the active thread stack across
	// all Lua callbacks so GC cannot prematurely finalize them while require is running.
	th := s.cur
	savedTop := th.top
	baseTop := savedTop
	for fi := range th.nframe {
		cf := &th.frames[fi]
		if cf.cl != nil {
			if end := cf.base + int(cf.cl.bp.proto.MaxStackSize); end > baseTop {
				baseTop = end
			}
		}
	}
	if err := th.ensureStack(baseTop + 4); err != nil {
		return nil, err
	}
	th.stack[baseTop] = sv
	th.stack[baseTop+1] = NilValue()
	th.top = baseTop + 2
	defer func() {
		th.stack[baseTop] = NilValue()
		th.stack[baseTop+1] = NilValue()
		th.top = savedTop
	}()
	var errs strings.Builder
	var loader, extra Value
	for i := int64(1); ; i++ {
		searcher := st.RawGetInt(i)
		if searcher.IsNil() {
			return nil, fmt.Errorf("module '%s' not found:%s", name, errs.String())
		}
		res, err := s.Call(searcher, StringValue(key))
		if err != nil {
			return nil, err
		}
		if len(res) > 0 && res[0].IsFunction() {
			loader = res[0]
			if len(res) > 1 {
				extra = res[1]
			} else {
				extra = NilValue()
			}
			break
		}
		if len(res) > 0 && res[0].IsString() {
			errs.WriteString("\n\t")
			errs.WriteString(res[0].AsString().S)
		}
	}
	th.stack[baseTop] = loader
	th.stack[baseTop+1] = extra
	res, err := s.Call(loader, StringValue(key), extra)
	if err != nil {
		return nil, err
	}
	if len(res) > 0 && !res[0].IsNil() {
		s.loadedTbl.RawSetStr(s, key, res[0])
	} else if s.loadedTbl.RawGetStr(key).IsNil() {
		s.loadedTbl.RawSetStr(s, key, TrueValue())
	}
	return []Value{s.loadedTbl.RawGetStr(key), extra}, nil
}

func searchPath(name, path, sep, rep string) (string, string) {
	if sep != "" {
		name = strings.ReplaceAll(name, sep, rep)
	}
	expanded := strings.ReplaceAll(path, "?", name)
	for tmpl := range strings.SplitSeq(expanded, ";") {
		if tmpl == "" {
			continue
		}
		if f, err := os.Open(tmpl); err == nil {
			_ = f.Close()
			return tmpl, ""
		}
	}
	// Exact Lua 5.5 loadlib.c:pusherrornotfound formatting
	errmsg := "no file '" + strings.ReplaceAll(expanded, ";", "'\n\tno file '") + "'"
	return "", errmsg
}

func nativePackageSearchPath(s *State, args []Value) ([]Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("bad arguments to 'searchpath'")
	}
	name := FormatValue(args[0])
	path := FormatValue(args[1])
	sep := "."
	if len(args) >= 3 && !args[2].IsNil() {
		sep = FormatValue(args[2])
	}
	rep := string(filepath.Separator)
	if len(args) >= 4 && !args[3].IsNil() {
		rep = FormatValue(args[3])
	}
	fname, errmsg := searchPath(name, path, sep, rep)
	if fname == "" {
		return []Value{NilValue(), StringValue(s.intern.Intern(errmsg))}, nil
	}
	return []Value{StringValue(s.intern.Intern(fname))}, nil
}

// AddFS registers a Lua package.searchers loader backed by fsys (e.g. embed.FS or os.DirFS).
// Multiple AddFS calls are searched in registration order, right after package.preload and before
// OS filesystem searchers. Module paths honor package.path templates (with leading "./" stripped)
// and reject invalid io/fs paths (such as ".." traversal or absolute paths).
func (s *State) AddFS(fsys fs.FS) {
	if fsys == nil || s.pkgTbl == nil {
		return
	}
	sv := s.pkgTbl.RawGetStr(s.intern.Intern("searchers"))
	if !sv.IsTable() {
		return
	}
	st := sv.AsTable()
	searcher := NativeValue(&NativeFunc{
		Name: "searcher_fs",
		Fn: func(s *State, args []Value) ([]Value, error) {
			return searchFSModule(s, fsys, args)
		},
	})
	n := st.Len()
	pos := min(int64(2+s.fsSearchers), n+1)
	if pos < 1 {
		pos = 1
	}
	for i := n; i >= pos; i-- {
		st.RawSetInt(s, i+1, st.RawGetInt(i))
	}
	st.RawSetInt(s, pos, searcher)
	s.fsSearchers++
}

// AddSearcher appends a custom module searcher function to package.searchers.
func (s *State) AddSearcher(fn *NativeFunc) {
	if fn == nil || s.pkgTbl == nil {
		return
	}
	sv := s.pkgTbl.RawGetStr(s.intern.Intern("searchers"))
	if !sv.IsTable() {
		return
	}
	st := sv.AsTable()
	st.RawSetInt(s, st.Len()+1, NativeValue(fn))
}

func searchFSModule(s *State, fsys fs.FS, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_fs' (string expected)")
	}
	name := FormatValue(args[0])
	pathStr := defaultLuaPath
	if pv := s.pkgTbl.RawGetStr(s.intern.Intern("path")); pv.IsString() {
		pathStr = pv.AsString().S
	}
	rel := strings.ReplaceAll(name, ".", "/")
	expanded := strings.ReplaceAll(pathStr, "?", rel)
	var tried []string
	for tmpl := range strings.SplitSeq(expanded, ";") {
		if tmpl == "" {
			continue
		}
		cand := strings.ReplaceAll(tmpl, "\\", "/")
		for strings.HasPrefix(cand, "./") {
			cand = cand[2:]
		}
		if !fs.ValidPath(cand) {
			tried = append(tried, fmt.Sprintf("no file '%s' (invalid fs path)", cand))
			continue
		}
		data, err := fs.ReadFile(fsys, cand)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				tried = append(tried, fmt.Sprintf("no file '%s' (in fs.FS)", cand))
				continue
			}
			return nil, fmt.Errorf("error reading module '%s' from file '%s': %w", name, cand, err)
		}
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(data)) * 2); err != nil {
				return nil, err
			}
		}
		fn, err := s.LoadChunk(data, "@"+cand)
		if err != nil {
			return nil, fmt.Errorf("error loading module '%s' from file '%s':\n\t%s", name, cand, err.Error())
		}
		return []Value{fn, StringValue(s.intern.Intern(cand))}, nil
	}
	if len(tried) == 0 {
		return []Value{StringValue(s.intern.Intern("no file in fs.FS"))}, nil
	}
	return []Value{StringValue(s.intern.Intern(strings.Join(tried, "\n\t")))}, nil
}
