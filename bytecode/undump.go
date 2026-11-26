package bytecode

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Lua 5.5.1 binary header constants (matching lundump.h / lua.h).
const (
	luaSignature = "\x1bLua"
	luacVersion  = 0x55 // 5*16 + 5
	luacFormat   = 0
	luacData     = "\x19\x93\r\n\x1a\n"
	luacInt      = int64(-0x5678)
	luacInst     = uint32(0x12345678)
	luacNum      = float64(-370.5)
	maxShortLen  = 40

	// Tag constants matching lobject.h makevariant(t, v).
	luaVNil       = 0x00 // makevariant(0, 0)
	luaVFalse     = 0x01 // makevariant(1, 0)
	luaVTrue      = 0x11 // makevariant(1, 1)
	luaVNumInt    = 0x03 // makevariant(3, 0)
	luaVNumFlt    = 0x13 // makevariant(3, 1)
	luaVShrStr    = 0x04 // makevariant(4, 0)
	luaVLngStr    = 0x14 // makevariant(4, 1)
	maxProtoDepth = 200  // cap nested function proto depth (LUAI_MAXCCALLS) to prevent Go stack overflow
)

type loader struct {
	data     []byte        // slice fast path (br == nil)
	br       *bufio.Reader // streaming path (br != nil)
	off      int
	strings  []string          // 1-indexed saved strings for Lua 5.5 deduplication
	checkMem func(int64) error // optional incremental memory budget check
	memUsed  int64
	cancelFn func() error
	work     int64
	scratch  [64]byte
}

// Undump parses a PUC-Rio Lua 5.5.1 binary chunk (as produced by luac 5.5.1 or string.dump)
// and verifies it before returning an immutable Proto tree.
func Undump(data []byte) (*Proto, error) {
	return undumpWithLoader(&loader{data: data})
}

// UndumpWithLimit parses and verifies a PUC-Rio Lua 5.5.1 binary chunk from data,
// invoking checkMem(totalBytes) before allocating decoded Proto buffers when checkMem != nil.
func UndumpWithLimit(data []byte, checkMem func(int64) error) (*Proto, error) {
	return UndumpWithControls(data, checkMem, nil)
}

// UndumpWithControls parses and verifies a binary chunk from data while honoring
// optional memory-limit and cooperative cancellation callbacks.
func UndumpWithControls(data []byte, checkMem func(int64) error, cancelFn func() error) (*Proto, error) {
	return undumpWithLoader(&loader{data: data, checkMem: checkMem, cancelFn: cancelFn})
}

// UndumpReader streams and verifies a PUC-Rio Lua 5.5.1 binary chunk from r without
// materializing the entire input buffer in memory first.
func UndumpReader(r io.Reader) (*Proto, error) {
	return UndumpReaderWithControls(r, nil, nil)
}

// UndumpReaderWithLimit streams and verifies a PUC-Rio Lua 5.5.1 binary chunk from r,
// invoking checkMem(totalBytes) before growing decoded Proto buffers when checkMem != nil.
func UndumpReaderWithLimit(r io.Reader, checkMem func(int64) error) (*Proto, error) {
	return UndumpReaderWithControls(r, checkMem, nil)
}

// UndumpReaderWithControls streams and verifies a binary chunk from r while honoring
// optional memory-limit and cooperative cancellation callbacks.
func UndumpReaderWithControls(r io.Reader, checkMem func(int64) error, cancelFn func() error) (*Proto, error) {
	if r == nil {
		return nil, fmt.Errorf("undump: nil reader")
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 4096)
	}
	return undumpWithLoader(&loader{br: br, checkMem: checkMem, cancelFn: cancelFn})
}

