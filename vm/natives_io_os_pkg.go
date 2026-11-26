package vm

import (
	"bufio"
	"bytes"
	"context"
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
		got := "no value"
		if len(args) > 0 {
			got = s.objTypeName(args[0])
		}
		return nil, fmt.Errorf("bad argument #1 to 'warn' (string expected, got %s)", got)
	}
	for i := 1; i < len(args); i++ {
		if !args[i].IsString() && !args[i].IsNumber() {
			return nil, fmt.Errorf("bad argument #%d to 'warn' (string expected, got %s)", i+1, s.objTypeName(args[i]))
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
	cancelFn := s.nativeCancelFunc()
	work := 0
	_, _ = io.WriteString(s.errOut, "Lua warning: ")
	for _, a := range args {
		var str string
		if a.IsString() {
			str = a.AsString().S
		} else {
			str = FormatValue(a)
		}
		if err := writeIOStringWithCancel(s.errOut, str, cancelFn, &work); err != nil {
			return nil, err
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
	cancelFn := s.nativeCancelFunc()
	if s.memLimit > 0 || cancelFn != nil {
		var checkMem func(int64) error
		if s.memLimit > 0 {
			checkMem = func(n int64) error {
				return s.checkMem(n)
			}
		}
		return bytecode.UndumpWithControls(data, checkMem, cancelFn)
	}
	return bytecode.Undump(data)
}

func (s *State) loadChunkString(src, chunkName, mode string, env Value, hasEnv, stripFileHeader bool) (Value, error) {
	cancelFn := s.nativeCancelFunc()
	isBin := len(src) > 0 && src[0] == 0x1b
	if isBin {
		allowBin, cerr := containsByteWithCancel(mode, 'b', cancelFn)
		if cerr != nil {
			return NilValue(), cerr
		}
		if !allowBin {
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
	allowText, cerr := containsByteWithCancel(mode, 't', cancelFn)
	if cerr != nil {
		return NilValue(), cerr
	}
	if !allowText {
		return NilValue(), fmt.Errorf("attempt to load a text chunk (mode is '%s')", mode)
	}
	if s.compilerFn == nil {
		return NilValue(), fmt.Errorf("text compiler not configured")
	}
	var p *bytecode.Proto
	var err error
	if s.compilerReaderFn != nil {
		// Default compiler: preserve string fast path while honoring memLimit, execution controls,
		// and exact Lua 5.5 file-header stripping policy (load(str) does NOT strip BOM/#!).
		opts := []compiler.Option{compiler.SkipFileHeader(stripFileHeader)}
		if s.memLimit > 0 {
			opts = append(opts, compiler.WithMemoryCheck(func(n int64) error {
				return s.checkMem(n)
			}))
		}
		if cancelFn != nil {
			opts = append(opts, compiler.WithCancel(cancelFn))
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
		allowBin, cerr := containsByteWithCancel(mode, 'b', s.nativeCancelFunc())
		if cerr != nil {
			return NilValue(), cerr
		}
		if !allowBin {
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
	return s.loadChunkString(string(data), chunkName, mode, env, hasEnv, false)
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
			cancelFn := s.nativeCancelFunc()
			var skipped int64
			for {
				skipped++
				if cancelFn != nil && (skipped&4095) == 0 {
					if cerr := cancelFn(); cerr != nil {
						return NilValue(), cerr
					}
				}
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
	cancelFn := s.nativeCancelFunc()
	if isBin {
		allowBin, cerr := containsByteWithCancel(mode, 'b', cancelFn)
		if cerr != nil {
			return NilValue(), cerr
		}
		if !allowBin {
			return NilValue(), fmt.Errorf("attempt to load a binary chunk (mode is '%s')", mode)
		}
		p, uerr := bytecode.UndumpReaderWithControls(br, checkMem, cancelFn)
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
	allowText, cerr := containsByteWithCancel(mode, 't', cancelFn)
	if cerr != nil {
		return NilValue(), cerr
	}
	if !allowText {
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
		if cancelFn != nil {
			opts = append(opts, compiler.WithCancel(cancelFn))
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
	var reads int64
	defer func() {
		s.gc.tempBytes -= readBytes
	}()
	cancelFn := s.nativeCancelFunc()
	var tmp [4096]byte
	for {
		n, err := r.Read(tmp[:])
		reads++
		if n > 0 {
			if merr := s.checkMem(int64(n)); merr != nil {
				return nil, merr
			}
			if readBytes+int64(n) > (1 << 30) {
				return nil, &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
			}
			prevStride := readBytes / 4096
			buf.Write(tmp[:n])
			readBytes += int64(n)
			s.gc.tempBytes += int64(n)
			if cancelFn != nil && (readBytes/4096 > prevStride || (reads&63) == 0) {
				if cerr := cancelFn(); cerr != nil {
					return nil, cerr
				}
			}
		} else if cancelFn != nil && (reads&63) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return nil, cerr
			}
		}
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s *State) internErrStr(msg string) Value {
	if ls, err := s.TryIntern(msg); err == nil {
		return StringValue(ls)
	}
	return StringValue(s.intern.Intern("not enough memory"))
}

func nativeLoad(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("bad argument #1 to 'load' (string or function expected, got no value)")
	}
	chunkName := "=(load)"
	isStr := args[0].IsString() || args[0].IsNumber()
	var strSrc string
	if isStr {
		strSrc = FormatValue(args[0])
		chunkName = strSrc
	} else if !args[0].IsFunction() {
		return nil, fmt.Errorf("bad argument #1 to 'load' (string or function expected, got %s)", s.objTypeName(args[0]))
	}
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'load' (string expected, got %s)", s.objTypeName(args[1]))
		}
		chunkName = FormatValue(args[1])
	}
	mode := "bt"
	if len(args) >= 3 && !args[2].IsNil() {
		if !args[2].IsString() && !args[2].IsNumber() {
			return nil, fmt.Errorf("bad argument #3 to 'load' (string expected, got %s)", s.objTypeName(args[2]))
		}
		mode = FormatValue(args[2])
		// Lua 5.5 rejects fixed-buffer mode 'B' from Lua scripts (lbaselib.c:342-349).
		hasB, cerr := containsByteWithCancel(mode, 'B', s.nativeCancelFunc())
		if cerr != nil {
			return nil, cerr
		}
		if hasB {
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
		fn, err = s.loadChunkString(strSrc, chunkName, mode, env, hasEnv, false)
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
		return []Value{NilValue(), s.internErrStr(err.Error())}, nil
	}
	return []Value{fn}, nil
}

func (s *State) checkPreCancel() error {
	if s.abortErr != nil {
		return s.abortErr
	}
	if s.insnLimit > 0 && s.insnCount >= s.insnLimit {
		s.abortErr = ErrInstructionLimit
		return ErrInstructionLimit
	}
	if s.ctx != nil {
		if err := s.ctx.Err(); err != nil {
			s.abortErr = err
			return err
		}
	}
	return nil
}

// CompileString compiles Lua 5.5.1 source text into a verified *bytecode.Proto while
// honoring the State's memory limit, cancellation context, and instruction budget.
func (s *State) CompileString(name, src string) (*bytecode.Proto, error) {
	if err := s.checkPreCancel(); err != nil {
		return nil, err
	}
	var compileTempBytes int64
	defer func() {
		s.gc.tempBytes -= compileTempBytes
	}()
	var opts []compiler.Option
	if s.memLimit > 0 {
		opts = append(opts, compiler.WithMemoryCheck(func(n int64) error {
			if n <= compileTempBytes {
				return nil
			}
			delta := n - compileTempBytes
			if merr := s.checkMem(delta); merr != nil {
				return merr
			}
			compileTempBytes = n
			s.gc.tempBytes += delta
			return nil
		}))
	}
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		opts = append(opts, compiler.WithCancel(cancelFn))
	}
	return compiler.Compile(name, src, opts...)
}

// CompileReader streams Lua 5.5.1 source text from r into a verified *bytecode.Proto while
// honoring the State's memory limit, cancellation context, and instruction budget.
func (s *State) CompileReader(name string, r io.Reader) (*bytecode.Proto, error) {
	if r == nil {
		return nil, fmt.Errorf("cannot read %s: nil reader", name)
	}
	if err := s.checkPreCancel(); err != nil {
		return nil, err
	}
	var compileTempBytes int64
	defer func() {
		s.gc.tempBytes -= compileTempBytes
	}()
	var opts []compiler.Option
	if s.memLimit > 0 {
		opts = append(opts, compiler.WithMemoryCheck(func(n int64) error {
			if n <= compileTempBytes {
				return nil
			}
			delta := n - compileTempBytes
			if merr := s.checkMem(delta); merr != nil {
				return merr
			}
			compileTempBytes = n
			s.gc.tempBytes += delta
			return nil
		}))
	}
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		opts = append(opts, compiler.WithCancel(cancelFn))
	}
	return compiler.CompileReader(name, r, opts...)
}

// LoadChunk loads a text or binary Lua 5.5.1 chunk from raw bytes, stripping UTF-8 BOM
// and an initial '#' comment line per Lua 5.5 lauxlib.c:luaL_loadfilex.
func (s *State) LoadChunk(data []byte, chunkName string) (Value, error) {
	data = stripBOMAndShebang(data)
	return s.loadChunkBytes(data, chunkName, "bt", NilValue(), false)
}

// LoadTextChunk loads a text-only Lua 5.5.1 chunk from raw bytes without stripping BOM or '#'
// (matching PUC-Rio luaL_loadbufferx(..., "t") used by lua.c -e, LUA_INIT, and REPL).
func (s *State) LoadTextChunk(data []byte, chunkName string) (Value, error) {
	return s.loadChunkBytes(data, chunkName, "t", NilValue(), false)
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
	mode := "bt"
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'loadfile' (string expected, got %s)", s.objTypeName(args[1]))
		}
		mode = FormatValue(args[1])
		hasB, cerr := containsByteWithCancel(mode, 'B', s.nativeCancelFunc())
		if cerr != nil {
			return nil, cerr
		}
		if hasB {
			return nil, fmt.Errorf("bad argument #2 to 'loadfile' (invalid mode)")
		}
	}
	chunkName := "=stdin"
	var fname string
	var r io.Reader
	var f *os.File
	if len(args) == 0 || args[0].IsNil() {
		// Pass io.stdin's *bufio.Reader directly so loadChunkReader reuses its buffer without
		// creating a second read-ahead buffer that would consume bytes past the chunk.
		r = s.StdinReader()
	} else {
		if !args[0].IsString() && !args[0].IsNumber() {
			return nil, fmt.Errorf("bad argument #1 to 'loadfile' (string expected, got %s)", s.objTypeName(args[0]))
		}
		fname = FormatValue(args[0])
		chunkName = "@" + fname
		var err error
		f, err = os.Open(fname)
		if err != nil {
			return []Value{NilValue(), s.internErrStr(fmt.Sprintf("cannot open %s", formatOSErr(err)))}, nil
		}
		defer f.Close()
		r = fileErrReader{r: f}
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
			return []Value{NilValue(), s.internErrStr(fmt.Sprintf("cannot read %s: %s", target, fre.err.Error()))}, nil
		}
		return []Value{NilValue(), s.internErrStr(err.Error())}, nil
	}
	return []Value{fn}, nil
}

// dofileArgs truncates dofile arguments to the single optional filename parameter
// matching PUC-Rio lbaselib.c:luaB_dofile (lua_settop(L, 1); luaL_loadfilex(L, fname, "bt")).
func dofileArgs(args []Value) []Value {
	if len(args) > 1 {
		return args[:1]
	}
	return args
}

func nativeDoFile(s *State, args []Value) ([]Value, error) {
	res, err := nativeLoadFile(s, dofileArgs(args))
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
	fileMT.markProto(s)

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
		// Once fileMeta.__gc is customized, retain u strongly so Go GC cannot drop it
		// before the VM invokes the custom Lua finalizer.
		s.gc.finalSeq++
		entry := gcWeakFileEntry{u: weak.Make(u), fi: fi, seq: s.gc.finalSeq}
		if s.gc.customFileGC {
			entry.strong = u
		}
		s.gc.weakFiles = append(s.gc.weakFiles, entry)
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
					if lf.cmd.Process != nil {
						if lf.cmd.Cancel != nil {
							_ = lf.cmd.Cancel()
						} else {
							_ = lf.cmd.Process.Kill()
						}
					}
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
		u := wf.userdata()
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

// formatOSErr strips Go's *os.PathError / *os.LinkError operation verb prefix ("open ", "remove ", etc.)
// to match PUC-Rio Lua's luaL_fileresult ("filename: strerror").
func formatOSErr(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		if pe.Path != "" {
			return fmt.Sprintf("%s: %s", pe.Path, pe.Err.Error())
		}
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		if le.Old != "" {
			return fmt.Sprintf("%s: %s", le.Old, le.Err.Error())
		}
		return le.Err.Error()
	}
	return err.Error()
}

func ioFail(s *State, err error) []Value {
	code := int64(1)
	var errno syscall.Errno
	if errors.As(err, &errno) && errno != 0 {
		code = int64(errno)
	}
	return []Value{NilValue(), StringValue(s.intern.Intern(formatOSErr(err))), IntValue(code)}
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
	if args[0].IsString() || args[0].IsNumber() {
		fname := FormatValue(args[0])
		lf, err := openFileWithMode(fname, "r")
		if err != nil {
			return nil, fmt.Errorf("cannot open file '%s' (%v)", fname, err)
		}
		ud, err := s.tryNewFileUD(lf)
		if err != nil {
			return nil, err
		}
		s.defaultInput = ud
		return []Value{s.defaultInput}, nil
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'input' (FILE* expected, got %s)", s.objTypeName(args[0]))
	}
	if lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	s.defaultInput = args[0]
	return []Value{s.defaultInput}, nil
}

func nativeIOOutput(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || args[0].IsNil() {
		return []Value{s.defaultOutput}, nil
	}
	if args[0].IsString() || args[0].IsNumber() {
		fname := FormatValue(args[0])
		lf, err := openFileWithMode(fname, "w")
		if err != nil {
			return nil, fmt.Errorf("cannot open file '%s' (%v)", fname, err)
		}
		ud, err := s.tryNewFileUD(lf)
		if err != nil {
			return nil, err
		}
		s.defaultOutput = ud
		return []Value{s.defaultOutput}, nil
	}
	lf, ok := asLuaFile(args[0])
	if !ok {
		return nil, fmt.Errorf("bad argument #1 to 'output' (FILE* expected, got %s)", s.objTypeName(args[0]))
	}
	if lf.closed {
		return nil, fmt.Errorf("attempt to use a closed file")
	}
	s.defaultOutput = args[0]
	return []Value{s.defaultOutput}, nil
}

func nativeIORead(s *State, args []Value) ([]Value, error) {
	if lf, ok := asLuaFile(s.defaultInput); !ok || lf.closed {
		return nil, fmt.Errorf("default input file is closed")
	}
	all := make([]Value, 1+len(args))
	all[0] = s.defaultInput
	copy(all[1:], args)
	return nativeFileRead(s, all)
}

func nativeIOWrite(s *State, args []Value) ([]Value, error) {
	if lf, ok := asLuaFile(s.defaultOutput); !ok || lf.closed {
		return nil, fmt.Errorf("default output file is closed")
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

func shellCommand(ctx context.Context, cmdStr string) *exec.Cmd {
	var cmd *exec.Cmd
	if ctx != nil {
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", cmdStr)
		} else {
			cmd = exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
		}
		configureCancelableCmd(cmd)
		return cmd
	}
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
	if err := s.checkPreCancel(); err != nil {
		return nil, err
	}
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		if cerr := cancelFn(); cerr != nil {
			return nil, cerr
		}
	}
	if s.memLimit > 0 {
		// Pre-check file userdata budget before spawning a child OS process.
		if err := s.checkMem(128); err != nil {
			return nil, err
		}
	}
	// Flush buffered output streams before spawning the child process
	// (matches Lua 5.5 liolib.c:l_popen calling fflush(NULL) before popen).
	s.flushOpenFiles()
	cmd := shellCommand(s.ctx, prog)
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
		// tryNewFileUD already closed f and terminated+reaped cmd on allocation error.
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
	if lf.isStd {
		// Per Lua 5.5 liolib.c:io_noclose: returns exactly 2 values (fail, "cannot close standard file").
		return []Value{NilValue(), StringValue(s.intern.Intern("cannot close standard file"))}, nil
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
	if isPipe && s.ctx != nil {
		if cerr := s.ctx.Err(); cerr != nil {
			s.abortErr = cerr
			return nil, cerr
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
	if lf.r != nil {
		// Synchronize the underlying fd offset with the logical reader position
		// and invalidate buffered read-ahead bytes on seekable streams (matching
		// POSIX fflush on seekable input streams). Non-seekable streams (pipes/stdin)
		// return ESPIPE/EINVAL and keep their unread buffered data intact.
		if _, err := lf.f.Seek(-int64(lf.r.Buffered()), io.SeekCurrent); err == nil {
			lf.r.Reset(lf.f)
		}
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
		v, ok, err := s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if ok && v > 0 {
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

func (s *State) methodArgNum(n int) int {
	if s.cur != nil && s.cur.nframe > 0 {
		idx := s.cur.nframe - 1
		if s.cur.frames[idx].kind == frameNative && idx > 0 {
			idx--
		}
		if s.cur.frames[idx].kind == frameLua {
			if _, nw := funcNameFromCallerFrame(&s.cur.frames[idx]); nw == "method" {
				return n - 1
			}
		}
	}
	return n
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
		opt := FormatValue(args[1])
		switch opt {
		case "set":
			whence = io.SeekStart
		case "cur":
			whence = io.SeekCurrent
		case "end":
			whence = io.SeekEnd
		default:
			return nil, fmt.Errorf("bad argument #%d to 'seek' (invalid option '%s')", s.methodArgNum(2), opt)
		}
	}
	if len(args) >= 3 && !args[2].IsNil() {
		var ok bool
		var err error
		offset, ok, err = s.coerceInt(args[2])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("bad argument #%d to 'seek' (number expected)", s.methodArgNum(3))
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
	// Only discard buffered input after the underlying seek succeeds; reuse the
	// existing bufio.Reader buffer so repeated seek/read cycles do not re-charge memLimit.
	if lf.r != nil {
		lf.r.Reset(lf.f)
	}
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

// readBoundedLine reads one line in bufio-sized fragments, checking each fragment against the
// memory limit (when set) and polling execution controls (context deadline / instruction limit).
func readBoundedLine(s *State, r *bufio.Reader, keepNL bool) (string, bool, error) {
	var buf bytes.Buffer
	cancelFn := s.nativeCancelFunc()
	for {
		// Poll only once a line spans multiple fragments so short-line reads are not charged.
		if cancelFn != nil && buf.Len() > 0 {
			if cerr := cancelFn(); cerr != nil {
				return "", false, cerr
			}
		}
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			if s.memLimit > 0 {
				if merr := s.checkMem(int64(buf.Len()) + int64(len(frag)) + 64); merr != nil {
					return "", false, merr
				}
			}
			buf.Write(frag)
		}
		if err == nil {
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return "", false, cerr
				}
			}
			line := buf.String()
			if !keepNL {
				line = strings.TrimSuffix(line, "\n")
			}
			if s.memLimit > 0 || s.gc.tier1 {
				sz := int64(len(line)) + 64
				s.gc.allocBytes += sz
				s.gcAddDebt(sz)
			}
			return line, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return "", false, cerr
				}
			}
			if buf.Len() == 0 {
				return "", false, nil
			}
			line := buf.String()
			if !keepNL {
				line = strings.TrimSuffix(line, "\n")
			}
			if s.memLimit > 0 || s.gc.tier1 {
				sz := int64(len(line)) + 64
				s.gc.allocBytes += sz
				s.gcAddDebt(sz)
			}
			return line, true, nil
		}
		if s.ctx != nil {
			if cerr := s.ctx.Err(); cerr != nil {
				s.abortErr = cerr
				return "", false, cerr
			}
		}
		return "", false, ioReadErr{err}
	}
}

// readBoundedAll reads to EOF in chunks, checking each chunk against the memory limit (when set)
// and polling execution controls (context deadline / instruction limit).
func readBoundedAll(s *State, r *bufio.Reader) (string, error) {
	var buf bytes.Buffer
	var chunk [32768]byte
	cancelFn := s.nativeCancelFunc()
	for {
		if cancelFn != nil && buf.Len() > 0 {
			if cerr := cancelFn(); cerr != nil {
				return "", cerr
			}
		}
		n, err := r.Read(chunk[:])
		if n > 0 {
			if s.memLimit > 0 {
				if merr := s.checkMem(int64(buf.Len()) + int64(n) + 64); merr != nil {
					return "", merr
				}
			}
			buf.Write(chunk[:n])
		}
		if err == io.EOF {
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return "", cerr
				}
			}
			res := buf.String()
			if s.memLimit > 0 || s.gc.tier1 {
				sz := int64(len(res)) + 64
				s.gc.allocBytes += sz
				s.gcAddDebt(sz)
			}
			return res, nil
		}
		if err != nil {
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return "", cerr
				}
			}
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
				if s.ctx != nil {
					if cerr := s.ctx.Err(); cerr != nil {
						s.abortErr = cerr
						return NilValue(), false, cerr
					}
				}
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
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return NilValue(), false, cerr
				}
			}
			if got == 0 && err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return NilValue(), false, nil
				}
				return NilValue(), false, ioReadErr{err}
			}
			if s.memLimit > 0 || s.gc.tier1 {
				sz := int64(got) + 64
				s.gc.allocBytes += sz
				s.gcAddDebt(sz)
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
		cancelFn := s.nativeCancelFunc()
		for rem > 0 {
			if cancelFn != nil && rem < n {
				if cerr := cancelFn(); cerr != nil {
					return NilValue(), false, cerr
				}
			}
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
				if s.ctx != nil {
					if cerr := s.ctx.Err(); cerr != nil {
						s.abortErr = cerr
						return NilValue(), false, cerr
					}
				}
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
			sz := int64(out.Len()) + 64
			s.gc.allocBytes += sz
			s.gcAddDebt(sz)
		}
		return StringValue(s.intern.Intern(out.String())), true, nil
	}
	spec := strings.TrimPrefix(FormatValue(fmtArg), "*")
	if len(spec) == 0 {
		return NilValue(), false, fmt.Errorf("invalid format")
	}
	switch spec[0] {
	case 'l', 'L':
		if s.memLimit > 0 || s.nativeCancelFunc() != nil {
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
		if s.gc.tier1 {
			sz := int64(len(line)) + 48
			s.gc.allocBytes += sz
			s.gcAddDebt(sz)
		}
		return StringValue(s.intern.Intern(line)), true, nil
	case 'a':
		if s.memLimit > 0 || s.nativeCancelFunc() != nil {
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
		if s.gc.tier1 {
			sz := int64(len(all)) + 48
			s.gc.allocBytes += sz
			s.gcAddDebt(sz)
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
	// Skip leading whitespace, polling execution controls so long whitespace prefixes
	// cannot bypass context deadlines or instruction limits.
	var cur byte
	cancelFn := s.nativeCancelFunc()
	for skipped := 0; ; skipped++ {
		if cancelFn != nil && skipped&4095 == 4095 {
			if err := cancelFn(); err != nil {
				return NilValue(), false, err
			}
		}
		b, err := r.ReadByte()
		if err != nil {
			if s.ctx != nil {
				if cerr := s.ctx.Err(); cerr != nil {
					s.abortErr = cerr
					return NilValue(), false, cerr
				}
			}
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
	const writeCancelChunk = 64 * 1024
	cancelFn := s.nativeCancelFunc()
	if cancelFn != nil {
		if err := s.checkPreCancel(); err != nil {
			return nil, err
		}
	}
	var totalBytes int64
	var bytesSincePoll int
	writeChunk := func(chunk string) ([]Value, error) {
		if lf.w != nil {
			n, err := lf.w.WriteString(chunk)
			totalBytes += int64(n)
			if err != nil {
				written := max(int64(0), totalBytes-int64(lf.w.Buffered()))
				lf.w.Reset(lf.f)
				return append(ioFail(s, err), IntValue(written)), nil
			}
			if lf.bufMode == bufModeLine && strings.ContainsRune(chunk, '\n') {
				if err := lf.w.Flush(); err != nil {
					written := max(int64(0), totalBytes-int64(lf.w.Buffered()))
					lf.w.Reset(lf.f)
					return append(ioFail(s, err), IntValue(written)), nil
				}
			}
			return nil, nil
		}
		n, err := lf.f.WriteString(chunk)
		totalBytes += int64(n)
		if err != nil {
			return append(ioFail(s, err), IntValue(totalBytes)), nil
		}
		return nil, nil
	}
	for i, a := range args[1:] {
		if !a.IsString() && !a.IsNumber() {
			return nil, fmt.Errorf("bad argument #%d to 'write' (string expected, got %s)", s.methodArgNum(i+2), s.objTypeName(a))
		}
		if cancelFn != nil && i > 0 && (i&1023) == 0 {
			if err := cancelFn(); err != nil {
				return nil, err
			}
		}
		str := FormatValue(a)
		if cancelFn == nil || (len(str) < writeCancelChunk && bytesSincePoll+len(str) < writeCancelChunk) {
			if cancelFn != nil {
				bytesSincePoll += len(str)
			}
			if fail, err := writeChunk(str); fail != nil || err != nil {
				return fail, err
			}
			continue
		}
		for off := 0; off < len(str); {
			end := min(off+writeCancelChunk, len(str))
			chunk := str[off:end]
			bytesSincePoll += len(chunk)
			if bytesSincePoll >= writeCancelChunk {
				bytesSincePoll = 0
				if err := cancelFn(); err != nil {
					return nil, err
				}
			}
			if fail, err := writeChunk(chunk); fail != nil || err != nil {
				return fail, err
			}
			off = end
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
		if lf, ok := asLuaFile(s.defaultInput); !ok || lf.closed {
			return nil, fmt.Errorf("default input file is closed")
		}
		var fmts []Value
		if len(args) > 1 {
			fmts = args[1:]
		}
		if len(fmts) > 250 {
			return nil, fmt.Errorf("too many arguments")
		}
		return makeLinesIter(s, s.defaultInput, fmts, false)
	}
	if !args[0].IsString() && !args[0].IsNumber() {
		return nil, fmt.Errorf("bad argument #1 to 'lines' (string expected, got %s)", s.objTypeName(args[0]))
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
		iv, ok, err := s.coerceInt(v)
		if err != nil {
			return 0, err
		}
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
	i, ok, nonInt, err := s.coerceIntKind(a)
	if err != nil {
		return 0, err
	}
	if ok {
		return i, nil
	}
	if nonInt {
		return 0, fmt.Errorf("bad argument #%d to '%s' (number has no integer representation)", idx+1, fnName)
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
		if !args[0].IsString() && !args[0].IsNumber() {
			return nil, fmt.Errorf("bad argument #1 to 'date' (string expected, got %s)", s.objTypeName(args[0]))
		}
		fmtStr = FormatValue(args[0])
	}
	tm := time.Now()
	if len(args) >= 2 && !args[1].IsNil() {
		sec, ok, nonInt, err := s.coerceIntKind(args[1])
		if err != nil {
			return nil, err
		}
		if !ok {
			if nonInt {
				return nil, fmt.Errorf("bad argument #2 to 'date' (number has no integer representation)")
			}
			return nil, fmt.Errorf("bad argument #2 to 'date' (number expected, got %s)", s.objTypeName(args[1]))
		}
		// Match POSIX struct tm.tm_year (int32) representable range in loslib.c:os_date.
		const (
			minRepresentableUnix = -67768040609740800
			maxRepresentableUnix = 67768036191676799
		)
		if sec < minRepresentableUnix || sec > maxRepresentableUnix {
			return nil, fmt.Errorf("date result cannot be represented in this installation")
		}
		tm = time.Unix(sec, 0)
		if y := int64(tm.Year()) - 1900; y < math.MinInt32 || y > math.MaxInt32 {
			return nil, fmt.Errorf("date result cannot be represented in this installation")
		}
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
		if s.memLimit > 0 {
			ref := s.Ref(TableValue(tbl))
			defer s.Unref(ref)
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
		if err := s.ConsumeMemErr(); err != nil {
			return nil, err
		}
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
			return nil, fmt.Errorf("bad argument #1 to 'date' (invalid conversion specifier '%%')")
		}
		spec := fmtStr[i]
		// Support POSIX/C99 two-character %E / %O modifiers (loslib.c:LUA_STRFTIMEOPTIONS).
		if spec == 'E' || spec == 'O' {
			if i+1 >= len(fmtStr) {
				return nil, fmt.Errorf("bad argument #1 to 'date' (invalid conversion specifier '%%%c')", spec)
			}
			next := fmtStr[i+1]
			validMod := false
			if spec == 'E' {
				switch next {
				case 'c', 'C', 'x', 'X', 'y', 'Y':
					validMod = true
				}
			} else {
				switch next {
				case 'd', 'e', 'H', 'I', 'm', 'M', 'S', 'u', 'U', 'V', 'w', 'W', 'y':
					validMod = true
				}
			}
			if !validMod {
				return nil, fmt.Errorf("bad argument #1 to 'date' (invalid conversion specifier '%%%c%c')", spec, next)
			}
			i++
			spec = next
		}
		var err error
		switch spec {
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
			return nil, fmt.Errorf("bad argument #1 to 'date' (invalid conversion specifier '%%%c')", fmtStr[i])
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
	key, err := checkStringArg(args, 0, "getenv")
	if err != nil {
		return nil, err
	}
	v, ok := os.LookupEnv(key)
	if !ok {
		return []Value{NilValue()}, nil
	}
	ls, err := s.TryIntern(v)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeOSRemove(s *State, args []Value) ([]Value, error) {
	fname, err := checkStringArg(args, 0, "remove")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(fname); err != nil {
		return ioFail(s, err), nil
	}
	return []Value{TrueValue()}, nil
}

func nativeOSRename(s *State, args []Value) ([]Value, error) {
	oldName, err := checkStringArg(args, 0, "rename")
	if err != nil {
		return nil, err
	}
	newName, err := checkStringArg(args, 1, "rename")
	if err != nil {
		return nil, err
	}
	if err := os.Rename(oldName, newName); err != nil {
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
	ls, err := s.TryIntern(name)
	if err != nil {
		return nil, err
	}
	return []Value{StringValue(ls)}, nil
}

func nativeOSSetLocale(s *State, args []Value) ([]Value, error) {
	hasLoc := len(args) >= 1 && !args[0].IsNil()
	var loc string
	if hasLoc {
		if !args[0].IsString() && !args[0].IsNumber() {
			return nil, fmt.Errorf("bad argument #1 to 'setlocale' (string expected, got %s)", s.objTypeName(args[0]))
		}
		loc = FormatValue(args[0])
	}
	if len(args) >= 2 && !args[1].IsNil() {
		if !args[1].IsString() && !args[1].IsNumber() {
			return nil, fmt.Errorf("bad argument #2 to 'setlocale' (string expected, got %s)", s.objTypeName(args[1]))
		}
		cat := FormatValue(args[1])
		switch cat {
		case "all", "collate", "ctype", "monetary", "numeric", "time":
		default:
			return nil, fmt.Errorf("bad argument #2 to 'setlocale' (invalid option '%s')", cat)
		}
	}
	if hasLoc {
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
	if err := s.checkPreCancel(); err != nil {
		return nil, err
	}
	if cancelFn := s.nativeCancelFunc(); cancelFn != nil {
		if cerr := cancelFn(); cerr != nil {
			return nil, cerr
		}
	}
	cmd := shellCommand(s.ctx, FormatValue(args[0]))
	s.inheritedStdStreams(cmd)
	s.childExecDepth.Add(1)
	defer s.childExecDepth.Add(-1)
	err := cmd.Run()
	if s.ctx != nil {
		if cerr := s.ctx.Err(); cerr != nil {
			s.abortErr = cerr
			return nil, cerr
		}
	}
	return execResult(s, err), nil
}

func nativeOSExit(s *State, args []Value) ([]Value, error) {
	code := 0
	if len(args) >= 1 && !args[0].IsNil() {
		a := args[0]
		if a.IsBool() {
			if a.IsFalse() {
				code = 1
			}
		} else {
			i, ok, nonInt, err := s.coerceIntKind(a)
			if err != nil {
				return nil, err
			}
			if ok {
				code = int(i)
			} else if nonInt {
				return nil, fmt.Errorf("bad argument #1 to 'os.exit' (number has no integer representation)")
			} else {
				return nil, fmt.Errorf("bad argument #1 to 'os.exit' (number expected, got %s)", s.objTypeName(a))
			}
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
	s.registerFunc(pkg, "loadlib", intrNone, nativePackageLoadLib)

	s.pkgTbl = pkg
	s.SetGlobal("package", TableValue(pkg))
	s.registry.RawSetStr(s, s.intern.Intern("_LOADED"), TableValue(loaded))
	s.registry.RawSetStr(s, s.intern.Intern("_PRELOAD"), TableValue(preload))

	s.registerFunc(s.env, "require", intrNone, nativeRequire)
}

func (s *State) pkgField(name string) (Value, error) {
	key := s.intern.Intern(name)
	v := s.pkgTbl.RawGetStr(key)
	if v.IsNil() && s.pkgTbl.meta != nil {
		return s.finishGet(TableValue(s.pkgTbl), StringValue(key))
	}
	return v, nil
}

func (s *State) registryField(name string) (Value, error) {
	key := s.intern.Intern(name)
	v := s.registry.RawGetStr(key)
	if v.IsNil() && s.registry.meta != nil {
		return s.finishGet(TableValue(s.registry), StringValue(key))
	}
	return v, nil
}

func (s *State) tableGetStrAny(obj Value, key *LuaString) (Value, error) {
	if obj.IsTable() {
		t := obj.AsTable()
		if err := s.ensureLookupStrKeyHash(t, key); err != nil {
			return NilValue(), err
		}
		v := t.RawGetStr(key)
		if !v.IsNil() || t.meta == nil {
			return v, nil
		}
	}
	return s.finishGet(obj, StringValue(key))
}

func nativeSearcherPreload(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_preload' (string expected)")
	}
	name := FormatValue(args[0])
	key := s.intern.Intern(name)
	preloadVal, err := s.registryField("_PRELOAD")
	if err != nil {
		return nil, err
	}
	v, err := s.tableGetStrAny(preloadVal, key)
	if err != nil {
		return nil, err
	}
	if v.IsNil() {
		if s.memLimit > 0 {
			if err := s.checkMem(int64(len(name)) + 80); err != nil {
				return nil, err
			}
		}
		ls, err := s.TryIntern(fmt.Sprintf("no field package.preload['%s']", name))
		if err != nil {
			return nil, err
		}
		return []Value{StringValue(ls)}, nil
	}
	return []Value{v, StringValue(s.intern.Intern(":preload:"))}, nil
}

func nativeSearcherLua(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_lua' (string expected)")
	}
	name := FormatValue(args[0])
	pathVal, err := s.pkgField("path")
	if err != nil {
		return nil, err
	}
	if !pathVal.IsString() && !pathVal.IsNumber() {
		return nil, fmt.Errorf("'package.path' must be a string")
	}
	fname, errmsg, err := s.searchPath(name, FormatValue(pathVal), ".", string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	if fname == "" {
		ls, ierr := s.TryIntern(errmsg)
		if ierr != nil {
			return nil, ierr
		}
		return []Value{StringValue(ls)}, nil
	}
	fnameLS, ierr := s.TryIntern(fname)
	if ierr != nil {
		return nil, ierr
	}
	res, err := nativeLoadFile(s, []Value{StringValue(fnameLS)})
	if err != nil {
		return nil, err
	}
	if res[0].IsNil() {
		return nil, fmt.Errorf("error loading module '%s' from file '%s':\n\t%s", name, fname, FormatValue(res[1]))
	}
	return []Value{res[0], StringValue(fnameLS)}, nil
}

func nativeSearcherC(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'searcher_C' (string expected)")
	}
	name := FormatValue(args[0])
	cpathVal, err := s.pkgField("cpath")
	if err != nil {
		return nil, err
	}
	if !cpathVal.IsString() && !cpathVal.IsNumber() {
		return nil, fmt.Errorf("'package.cpath' must be a string")
	}
	_, errmsg, err := s.searchPath(name, FormatValue(cpathVal), ".", string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	ls, ierr := s.TryIntern(errmsg)
	if ierr != nil {
		return nil, ierr
	}
	return []Value{StringValue(ls)}, nil
}

func nativeRequire(s *State, args []Value) ([]Value, error) {
	if len(args) == 0 || (!args[0].IsString() && !args[0].IsNumber()) {
		return nil, fmt.Errorf("bad argument #1 to 'require' (string expected)")
	}
	name := FormatValue(args[0])
	key := s.intern.Intern(name)
	loadedVal, err := s.registryField("_LOADED")
	if err != nil {
		return nil, err
	}
	// Root _LOADED (and later searchers / loader / extra / result) on the active thread
	// stack across all Lua callbacks so GC cannot prematurely finalize them while require runs.
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
	if err := s.ensureStackWithRoots(th, baseTop+5, loadedVal, []Value{StringValue(key)}); err != nil {
		return nil, err
	}
	th.stack[baseTop] = loadedVal
	th.stack[baseTop+1] = NilValue()
	th.stack[baseTop+2] = NilValue()
	th.top = baseTop + 3
	defer func() {
		th.stack[baseTop] = NilValue()
		th.stack[baseTop+1] = NilValue()
		th.stack[baseTop+2] = NilValue()
		th.top = savedTop
	}()
	existing, err := s.tableGetStrAny(loadedVal, key)
	if err != nil {
		return nil, err
	}
	if !existing.IsFalsy() {
		return []Value{existing}, nil
	}
	sv, err := s.pkgField("searchers")
	if err != nil {
		return nil, err
	}
	if !sv.IsTable() {
		return nil, fmt.Errorf("'package.searchers' must be a table")
	}
	st := sv.AsTable()
	th.stack[baseTop+1] = sv
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
		if len(res) > 0 && (res[0].IsString() || res[0].IsNumber()) {
			smsg := FormatValue(res[0])
			if s.memLimit > 0 {
				if merr := s.checkMem(int64(errs.Len()) + int64(len(smsg)) + 64); merr != nil {
					return nil, merr
				}
			}
			errs.WriteString("\n\t")
			errs.WriteString(smsg)
		}
	}
	th.stack[baseTop+1] = loader
	th.stack[baseTop+2] = extra
	res, err := s.Call(loader, StringValue(key), extra)
	if err != nil {
		return nil, err
	}
	if len(res) > 0 && !res[0].IsNil() {
		th.stack[baseTop+1] = res[0]
		if err := s.tableSetStr(loadedVal, key, res[0]); err != nil {
			return nil, err
		}
	}
	finalVal, err := s.tableGetStrAny(loadedVal, key)
	if err != nil {
		return nil, err
	}
	if finalVal.IsNil() {
		finalVal = TrueValue()
		if err := s.tableSetStr(loadedVal, key, finalVal); err != nil {
			return nil, err
		}
	}
	return []Value{finalVal, extra}, nil
}

func (s *State) checkedReplaceSize(src, oldStr, newStr string) (int64, error) {
	baseLen := int64(len(src))
	if oldStr == "" {
		return baseLen, nil
	}
	cnt, err := countStringWithCancel(src, oldStr, s.nativeCancelFunc())
	if err != nil {
		return 0, err
	}
	if cnt == 0 {
		return baseLen, nil
	}
	diff := int64(len(newStr)) - int64(len(oldStr))
	if diff > 0 && cnt > (math.MaxInt64-baseLen)/diff {
		return 0, &LuaError{Val: StringValue(s.intern.Intern("not enough memory"))}
	}
	outLen := baseLen + cnt*diff
	if s.memLimit > 0 {
		if err := s.checkMem(outLen + 48); err != nil {
			return 0, err
		}
	}
	return outLen, nil
}

// forEachSearchCandidate streams ';' -delimited candidates after replacing '?' with name
// (matching PUC-Rio loadlib.c:488-495: '?' expansion happens BEFORE ';' splitting), while
// enforcing memLimit and cooperative cancellation without materializing quadratic expansions.
func (s *State) forEachSearchCandidate(path, name string, cancelFn func() error, fn func(cand string) (bool, error)) error {
	var steps int64
	hasSemi, err := containsByteWithCancel(name, ';', cancelFn)
	if err != nil {
		return err
	}
	if !hasSemi {
		for tmpl := range strings.SplitSeq(path, ";") {
			steps++
			if cancelFn != nil && (steps&1023) == 0 {
				if err := cancelFn(); err != nil {
					return err
				}
			}
			if _, err := s.checkedReplaceSize(tmpl, "?", name); err != nil {
				return err
			}
			cand, err := replaceAllWithCancel(tmpl, "?", name, cancelFn)
			if err != nil {
				return err
			}
			stop, err := fn(cand)
			if err != nil || stop {
				return err
			}
		}
		return nil
	}
	var cur strings.Builder
	var stopped bool
	copyWork := 0
	emitPiece := func(piece string) error {
		for len(piece) > 0 {
			semi := strings.IndexByte(piece, ';')
			if semi < 0 {
				if s.memLimit > 0 {
					if err := s.checkMem(int64(cur.Len()) + int64(len(piece)) + 48); err != nil {
						return err
					}
				}
				return writeStringWithCancel(&cur, piece, cancelFn, &copyWork)
			}
			if semi > 0 {
				if s.memLimit > 0 {
					if err := s.checkMem(int64(cur.Len()) + int64(semi) + 48); err != nil {
						return err
					}
				}
				if err := writeStringWithCancel(&cur, piece[:semi], cancelFn, &copyWork); err != nil {
					return err
				}
			}
			steps++
			if cancelFn != nil && (steps&1023) == 0 {
				if err := cancelFn(); err != nil {
					return err
				}
			}
			cand := cur.String()
			cur.Reset()
			stop, err := fn(cand)
			if err != nil {
				return err
			}
			if stop {
				stopped = true
				return nil
			}
			piece = piece[semi+1:]
		}
		return nil
	}
	rest := path
	for !stopped {
		q := strings.IndexByte(rest, '?')
		if q < 0 {
			if err := emitPiece(rest); err != nil {
				return err
			}
			break
		}
		if err := emitPiece(rest[:q]); err != nil {
			return err
		}
		if stopped {
			break
		}
		if err := emitPiece(name); err != nil {
			return err
		}
		rest = rest[q+1:]
	}
	if !stopped {
		_, err := fn(cur.String())
		return err
	}
	return nil
}

func (s *State) searchPath(name, path, sep, rep string) (string, string, error) {
	cancelFn := s.nativeCancelFunc()
	if sep != "" {
		if _, err := s.checkedReplaceSize(name, sep, rep); err != nil {
			return "", "", err
		}
		var err error
		name, err = replaceAllWithCancel(name, sep, rep, cancelFn)
		if err != nil {
			return "", "", err
		}
	}
	var errBuf strings.Builder
	var found string
	errWork := 0
	idx := 0
	err := s.forEachSearchCandidate(path, name, cancelFn, func(cand string) (bool, error) {
		if cand != "" {
			if f, oerr := os.Open(cand); oerr == nil {
				_ = f.Close()
				found = cand
				return true, nil
			}
		}
		// Exact Lua 5.5 loadlib.c:pusherrornotfound formatting
		addLen := int64(len(cand)) + 11
		if idx == 0 {
			addLen = int64(len(cand)) + 10
		}
		if s.memLimit > 0 {
			if merr := s.checkMem(int64(errBuf.Len()) + addLen + 48); merr != nil {
				return false, merr
			}
		}
		if idx == 0 {
			errBuf.WriteString("no file '")
		} else {
			errBuf.WriteString("'\n\tno file '")
		}
		if cerr := writeStringWithCancel(&errBuf, cand, cancelFn, &errWork); cerr != nil {
			return false, cerr
		}
		idx++
		return false, nil
	})
	if err != nil {
		return "", "", err
	}
	if found != "" {
		return found, "", nil
	}
	if s.memLimit > 0 {
		if err := s.checkMem(int64(errBuf.Len()) + 49); err != nil {
			return "", "", err
		}
	}
	errBuf.WriteByte('\'')
	return "", errBuf.String(), nil
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
	fname, errmsg, err := s.searchPath(name, path, sep, rep)
	if err != nil {
		return nil, err
	}
	if fname == "" {
		ls, ierr := s.TryIntern(errmsg)
		if ierr != nil {
			return nil, ierr
		}
		return []Value{NilValue(), StringValue(ls)}, nil
	}
	ls, ierr := s.TryIntern(fname)
	if ierr != nil {
		return nil, ierr
	}
	return []Value{StringValue(ls)}, nil
}

func nativePackageLoadLib(s *State, args []Value) ([]Value, error) {
	if _, err := checkStringArg(args, 0, "loadlib"); err != nil {
		return nil, err
	}
	if _, err := checkStringArg(args, 1, "loadlib"); err != nil {
		return nil, err
	}
	return []Value{
		NilValue(),
		StringValue(s.intern.Intern("dynamic libraries not enabled; check your Lua installation")),
		StringValue(s.intern.Intern("absent")),
	}, nil
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
	searcher, err := s.TryAllocNative(&NativeFunc{
		Name:        "searcher_fs",
		Collectable: true,
		Fn: func(s *State, args []Value) ([]Value, error) {
			return searchFSModule(s, fsys, args)
		},
	})
	if err != nil {
		_ = s.ConsumeMemErr()
		return
	}
	var pinID int
	if s.memLimit > 0 {
		pinID = s.Ref(searcher)
		defer s.Unref(pinID)
	}
	n := st.Len()
	pos := min(int64(2+s.fsSearchers), n+1)
	if pos < 1 {
		pos = 1
	}
	// Grow slot n+1 first; if allocation fails under memLimit, abort before overwriting any
	// existing searcher slot and consume the deferred memExceeded flag cleanly.
	if pos <= n {
		st.RawSetInt(s, n+1, st.RawGetInt(n))
		if err := s.ConsumeMemErr(); err != nil {
			return
		}
		for i := n - 1; i >= pos; i-- {
			st.RawSetInt(s, i+1, st.RawGetInt(i))
			if err := s.ConsumeMemErr(); err != nil {
				return
			}
		}
	}
	st.RawSetInt(s, pos, searcher)
	if err := s.ConsumeMemErr(); err != nil {
		return
	}
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
	v := NativeValue(fn)
	if s.memLimit > 0 {
		id := s.Ref(v)
		defer s.Unref(id)
	}
	st.RawSetInt(s, st.Len()+1, v)
	_ = s.ConsumeMemErr()
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
	if _, err := s.checkedReplaceSize(name, ".", "/"); err != nil {
		return nil, err
	}
	rel := strings.ReplaceAll(name, ".", "/")
	var tried strings.Builder
	var triedCount int
	appendTried := func(msg string) error {
		add := int64(len(msg))
		if triedCount > 0 {
			add += 2
		}
		if s.memLimit > 0 {
			if err := s.checkMem(int64(tried.Len()) + add + 48); err != nil {
				return err
			}
		}
		if triedCount > 0 {
			tried.WriteString("\n\t")
		}
		tried.WriteString(msg)
		triedCount++
		return nil
	}
	cancelFn := s.nativeCancelFunc()
	var loadedRes []Value
	err := s.forEachSearchCandidate(pathStr, rel, cancelFn, func(rawCand string) (bool, error) {
		if rawCand == "" {
			return false, nil
		}
		cand := strings.ReplaceAll(rawCand, "\\", "/")
		for strings.HasPrefix(cand, "./") {
			cand = cand[2:]
		}
		if !fs.ValidPath(cand) {
			if err := appendTried(fmt.Sprintf("no file '%s' (invalid fs path)", cand)); err != nil {
				return false, err
			}
			return false, nil
		}
		f, err := fsys.Open(cand)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if err := appendTried(fmt.Sprintf("no file '%s' (in fs.FS)", cand)); err != nil {
					return false, err
				}
				return false, nil
			}
			return false, fmt.Errorf("error reading module '%s' from file '%s': %w", name, cand, err)
		}
		if fi, serr := f.Stat(); serr == nil && fi.IsDir() {
			_ = f.Close()
			return false, fmt.Errorf("error reading module '%s' from file '%s': is a directory", name, cand)
		}
		fn, err := s.LoadChunkReader(fileErrReader{r: f}, "@"+cand)
		_ = f.Close()
		if err != nil {
			if errors.Is(err, errCoroCloseSelf) || isUncatchableErr(err) {
				return false, err
			}
			var fre loadFileReadErr
			if errors.As(err, &fre) {
				return false, fmt.Errorf("error reading module '%s' from file '%s': %w", name, cand, fre.err)
			}
			if strings.Contains(err.Error(), "not enough memory") {
				return false, err
			}
			return false, fmt.Errorf("error loading module '%s' from file '%s':\n\t%s", name, cand, err.Error())
		}
		candStr, ierr := s.TryIntern(cand)
		if ierr != nil {
			return false, ierr
		}
		loadedRes = []Value{fn, StringValue(candStr)}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if loadedRes != nil {
		return loadedRes, nil
	}
	if triedCount == 0 {
		return []Value{StringValue(s.intern.Intern("no file in fs.FS"))}, nil
	}
	ls, ierr := s.TryIntern(tried.String())
	if ierr != nil {
		return nil, ierr
	}
	return []Value{StringValue(ls)}, nil
}
