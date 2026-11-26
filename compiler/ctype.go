package compiler

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

const (
	alphaBit  = 1 << 0
	digitBit  = 1 << 1
	printBit  = 1 << 2
	spaceBit  = 1 << 3
	xdigitBit = 1 << 4
)

// luaCtype matches PUC-Rio lctype.c (default NONA=0x00) with index 0 = EOZ (-1).
var luaCtype = [257]uint8{
	0x00, // EOZ (-1)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 0x00..0x07
	0x00, 0x08, 0x08, 0x08, 0x08, 0x08, 0x00, 0x00, // 0x08..0x0f
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 0x10..0x17
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 0x18..0x1f
	0x0c, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, // 0x20..0x27
	0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, // 0x28..0x2f
	0x16, 0x16, 0x16, 0x16, 0x16, 0x16, 0x16, 0x16, // 0x30..0x37 ('0'..'7')
	0x16, 0x16, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, // 0x38..0x3f ('8'..'9',...)
	0x04, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x05, // 0x40..0x47 ('@','A'..'G')
	0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, // 0x48..0x4f
	0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, // 0x50..0x57
	0x05, 0x05, 0x05, 0x04, 0x04, 0x04, 0x04, 0x05, // 0x58..0x5f (..., '_')
	0x04, 0x15, 0x15, 0x15, 0x15, 0x15, 0x15, 0x05, // 0x60..0x67 ('`','a'..'g')
	0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, // 0x68..0x6f
	0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, 0x05, // 0x70..0x77
	0x05, 0x05, 0x05, 0x04, 0x04, 0x04, 0x04, 0x00, // 0x78..0x7f
	// 0x80..0xff are all 0x00
}

func isAlpha(c int) bool  { return c >= -1 && c <= 255 && (luaCtype[c+1]&alphaBit) != 0 }
func isAlnum(c int) bool  { return c >= -1 && c <= 255 && (luaCtype[c+1]&(alphaBit|digitBit)) != 0 }
func isDigit(c int) bool  { return c >= -1 && c <= 255 && (luaCtype[c+1]&digitBit) != 0 }
func isSpace(c int) bool  { return c >= -1 && c <= 255 && (luaCtype[c+1]&spaceBit) != 0 }
func isPrint(c int) bool  { return c >= -1 && c <= 255 && (luaCtype[c+1]&printBit) != 0 }
func isXDigit(c int) bool { return c >= -1 && c <= 255 && (luaCtype[c+1]&xdigitBit) != 0 }

func hexValue(c int) int {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// utf8Esc encodes an extended UTF-8 codepoint (up to 0x7FFFFFFF, 1..6 bytes)
// matching PUC-Rio lobject.c:luaO_utf8esc.
func utf8Esc(x uint32) ([]byte, int) {
	var buff [8]byte
	n := 1
	if x < 0x80 {
		buff[len(buff)-1] = byte(x)
	} else {
		mfb := uint32(0x3f)
		for {
			buff[len(buff)-n] = byte(0x80 | (x & 0x3f))
			n++
			x >>= 6
			mfb >>= 1
			if x <= mfb {
				break
			}
		}
		buff[len(buff)-n] = byte((^mfb << 1) | x)
	}
	return buff[len(buff)-n:], n
}

// str2Int parses a Lua integer literal matching lobject.c:l_str2int.
// Hex integers wrap modulo 2^64; decimal integers fail on overflow so they
// fall back to float parsing.
func str2Int(s string) (int64, bool) {
	i := 0
	for i < len(s) && isSpace(int(s[i])) {
		i++
	}
	neg := false
	if i < len(s) {
		if s[i] == '-' {
			neg = true
			i++
		} else if s[i] == '+' {
			i++
		}
	}
	var a uint64
	empty := true
	if i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
		for i < len(s) && isXDigit(int(s[i])) {
			a = a*16 + uint64(hexValue(int(s[i])))
			empty = false
			i++
		}
	} else {
		const maxBy10 = uint64(math.MaxInt64 / 10)
		const maxLastD = uint64(math.MaxInt64 % 10)
		var negExtra uint64
		if neg {
			negExtra = 1
		}
		for i < len(s) && isDigit(int(s[i])) {
			d := uint64(s[i] - '0')
			if a >= maxBy10 && (a > maxBy10 || d > maxLastD+negExtra) {
				return 0, false
			}
			a = a*10 + d
			empty = false
			i++
		}
	}
	for i < len(s) && isSpace(int(s[i])) {
		i++
	}
	if empty || i != len(s) {
		return 0, false
	}
	if neg {
		return int64(0 - a), true
	}
	return int64(a), true
}

