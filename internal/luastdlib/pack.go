package luastdlib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// LP64 native sizes matching Lua 5.5.1 on 64-bit POSIX platforms.
const (
	packSizeChar   = 1
	packSizeShort  = 2
	packSizeInt    = 4
	packSizeLong   = 8
	packSizeSizeT  = 8
	packSizeLuaInt = 8
	packSizeFloat  = 4
	packSizeDouble = 8
	packSizeLuaNum = 8
	packMaxAlign   = 8
	packMaxIntSize = 16
	packPadByte    = 0x00
	packMaxSize    = uint64(math.MaxInt64)
)

var nativeLittleEndian = detectNativeLittleEndian()

func detectNativeLittleEndian() bool {
	var buf [2]byte
	binary.NativeEndian.PutUint16(buf[:], 1)
	return buf[0] == 1
}

// PackKind identifies the value kind used by Pack and Unpack.
type PackKind uint8

const (
	PackInt PackKind = iota
	PackFloat
	PackString
)

// PackValue represents a value passed to Pack or returned by Unpack.
type PackValue struct {
	Kind PackKind
	Int  int64
	Flt  float64
	Str  string
}

// PackIntVal constructs an integer PackValue.
func PackIntVal(v int64) PackValue { return PackValue{Kind: PackInt, Int: v} }

// PackFloatVal constructs a floating-point PackValue.
func PackFloatVal(v float64) PackValue { return PackValue{Kind: PackFloat, Flt: v} }

// PackStringVal constructs a string PackValue.
func PackStringVal(v string) PackValue { return PackValue{Kind: PackString, Str: v} }

type packHeader struct {
	isLittle bool
	maxAlign uint64
}

func initPackHeader() packHeader {
	return packHeader{
		isLittle: nativeLittleEndian,
		maxAlign: 1,
	}
}

type kOption uint8

const (
	kInt kOption = iota
	kUint
	kFloat
	kNumber
	kDouble
	kChar
	kString
	kZstr
	kPadding
	kPaddAlign
	kNop
)

func packGetNum(fmtStr string, pos int, df uint64) (uint64, int, bool) {
	if pos >= len(fmtStr) || !isDigit(fmtStr[pos]) {
		return df, pos, true
	}
	var a uint64
	for pos < len(fmtStr) && isDigit(fmtStr[pos]) && a <= (packMaxSize-9)/10 {
		a = a*10 + uint64(fmtStr[pos]-'0')
		pos++
	}
	return a, pos, false
}

func packGetNumLimit(fmtStr string, pos int, df uint64) (uint64, int, error) {
	sz, pos, _ := packGetNum(fmtStr, pos, df)
	if sz == 0 || sz > packMaxIntSize {
		return 0, pos, fmt.Errorf("integral size (%d) out of limits [1,%d]", sz, packMaxIntSize)
	}
	return sz, pos, nil
}

