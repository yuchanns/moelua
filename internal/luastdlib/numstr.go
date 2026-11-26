package luastdlib

import (
	"errors"
	"math"
	"math/bits"
	"strconv"
)

const (
	maxIntBy10  = uint64(math.MaxInt64 / 10)
	maxIntLastD = uint64(math.MaxInt64 % 10)
)

// StrToInt converts a Lua integer numeral string to int64, matching l_str2int
// in Lua 5.5.1 lobject.c.
// Accepts optional leading/trailing ASCII whitespace, optional +/-, and either
// decimal digits (no overflow allowed) or 0x/0X hex digits (unsigned wraparound).
func StrToInt(s string) (int64, bool) {
	i := 0
	n := len(s)
	for i < n && isSpace(s[i]) {
		i++
	}
	neg := false
	if i < n {
		if s[i] == '-' {
			neg = true
			i++
		} else if s[i] == '+' {
			i++
		}
	}
	var a uint64
	empty := true
	if i+1 < n && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		i += 2
		for i < n && isXDigit(s[i]) {
			a = a*16 + uint64(hexValue(s[i]))
			empty = false
			i++
		}
	} else {
		limit := maxIntLastD
		if neg {
			limit++
		}
		for i < n && isDigit(s[i]) {
			d := uint64(s[i] - '0')
			if a >= maxIntBy10 && (a > maxIntBy10 || d > limit) {
				return 0, false
			}
			a = a*10 + d
			empty = false
			i++
		}
	}
	for i < n && isSpace(s[i]) {
		i++
	}
	if empty || i != n {
		return 0, false
	}
	if neg {
		a = 0 - a
	}
	return int64(a), true
}

// StrToIntBase converts a string in base [2..36] to int64, matching b_str2int
// in Lua 5.5.1 lbaselib.c (unsigned wraparound).
func StrToIntBase(s string, base int) (int64, bool) {
	if base < 2 || base > 36 {
		return 0, false
	}
	i := 0
	n := len(s)
	for i < n && isSpace(s[i]) {
		i++
	}
	neg := false
	if i < n {
		if s[i] == '-' {
			neg = true
			i++
		} else if s[i] == '+' {
			i++
		}
	}
	var a uint64
	empty := true
	ub := uint64(base)
	for i < n && isAlnum(s[i]) {
		var d byte
		if isDigit(s[i]) {
			d = s[i] - '0'
		} else {
			d = toLowerASCII(s[i]) - 'a' + 10
		}
		if int(d) >= base {
			return 0, false
		}
		a = a*ub + uint64(d)
		empty = false
		i++
	}
	for i < n && isSpace(s[i]) {
		i++
	}
	if empty || i != n {
		return 0, false
	}
	if neg {
		a = 0 - a
	}
	return int64(a), true
}

