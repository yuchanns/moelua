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
	"unsafe"
	"weak"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
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
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'warn' (string expected)")
	}
	for i := 1; i < len(args); i++ {
		if !args[i].IsString() && !args[i].IsNumber() {
			return nil, fmt.Errorf("bad argument #%d to 'warn' (string expected)", i+1)
		}
	}
	if len(args) == 1 && args[0].IsString() {
		first := args[0].AsString().S
		if strings.HasPrefix(first, "@") {
			switch first {
			case "@on":
				s.warnOn = true
			case "@off":
				s.warnOn = false
			}
			return nil, nil
		}
	}
	if !s.warnOn || s.errOut == nil {
		return nil, nil
	}
	// Stream arguments directly to s.errOut without allocating a concatenated buffer
	// (matches Lua 5.5 lbaselib.c:luaB_warn).
	_, _ = io.WriteString(s.errOut, "Lua warning: ")
	for _, a := range args {
		if a.IsString() {
			_, _ = io.WriteString(s.errOut, a.AsString().S)
		} else {
			_, _ = io.WriteString(s.errOut, FormatValue(a))
		}
	}
	_, _ = io.WriteString(s.errOut, "\n")
	return nil, nil
}

type loadReaderLuaErr struct {
	val Value
}

func (e loadReaderLuaErr) Error() string { return FormatValue(e.val) }

type loadFileReadErr struct {
	err error
}

func (e loadFileReadErr) Error() string { return e.err.Error() }

func (e loadFileReadErr) Unwrap() error { return e.err }

type fileErrReader struct {
	r io.Reader
}

func (f fileErrReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, loadFileReadErr{err: err}
	}
	return n, err
}

// luaFuncReader adapts a Lua reader function passed to load(reader, ...) into a lazy,
// ZIO-style io.Reader without draining the whole stream into memory before compiling/undumping.
type luaFuncReader struct {
	s   *State
	fn  Value
	cur string
	off int
	eof bool
}

func (r *luaFuncReader) Read(p []byte) (int, error) {
	if r.off < len(r.cur) {
		n := copy(p, r.cur[r.off:])
		r.off += n
		if r.off >= len(r.cur) {
			r.cur = ""
			r.off = 0
		}
		return n, nil
	}
	if r.eof {
		return 0, io.EOF
	}
	res, err := r.s.Call(r.fn)
	if err != nil {
		r.eof = true
		if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
			return 0, err
		}
		return 0, loadReaderLuaErr{val: errorToLuaValue(r.s, err)}
	}
	if len(res) == 0 || res[0].IsNil() {
		r.eof = true
		return 0, io.EOF
	}
	if !res[0].IsString() && !res[0].IsNumber() {
		r.eof = true
		return 0, fmt.Errorf("reader function must return a string")
	}
	piece := FormatValue(res[0])
	if piece == "" {
		r.eof = true
		return 0, io.EOF
	}
	n := copy(p, piece)
	if n < len(piece) {
		r.cur = piece
		r.off = n
	}
	return n, nil
}

func (s *State) undumpBytes(data []byte) (*bytecode.Proto, error) {
	if s.memLimit > 0 {
		return bytecode.UndumpWithLimit(data, func(n int64) error {
			return s.checkMem(n)
		})
	}
	return bytecode.Undump(data)
}