// str2Float parses a Lua decimal or hexadecimal float literal matching
// lobject.c:l_str2d / C99 strtod semantics.
func str2Float(s string) (float64, bool) {
	i := 0
	for i < len(s) && isSpace(int(s[i])) {
		i++
	}
	end := len(s)
	for end > i && isSpace(int(s[end-1])) {
		end--
	}
	s = s[i:end]
	if len(s) == 0 {
		return 0, false
	}

	p := 0
	if s[p] == '+' || s[p] == '-' {
		p++
	}
	if p >= len(s) {
		return 0, false
	}

	// Hexadecimal float: 0[xX] (hex digits with optional single '.') [pP [+-]? digits]
	if p+1 < len(s) && s[p] == '0' && (s[p+1] == 'x' || s[p+1] == 'X') {
		q := p + 2
		digits := 0
		hasDot := false
		for q < len(s) {
			c := s[q]
			if c == '.' {
				if hasDot {
					return 0, false
				}
				hasDot = true
				q++
			} else if isXDigit(int(c)) {
				digits++
				q++
			} else {
				break
			}
		}
		if digits == 0 {
			return 0, false
		}
		hasExp := false
		if q < len(s) && (s[q] == 'p' || s[q] == 'P') {
			hasExp = true
			q++
			if q < len(s) && (s[q] == '+' || s[q] == '-') {
				q++
			}
			expDigits := 0
			for q < len(s) && isDigit(int(s[q])) {
				expDigits++
				q++
			}
			if expDigits == 0 {
				return 0, false
			}
		}
		if q != len(s) {
			return 0, false
		}
		// Go's strconv.ParseFloat requires a 'p' exponent on hex floats.
		parseStr := s
		if !hasExp {
			parseStr = s + "p0"
		}
		f, err := strconv.ParseFloat(parseStr, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		return f, true
	}

	// Decimal float: digits with optional single '.' and optional [eE][+-]?digits.
	q := p
	digits := 0
	hasDot := false
	for q < len(s) {
		c := s[q]
		if c == '.' {
			if hasDot {
				return 0, false
			}
			hasDot = true
			q++
		} else if isDigit(int(c)) {
			digits++
			q++
		} else {
			break
		}
	}
	if digits == 0 {
		return 0, false
	}
	if q < len(s) && (s[q] == 'e' || s[q] == 'E') {
		q++
		if q < len(s) && (s[q] == '+' || s[q] == '-') {
			q++
		}
		expDigits := 0
		for q < len(s) && isDigit(int(s[q])) {
			expDigits++
			q++
		}
		if expDigits == 0 {
			return 0, false
		}
	}
	if q != len(s) {
		return 0, false
	}
	if strings.ContainsAny(s, "_nNiI") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// str2Num parses a Lua numeral, preferring int64 over float64 (matching luaO_str2num).
func str2Num(s string) (iv int64, fv float64, isInt bool, ok bool) {
	if i, good := str2Int(s); good {
		return i, 0, true, true
	}
	if f, good := str2Float(s); good {
		return 0, f, false, true
	}
	return 0, 0, false, false
}

// ceilLog2 computes ceil(log2(x)), matching lobject.c:luaO_ceillog2.
func ceilLog2(x uint32) uint8 {
	var log2 = [256]uint8{
		0, 1, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5,
		6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6,
		7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
		7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7,
		8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
		8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
		8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
		8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8,
	}
	l := uint8(0)
	x--
	for x >= 256 {
		l += 8
		x >>= 8
	}
	return l + log2[x]
}