// StrToFloat converts a Lua numeric string (decimal or hexadecimal float) to
// float64, matching l_str2d in Lua 5.5.1 lobject.c.
// Accepts leading/trailing ASCII whitespace, optional +/-, decimal floats, and
// C99 hex floats (with optional p/P exponent). Rejects underscores, inf,
// infinity, and nan.
func StrToFloat(s string) (float64, bool) {
	i := 0
	n := len(s)
	for i < n && isSpace(s[i]) {
		i++
	}
	for n > i && isSpace(s[n-1]) {
		n--
	}
	if i == n {
		return 0, false
	}
	trimmed := s[i:n]

	pos := 0
	neg := false
	if trimmed[pos] == '-' {
		neg = true
		pos++
	} else if trimmed[pos] == '+' {
		pos++
	}
	if pos >= len(trimmed) {
		return 0, false
	}

	// Hexadecimal floating-point numeral: 0x... / 0X...
	if pos+1 < len(trimmed) && trimmed[pos] == '0' && (trimmed[pos+1] == 'x' || trimmed[pos+1] == 'X') {
		return parseHexFloat(trimmed[pos+2:], neg)
	}

	// Decimal floating-point numeral: validate strict Lua/C decimal syntax first
	// so Go extensions like '_', "inf", "infinity", "nan" are rejected.
	p := pos
	hasDigits := false
	for p < len(trimmed) && isDigit(trimmed[p]) {
		hasDigits = true
		p++
	}
	if p < len(trimmed) && trimmed[p] == '.' {
		p++
		for p < len(trimmed) && isDigit(trimmed[p]) {
			hasDigits = true
			p++
		}
	}
	if !hasDigits {
		return 0, false
	}
	if p < len(trimmed) && (trimmed[p] == 'e' || trimmed[p] == 'E') {
		p++
		if p < len(trimmed) && (trimmed[p] == '+' || trimmed[p] == '-') {
			p++
		}
		expDigits := false
		for p < len(trimmed) && isDigit(trimmed[p]) {
			expDigits = true
			p++
		}
		if !expDigits {
			return 0, false
		}
	}
	if p != len(trimmed) {
		return 0, false
	}

	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// parseHexFloat parses a C99 hexadecimal floating-point body (after optional
// sign and "0x"/"0X") with exact IEEE-754 binary64 round-to-nearest-even.
func parseHexFloat(s string, neg bool) (float64, bool) {
	const expClamp = 1_000_000_000

	var signBit uint64
	if neg {
		signBit = 1 << 63
	}

	p := 0
	n := len(s)
	hasDot := false
	hasDigits := false
	var mant uint64
	sigBits := 0
	sticky := false
	var bitExp int64
	var leadingFracZeros int64

	for p < n {
		c := s[p]
		if c == '.' {
			if hasDot {
				return 0, false
			}
			hasDot = true
			p++
			continue
		}
		if !isXDigit(c) {
			break
		}
		hasDigits = true
		d := hexValue(c)
		p++

		if sigBits == 0 {
			if d == 0 {
				if hasDot && leadingFracZeros < expClamp {
					leadingFracZeros++
				}
				continue
			}
			w := bits.Len8(d) // 1..4
			mant = uint64(d)
			sigBits = w
			if hasDot {
				bitExp = -4*(leadingFracZeros+1) + int64(w-1)
			} else {
				bitExp = int64(w - 1)
			}
		} else {
			if !hasDot && bitExp < expClamp {
				bitExp += 4
			}
			if sigBits+4 <= 60 {
				mant = (mant << 4) | uint64(d)
				sigBits += 4
			} else if sigBits < 60 {
				take := 60 - sigBits
				drop := 4 - take
				mant = (mant << take) | uint64(d>>drop)
				if (d & ((1 << drop) - 1)) != 0 {
					sticky = true
				}
				sigBits = 60
			} else if d != 0 {
				sticky = true
			}
		}
	}

	if !hasDigits {
		return 0, false
	}

	var expPart int64
	if p < n {
		if s[p] != 'p' && s[p] != 'P' {
			return 0, false
		}
		p++
		expNeg := false
		if p < n {
			if s[p] == '-' {
				expNeg = true
				p++
			} else if s[p] == '+' {
				p++
			}
		}
		if p >= n || !isDigit(s[p]) {
			return 0, false
		}
		for p < n && isDigit(s[p]) {
			if expPart < expClamp {
				expPart = expPart*10 + int64(s[p]-'0')
			}
			p++
		}
		if p != n {
			return 0, false
		}
		if expNeg {
			expPart = -expPart
		}
	}

	// Zero value preserves sign regardless of exponent.
	if sigBits == 0 {
		return math.Float64frombits(signBit), true
	}

	bitExp += expPart
	// Align MSB of mant to bit 59 (60 bits total).
	mant <<= (60 - sigBits)

	if bitExp > 1023 {
		return math.Float64frombits(signBit | (0x7FF << 52)), true
	}

	if bitExp >= -1022 {
		// Normal number: shift right by 7 to place MSB at bit 52.
		sig := mant >> 7
		rem := mant & 0x7F
		if rem > 0x40 || (rem == 0x40 && (sticky || (sig&1) != 0)) {
			sig++
			if sig == (1 << 53) {
				sig >>= 1
				bitExp++
				if bitExp > 1023 {
					return math.Float64frombits(signBit | (0x7FF << 52)), true
				}
			}
		}
		return math.Float64frombits(signBit | (uint64(bitExp+1023) << 52) | (sig & ((1 << 52) - 1))), true
	}

	// Subnormal or underflow.
	shift := (-1022 - bitExp) + 7
	if shift > 60 {
		return math.Float64frombits(signBit), true
	}
	uShift := uint(shift)
	var sig uint64
	if uShift < 64 {
		sig = mant >> uShift
	}
	half := uint64(1) << (uShift - 1)
	mask := (uint64(1) << uShift) - 1
	rem := mant & mask
	if rem > half || (rem == half && (sticky || (sig&1) != 0)) {
		sig++
	}
	return math.Float64frombits(signBit | sig), true
}

// StrToNum converts a string to a Lua integer or float, mirroring luaO_str2num
// in Lua 5.5.1 lobject.c (integer is attempted before float).
func StrToNum(s string) (i int64, f float64, isInt bool, ok bool) {
	if iv, ok := StrToInt(s); ok {
		return iv, 0, true, true
	}
	if fv, ok := StrToFloat(s); ok {
		return 0, fv, false, true
	}
	return 0, 0, false, false
}

// floatToInt implements lua_numbertointeger from Lua 5.5.1 llimits.h for exact
// integral floats (no truncation).
func floatToInt(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if math.Floor(f) != f {
		return 0, false
	}
	const minIntF = float64(math.MinInt64)
	const maxBoundF = -minIntF
	if f >= minIntF && f < maxBoundF {
		return int64(f), true
	}
	return 0, false
}

// FormatFloat implements Lua 5.5.1 tostringbuffFloat from lobject.c:
// format with %.15g, verify exact float64 roundtrip, fall back to %.17g if
// precision was lost, and append ".0" if the result looks like an integer.
func FormatFloat(f float64) string {
	if math.IsNaN(f) {
		return "nan"
	}
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	check, err := strconv.ParseFloat(s, 64)
	if err != nil || check != f {
		s = strconv.FormatFloat(f, 'g', 17, 64)
	}
	if looksLikeInt(s) {
		return s + ".0"
	}
	return s
}

func looksLikeInt(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '-' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
