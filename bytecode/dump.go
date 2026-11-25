package bytecode

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
)

type dumper struct {
	buf     []byte
	w       io.Writer // nil on in-memory Dump fast path; non-nil for streaming DumpTo
	written int       // bytes already flushed to w
	err     error     // sticky write error for streaming DumpTo
	strip   bool
	strings map[string]uint64 // 1-indexed saved strings for Lua 5.5 deduplication
	nstr    uint64
}

// Dump serializes a Proto tree into a PUC-Rio Lua 5.5.1 binary chunk
// matching ldump.c byte-for-byte. When strip is true, debug information
// (source, LineInfo, AbsLineInfo, LocVars, and upvalue names) is omitted.
func Dump(p *Proto, strip bool) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("dump: nil proto")
	}
	if len(p.Upvalues) > MaxArgA {
		return nil, fmt.Errorf("dump: too many root upvalues (%d)", len(p.Upvalues))
	}
	d := &dumper{
		buf:     make([]byte, 0, 256),
		strip:   strip,
		strings: make(map[string]uint64),
	}
	d.writeHeader()
	d.writeByte(byte(len(p.Upvalues)))
	if err := d.writeFunction(p); err != nil {
		return nil, err
	}
	return d.buf, nil
}

// DumpTo streams a PUC-Rio Lua 5.5.1 binary chunk directly to w using a small
// chunk buffer (matching ldump.c byte-for-byte without materializing the full chunk).
func DumpTo(w io.Writer, p *Proto, strip bool) error {
	if w == nil {
		return fmt.Errorf("dump: nil writer")
	}
	if p == nil {
		return fmt.Errorf("dump: nil proto")
	}
	if len(p.Upvalues) > MaxArgA {
		return fmt.Errorf("dump: too many root upvalues (%d)", len(p.Upvalues))
	}
	d := &dumper{
		buf:     make([]byte, 0, 4096),
		w:       w,
		strip:   strip,
		strings: make(map[string]uint64),
	}
	d.writeHeader()
	d.writeByte(byte(len(p.Upvalues)))
	if err := d.writeFunction(p); err != nil {
		return err
	}
	return d.flush()
}