func (s *State) loadChunkString(src, chunkName, mode string, env Value, hasEnv bool) (Value, error) {
	isBin := len(src) > 0 && src[0] == 0x1b
	if isBin {
		if !strings.ContainsRune(mode, 'b') {
			return NilValue(), fmt.Errorf("attempt to load a binary chunk (mode is '%s')", mode)
		}
		p, err := s.undumpBytes(unsafe.Slice(unsafe.StringData(src), len(src)))
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
	var p *bytecode.Proto
	var err error
	if s.compilerReaderFn != nil {
		// Default compiler: preserve string fast path while honoring memLimit if configured.
		var opts []compiler.Option
		if s.memLimit > 0 {
			opts = append(opts, compiler.WithMemoryCheck(func(n int64) error {
				return s.checkMem(n)
			}))
		}
		p, err = compiler.Compile(chunkName, src, opts...)
	} else {
		p, err = s.compilerFn(chunkName, src)
	}
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

func (s *State) loadChunkBytes(data []byte, chunkName, mode string, env Value, hasEnv bool) (Value, error) {
	isBin := len(data) > 0 && data[0] == 0x1b
	if isBin {
		if !strings.ContainsRune(mode, 'b') {
			return NilValue(), fmt.Errorf("attempt to load a binary chunk (mode is '%s')", mode)
		}
		p, err := s.undumpBytes(data)
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
	return s.loadChunkString(string(data), chunkName, mode, env, hasEnv)
}

func (s *State) loadChunkReader(r io.Reader, chunkName, mode string, env Value, hasEnv, stripFileHeader bool) (Value, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 4096)
	}
	if stripFileHeader {
		// Strip optional UTF-8 BOM (only peek 3 bytes if byte 0 == 0xEF).
		b0, err := br.Peek(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return NilValue(), loadFileReadErr{err: err}
		}
		if len(b0) == 1 && b0[0] == 0xEF {
			b3, perr := br.Peek(3)
			if perr != nil && !errors.Is(perr, io.EOF) {
				return NilValue(), loadFileReadErr{err: perr}
			}
			if len(b3) == 3 && b3[0] == 0xEF && b3[1] == 0xBB && b3[2] == 0xBF {
				_, _ = br.Discard(3)
			}
		}
		// Strip optional initial '#' comment line (lauxlib.c:luaL_loadfilex).
		b0, err = br.Peek(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return NilValue(), loadFileReadErr{err: err}
		}
		if len(b0) == 1 && b0[0] == '#' {
			for {
				pb, perr := br.Peek(1)
				if perr != nil {
					if errors.Is(perr, io.EOF) {
						break
					}
					return NilValue(), loadFileReadErr{err: perr}
				}
				if pb[0] == '\n' {
					b2, perr2 := br.Peek(2)
					if perr2 != nil && !errors.Is(perr2, io.EOF) {
						return NilValue(), loadFileReadErr{err: perr2}
					}
					// Keep '\n' for text chunks so line 2 stays line 2; consume it if binary chunk follows.
					if len(b2) == 2 && b2[1] == 0x1b {
						_, _ = br.Discard(1)
					}
					break
				}
				if _, rerr := br.ReadByte(); rerr != nil {
					if errors.Is(rerr, io.EOF) {
						break
					}
					return NilValue(), loadFileReadErr{err: rerr}
				}
			}
		}
	}

	b0, err := br.Peek(1)
	if err != nil && !errors.Is(err, io.EOF) {
		if stripFileHeader {
			return NilValue(), loadFileReadErr{err: err}
		}
		return NilValue(), err
	}
	isBin := len(b0) == 1 && b0[0] == 0x1b
	var streamTempBytes int64
	defer func() {
		s.gc.tempBytes -= streamTempBytes
	}()
	var checkMem func(int64) error
	if s.memLimit > 0 {
		checkMem = func(n int64) error {
			if n <= streamTempBytes {
				return nil
			}
			delta := n - streamTempBytes
			if merr := s.checkMem(delta); merr != nil {
				return merr
			}
			streamTempBytes = n
			s.gc.tempBytes += delta
			return nil
		}
	}
	if isBin {
		if !strings.ContainsRune(mode, 'b') {
			return NilValue(), fmt.Errorf("attempt to load a binary chunk (mode is '%s')", mode)
		}
		p, uerr := bytecode.UndumpReaderWithLimit(br, checkMem)
		if uerr != nil {
			return NilValue(), uerr
		}
		s.gc.tempBytes -= streamTempBytes
		streamTempBytes = 0
		cl, lerr := s.Load(p)
		if lerr != nil {
			return NilValue(), lerr
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
	var p *bytecode.Proto
	if s.compilerReaderFn != nil {
		opts := []compiler.Option{compiler.SkipFileHeader(false)}
		if checkMem != nil {
			opts = append(opts, compiler.WithMemoryCheck(checkMem))
		}
		p, err = s.compilerReaderFn(chunkName, br, opts...)
		if err != nil {
			return NilValue(), err
		}
	} else {
		// Custom host compiler registered via SetCompiler: read bounded stream and delegate.
		data, rerr := s.readBoundedStream(br)
		if rerr != nil {
			return NilValue(), rerr
		}
		p, err = s.compilerFn(chunkName, string(data))
		if err != nil {
			return NilValue(), err
		}
	}
	s.gc.tempBytes -= streamTempBytes
	streamTempBytes = 0
	cl, lerr := s.Load(p)
	if lerr != nil {
		return NilValue(), lerr
	}
	if hasEnv && len(cl.upvals) > 0 {
		cl.upvals[0] = &Upvalue{closed: env}
	}
	return ClosureValue(cl), nil
}

func (s *State) readBoundedStream(r io.Reader) ([]byte, error) {
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
			if readBytes+int64(n) > (1 << 30) {
				return nil, &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
			}
			buf.Write(tmp[:n])
			readBytes += int64(n)
			s.gc.tempBytes += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func nativeLoad(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'load'")
	}
	chunkName := "=(load)"
	isStr := args[0].IsString() || args[0].IsNumber()
	var strSrc string
	if isStr {
		strSrc = FormatValue(args[0])
		chunkName = strSrc
	} else if !args[0].IsFunction() {
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
	var fn Value
	var err error
	if isStr {
		fn, err = s.loadChunkString(strSrc, chunkName, mode, env, hasEnv)
	} else {
		lr := &luaFuncReader{s: s, fn: args[0]}
		fn, err = s.loadChunkReader(lr, chunkName, mode, env, hasEnv, false)
	}
	if err != nil {
		if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
			return nil, err
		}
		var rle loadReaderLuaErr
		if errors.As(err, &rle) {
			return []Value{NilValue(), rle.val}, nil
		}
		var le *LuaError
		if errors.As(err, &le) {
			return []Value{NilValue(), le.Val}, nil
		}
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

// LoadChunkReader streams a text or binary Lua 5.5.1 chunk from r without materializing
// the entire input buffer in memory, stripping UTF-8 BOM and an initial '#' comment line.
func (s *State) LoadChunkReader(r io.Reader, chunkName string) (Value, error) {
	if r == nil {
		return NilValue(), fmt.Errorf("cannot read %s: nil reader", chunkName)
	}
	return s.loadChunkReader(r, chunkName, "bt", NilValue(), false, true)
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
	chunkName := "=stdin"
	var fname string
	var r io.Reader
	var f *os.File
	if len(args) == 0 || args[0].IsNil() {
		// Pass io.stdin's *bufio.Reader directly so loadChunkReader reuses its buffer without
		// creating a second read-ahead buffer that would consume bytes past the chunk.
		r = s.StdinReader()
	} else {
		fname = FormatValue(args[0])
		chunkName = "@" + fname
		var err error
		f, err = os.Open(fname)
		if err != nil {
			return []Value{NilValue(), StringValue(s.intern.Intern(fmt.Sprintf("cannot open %s", err.Error())))}, nil
		}
		defer f.Close()
		r = fileErrReader{r: f}
	}
	mode := "bt"
	if len(args) >= 2 && !args[1].IsNil() {
		mode = FormatValue(args[1])
	}
	hasEnv := len(args) >= 3
	env := NilValue()
	if hasEnv {
		env = args[2]
	}
	fn, err := s.loadChunkReader(r, chunkName, mode, env, hasEnv, true)
	if err != nil {
		if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
			return nil, err
		}
		var le *LuaError
		if errors.As(err, &le) {
			return []Value{NilValue(), le.Val}, nil
		}
		var fre loadFileReadErr
		if errors.As(err, &fre) {
			target := "stdin"
			if fname != "" {
				target = fname
			}
			return []Value{NilValue(), StringValue(s.intern.Intern(fmt.Sprintf("cannot read %s: %s", target, fre.err.Error())))}, nil
		}
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
	if lf, ok := asLuaFile(s.defaultOutput); ok && !lf.closed {
		_ = lf.flushWriter()
	}
	if ioVal := s.env.RawGetStr(s.intern.Intern("io")); ioVal.IsTable() {
		ioTbl := ioVal.AsTable()
		for _, name := range [...]string{"stdout", "stderr"} {
			if lf, ok := asLuaFile(ioTbl.RawGetStr(s.intern.Intern(name))); ok && !lf.closed {
				_ = lf.flushWriter()
			}
		}
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

func validPopenMode(mode string) bool {
	if runtime.GOOS == "windows" {
		switch mode {
		case "r", "w", "rb", "rt", "wb", "wt":
			return true
		}
		return false
	}
	return mode == "r" || mode == "w"
}

func nativeIOPopen(s *State, args []Value) ([]Value, error) {
	prog, err := checkStringArg(args, 0, "popen")
	if err != nil {
		return nil, err
	}
	mode := "r"
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'popen' (string expected, got %s)", s.objTypeName(args[1]))
		}
		mode = FormatValue(args[1])
		if !validPopenMode(mode) {
			return nil, fmt.Errorf("bad argument #2 to 'popen' (invalid mode)")
		}
	}
	// Flush buffered output streams before spawning the child process
	// (matches Lua 5.5 liolib.c:l_popen calling fflush(NULL) before popen).
	s.flushOpenFiles()
	cmd := shellCommand(prog)
	s.inheritedStdStreams(cmd)
	var f *os.File
	if mode[0] == 'w' {
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

// StdinReader returns the buffered io.Reader backing the State's original io.stdin
// so streaming script loads (`loadfile()`, `moelua -`) share buffering with earlier
// `io.read` calls without draining all of stdin into memory up front.
func (s *State) StdinReader() io.Reader {
	if lf, ok := asLuaFile(s.stdinInput); ok && !lf.closed && lf.f != nil {
		return lf.reader()
	}
	return bytes.NewReader(nil)
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
			if !errors.Is(err, io.EOF) {
				return NilValue(), false, ioReadErr{err}
			}
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
	var readErr error
	nextChar := func() {
		b, err := r.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) && readErr == nil {
				readErr = err
			}
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
		if readErr != nil {
			return NilValue(), false, ioReadErr{readErr}
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
			if readErr != nil {
				return NilValue(), false, ioReadErr{readErr}
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
				if readErr != nil {
					return NilValue(), false, ioReadErr{readErr}
				}
				return NilValue(), false, nil
			}
		}
	}
	if !eof {
		_ = r.UnreadByte()
	}
	if readErr != nil {
		return NilValue(), false, ioReadErr{readErr}
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
	if len(fmts) == 1 {
		v, _, err := readOneItem(s, lf, fmts[0])
		if err != nil {
			if re, ok := err.(ioReadErr); ok {
				return ioFail(s, re.err), nil
			}
			return nil, err
		}
		return []Value{v}, nil
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(len(fmts)) * 16); err != nil {
			return nil, err
		}
	}
	// Root accumulated results on the active thread stack across subsequent readOneItem calls
	// so emergency GC triggered while reading later formats still accounts for earlier strings.
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
	if err := th.ensureStack(baseTop + len(fmts) + 2); err != nil {
		return nil, err
	}
	written := 0
	defer func() {
		if written > 0 {
			fillNil(th.stack[baseTop : baseTop+written])
		}
		th.top = savedTop
	}()
	cancelFn := s.nativeCancelFunc()
	for idx, f := range fmts {
		if cancelFn != nil && idx > 0 && (idx&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		v, ok, err := readOneItem(s, lf, f)
		if err != nil {
			if re, ok := err.(ioReadErr); ok {
				return ioFail(s, re.err), nil
			}
			return nil, err
		}
		th.stack[baseTop+written] = v
		written++
		th.top = baseTop + written
		if !ok {
			break
		}
	}
	out := make([]Value, written)
	copy(out, th.stack[baseTop:baseTop+written])
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

func makeLinesIter(s *State, fileVal Value, fmts []Value, autoClose bool) ([]Value, error) {
	// Match Lua 5.5 liolib.c:aux_lines upvalue layout:
	//   1) file handle
	//   2) number of format arguments
	//   3) boolean indicating whether to close on EOF
	//   4..3+n) format arguments
	upvals := make([]Value, 3+len(fmts))
	upvals[0] = fileVal
	upvals[1] = IntValue(int64(len(fmts)))
	upvals[2] = BoolValue(autoClose)
	copy(upvals[3:], fmts)
	if s.memLimit > 0 || s.gc.tier1 {
		if err := s.checkMemAndAccount(48 + int64(len(upvals))*16); err != nil {
			return nil, err
		}
	}
	var iter *NativeFunc
	iter = &NativeFunc{
		Name:        "io.lines_iter",
		NumUpvals:   uint8(len(upvals)),
		Collectable: true,
		upvals:      upvals,
		Fn: func(s *State, _ []Value) ([]Value, error) {
			curFileVal := iter.upvals[0]
			curAutoClose := !iter.upvals[2].IsFalsy()
			curFmts := iter.upvals[3:]
			lf, ok := asLuaFile(curFileVal)
			if !ok || lf.closed {
				return nil, fmt.Errorf("file is already closed")
			}
			callArgs := make([]Value, 1+len(curFmts))
			callArgs[0] = curFileVal
			copy(callArgs[1:], curFmts)
			res, err := nativeFileRead(s, callArgs)
			if err != nil {
				return nil, err
			}
			if len(res) >= 2 && res[0].IsNil() && res[1].IsString() {
				// Propagate I/O error from f:lines() iterator (files.lua:386)!
				return nil, fmt.Errorf("%s", res[1].AsString().S)
			}
			if len(res) == 0 || res[0].IsNil() {
				if curAutoClose && !lf.closed && !lf.isStd {
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
	return []Value{NativeValue(iter), NilValue(), NilValue(), closeVar}, nil
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
	return makeLinesIter(s, args[0], args[1:], false)
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
		return makeLinesIter(s, s.defaultInput, fmts, false)
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
	res, err := makeLinesIter(s, ud, args[1:], true)
	if err != nil {
		_ = lf.closeFile()
		return nil, err
	}
	return res, nil
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

func checkTimeArg(s *State, args []Value, idx int, fnName string) (int64, error) {
	if idx >= len(args) {
		return 0, fmt.Errorf("bad argument #%d to '%s' (number expected, got no value)", idx+1, fnName)
	}
	a := args[idx]
	if i, ok := a.CoerceInt(); ok {
		return i, nil
	}
	if a.IsNumber() {
		return 0, fmt.Errorf("bad argument #%d to '%s' (number has no integer representation)", idx+1, fnName)
	}
	if a.IsString() {
		if _, ok := luastdlib.StrToFloat(a.AsString().S); ok {
			return 0, fmt.Errorf("bad argument #%d to '%s' (number has no integer representation)", idx+1, fnName)
		}
	}
	return 0, fmt.Errorf("bad argument #%d to '%s' (number expected, got %s)", idx+1, fnName, s.objTypeName(a))
}

func nativeOSDiffTime(s *State, args []Value) ([]Value, error) {
	t1, err := checkTimeArg(s, args, 0, "os.difftime")
	if err != nil {
		return nil, err
	}
	t2, err := checkTimeArg(s, args, 1, "os.difftime")
	if err != nil {
		return nil, err
	}
	var diff float64
	if t1 >= t2 {
		diff = float64(uint64(t1) - uint64(t2))
	} else {
		diff = -float64(uint64(t2) - uint64(t1))
	}
	return []Value{FloatValue(diff)}, nil
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
		tbl, err := s.TryNewTable(0, 10)
		if err != nil {
			return nil, err
		}
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
	checkGrow := func(add int) error {
		if s.memLimit > 0 {
			nextLen := sb.Len() + add
			need := int64(nextLen) + 48
			if sb.Cap() < nextLen {
				if doubled := sb.Cap() * 2; doubled > nextLen {
					need = int64(doubled) + 48
				}
			}
			if err := s.checkMem(need); err != nil {
				return err
			}
		}
		return nil
	}
	writeByte := func(b byte) error {
		if err := checkGrow(1); err != nil {
			return err
		}
		sb.WriteByte(b)
		return nil
	}
	writeStr := func(chunk string) error {
		if err := checkGrow(len(chunk)); err != nil {
			return err
		}
		sb.WriteString(chunk)
		return nil
	}
	cancelFn := s.nativeCancelFunc()
	var steps int64
	for i := 0; i < len(fmtStr); i++ {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		if fmtStr[i] != '%' {
			if err := writeByte(fmtStr[i]); err != nil {
				return nil, err
			}
			continue
		}
		i++
		if i >= len(fmtStr) {
			return nil, fmt.Errorf("invalid conversion specifier '%%'")
		}
		var err error
		switch fmtStr[i] {
		case '%':
			err = writeByte('%')
		case 'Y':
			err = writeStr(fmt.Sprintf("%04d", tm.Year()))
		case 'y':
			err = writeStr(fmt.Sprintf("%02d", tm.Year()%100))
		case 'C':
			err = writeStr(fmt.Sprintf("%02d", tm.Year()/100))
		case 'm':
			err = writeStr(fmt.Sprintf("%02d", tm.Month()))
		case 'd':
			err = writeStr(fmt.Sprintf("%02d", tm.Day()))
		case 'e':
			err = writeStr(fmt.Sprintf("%2d", tm.Day()))
		case 'H':
			err = writeStr(fmt.Sprintf("%02d", tm.Hour()))
		case 'I':
			h12 := tm.Hour() % 12
			if h12 == 0 {
				h12 = 12
			}
			err = writeStr(fmt.Sprintf("%02d", h12))
		case 'p':
			if tm.Hour() < 12 {
				err = writeStr("AM")
			} else {
				err = writeStr("PM")
			}
		case 'M':
			err = writeStr(fmt.Sprintf("%02d", tm.Minute()))
		case 'S':
			err = writeStr(fmt.Sprintf("%02d", tm.Second()))
		case 'a':
			err = writeStr(tm.Format("Mon"))
		case 'A':
			err = writeStr(tm.Format("Monday"))
		case 'b', 'h':
			err = writeStr(tm.Format("Jan"))
		case 'B':
			err = writeStr(tm.Format("January"))
		case 'c':
			err = writeStr(tm.Format("Mon Jan _2 15:04:05 2006"))
		case 'D', 'x':
			err = writeStr(tm.Format("01/02/06"))
		case 'F':
			err = writeStr(fmt.Sprintf("%04d-%02d-%02d", tm.Year(), tm.Month(), tm.Day()))
		case 'r':
			err = writeStr(tm.Format("03:04:05 PM"))
		case 'R':
			err = writeStr(fmt.Sprintf("%02d:%02d", tm.Hour(), tm.Minute()))
		case 'T', 'X':
			err = writeStr(tm.Format("15:04:05"))
		case 'j':
			err = writeStr(fmt.Sprintf("%03d", tm.YearDay()))
		case 'w':
			err = writeStr(fmt.Sprintf("%d", tm.Weekday()))
		case 'u':
			u := int(tm.Weekday())
			if u == 0 {
				u = 7
			}
			err = writeStr(fmt.Sprintf("%d", u))
		case 'U':
			err = writeStr(fmt.Sprintf("%02d", (tm.YearDay()-1+7-int(tm.Weekday()))/7))
		case 'W':
			monWday := (int(tm.Weekday()) + 6) % 7
			err = writeStr(fmt.Sprintf("%02d", (tm.YearDay()-1+7-monWday)/7))
		case 'g':
			isoY, _ := tm.ISOWeek()
			err = writeStr(fmt.Sprintf("%02d", isoY%100))
		case 'G':
			isoY, _ := tm.ISOWeek()
			err = writeStr(fmt.Sprintf("%04d", isoY))
		case 'V':
			_, isoW := tm.ISOWeek()
			err = writeStr(fmt.Sprintf("%02d", isoW))
		case 'n':
			err = writeByte('\n')
		case 't':
			err = writeByte('\t')
		case 'Z':
			zone, _ := tm.Zone()
			err = writeStr(zone)
		case 'z':
			err = writeStr(tm.Format("-0700"))
		default:
			return nil, fmt.Errorf("invalid conversion specifier '%%%c'", fmtStr[i])
		}
		if err != nil {
			return nil, err
		}
	}
	ls, err := s.TryIntern(sb.String())
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
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
	if !args[0].IsString() && !args[0].IsNumber() {
		return nil, fmt.Errorf("bad argument #1 to 'os.execute' (string expected, got %s)", s.objTypeName(args[0]))
	}
	cmd := shellCommand(FormatValue(args[0]))
	s.inheritedStdStreams(cmd)
	return execResult(s, cmd.Run()), nil
}

func nativeOSExit(s *State, args []Value) ([]Value, error) {
	code := 0
	if len(args) >= 1 && !args[0].IsNil() {
		a := args[0]
		if a.IsBool() {
			if a.IsFalse() {
				code = 1
			}
		} else if i, ok := a.CoerceInt(); ok {
			code = int(i)
		} else if a.IsNumber() {
			return nil, fmt.Errorf("bad argument #1 to 'os.exit' (number has no integer representation)")
		} else {
			if a.IsString() {
				if _, ok := luastdlib.StrToFloat(a.AsString().S); ok {
					return nil, fmt.Errorf("bad argument #1 to 'os.exit' (number has no integer representation)")
				}
			}
			return nil, fmt.Errorf("bad argument #1 to 'os.exit' (number expected, got %s)", s.objTypeName(a))
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
	loadedVal := TableValue(s.loadedTbl)
	existing := s.loadedTbl.RawGetStr(key)
	if existing.IsNil() && s.loadedTbl.meta != nil {
		var gerr error
		existing, gerr = s.finishGet(loadedVal, StringValue(key))
		if gerr != nil {
			return nil, gerr
		}
	}
	if !existing.IsFalsy() {
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
		th.stack[baseTop] = res[0]
		if err := s.tableSetStr(loadedVal, key, res[0]); err != nil {
			return nil, err
		}
	}
	finalVal := s.loadedTbl.RawGetStr(key)
	if finalVal.IsNil() && s.loadedTbl.meta != nil {
		var gerr error
		finalVal, gerr = s.finishGet(loadedVal, StringValue(key))
		if gerr != nil {
			return nil, gerr
		}
	}
	if finalVal.IsNil() {
		finalVal = TrueValue()
		if err := s.tableSetStr(loadedVal, key, finalVal); err != nil {
			return nil, err
		}
	}
	return []Value{finalVal, extra}, nil
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
		f, err := fsys.Open(cand)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				tried = append(tried, fmt.Sprintf("no file '%s' (in fs.FS)", cand))
				continue
			}
			return nil, fmt.Errorf("error reading module '%s' from file '%s': %w", name, cand, err)
		}
		if fi, serr := f.Stat(); serr == nil && fi.IsDir() {
			_ = f.Close()
			return nil, fmt.Errorf("error reading module '%s' from file '%s': is a directory", name, cand)
		}
		fn, err := s.LoadChunkReader(fileErrReader{r: f}, "@"+cand)
		_ = f.Close()
		if err != nil {
			if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
				return nil, err
			}
			var fre loadFileReadErr
			if errors.As(err, &fre) {
				return nil, fmt.Errorf("error reading module '%s' from file '%s': %w", name, cand, fre.err)
			}
			if strings.Contains(err.Error(), "not enough memory") {
				return nil, err
			}
			return nil, fmt.Errorf("error loading module '%s' from file '%s':\n\t%s", name, cand, err.Error())
		}
		candStr, ierr := s.TryIntern(cand)
		if ierr != nil {
			return nil, ierr
		}
		return []Value{fn, StringValue(candStr)}, nil
	}
	if len(tried) == 0 {
		return []Value{StringValue(s.intern.Intern("no file in fs.FS"))}, nil
	}
	return []Value{StringValue(s.intern.Intern(strings.Join(tried, "\n\t")))}, nil
}