func undumpWithLoader(l *loader) (*Proto, error) {
	if err := l.checkHeader(); err != nil {
		return nil, err
	}
	nup, err := l.readByte()
	if err != nil {
		return nil, err
	}
	p, err := l.readFunction(0)
	if err != nil {
		return nil, err
	}
	if int(nup) != len(p.Upvalues) {
		return nil, fmt.Errorf("undump: root upvalue count mismatch (%d != %d)", nup, len(p.Upvalues))
	}
	if err := VerifyWithControls(p, func(extra int64) error {
		if l.checkMem == nil {
			return nil
		}
		return l.checkMem(l.memUsed + extra)
	}, l.cancelFn); err != nil {
		return nil, err
	}
	return p, nil
}

func (l *loader) accountMem(delta int64) error {
	if l.checkMem == nil || delta <= 0 {
		return nil
	}
	if err := l.checkMem(l.memUsed + delta); err != nil {
		return err
	}
	l.memUsed += delta
	return nil
}

func (l *loader) tickCancel(delta int64) error {
	if l.cancelFn == nil || delta <= 0 {
		return nil
	}
	prev := l.work >> 12
	l.work += delta
	next := l.work >> 12
	for prev < next {
		if err := l.cancelFn(); err != nil {
			return err
		}
		prev++
	}
	return nil
}

func (l *loader) readByte() (byte, error) {
	if l.cancelFn != nil {
		if err := l.tickCancel(1); err != nil {
			return 0, err
		}
	}
	if l.br == nil {
		if l.off >= len(l.data) {
			return 0, fmt.Errorf("undump: truncated chunk at offset %d", l.off)
		}
		b := l.data[l.off]
		l.off++
		return b, nil
	}
	b, err := l.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("undump: truncated chunk at offset %d", l.off)
		}
		return 0, err
	}
	l.off++
	return b, nil
}

func (l *loader) readBytes(n int) ([]byte, error) {
	if l.br == nil {
		if n < 0 || n > len(l.data)-l.off {
			return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
		}
		if l.cancelFn != nil && n > 0 {
			if err := l.tickCancel(int64(n)); err != nil {
				return nil, err
			}
		}
		b := l.data[l.off : l.off+n]
		l.off += n
		return b, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
	}
	if n == 0 {
		return nil, nil
	}
	if n <= len(l.scratch) {
		if l.cancelFn != nil {
			if err := l.tickCancel(int64(n)); err != nil {
				return nil, err
			}
		}
		got, err := io.ReadFull(l.br, l.scratch[:n])
		l.off += got
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
			}
			return nil, err
		}
		return l.scratch[:n], nil
	}
	// Stream large byte blocks in bounded 32 KiB chunks so truncated/corrupted streams
	// declaring huge lengths fail at EOF without pre-allocating gigabytes.
	const maxChunk = 32768
	var out []byte
	if n <= maxChunk {
		if l.cancelFn != nil {
			if err := l.tickCancel(int64(n)); err != nil {
				return nil, err
			}
		}
		if err := l.accountMem(int64(n)); err != nil {
			return nil, err
		}
		out = make([]byte, n)
		got, err := io.ReadFull(l.br, out)
		l.off += got
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
			}
			return nil, err
		}
		return out, nil
	}
	out = make([]byte, 0, maxChunk)
	var chunk [maxChunk]byte
	rem := n
	for rem > 0 {
		want := min(rem, maxChunk)
		if l.cancelFn != nil {
			if err := l.tickCancel(int64(want)); err != nil {
				return nil, err
			}
		}
		got, err := io.ReadFull(l.br, chunk[:want])
		l.off += got
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
			}
			return nil, err
		}
		if err := l.accountMem(int64(got)); err != nil {
			return nil, err
		}
		out = append(out, chunk[:got]...)
		rem -= got
	}
	return out, nil
}

func (l *loader) readAlign(align int) error {
	pad := align - (l.off % align)
	if pad < align {
		_, err := l.readBytes(pad)
		return err
	}
	return nil
}

