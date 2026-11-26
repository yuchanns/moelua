package vm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/yuchanns/moelua/internal/luastdlib"
)

const (
	vmPackSizeChar   = 1
	vmPackSizeShort  = 2
	vmPackSizeInt    = 4
	vmPackSizeLong   = 8
	vmPackSizeSizeT  = 8
	vmPackSizeLuaInt = 8
	vmPackSizeFloat  = 4
	vmPackSizeDouble = 8
	vmPackSizeLuaNum = 8
	vmPackMaxAlign   = 8
	vmPackMaxIntSize = 16
	vmPackMaxSize    = uint64(math.MaxInt64)
)

var (
	vmNativeLittleEndian = detectVMNativeLittleEndian()
	vmZeroPadChunk       [strCloneCancelChunk]byte
)

func detectVMNativeLittleEndian() bool {
	var buf [2]byte
	binary.NativeEndian.PutUint16(buf[:], 1)
	return buf[0] == 1
}

type vmPackHeader struct {
	isLittle bool
	maxAlign uint64
}

type vmPackOpt uint8

const (
	vpkInt vmPackOpt = iota
	vpkUint
	vpkFloat
	vpkNumber
	vpkDouble
	vpkChar
	vpkString
	vpkZstr
	vpkPadding
	vpkPaddAlign
	vpkNop
)

func writeZeroPadWithCancel(sb *strings.Builder, pad uint64, cancelFn func() error, work *int) error {
	for pad > 0 {
		rem := uint64(strCloneCancelChunk - *work)
		if rem == 0 {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
			rem = strCloneCancelChunk
		}
		n := min(pad, rem)
		_, _ = sb.Write(vmZeroPadChunk[:n])
		pad -= n
		*work += int(n)
		if *work >= strCloneCancelChunk {
			if err := cancelFn(); err != nil {
				return err
			}
			*work = 0
		}
	}
	return nil
}

func vmPackGetNum(fmtStr string, pos int, df uint64) (uint64, int, bool) {
	if pos >= len(fmtStr) || fmtStr[pos] < '0' || fmtStr[pos] > '9' {
		return df, pos, true
	}
	var a uint64
	for pos < len(fmtStr) && fmtStr[pos] >= '0' && fmtStr[pos] <= '9' && a <= (vmPackMaxSize-9)/10 {
		a = a*10 + uint64(fmtStr[pos]-'0')
		pos++
	}
	return a, pos, false
}

func vmPackGetNumLimit(fmtStr string, pos int, df uint64) (uint64, int, error) {
	sz, pos, _ := vmPackGetNum(fmtStr, pos, df)
	if sz == 0 || sz > vmPackMaxIntSize {
		return 0, pos, fmt.Errorf("integral size (%d) out of limits [1,%d]", sz, vmPackMaxIntSize)
	}
	return sz, pos, nil
}