func packGetOption(h *packHeader, fmtStr string, pos int) (kOption, uint64, int, error) {
	opt := fmtStr[pos]
	pos++
	switch opt {
	case 'b':
		return kInt, packSizeChar, pos, nil
	case 'B':
		return kUint, packSizeChar, pos, nil
	case 'h':
		return kInt, packSizeShort, pos, nil
	case 'H':
		return kUint, packSizeShort, pos, nil
	case 'l':
		return kInt, packSizeLong, pos, nil
	case 'L':
		return kUint, packSizeLong, pos, nil
	case 'j':
		return kInt, packSizeLuaInt, pos, nil
	case 'J':
		return kUint, packSizeLuaInt, pos, nil
	case 'T':
		return kUint, packSizeSizeT, pos, nil
	case 'f':
		return kFloat, packSizeFloat, pos, nil
	case 'n':
		return kNumber, packSizeLuaNum, pos, nil
	case 'd':
		return kDouble, packSizeDouble, pos, nil
	case 'i':
		sz, np, err := packGetNumLimit(fmtStr, pos, packSizeInt)
		return kInt, sz, np, err
	case 'I':
		sz, np, err := packGetNumLimit(fmtStr, pos, packSizeInt)
		return kUint, sz, np, err
	case 's':
		sz, np, err := packGetNumLimit(fmtStr, pos, packSizeSizeT)
		return kString, sz, np, err
	case 'c':
		sz, np, isDef := packGetNum(fmtStr, pos, 0)
		if isDef {
			return kChar, 0, np, errors.New("missing size for format option 'c'")
		}
		return kChar, sz, np, nil
	case 'z':
		return kZstr, 0, pos, nil
	case 'x':
		return kPadding, 1, pos, nil
	case 'X':
		return kPaddAlign, 0, pos, nil
	case ' ':
		return kNop, 0, pos, nil
	case '<':
		h.isLittle = true
		return kNop, 0, pos, nil
	case '>':
		h.isLittle = false
		return kNop, 0, pos, nil
	case '=':
		h.isLittle = nativeLittleEndian
		return kNop, 0, pos, nil
	case '!':
		sz, np, err := packGetNumLimit(fmtStr, pos, packMaxAlign)
		if err != nil {
			return kNop, 0, np, err
		}
		h.maxAlign = sz
		return kNop, 0, np, nil
	default:
		return kNop, 0, pos, fmt.Errorf("invalid format option '%c'", opt)
	}
}

func isPow2(x uint64) bool {
	return (x & (x - 1)) == 0
}

func packGetDetails(h *packHeader, totalSize uint64, fmtStr string, pos int) (opt kOption, size uint64, nToAlign uint64, nextPos int, err error) {
	opt, size, pos, err = packGetOption(h, fmtStr, pos)
	if err != nil {
		return kNop, 0, 0, pos, err
	}
	align := size
	if opt == kPaddAlign {
		if pos >= len(fmtStr) {
			return kNop, 0, 0, pos, errors.New("invalid next option for option 'X'")
		}
		opt2, align2, pos2, err2 := packGetOption(h, fmtStr, pos)
		if err2 != nil {
			return kNop, 0, 0, pos2, err2
		}
		if opt2 == kChar || align2 == 0 {
			return kNop, 0, 0, pos2, errors.New("invalid next option for option 'X'")
		}
		align = align2
		pos = pos2
	}
	if align <= 1 || opt == kChar {
		nToAlign = 0
	} else {
		if align > h.maxAlign {
			align = h.maxAlign
		}
		if !isPow2(align) {
			return kNop, 0, 0, pos, errors.New("format asks for alignment not power of 2")
		}
		szModA := totalSize & (align - 1)
		nToAlign = (align - szModA) & (align - 1)
	}
	return opt, size, nToAlign, pos, nil
}

func packIntBytes(buf *[]byte, n uint64, isLittle bool, size int, neg bool) {
	var tmp [packMaxIntSize]byte
	if isLittle {
		tmp[0] = byte(n)
		for i := 1; i < size; i++ {
			n >>= 8
			tmp[i] = byte(n)
		}
		if neg && size > packSizeLuaInt {
			for i := packSizeLuaInt; i < size; i++ {
				tmp[i] = 0xFF
			}
		}
	} else {
		tmp[size-1] = byte(n)
		for i := 1; i < size; i++ {
			n >>= 8
			tmp[size-1-i] = byte(n)
		}
		if neg && size > packSizeLuaInt {
			for i := packSizeLuaInt; i < size; i++ {
				tmp[size-1-i] = 0xFF
			}
		}
	}
	*buf = append(*buf, tmp[:size]...)
}

