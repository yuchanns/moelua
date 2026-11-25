package luastdlib

import (
	"errors"
	"fmt"
)

const (
	// MaxUnicode is the maximum strict Unicode scalar value (U+10FFFF).
	MaxUnicode uint32 = 0x10FFFF
	// MaxUTF is the maximum extended UTF-8 value supported by Lua 5.4/5.5 (0x7FFFFFFF).
	MaxUTF uint32 = 0x7FFFFFFF

	// CharPattern matches UTF8PATT in Lua 5.5.1 lutf8lib.c.
	CharPattern = "[\x00-\x7f\xc2-\xfd][\x80-\xbf]*"
)

var (
	errInvalidUTF8      = errors.New("invalid UTF-8 code")
	errUTF8ContByte     = errors.New("initial position is a continuation byte")
	errUTF8PosOutBounds = errors.New("position out of bounds")
	errUTF8InitBounds   = errors.New("initial position out of bounds")
	errUTF8FinalBounds  = errors.New("final position out of bounds")
	errUTF8OutOfBounds  = errors.New("out of bounds")
)

var utf8Limits = [6]uint32{
	^uint32(0),
	0x80,
	0x800,
	0x10000,
	0x200000,
	0x4000000,
}

func isUTF8Cont(b byte) bool {
	return (b & 0xC0) == 0x80
}

// uPosrelat mirrors u_posrelat in lutf8lib.c.
func uPosrelat(pos int64, length int) int64 {
	if pos >= 0 {
		return pos
	}
	if uint64(0)-uint64(pos) > uint64(length) {
		return 0
	}
	return int64(length) + pos + 1
}

// utf8Decode mirrors utf8_decode in lutf8lib.c.
func utf8Decode(s string, pos int, strict bool) (val uint32, next int, ok bool) {
	if pos >= len(s) {
		return 0, pos, false
	}
	c := uint32(s[pos])
	var res uint32
	if c < 0x80 {
		res = c
	} else if c >= 0xFE {
		return 0, pos, false
	} else {
		count := 0
		for ; (c & 0x40) != 0; c <<= 1 {
			count++
			if pos+count >= len(s) {
				return 0, pos, false
			}
			cc := s[pos+count]
			if !isUTF8Cont(cc) {
				return 0, pos, false
			}
			res = (res << 6) | uint32(cc&0x3F)
		}
		res |= (c & 0x7F) << (count * 5)
		if res > MaxUTF || res < utf8Limits[count] {
			return 0, pos, false
		}
		pos += count
	}
	if strict {
		if res > MaxUnicode || (0xD800 <= res && res <= 0xDFFF) {
			return 0, pos, false
		}
	}
	return res, pos + 1, true
}

// appendUTF8Esc mirrors luaO_utf8esc in lobject.c.
func appendUTF8Esc(dst []byte, x uint32) []byte {
	if x < 0x80 {
		return append(dst, byte(x))
	}
	var buff [8]byte
	n := 1
	mfb := uint32(0x3F)
	for {
		buff[8-n] = byte(0x80 | (x & 0x3F))
		n++
		x >>= 6
		mfb >>= 1
		if x <= mfb {
			break
		}
	}
	buff[8-n] = byte((^mfb << 1) | x)
	return append(dst, buff[8-n:]...)
}

// Char encodes codepoints as UTF-8 bytes, mirroring Lua 5.5.1 utf8.char.
// When lax is true, accepts extended UTF-8 codepoints in [0, 0x7FFFFFFF]
// (exact Lua 5.5.1 utf8.char semantics). When lax is false, restricts to
// strict Unicode scalar values (<= 0x10FFFF, excluding surrogates 0xD800..0xDFFF).
func Char(codepoints []int64, lax bool) (string, error) {
	if len(codepoints) == 0 {
		return "", nil
	}
	buf := make([]byte, 0, len(codepoints))
	for idx, cp := range codepoints {
		u := uint64(cp)
		if lax {
			if u > uint64(MaxUTF) {
				return "", fmt.Errorf("bad argument #%d to 'char' (value out of range)", idx+1)
			}
		} else {
			if u > uint64(MaxUnicode) || (0xD800 <= u && u <= 0xDFFF) {
				return "", fmt.Errorf("bad argument #%d to 'char' (value out of range)", idx+1)
			}
		}
		buf = appendUTF8Esc(buf, uint32(u))
	}
	return string(buf), nil
}

// CodepointEach implements Lua 5.5.1 utf8.codepoint(s [, i [, j [, lax]]]),
// invoking emit(codepoint) for each decoded character in [i, j] without allocating
// an intermediate []int64 slice. Decoding stops immediately if emit returns an error.
func CodepointEach(s string, i, j int64, lax bool, emit func(cp int64) error) error {
	posi := uPosrelat(i, len(s))
	pose := uPosrelat(j, len(s))
	if posi < 1 {
		return errUTF8OutOfBounds
	}
	if pose > int64(len(s)) {
		return errUTF8OutOfBounds
	}
	if posi > pose {
		return nil
	}
	pos := int(posi - 1)
	se := int(pose)
	for pos < se {
		code, next, ok := utf8Decode(s, pos, !lax)
		if !ok {
			return errInvalidUTF8
		}
		if err := emit(int64(code)); err != nil {
			return err
		}
		pos = next
	}
	return nil
}