func vmPackGetOption(h *vmPackHeader, fmtStr string, pos int) (vmPackOpt, uint64, int, error) {
	opt := fmtStr[pos]
	pos++
	switch opt {
	case 'b':
		return vpkInt, vmPackSizeChar, pos, nil
	case 'B':
		return vpkUint, vmPackSizeChar, pos, nil
	case 'h':
		return vpkInt, vmPackSizeShort, pos, nil
	case 'H':
		return vpkUint, vmPackSizeShort, pos, nil
	case 'l':
		return vpkInt, vmPackSizeLong, pos, nil
	case 'L':
		return vpkUint, vmPackSizeLong, pos, nil
	case 'j':
		return vpkInt, vmPackSizeLuaInt, pos, nil
	case 'J':
		return vpkUint, vmPackSizeLuaInt, pos, nil
	case 'T':
		return vpkUint, vmPackSizeSizeT, pos, nil
	case 'f':
		return vpkFloat, vmPackSizeFloat, pos, nil
	case 'n':
		return vpkNumber, vmPackSizeLuaNum, pos, nil
	case 'd':
		return vpkDouble, vmPackSizeDouble, pos, nil
	case 'i':
		sz, np, err := vmPackGetNumLimit(fmtStr, pos, vmPackSizeInt)
		return vpkInt, sz, np, err
	case 'I':
		sz, np, err := vmPackGetNumLimit(fmtStr, pos, vmPackSizeInt)
		return vpkUint, sz, np, err
	case 's':
		sz, np, err := vmPackGetNumLimit(fmtStr, pos, vmPackSizeSizeT)
		return vpkString, sz, np, err
	case 'c':
		sz, np, isDef := vmPackGetNum(fmtStr, pos, 0)
		if isDef {
			return vpkChar, 0, np, errors.New("missing size for format option 'c'")
		}
		return vpkChar, sz, np, nil
	case 'z':
		return vpkZstr, 0, pos, nil
	case 'x':
		return vpkPadding, 1, pos, nil
	case 'X':
		return vpkPaddAlign, 0, pos, nil
	case ' ':
		return vpkNop, 0, pos, nil
	case '<':
		h.isLittle = true
		return vpkNop, 0, pos, nil
	case '>':
		h.isLittle = false
		return vpkNop, 0, pos, nil
	case '=':
		h.isLittle = vmNativeLittleEndian
		return vpkNop, 0, pos, nil
	case '!':
		sz, np, err := vmPackGetNumLimit(fmtStr, pos, vmPackMaxAlign)
		if err != nil {
			return vpkNop, 0, np, err
		}
		h.maxAlign = sz
		return vpkNop, 0, np, nil
	default:
		return vpkNop, 0, pos, fmt.Errorf("invalid format option '%c'", opt)
	}
}

func vmPackGetDetails(h *vmPackHeader, totalSize uint64, fmtStr string, pos int) (opt vmPackOpt, size uint64, nToAlign uint64, nextPos int, err error) {
	opt, size, pos, err = vmPackGetOption(h, fmtStr, pos)
	if err != nil {
		return vpkNop, 0, 0, pos, err
	}
	align := size
	if opt == vpkPaddAlign {
		if pos >= len(fmtStr) {
			return vpkNop, 0, 0, pos, errors.New("invalid next option for option 'X'")
		}
		opt2, align2, pos2, err2 := vmPackGetOption(h, fmtStr, pos)
		if err2 != nil {
			return vpkNop, 0, 0, pos2, err2
		}
		if opt2 == vpkChar || align2 == 0 {
			return vpkNop, 0, 0, pos2, errors.New("invalid next option for option 'X'")
		}
		align = align2
		pos = pos2
	}
	if align <= 1 || opt == vpkChar {
		nToAlign = 0
	} else {
		if align > h.maxAlign {
			align = h.maxAlign
		}
		if (align & (align - 1)) != 0 {
			return vpkNop, 0, 0, pos, errors.New("format asks for alignment not power of 2")
		}
		szModA := totalSize & (align - 1)
		nToAlign = (align - szModA) & (align - 1)
	}
	return opt, size, nToAlign, pos, nil
}

func vmPackWriteInt(sb *strings.Builder, n uint64, isLittle bool, size int, neg bool) {
	var tmp [vmPackMaxIntSize]byte
	if isLittle {
		tmp[0] = byte(n)
		for i := 1; i < size; i++ {
			n >>= 8
			tmp[i] = byte(n)
		}
		if neg && size > vmPackSizeLuaInt {
			for i := vmPackSizeLuaInt; i < size; i++ {
				tmp[i] = 0xFF
			}
		}
	} else {
		tmp[size-1] = byte(n)
		for i := 1; i < size; i++ {
			n >>= 8
			tmp[size-1-i] = byte(n)
		}
		if neg && size > vmPackSizeLuaInt {
			for i := vmPackSizeLuaInt; i < size; i++ {
				tmp[size-1-i] = 0xFF
			}
		}
	}
	_, _ = sb.Write(tmp[:size])
}