func unpackIntBytes(data string, isLittle bool, size int, isSigned bool) (int64, error) {
	limit := size
	if limit > packSizeLuaInt {
		limit = packSizeLuaInt
	}
	var res uint64
	for i := limit - 1; i >= 0; i-- {
		res <<= 8
		if isLittle {
			res |= uint64(data[i])
		} else {
			res |= uint64(data[size-1-i])
		}
	}
	if size < packSizeLuaInt {
		if isSigned {
			mask := uint64(1) << (size*8 - 1)
			res = (res ^ mask) - mask
		}
	} else if size > packSizeLuaInt {
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

func packArgToInt(v PackValue, argIdx int) (int64, error) {
	switch v.Kind {
	case PackInt:
		return v.Int, nil
	case PackFloat:
		if i, ok := floatToInt(v.Flt); ok {
			return i, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number has no integer representation)", argIdx)
	case PackString:
		if i, ok := StrToInt(v.Str); ok {
			return i, nil
		}
		if f, ok := StrToFloat(v.Str); ok {
			if i, ok := floatToInt(f); ok {
				return i, nil
			}
			return 0, fmt.Errorf("bad argument #%d to 'pack' (number has no integer representation)", argIdx)
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	}
}

func packArgToFloat(v PackValue, argIdx int) (float64, error) {
	switch v.Kind {
	case PackFloat:
		return v.Flt, nil
	case PackInt:
		return float64(v.Int), nil
	case PackString:
		if i, f, isInt, ok := StrToNum(v.Str); ok {
			if isInt {
				return float64(i), nil
			}
			return f, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'pack' (number expected)", argIdx)
	}
}

func packArgToString(v PackValue, argIdx int) (string, error) {
	switch v.Kind {
	case PackString:
		return v.Str, nil
	case PackInt:
		return strconv.FormatInt(v.Int, 10), nil
	case PackFloat:
		return FormatFloat(v.Flt), nil
	default:
		return "", fmt.Errorf("bad argument #%d to 'pack' (string expected)", argIdx)
	}
}

func trimAtNUL(s string) string {
	if idx := strings.IndexByte(s, 0); idx >= 0 {
		return s[:idx]
	}
	return s
}

// Pack implements Lua 5.5.1 string.pack(fmt, v1, v2, ...).
func Pack(fmtStr string, args ...PackValue) (string, error) {
	return PackWithLimits(fmtStr, nil, args...)
}

// PackWithLimits implements Lua 5.5.1 string.pack with an optional output size-limit
// callback invoked before appending any bytes to the output buffer.
func PackWithLimits(fmtStr string, checkSize func(int64) error, args ...PackValue) (string, error) {
	return PackWithControls(fmtStr, checkSize, nil, args...)
}

// PackWithControls implements Lua 5.5.1 string.pack with optional output size-limit
// and cooperative cancellation callbacks polled during format scanning.
func PackWithControls(fmtStr string, checkSize func(int64) error, cancelFn func() error, args ...PackValue) (string, error) {
	fmtStr = trimAtNUL(fmtStr)
	h := initPackHeader()
	var buf []byte
	var totalSize uint64
	argIdx := 0
	pos := 0
	var steps int64

	for pos < len(fmtStr) {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return "", err
			}
		}
		opt, size, nToAlign, nextPos, err := packGetDetails(&h, totalSize, fmtStr, pos)
		if err != nil {
			return "", err
		}
		pos = nextPos
		if size > packMaxSize-nToAlign || size+nToAlign > packMaxSize-totalSize {
			return "", errors.New("result too long")
		}
		nextTotal := totalSize + nToAlign + size
		if checkSize != nil && nextTotal > totalSize {
			if err := checkSize(int64(nextTotal)); err != nil {
				return "", err
			}
		}
		totalSize = nextTotal
		for i := uint64(0); i < nToAlign; i++ {
			buf = append(buf, packPadByte)
		}

		switch opt {
		case kPadding:
			buf = append(buf, packPadByte)
			continue
		case kPaddAlign, kNop:
			continue
		}

		if argIdx >= len(args) {
			return "", fmt.Errorf("bad argument #%d to 'pack' (value expected)", argIdx+2)
		}
		v := args[argIdx]
		argNum := argIdx + 2
		argIdx++

		switch opt {
		case kInt:
			n, err := packArgToInt(v, argNum)
			if err != nil {
				return "", err
			}
			if size < packSizeLuaInt {
				lim := int64(1) << (size*8 - 1)
				if n < -lim || n >= lim {
					return "", fmt.Errorf("bad argument #%d to 'pack' (integer overflow)", argNum)
				}
			}
			packIntBytes(&buf, uint64(n), h.isLittle, int(size), n < 0)

		case kUint:
			n, err := packArgToInt(v, argNum)
			if err != nil {
				return "", err
			}
			if size < packSizeLuaInt {
				if uint64(n) >= (uint64(1) << (size * 8)) {
					return "", fmt.Errorf("bad argument #%d to 'pack' (unsigned overflow)", argNum)
				}
			}
			packIntBytes(&buf, uint64(n), h.isLittle, int(size), false)

		case kFloat:
			f, err := packArgToFloat(v, argNum)
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
			buf = append(buf, tmp[:]...)

		case kNumber, kDouble:
			f, err := packArgToFloat(v, argNum)
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
			buf = append(buf, tmp[:]...)

		case kChar:
			s, err := packArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			if uint64(len(s)) > size {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string longer than given size)", argNum)
			}
			buf = append(buf, s...)
			if pad := size - uint64(len(s)); pad > 0 {
				if pad > (1 << 30) {
					return "", errors.New("result too long")
				}
				buf = append(buf, make([]byte, pad)...)
			}

		case kString:
			s, err := packArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			slen := uint64(len(s))
			if size < packSizeLuaInt && slen >= (uint64(1)<<(size*8)) {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string length does not fit in given size)", argNum)
			}
			if slen > packMaxSize-totalSize {
				return "", errors.New("result too long")
			}
			if checkSize != nil && slen > 0 {
				if err := checkSize(int64(totalSize + slen)); err != nil {
					return "", err
				}
			}
			packIntBytes(&buf, slen, h.isLittle, int(size), false)
			buf = append(buf, s...)
			totalSize += slen

		case kZstr:
			s, err := packArgToString(v, argNum)
			if err != nil {
				return "", err
			}
			if strings.IndexByte(s, 0) >= 0 {
				return "", fmt.Errorf("bad argument #%d to 'pack' (string contains zeros)", argNum)
			}
			slen := uint64(len(s)) + 1
			if slen > packMaxSize-totalSize {
				return "", errors.New("result too long")
			}
			if checkSize != nil {
				if err := checkSize(int64(totalSize + slen)); err != nil {
					return "", err
				}
			}
			buf = append(buf, s...)
			buf = append(buf, 0)
			totalSize += slen
		}
	}
	return string(buf), nil
}