// Codepoint implements Lua 5.5.1 utf8.codepoint(s [, i [, j [, lax]]]).
// Returns codepoints for all UTF-8 characters starting in the 1-based byte range [i, j].
func Codepoint(s string, i, j int64, lax bool) ([]int64, error) {
	var out []int64
	err := CodepointEach(s, i, j, lax, func(cp int64) error {
		out = append(out, cp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Len implements Lua 5.5.1 utf8.len(s [, i [, j [, lax]]]).
// On valid UTF-8, returns (count, 0, nil).
// On invalid UTF-8 sequence, returns (0, errPos, nil) where errPos is the 1-based
// byte position of the first invalid byte.
// On out-of-bounds indices, returns a non-nil error.
func Len(s string, i, j int64, lax bool) (count int64, errPos int64, err error) {
	return LenWithCancel(s, i, j, lax, nil)
}

// LenWithCancel implements Lua 5.5.1 utf8.len while polling cancelFn every 1024
// decoded characters when cancelFn != nil.
func LenWithCancel(s string, i, j int64, lax bool, cancelFn func() error) (count int64, errPos int64, err error) {
	posi := uPosrelat(i, len(s))
	posj := uPosrelat(j, len(s))
	if posi < 1 || posi-1 > int64(len(s)) {
		return 0, 0, errUTF8InitBounds
	}
	posi--
	if posj-1 >= int64(len(s)) {
		return 0, 0, errUTF8FinalBounds
	}
	posj--
	var n int64
	for posi <= posj {
		if cancelFn != nil && n > 0 && (n&1023) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return 0, 0, cerr
			}
		}
		_, next, ok := utf8Decode(s, int(posi), !lax)
		if !ok {
			return 0, posi + 1, nil
		}
		posi = int64(next)
		n++
	}
	return n, 0, nil
}

// ValidCodesInit checks the initial condition performed by Lua 5.5.1 utf8.codes(s, lax).
func ValidCodesInit(s string) error {
	if len(s) > 0 && isUTF8Cont(s[0]) {
		return errInvalidUTF8
	}
	return nil
}

// Next implements the iteration step of Lua 5.5.1 utf8.codes(s [, lax]).
// Start with pos = 0, then pass the returned nextPos on subsequent calls.
// Returns (nextPos, codepoint, true, nil) while characters remain, or
// (0, 0, false, nil) when iteration ends.
func Next(s string, pos int64, lax bool) (nextPos int64, cp int64, ok bool, err error) {
	n := uint64(pos)
	slen := uint64(len(s))
	if n < slen {
		for n < slen && isUTF8Cont(s[n]) {
			n++
		}
	}
	if n >= slen {
		return 0, 0, false, nil
	}
	code, endIdx, valid := utf8Decode(s, int(n), !lax)
	if !valid || (endIdx < len(s) && isUTF8Cont(s[endIdx])) {
		return 0, 0, false, errInvalidUTF8
	}
	return int64(n) + 1, int64(code), true, nil
}

// Offset implements Lua 5.5.1 utf8.offset(s, n, i), returning BOTH the initial
// and final 1-based byte positions of the n-th character counting from i
// (or the character containing byte i when n == 0).
// If the requested character does not exist, ok is false and err is nil.
func Offset(s string, n int64, i int64) (start int64, end int64, ok bool, err error) {
	return OffsetWithCancel(s, n, i, nil)
}

// OffsetWithCancel implements Lua 5.5.1 utf8.offset while polling cancelFn every 1024
// steps when cancelFn != nil.
func OffsetWithCancel(s string, n int64, i int64, cancelFn func() error) (start int64, end int64, ok bool, err error) {
	length := int64(len(s))
	posi := uPosrelat(i, len(s))
	if posi < 1 || posi-1 > length {
		return 0, 0, false, errUTF8PosOutBounds
	}
	posi--

	var steps uint64
	if n == 0 {
		for posi > 0 && posi < length && isUTF8Cont(s[posi]) {
			steps++
			if cancelFn != nil && (steps&1023) == 0 {
				if cerr := cancelFn(); cerr != nil {
					return 0, 0, false, cerr
				}
			}
			posi--
		}
	} else {
		if posi < length && isUTF8Cont(s[posi]) {
			return 0, 0, false, errUTF8ContByte
		}
		if n < 0 {
			for n < 0 && posi > 0 {
				steps++
				if cancelFn != nil && (steps&1023) == 0 {
					if cerr := cancelFn(); cerr != nil {
						return 0, 0, false, cerr
					}
				}
				for {
					posi--
					if posi <= 0 || !isUTF8Cont(s[posi]) {
						break
					}
				}
				n++
			}
		} else {
			n-- // do not move for 1st character
			for n > 0 && posi < length {
				steps++
				if cancelFn != nil && (steps&1023) == 0 {
					if cerr := cancelFn(); cerr != nil {
						return 0, 0, false, cerr
					}
				}
				for {
					posi++
					if posi >= length || !isUTF8Cont(s[posi]) {
						break
					}
				}
				n--
			}
		}
	}

	if n != 0 {
		return 0, 0, false, nil
	}

	start = posi + 1
	if posi < length && (s[posi]&0x80) != 0 {
		if isUTF8Cont(s[posi]) {
			return 0, 0, false, errUTF8ContByte
		}
		for posi+1 < length && isUTF8Cont(s[posi+1]) {
			posi++
		}
	}
	end = posi + 1
	return start, end, true, nil
}

// OffsetDefault invokes Offset using Lua's default i (1 when n >= 0, len(s)+1 when n < 0).
func OffsetDefault(s string, n int64) (start int64, end int64, ok bool, err error) {
	i := int64(1)
	if n < 0 {
		i = int64(len(s)) + 1
	}
	return Offset(s, n, i)
}