// DumpString serializes a Proto tree directly into a single Go string via DumpTo
// without an intermediate full []byte copy.
func DumpString(p *Proto, strip bool) (string, error) {
	var sb strings.Builder
	if err := DumpTo(&sb, p, strip); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func (d *dumper) flush() error {
	if d.err != nil {
		return d.err
	}
	if d.w == nil || len(d.buf) == 0 {
		return nil
	}
	n, err := d.w.Write(d.buf)
	d.written += n
	if err == nil && n < len(d.buf) {
		err = io.ErrShortWrite
	}
	d.buf = d.buf[:0]
	if err != nil {
		d.err = err
		return err
	}
	return nil
}

func (d *dumper) maybeFlush() {
	if d.w != nil && len(d.buf) >= 4096 {
		_ = d.flush()
	}
}

func (d *dumper) writeByte(b byte) {
	if d.err != nil {
		return
	}
	d.buf = append(d.buf, b)
	d.maybeFlush()
}

func (d *dumper) writeBytes(b []byte) {
	if d.err != nil {
		return
	}
	if d.w != nil && len(d.buf)+len(b) >= 4096 {
		if err := d.flush(); err != nil {
			return
		}
		if len(b) >= 4096 {
			n, err := d.w.Write(b)
			d.written += n
			if err == nil && n < len(b) {
				err = io.ErrShortWrite
			}
			if err != nil {
				d.err = err
			}
			return
		}
	}
	d.buf = append(d.buf, b...)
	d.maybeFlush()
}

func (d *dumper) writeStringLiteral(s string) {
	if d.err != nil {
		return
	}
	if d.w != nil && len(d.buf)+len(s) >= 4096 {
		if err := d.flush(); err != nil {
			return
		}
		if len(s) >= 4096 {
			n, err := io.WriteString(d.w, s)
			d.written += n
			if err == nil && n < len(s) {
				err = io.ErrShortWrite
			}
			if err != nil {
				d.err = err
			}
			return
		}
	}
	d.buf = append(d.buf, s...)
	d.maybeFlush()
}

func (d *dumper) writeUint32(v uint32) {
	if d.err != nil {
		return
	}
	d.buf = binary.LittleEndian.AppendUint32(d.buf, v)
	d.maybeFlush()
}

func (d *dumper) writeUint64(v uint64) {
	if d.err != nil {
		return
	}
	d.buf = binary.LittleEndian.AppendUint64(d.buf, v)
	d.maybeFlush()
}

func (d *dumper) writeAlign(align int) {
	if d.err != nil {
		return
	}
	pad := align - ((d.written + len(d.buf)) % align)
	if pad < align {
		var zeros [8]byte
		d.buf = append(d.buf, zeros[:pad]...)
		d.maybeFlush()
	}
}

// writeVarint encodes an unsigned integer using Lua 5.5.1's MSB continuation-bit
// varint encoding (ldump.c:dumpVarint). Bit 7 == 1 means more bytes follow;
// bit 7 == 0 terminates.
func (d *dumper) writeVarint(x uint64) {
	var buff [10]byte // ceil(64 / 7) == 10
	n := 1
	buff[len(buff)-1] = byte(x & 0x7f)
	for x >>= 7; x != 0; x >>= 7 {
		n++
		buff[len(buff)-n] = byte((x & 0x7f) | 0x80)
	}
	d.buf = append(d.buf, buff[len(buff)-n:]...)
}

func (d *dumper) writeInt(x int) error {
	if x < 0 {
		return fmt.Errorf("dump: negative int %d", x)
	}
	d.writeVarint(uint64(x))
	return nil
}

func (d *dumper) writeSize(sz int) error {
	if sz < 0 {
		return fmt.Errorf("dump: negative size %d", sz)
	}
	d.writeVarint(uint64(sz))
	return nil
}

// writeInteger encodes a signed int64 using Lua 5.5.1's ZigZag varint
// (ldump.c:dumpInteger): x >= 0 => 2*x; x < 0 => 2*(~x) + 1.
func (d *dumper) writeInteger(x int64) {
	var cx uint64
	if x >= 0 {
		cx = 2 * uint64(x)
	} else {
		cx = 2*(^uint64(x)) + 1
	}
	d.writeVarint(cx)
}

func (d *dumper) writeNumber(x float64) {
	d.writeUint64(math.Float64bits(x))
}

func (d *dumper) writeHeader() {
	d.writeStringLiteral(luaSignature)
	d.writeByte(luacVersion)
	d.writeByte(luacFormat)
	d.writeStringLiteral(luacData)
	// C int (4 bytes)
	li32 := int32(luacInt)
	d.writeByte(4)
	d.writeUint32(uint32(li32))
	// Instruction (4 bytes)
	d.writeByte(4)
	d.writeUint32(luacInst)
	// lua_Integer (8 bytes)
	li64 := luacInt
	d.writeByte(8)
	d.writeUint64(uint64(li64))
	// lua_Number (8 bytes)
	d.writeByte(8)
	d.writeNumber(luacNum)
}

func (d *dumper) writeNullString() {
	d.writeVarint(0)
	d.writeVarint(0)
}

// writeString dumps a non-NULL string using Lua 5.5.1's string deduplication
// table (ldump.c:dumpString).
func (d *dumper) writeString(s string) error {
	if idx, ok := d.strings[s]; ok {
		d.writeVarint(0)
		d.writeVarint(idx)
		return nil
	}
	if err := d.writeSize(len(s) + 1); err != nil {
		return err
	}
	d.writeStringLiteral(s)
	d.writeByte(0) // trailing NUL byte required by Lua 5.5.1
	d.nstr++
	d.strings[s] = d.nstr
	return nil
}

func (d *dumper) writeCode(p *Proto) error {
	if err := d.writeInt(len(p.Code)); err != nil {
		return err
	}
	d.writeAlign(4)
	for _, inst := range p.Code {
		if d.err != nil {
			return d.err
		}
		d.writeUint32(uint32(inst))
	}
	return d.err
}

func (d *dumper) writeConstants(p *Proto) error {
	if err := d.writeInt(len(p.Consts)); err != nil {
		return err
	}
	for i, c := range p.Consts {
		if d.err != nil {
			return d.err
		}
		switch c.Kind {
		case ConstNil:
			d.writeByte(luaVNil)
		case ConstFalse:
			d.writeByte(luaVFalse)
		case ConstTrue:
			d.writeByte(luaVTrue)
		case ConstFloat:
			d.writeByte(luaVNumFlt)
			d.writeNumber(c.Float)
		case ConstInt:
			d.writeByte(luaVNumInt)
			d.writeInteger(c.Int)
		case ConstString:
			if len(c.Str) <= maxShortLen {
				d.writeByte(luaVShrStr)
			} else {
				d.writeByte(luaVLngStr)
			}
			if err := d.writeString(c.Str); err != nil {
				return err
			}
		default:
			return fmt.Errorf("dump: invalid constant kind %d at index %d", c.Kind, i)
		}
	}
	return d.err
}

func (d *dumper) writeUpvalues(p *Proto) error {
	if err := d.writeInt(len(p.Upvalues)); err != nil {
		return err
	}
	for _, uv := range p.Upvalues {
		if d.err != nil {
			return d.err
		}
		d.writeByte(uv.InStack)
		d.writeByte(uv.Idx)
		d.writeByte(uv.Kind)
	}
	return d.err
}

func (d *dumper) writeProtos(p *Proto) error {
	if err := d.writeInt(len(p.Protos)); err != nil {
		return err
	}
	for _, sub := range p.Protos {
		if d.err != nil {
			return d.err
		}
		if sub == nil {
			return fmt.Errorf("dump: nil child proto")
		}
		if err := d.writeFunction(sub); err != nil {
			return err
		}
	}
	return d.err
}

func (d *dumper) writeDebug(p *Proto) error {
	nline := len(p.LineInfo)
	if d.strip {
		nline = 0
	}
	if err := d.writeInt(nline); err != nil {
		return err
	}
	for i := range nline {
		if d.err != nil {
			return d.err
		}
		d.writeByte(byte(p.LineInfo[i]))
	}

	nabs := len(p.AbsLineInfo)
	if d.strip {
		nabs = 0
	}
	if err := d.writeInt(nabs); err != nil {
		return err
	}
	if nabs > 0 {
		d.writeAlign(4)
		for i := range nabs {
			if d.err != nil {
				return d.err
			}
			d.writeUint32(uint32(p.AbsLineInfo[i].PC))
			d.writeUint32(uint32(p.AbsLineInfo[i].Line))
		}
	}

	nloc := len(p.LocVars)
	if d.strip {
		nloc = 0
	}
	if err := d.writeInt(nloc); err != nil {
		return err
	}
	for i := range nloc {
		if d.err != nil {
			return d.err
		}
		lv := p.LocVars[i]
		if lv.Name == "" {
			d.writeNullString()
		} else if err := d.writeString(lv.Name); err != nil {
			return err
		}
		if err := d.writeInt(lv.StartPC); err != nil {
			return err
		}
		if err := d.writeInt(lv.EndPC); err != nil {
			return err
		}
	}

	nupNames := len(p.Upvalues)
	if d.strip {
		nupNames = 0
	}
	if err := d.writeInt(nupNames); err != nil {
		return err
	}
	for i := range nupNames {
		if d.err != nil {
			return d.err
		}
		if p.Upvalues[i].Name == "" {
			d.writeNullString()
		} else if err := d.writeString(p.Upvalues[i].Name); err != nil {
			return err
		}
	}
	return d.err
}

func (d *dumper) writeFunction(p *Proto) error {
	if d.err != nil {
		return d.err
	}
	if err := d.writeInt(p.LineDefined); err != nil {
		return err
	}
	if err := d.writeInt(p.LastLineDefined); err != nil {
		return err
	}
	d.writeByte(p.NumParams)
	d.writeByte(p.Flag &^ PFFixed)
	d.writeByte(p.MaxStackSize)
	if err := d.writeCode(p); err != nil {
		return err
	}
	if err := d.writeConstants(p); err != nil {
		return err
	}
	if err := d.writeUpvalues(p); err != nil {
		return err
	}
	if err := d.writeProtos(p); err != nil {
		return err
	}
	if d.strip || p.NoSource {
		d.writeNullString()
	} else {
		if err := d.writeString(p.Source); err != nil {
			return err
		}
	}
	if err := d.writeDebug(p); err != nil {
		return err
	}
	return d.err
}