// PackSize implements Lua 5.5.1 string.packsize(fmt).
func PackSize(fmtStr string) (int64, error) {
	return PackSizeWithCancel(fmtStr, nil)
}

// PackSizeWithCancel implements Lua 5.5.1 string.packsize(fmt), polling cancelFn
// periodically while scanning the format string (including no-output options).
func PackSizeWithCancel(fmtStr string, cancelFn func() error) (int64, error) {
	fmtStr = trimAtNUL(fmtStr)
	h := initPackHeader()
	var totalSize uint64
	pos := 0
	var steps int64
	for pos < len(fmtStr) {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
		opt, size, nToAlign, nextPos, err := packGetDetails(&h, totalSize, fmtStr, pos)
		if err != nil {
			return 0, err
		}
		pos = nextPos
		if opt == kString || opt == kZstr {
			return 0, errors.New("variable-length format")
		}
		if size > packMaxSize-nToAlign {
			return 0, errors.New("format result too large")
		}
		step := size + nToAlign
		if totalSize > packMaxSize-step {
			return 0, errors.New("format result too large")
		}
		totalSize += step
	}
	return int64(totalSize), nil
}

// UnpackEach implements Lua 5.5.1 string.unpack(fmt, s [, pos]), invoking emit(val) for
// each decoded item without allocating an intermediate []PackValue slice. Decoding stops
// immediately if emit returns a non-nil error.
// Returns the 1-based index of the first unread byte.
func UnpackEach(fmtStr, data string, init int64, emit func(PackValue) error) (int64, error) {
	return UnpackEachWithCancel(fmtStr, data, init, nil, emit)
}

