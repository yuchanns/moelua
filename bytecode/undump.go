package bytecode

import (
	"encoding/binary"
	"fmt"
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
	data    []byte
	off     int
	strings []string // 1-indexed saved strings for Lua 5.5 deduplication
}

// Undump parses a PUC-Rio Lua 5.5.1 binary chunk (as produced by luac 5.5.1 or string.dump)
// and verifies it before returning an immutable Proto tree.
func Undump(data []byte) (*Proto, error) {
	l := &loader{data: data}
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
	if err := Verify(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (l *loader) readByte() (byte, error) {
	if l.off >= len(l.data) {
		return 0, fmt.Errorf("undump: truncated chunk at offset %d", l.off)
	}
	b := l.data[l.off]
	l.off++
	return b, nil
}

func (l *loader) readBytes(n int) ([]byte, error) {
	if n < 0 || l.off+n > len(l.data) {
		return nil, fmt.Errorf("undump: truncated chunk reading %d bytes at offset %d", n, l.off)
	}
	b := l.data[l.off : l.off+n]
	l.off += n
	return b, nil
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
	s := string(raw[:slen])
	l.strings = append(l.strings, s)
	return s, true, nil
}

func (l *loader) readFunction(depth int) (*Proto, error) {
	if depth > maxProtoDepth {
		return nil, fmt.Errorf("undump: function nesting depth exceeds limit (%d)", maxProtoDepth)
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
	codeBytes, err := l.readBytes(ncode * 4)
	if err != nil {
		return nil, err
	}
	p.Code = make([]Instruction, ncode)
	for i := range ncode {
		p.Code[i] = Instruction(binary.LittleEndian.Uint32(codeBytes[i*4 : i*4+4]))
	}

	// Constants
	nconst, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nconst > len(l.data)-l.off {
		return nil, fmt.Errorf("undump: truncated chunk reading %d constants", nconst)
	}
	p.Consts = make([]Constant, nconst)
	for i := range nconst {
		tag, err := l.readByte()
		if err != nil {
			return nil, err
		}
		switch tag {
		case luaVNil:
			p.Consts[i] = Constant{Kind: ConstNil}
		case luaVFalse:
			p.Consts[i] = Constant{Kind: ConstFalse}
		case luaVTrue:
			p.Consts[i] = Constant{Kind: ConstTrue}
		case luaVNumFlt:
			f, err := l.readNumber()
			if err != nil {
				return nil, err
			}
			p.Consts[i] = Constant{Kind: ConstFloat, Float: f}
		case luaVNumInt:
			iv, err := l.readInteger()
			if err != nil {
				return nil, err
			}
			p.Consts[i] = Constant{Kind: ConstInt, Int: iv}
		case luaVShrStr, luaVLngStr:
			s, ok, err := l.readString()
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("undump: null constant string")
			}
			p.Consts[i] = Constant{
				Kind:    ConstString,
				Str:     s,
				IsShort: len(s) <= maxShortLen,
			}
		default:
			return nil, fmt.Errorf("undump: invalid constant tag 0x%02x", tag)
		}
	}

	// Upvalues
	nup, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nup*3 > len(l.data)-l.off {
		return nil, fmt.Errorf("undump: truncated chunk reading %d upvalues", nup)
	}
	p.Upvalues = make([]UpvalDesc, nup)
	for i := range nup {
		if p.Upvalues[i].InStack, err = l.readByte(); err != nil {
			return nil, err
		}
		if p.Upvalues[i].Idx, err = l.readByte(); err != nil {
			return nil, err
		}
		if p.Upvalues[i].Kind, err = l.readByte(); err != nil {
			return nil, err
		}
	}

	// Sub-protos
	np, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if np > len(l.data)-l.off {
		return nil, fmt.Errorf("undump: truncated chunk reading %d protos", np)
	}
	p.Protos = make([]*Proto, np)
	for i := range np {
		sub, err := l.readFunction(depth + 1)
		if err != nil {
			return nil, err
		}
		p.Protos[i] = sub
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
		lb, err := l.readBytes(nline)
		if err != nil {
			return nil, err
		}
		p.LineInfo = make([]int8, nline)
		for i, b := range lb {
			p.LineInfo[i] = int8(b)
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
	}
	nloc, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nloc > len(l.data)-l.off {
		return nil, fmt.Errorf("undump: truncated chunk reading %d locals", nloc)
	}
	p.LocVars = make([]LocVar, nloc)
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
		p.LocVars[i] = LocVar{Name: name, StartPC: startPC, EndPC: endPC}
	}
	nupNames, err := l.readInt()
	if err != nil {
		return nil, err
	}
	if nupNames != 0 {
		if nupNames != len(p.Upvalues) {
			return nil, fmt.Errorf("undump: upvalue debug name count mismatch (%d != %d)", nupNames, len(p.Upvalues))
		}
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