func vmUnpackIntBytes(data string, isLittle bool, size int, isSigned bool) (int64, error) {
	limit := min(size, vmPackSizeLuaInt)
	var res uint64
	for i := limit - 1; i >= 0; i-- {
		res <<= 8
		if isLittle {
			res |= uint64(data[i])
		} else {
			res |= uint64(data[size-1-i])
		}
	}
	if size < vmPackSizeLuaInt {
		if isSigned {
			mask := uint64(1) << (size*8 - 1)
			res = (res ^ mask) - mask
		}
	} else if size > vmPackSizeLuaInt {
		var mask byte
		if isSigned && int64(res) < 0 {
			mask = 0xFF
		}
		for i := limit; i < size; i++ {
			var b byte
			if isLittle {
				b = data[i]
			} else {
				b = data[size-1-i]
			}
			if b != mask {
				return 0, fmt.Errorf("%d-byte integer does not fit into Lua Integer", size)
			}
		}
	}
	return int64(res), nil
}

func (s *State) vmPackArgToInt(v luastdlib.PackValue, argIdx int) (int64, error) {
	switch v.Kind {
	case luastdlib.PackInt:
		return v.Int, nil
	case luastdlib.PackFloat:
		if i, ok := FloatToIntExact(v.Flt); ok {
			return i, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number has no integer representation)", argIdx)
	case luastdlib.PackString:
		iv, ok, nonInt, err := s.coerceIntKind(StringValue(s.intern.Intern(v.Str)))
		if err != nil {
			return 0, err
		}
		if ok {
			return iv, nil
		}
		if nonInt {
			return 0, fmt.Errorf("bad argument #%d to 'pack' (number has no integer representation)", argIdx)
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	}
}

func (s *State) vmPackArgToFloat(v luastdlib.PackValue, argIdx int) (float64, error) {
	switch v.Kind {
	case luastdlib.PackFloat:
		return v.Flt, nil
	case luastdlib.PackInt:
		return float64(v.Int), nil
	case luastdlib.PackString:
		fv, ok, err := s.coerceFloat(StringValue(s.intern.Intern(v.Str)))
		if err != nil {
			return 0, err
		}
		if ok {
			return fv, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	}
}

func vmPackArgToString(v luastdlib.PackValue, argIdx int) (string, error) {
	switch v.Kind {
	case luastdlib.PackString:
		return v.Str, nil
	case luastdlib.PackInt:
		return strconv.FormatInt(v.Int, 10), nil
	case luastdlib.PackFloat:
		return luastdlib.FormatFloat(v.Flt), nil
	default:
		return "", fmt.Errorf("bad argument #%d to 'pack' (string expected)", argIdx)
	}
}

// packWithCancel implements Lua 5.5.1 string.pack with cooperative 64 KiB-chunked
// polling across format options, string copies, NUL scans, and zero padding,
// returning a zero-copy strings.Builder string so the final conversion is O(1).
func (s *State) packWithCancel(fmtStr string, checkSize func(int64) error, cancelFn func() error, args []luastdlib.PackValue) (string, error) {
	if nul, err := indexByteWithCancel(fmtStr, 0, cancelFn); err != nil {
		return "", err
	} else if nul >= 0 {
		fmtStr = fmtStr[:nul]
	}
	h := vmPackHeader{isLittle: vmNativeLittleEndian, maxAlign: 1}
	var sb strings.Builder
	var totalSize uint64
	argIdx := 0
	pos := 0
	var steps int64
	copyWork := 0

	for pos < len(fmtStr) {
		steps++
		if (steps & 1023) == 0 {
			if err := cancelFn(); err != nil {
				return "", err
			}
		}
		opt, size, nToAlign, nextPos, err := vmPackGetDetails(&h, totalSize, fmtStr, pos)
		if err != nil {
			return "", err
		}
		pos = nextPos
		if size > vmPackMaxSize-nToAlign || size+nToAlign > vmPackMaxSize-totalSize {
			return "", errors.New("result too long")
		}
		nextTotal := totalSize + nToAlign + size
		if checkSize != nil && nextTotal > totalSize {
			if err := checkSize(int64(nextTotal)); err != nil {
				return "", err
			}
		}
		totalSize = nextTotal
		if nToAlign > 0 {
			if err := writeZeroPadWithCancel(&sb, nToAlign, cancelFn, &copyWork); err != nil {
				return "", err
			}
		}

		switch opt {
		case vpkPadding:
			sb.WriteByte(0)
			continue
		case vpkPaddAlign, vpkNop:
			continue
		}

		if argIdx >= len(args) {
			return "", fmt.Errorf("bad argument #%d to 'pack' (value expected)", argIdx+2)
		}
		v := args[argIdx]
		argNum := argIdx + 2
		argIdx++

		switch opt {
		case vpkInt:
			n, err := s.vmPackArgToInt(v, argNum)
			if err != nil {
				return "", err
			}
			if size < vmPackSizeLuaInt {
				lim := int64(1) << (size*8 - 1)
				if n < -lim || n >= lim {
					return "", fmt.Errorf("bad argument #%d to 'pack' (integer overflow)", argNum)
				}
			}
			vmPackWriteInt(&sb, uint64(n), h.isLittle, int(size), n < 0)

		case vpkUint:
			n, err := s.vmPackArgToInt(v, argNum)
			if err != nil {
				return "", err
			}
			if size < vmPackSizeLuaInt {
				if uint64(n) >= (uint64(1) << (size * 8)) {
					return "", fmt.Errorf("bad argument #%d to 'pack' (unsigned overflow)", argNum)
				}
			}
			vmPackWriteInt(&sb, uint64(n), h.isLittle, int(size), false)

		case vpkFloat:
			f, err := s.vmPackArgToFloat(v, argNum)
			if err != nil {
				return "", err
			}
			bits := math.Float32bits(float32(f))
			var tmp [4]byte
			if h.isLittle {
				binary.LittleEndian.PutUint32(tmp[:], bits)
			} else {
				binary.BigEndian.PutUint32(tmp[:], bits)
			}
			_, _ = sb.Write(tmp[:])

		case vpkNumber, vpkDouble:
			f, err := s.vmPackArgToFloat(v, argNum)
			if err != nil {
				return "", err
			}
			bits := math.Float64bits(f)
			var tmp [8]byte
			if h.isLittle {
				binary.LittleEndian.PutUint64(tmp[:], bits)
			} else {
				binary.BigEndian.PutUint64(tmp[:], bits)
			}
			_, _ = sb.Write(tmp[:])

		case vpkChar:
			str, err := vmPackArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			if uint64(len(str)) > size {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string longer than given size)", argNum)
			}
			if err := writeStringWithCancel(&sb, str, cancelFn, &copyWork); err != nil {
				return "", err
			}
			if pad := size - uint64(len(str)); pad > 0 {
				if pad > (1 << 30) {
					return "", errors.New("result too long")
				}
				if err := writeZeroPadWithCancel(&sb, pad, cancelFn, &copyWork); err != nil {
					return "", err
				}
			}

		case vpkString:
			str, err := vmPackArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			slen := uint64(len(str))
			if size < vmPackSizeLuaInt && slen >= (uint64(1)<<(size*8)) {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string length does not fit in given size)", argNum)
			}
			if slen > vmPackMaxSize-totalSize {
				return "", errors.New("result too long")
			}
			if checkSize != nil && slen > 0 {
				if err := checkSize(int64(totalSize + slen)); err != nil {
					return "", err
				}
			}
			vmPackWriteInt(&sb, slen, h.isLittle, int(size), false)
			if err := writeStringWithCancel(&sb, str, cancelFn, &copyWork); err != nil {
				return "", err
			}
			totalSize += slen

		case vpkZstr:
			str, err := vmPackArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			if nul, err := indexByteWithCancel(str, 0, cancelFn); err != nil {
				return "", err
			} else if nul >= 0 {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string contains zeros)", argNum)
			}
			slen := uint64(len(str)) + 1
			if slen > vmPackMaxSize-totalSize {
				return "", errors.New("result too long")
			}
			if checkSize != nil {
				if err := checkSize(int64(totalSize + slen)); err != nil {
					return "", err
				}
			}
			if err := writeStringWithCancel(&sb, str, cancelFn, &copyWork); err != nil {
				return "", err
			}
			sb.WriteByte(0)
			totalSize += slen
		}
	}
	return sb.String(), nil
}