// UnpackEachWithCancel is like UnpackEach, additionally polling cancelFn periodically
// while scanning the format string (including options that emit no values).
func UnpackEachWithCancel(fmtStr, data string, init int64, cancelFn func() error, emit func(PackValue) error) (int64, error) {
	fmtStr = trimAtNUL(fmtStr)
	pos := posrelatI(init, len(data)) - 1
	if pos > len(data) {
		return 0, errors.New("initial position out of string")
	}
	h := initPackHeader()
	fpos := 0
	var steps int64

	for fpos < len(fmtStr) {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return 0, err
			}
		}
		opt, size, nToAlign, nextPos, err := packGetDetails(&h, uint64(pos), fmtStr, fpos)
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
		case kInt, kUint:
			val, err := unpackIntBytes(data[pos:pos+int(size)], h.isLittle, int(size), opt == kInt)
			if err != nil {
				return 0, err
			}
			if err := emit(PackIntVal(val)); err != nil {
				return 0, err
			}

		case kFloat:
			var bits uint32
			if h.isLittle {
				bits = binary.LittleEndian.Uint32([]byte(data[pos : pos+4]))
			} else {
				bits = binary.BigEndian.Uint32([]byte(data[pos : pos+4]))
			}
			if err := emit(PackFloatVal(float64(math.Float32frombits(bits)))); err != nil {
				return 0, err
			}

		case kNumber, kDouble:
			var bits uint64
			if h.isLittle {
				bits = binary.LittleEndian.Uint64([]byte(data[pos : pos+8]))
			} else {
				bits = binary.BigEndian.Uint64([]byte(data[pos : pos+8]))
			}
			if err := emit(PackFloatVal(math.Float64frombits(bits))); err != nil {
				return 0, err
			}

		case kChar:
			if err := emit(PackStringVal(data[pos : pos+int(size)])); err != nil {
				return 0, err
			}

		case kString:
			rawLen, err := unpackIntBytes(data[pos:pos+int(size)], h.isLittle, int(size), false)
			if err != nil {
				return 0, err
			}
			slen := uint64(rawLen)
			avail := uint64(len(data) - pos - int(size))
			if slen > avail {
				return 0, errors.New("data string too short")
			}
			start := pos + int(size)
			if err := emit(PackStringVal(data[start : start+int(slen)])); err != nil {
				return 0, err
			}
			pos += int(slen)

		case kZstr:
			idx := strings.IndexByte(data[pos:], 0)
			if idx < 0 {
				return 0, errors.New("unfinished string for format 'z'")
			}
			if err := emit(PackStringVal(data[pos : pos+idx])); err != nil {
				return 0, err
			}
			pos += idx + 1

		case kPaddAlign, kPadding, kNop:
			// no value pushed
		}
		pos += int(size)
	}
	return int64(pos) + 1, nil
}

// Unpack implements Lua 5.5.1 string.unpack(fmt, s [, pos]).
// init is a 1-based Lua byte position (negative counts from end; 0 clips to 1).
// Returns the unpacked values and the 1-based index of the first unread byte.
func Unpack(fmtStr, data string, init int64) ([]PackValue, int64, error) {
	var results []PackValue
	nextPos, err := UnpackEach(fmtStr, data, init, func(pv PackValue) error {
		results = append(results, pv)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return results, nextPos, nil
}