// readVarint decodes Lua 5.5.1's big-endian continuation-bit varint (lundump.c:loadVarint).
// Note: bit 7 == 1 means MORE bytes follow; bit 7 == 0 terminates!
func (l *loader) readVarint(limit uint64) (uint64, error) {
	var x uint64
	lim := limit >> 7
	for {
		b, err := l.readByte()
		if err != nil {
			return 0, err
		}
		if x > lim {
			return 0, fmt.Errorf("undump: varint overflow at offset %d", l.off)
		}
		x = (x << 7) | uint64(b&0x7f)
		if (b & 0x80) == 0 {
			return x, nil
		}
	}
}

func (l *loader) readInt() (int, error) {
	v, err := l.readVarint(math.MaxInt32)
	return int(v), err
}

func (l *loader) readSize() (int, error) {
	v, err := l.readVarint(math.MaxInt32)
	return int(v), err
}

// readInteger decodes Lua 5.5.1's ZigZag-encoded varint signed int64 (lundump.c:loadInteger).
func (l *loader) readInteger() (int64, error) {
	cx, err := l.readVarint(math.MaxUint64)
	if err != nil {
		return 0, err
	}
	if (cx & 1) != 0 {
		return int64(^(cx >> 1)), nil
	}
	return int64(cx >> 1), nil
}