func vmPackPosRelatI(pos int64, l int) int {
	if pos > 0 {
		if pos > int64(l)+1 {
			return l + 2
		}
		return int(pos)
	}
	if pos == 0 {
		return 1
	}
	if -pos > int64(l) {
		return 1
	}
	return l + int(pos) + 1
}

// unpackEachWithCancel mirrors luastdlib.UnpackEachWithCancel while additionally polling
// cancelFn during large 'z' NUL scans and huge format strings.
func unpackEachWithCancel(fmtStr, data string, init int64, cancelFn func() error, emit func(luastdlib.PackValue) error) (int64, error) {
	if nul, err := indexByteWithCancel(fmtStr, 0, cancelFn); err != nil {
		return 0, err
	} else if nul >= 0 {
		fmtStr = fmtStr[:nul]
	}
	pos := vmPackPosRelatI(init, len(data)) - 1
	if pos > len(data) {
		return 0, errors.New("initial position out of string")
	}
	h := vmPackHeader{isLittle: vmNativeLittleEndian, maxAlign: 1}
	fpos := 0
	var steps int64

	for fpos < len(fmtStr) {
		steps++
		if (steps & 1023) == 0 {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
		opt, size, nToAlign, nextPos, err := vmPackGetDetails(&h, uint64(pos), fmtStr, fpos)
		if err != nil {
			return 0, err
		}
		fpos = nextPos
		rem := uint64(len(data) - pos)
		if nToAlign > rem || size > rem-nToAlign {
			return 0, errors.New("data string too short")
		}
		pos += int(nToAlign)

		switch opt {
		case vpkInt, vpkUint:
			val, err := vmUnpackIntBytes(data[pos:pos+int(size)], h.isLittle, int(size), opt == vpkInt)
			if err != nil {
				return 0, err
			}
			if err := emit(luastdlib.PackIntVal(val)); err != nil {
				return 0, err
			}

		case vpkFloat:
			var bits uint32
			if h.isLittle {
				bits = binary.LittleEndian.Uint32([]byte(data[pos : pos+4]))
			} else {
				bits = binary.BigEndian.Uint32([]byte(data[pos : pos+4]))
			}
			if err := emit(luastdlib.PackFloatVal(float64(math.Float32frombits(bits)))); err != nil {
				return 0, err
			}

		case vpkNumber, vpkDouble:
			var bits uint64
			if h.isLittle {
				bits = binary.LittleEndian.Uint64([]byte(data[pos : pos+8]))
			} else {
				bits = binary.BigEndian.Uint64([]byte(data[pos : pos+8]))
			}
			if err := emit(luastdlib.PackFloatVal(math.Float64frombits(bits))); err != nil {
				return 0, err
			}

		case vpkChar:
			if err := emit(luastdlib.PackStringVal(data[pos : pos+int(size)])); err != nil {
				return 0, err
			}

		case vpkString:
			rawLen, err := vmUnpackIntBytes(data[pos:pos+int(size)], h.isLittle, int(size), false)
			if err != nil {
				return 0, err
			}
			slen := uint64(rawLen)
			avail := uint64(len(data) - pos - int(size))
			if slen > avail {
				return 0, errors.New("data string too short")
			}
			start := pos + int(size)
			if err := emit(luastdlib.PackStringVal(data[start : start+int(slen)])); err != nil {
				return 0, err
			}
			pos += int(slen)

		case vpkZstr:
			idx, err := indexByteWithCancel(data[pos:], 0, cancelFn)
			if err != nil {
				return 0, err
			}
			if idx < 0 {
				return 0, errors.New("unfinished string for format 'z'")
			}
			if err := emit(luastdlib.PackStringVal(data[pos : pos+idx])); err != nil {
				return 0, err
			}
			pos += idx + 1

		case vpkPaddAlign, vpkPadding, vpkNop:
			// no value pushed
		}
		pos += int(size)
	}
	return int64(pos) + 1, nil
}