func (l *loader) readNumber() (float64, error) {
	b, err := l.readBytes(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

func (l *loader) checkHeader() error {
	sig, err := l.readBytes(len(luaSignature))
	if err != nil {
		return err
	}
	if string(sig) != luaSignature {
		return fmt.Errorf("undump: not a Lua binary chunk")
	}
	ver, err := l.readByte()
	if err != nil {
		return err
	}
	if ver != luacVersion {
		return fmt.Errorf("undump: version mismatch (got 0x%02x, want 0x%02x)", ver, luacVersion)
	}
	format, err := l.readByte()
	if err != nil {
		return err
	}
	if format != luacFormat {
		return fmt.Errorf("undump: format mismatch (got %d)", format)
	}
	ld, err := l.readBytes(len(luacData))
	if err != nil {
		return err
	}
	if string(ld) != luacData {
		return fmt.Errorf("undump: corrupted LUAC_DATA")
	}
	// int (4 bytes on LP64)
	intSz, err := l.readByte()
	if err != nil {
		return err
	}
	if intSz != 4 {
		return fmt.Errorf("undump: unsupported C int size %d", intSz)
	}
	ib, err := l.readBytes(4)
	if err != nil {
		return err
	}
	if int32(binary.LittleEndian.Uint32(ib)) != int32(luacInt) {
		return fmt.Errorf("undump: C int format mismatch")
	}
	// Instruction (4 bytes)
	instSz, err := l.readByte()
	if err != nil {
		return err
	}
	if instSz != 4 {
		return fmt.Errorf("undump: unsupported instruction size %d", instSz)
	}
	instb, err := l.readBytes(4)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(instb) != luacInst {
		return fmt.Errorf("undump: instruction format mismatch")
	}
	// lua_Integer (8 bytes)
	lintSz, err := l.readByte()
	if err != nil {
		return err
	}
	if lintSz != 8 {
		return fmt.Errorf("undump: unsupported lua_Integer size %d", lintSz)
	}
	lib, err := l.readBytes(8)
	if err != nil {
		return err
	}
	if int64(binary.LittleEndian.Uint64(lib)) != luacInt {
		return fmt.Errorf("undump: lua_Integer format mismatch")
	}
	// lua_Number (8 bytes)
	lnumSz, err := l.readByte()
	if err != nil {
		return err
	}
	if lnumSz != 8 {
		return fmt.Errorf("undump: unsupported lua_Number size %d", lnumSz)
	}
	lnb, err := l.readBytes(8)
	if err != nil {
		return err
	}
	if math.Float64frombits(binary.LittleEndian.Uint64(lnb)) != luacNum {
		return fmt.Errorf("undump: lua_Number format mismatch")
	}
	return nil
}

// readString decodes Lua 5.5.1's deduplicated string format (lundump.c:loadString).
// Returns (str, ok, err); ok == false means NULL string.
func (l *loader) readString() (string, bool, error) {
	size, err := l.readSize()
	if err != nil {
		return "", false, err
	}
	if size == 0 {
		idx, err := l.readVarint(math.MaxUint64)
		if err != nil {
			return "", false, err
		}
		if idx == 0 {
			return "", false, nil
		}
		if idx > uint64(len(l.strings)) {
			return "", false, fmt.Errorf("undump: invalid string backref %d (have %d)", idx, len(l.strings))
		}
		return l.strings[idx-1], true, nil
	}
	slen := size - 1
	// Lua 5.5.1 writes slen + 1 bytes including trailing '\0'.
	raw, err := l.readBytes(slen + 1)
	if err != nil {
		return "", false, err
	}
	if l.checkMem != nil {
		if l.br == nil || slen+1 <= len(l.scratch) {
			if err := l.accountMem(int64(slen) + 48); err != nil {
				return "", false, err
			}
		} else {
			if err := l.accountMem(48); err != nil {
				return "", false, err
			}
		}
	}
	s := string(raw[:slen])
	l.strings = append(l.strings, s)
	return s, true, nil
}

func (l *loader) readFunction(depth int) (*Proto, error) {
	if depth > maxProtoDepth {
		return nil, fmt.Errorf("undump: function nesting depth exceeds limit (%d)", maxProtoDepth)
	}
	if err := l.accountMem(192); err != nil {
		return nil, err
	}
	p := &Proto{}
	var err error
	if p.LineDefined, err = l.readInt(); err != nil {
		return nil, err
	}
	if p.LastLineDefined, err = l.readInt(); err != nil {
		return nil, err
	}
	if p.NumParams, err = l.readByte(); err != nil {
		return nil, err
	}
	flag, err := l.readByte()
	if err != nil {
		return nil, err
	}
	p.Flag = flag &^ PFFixed
	if p.MaxStackSize, err = l.readByte(); err != nil {
		return nil, err
	}

	// Code
	ncode, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if err := l.readAlign(4); err != nil {
		return nil, err
	}
	if l.br == nil {
		if ncode > (len(l.data)-l.off)/4 {
			return nil, fmt.Errorf("undump: truncated chunk reading %d instructions", ncode)
		}
		if err := l.accountMem(int64(ncode) * 4); err != nil {
			return nil, err
		}
		codeBytes, err := l.readBytes(ncode * 4)
		if err != nil {
			return nil, err
		}
		p.Code = make([]Instruction, ncode)
		for i := range ncode {
			if l.cancelFn != nil && (i&4095) == 0 && i > 0 {
				if err := l.tickCancel(4096); err != nil {
					return nil, err
				}
			}
			p.Code[i] = Instruction(binary.LittleEndian.Uint32(codeBytes[i*4 : i*4+4]))
		}
	} else if ncode > 0 {
		const batchInsts = 1024
		var batchBuf [batchInsts * 4]byte
		p.Code = make([]Instruction, 0, min(ncode, batchInsts))
		rem := ncode
		for rem > 0 {
			cnt := min(rem, batchInsts)
			if l.cancelFn != nil {
				if err := l.tickCancel(int64(cnt) * 4); err != nil {
					return nil, err
				}
			}
			if err := l.accountMem(int64(cnt) * 4); err != nil {
				return nil, err
			}
			got, rerr := io.ReadFull(l.br, batchBuf[:cnt*4])
			l.off += got
			if rerr != nil {
				if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
					return nil, fmt.Errorf("undump: truncated chunk reading %d instructions", ncode)
				}
				return nil, rerr
			}
			for i := range cnt {
				p.Code = append(p.Code, Instruction(binary.LittleEndian.Uint32(batchBuf[i*4:i*4+4])))
			}
			rem -= cnt
		}
	}

	// Constants
	nconst, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if l.br == nil {
		if nconst > len(l.data)-l.off {
			return nil, fmt.Errorf("undump: truncated chunk reading %d constants", nconst)
		}
		if err := l.accountMem(int64(nconst) * 48); err != nil {
			return nil, err
		}
		p.Consts = make([]Constant, nconst)
	} else {
		p.Consts = make([]Constant, 0, min(nconst, 256))
	}
	for i := range nconst {
		tag, err := l.readByte()
		if err != nil {
			return nil, err
		}
		if l.br != nil {
			if err := l.accountMem(48); err != nil {
				return nil, err
			}
		}
		var c Constant
		switch tag {
		case luaVNil:
			c = Constant{Kind: ConstNil}
		case luaVFalse:
			c = Constant{Kind: ConstFalse}
		case luaVTrue:
			c = Constant{Kind: ConstTrue}
		case luaVNumFlt:
			f, err := l.readNumber()
			if err != nil {
				return nil, err
			}
			c = Constant{Kind: ConstFloat, Float: f}
		case luaVNumInt:
			iv, err := l.readInteger()
			if err != nil {
				return nil, err
			}
			c = Constant{Kind: ConstInt, Int: iv}
		case luaVShrStr, luaVLngStr:
			s, ok, err := l.readString()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("undump: null constant string")
			}
			c = Constant{
				Kind:    ConstString,
				Str:     s,
				IsShort: len(s) <= maxShortLen,
			}
		default:
			return nil, fmt.Errorf("undump: invalid constant tag 0x%02x", tag)
		}
		if l.br == nil {
			p.Consts[i] = c
		} else {
			p.Consts = append(p.Consts, c)
		}
	}

	// Upvalues
	nup, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if l.br == nil {
		if nup > (len(l.data)-l.off)/3 {
			return nil, fmt.Errorf("undump: truncated chunk reading %d upvalues", nup)
		}
		if err := l.accountMem(int64(nup) * 24); err != nil {
			return nil, err
		}
		p.Upvalues = make([]UpvalDesc, nup)
	} else {
		p.Upvalues = make([]UpvalDesc, 0, min(nup, 256))
	}
	for i := range nup {
		var uv UpvalDesc
		if uv.InStack, err = l.readByte(); err != nil {
			return nil, err
		}
		if uv.Idx, err = l.readByte(); err != nil {
			return nil, err
		}
		if uv.Kind, err = l.readByte(); err != nil {
			return nil, err
		}
		if l.br == nil {
			p.Upvalues[i] = uv
		} else {
			if err := l.accountMem(24); err != nil {
				return nil, err
			}
			p.Upvalues = append(p.Upvalues, uv)
		}
	}

	// Sub-protos
	np, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if l.br == nil {
		if np > len(l.data)-l.off {
			return nil, fmt.Errorf("undump: truncated chunk reading %d protos", np)
		}
		if err := l.accountMem(int64(np) * 8); err != nil {
			return nil, err
		}
		p.Protos = make([]*Proto, np)
	} else {
		p.Protos = make([]*Proto, 0, min(np, 64))
	}
	for i := range np {
		sub, err := l.readFunction(depth + 1)
		if err != nil {
			return nil, err
		}
		if l.br == nil {
			p.Protos[i] = sub
		} else {
			p.Protos = append(p.Protos, sub)
		}
	}

	// Source
	src, hasSrc, err := l.readString()
	if err != nil {
		return nil, err
	}
	p.Source = src
	p.NoSource = !hasSrc

	// Debug info
	nline, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nline > 0 {
		if l.br == nil {
			if nline > len(l.data)-l.off {
				return nil, fmt.Errorf("undump: truncated chunk reading %d lineinfo bytes", nline)
			}
			if err := l.accountMem(int64(nline)); err != nil {
				return nil, err
			}
			lb, err := l.readBytes(nline)
			if err != nil {
				return nil, err
			}
			p.LineInfo = make([]int8, nline)
			for i, b := range lb {
				p.LineInfo[i] = int8(b)
			}
		} else {
			const batchLines = 4096
			var lbuf [batchLines]byte
			p.LineInfo = make([]int8, 0, min(nline, batchLines))
			rem := nline
			for rem > 0 {
				cnt := min(rem, batchLines)
				if l.cancelFn != nil {
					if err := l.tickCancel(int64(cnt)); err != nil {
						return nil, err
					}
				}
				if err := l.accountMem(int64(cnt)); err != nil {
					return nil, err
				}
				got, rerr := io.ReadFull(l.br, lbuf[:cnt])
				l.off += got
				if rerr != nil {
					if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
						return nil, fmt.Errorf("undump: truncated chunk reading %d lineinfo bytes", nline)
					}
					return nil, rerr
				}
				for i := range cnt {
					p.LineInfo = append(p.LineInfo, int8(lbuf[i]))
				}
				rem -= cnt
			}
		}
	}
	nabs, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nabs > 0 {
		if err := l.readAlign(4); err != nil {
			return nil, err
		}
		if l.br == nil {
			if nabs > (len(l.data)-l.off)/8 {
				return nil, fmt.Errorf("undump: truncated chunk reading %d abslineinfo entries", nabs)
			}
			if err := l.accountMem(int64(nabs) * 8); err != nil {
				return nil, err
			}
			ab, err := l.readBytes(nabs * 8)
			if err != nil {
				return nil, err
			}
			p.AbsLineInfo = make([]AbsLineInfo, nabs)
			for i := range nabs {
				p.AbsLineInfo[i] = AbsLineInfo{
					PC:   int32(binary.LittleEndian.Uint32(ab[i*8 : i*8+4])),
					Line: int32(binary.LittleEndian.Uint32(ab[i*8+4 : i*8+8])),
				}
			}
		} else {
			const batchAbs = 512
			var abuf [batchAbs * 8]byte
			p.AbsLineInfo = make([]AbsLineInfo, 0, min(nabs, batchAbs))
			rem := nabs
			for rem > 0 {
				cnt := min(rem, batchAbs)
				if l.cancelFn != nil {
					if err := l.tickCancel(int64(cnt) * 8); err != nil {
						return nil, err
					}
				}
				if err := l.accountMem(int64(cnt) * 8); err != nil {
					return nil, err
				}
				got, rerr := io.ReadFull(l.br, abuf[:cnt*8])
				l.off += got
				if rerr != nil {
					if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
						return nil, fmt.Errorf("undump: truncated chunk reading %d abslineinfo entries", nabs)
					}
					return nil, rerr
				}
				for i := range cnt {
					p.AbsLineInfo = append(p.AbsLineInfo, AbsLineInfo{
						PC:   int32(binary.LittleEndian.Uint32(abuf[i*8 : i*8+4])),
						Line: int32(binary.LittleEndian.Uint32(abuf[i*8+4 : i*8+8])),
					})
				}
				rem -= cnt
			}
		}
	}
	nloc, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if l.br == nil {
		if nloc > len(l.data)-l.off {
			return nil, fmt.Errorf("undump: truncated chunk reading %d locals", nloc)
		}
		if err := l.accountMem(int64(nloc) * 32); err != nil {
			return nil, err
		}
		p.LocVars = make([]LocVar, nloc)
	} else {
		p.LocVars = make([]LocVar, 0, min(nloc, 256))
	}
	for i := range nloc {
		name, _, err := l.readString()
		if err != nil {
			return nil, err
		}
		startPC, err := l.readInt()
		if err != nil {
			return nil, err
		}
		endPC, err := l.readInt()
		if err != nil {
			return nil, err
		}
		lv := LocVar{Name: name, StartPC: startPC, EndPC: endPC}
		if l.br == nil {
			p.LocVars[i] = lv
		} else {
			if err := l.accountMem(32); err != nil {
				return nil, err
			}
			p.LocVars = append(p.LocVars, lv)
		}
	}
	nupNames, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nupNames != 0 {
		// Match PUC-Rio lundump.c:loadDebug: any non-zero count means len(p.Upvalues) names follow.
		nupNames = len(p.Upvalues)
		for i := range nupNames {
			name, _, err := l.readString()
			if err != nil {
				return nil, err
			}
			p.Upvalues[i].Name = name
		}
	}
	return p, nil
}
