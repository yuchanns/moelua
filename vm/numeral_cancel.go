package vm

import (
	"errors"
	"math"
	"math/bits"
	"strconv"

	"github.com/yuchanns/moelua/internal/luastdlib"
)

const numeralCancelChunk = 4096

func isNumSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func isNumDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isNumXDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func numHexVal(c byte) byte {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func isNumAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// strToIntBase converts a string in base [2..36] to int64, polling the State's
// execution controls (context / instruction budget) every 4096 bytes for long inputs.
func (s *State) strToIntBase(str string, base int) (int64, bool, error) {
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil || len(str) <= numeralCancelChunk {
		iv, ok := luastdlib.StrToIntBase(str, base)
		return iv, ok, nil
	}
	if base < 2 || base > 36 {
		return 0, false, nil
	}
	i := 0
	n := len(str)
	for i < n && isNumSpace(str[i]) {
		i++
		if (i & (numeralCancelChunk - 1)) == 0 {
			if err := cancelFn(); err != nil {
				return 0, false, err
			}
		}
	}
	neg := false
	if i < n {
		if str[i] == '-' {
			neg = true
			i++
		} else if str[i] == '+' {
			i++
		}
	}
	var a uint64
	empty := true
	ub := uint64(base)
	for i < n && isNumAlnum(str[i]) {
		c := str[i]
		var d byte
		if isNumDigit(c) {
			d = c - '0'
		} else if c >= 'a' && c <= 'z' {
			d = c - 'a' + 10
		} else {
			d = c - 'A' + 10
		}
		if int(d) >= base {
			return 0, false, nil
		}
		a = a*ub + uint64(d)
		empty = false
		i++
		if (i & (numeralCancelChunk - 1)) == 0 {
			if err := cancelFn(); err != nil {
				return 0, false, err
			}
		}
	}
	for i < n && isNumSpace(str[i]) {
		i++
		if (i & (numeralCancelChunk - 1)) == 0 {
			if err := cancelFn(); err != nil {
				return 0, false, err
			}
		}
	}
	if empty || i != n {
		return 0, false, nil
	}
	if neg {
		a = 0 - a
	}
	return int64(a), true, nil
}

// strToNum converts a Lua numeral string to int64 or float64 (matching luaO_str2num),
// polling the State's execution controls every 4096 bytes for long inputs.
func (s *State) strToNum(str string) (iv int64, fv float64, isInt bool, ok bool, err error) {
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil || len(str) <= numeralCancelChunk {
		i, f, ii, o := luastdlib.StrToNum(str)
		return i, f, ii, o, nil
	}
	i := 0
	n := len(str)
	for i < n && isNumSpace(str[i]) {
		i++
		if (i & (numeralCancelChunk - 1)) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return 0, 0, false, false, cerr
			}
		}
	}
	trailingScanned := 0
	for n > i && isNumSpace(str[n-1]) {
		n--
		trailingScanned++
		if (trailingScanned & (numeralCancelChunk - 1)) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return 0, 0, false, false, cerr
			}
		}
	}
	if i == n {
		return 0, 0, false, false, nil
	}
	trimmed := str[i:n]
	if len(trimmed) <= numeralCancelChunk {
		ri, rf, rii, rok := luastdlib.StrToNum(trimmed)
		return ri, rf, rii, rok, nil
	}

	pos := 0
	neg := false
	if trimmed[pos] == '-' {
		neg = true
		pos++
	} else if trimmed[pos] == '+' {
		pos++
	}
	if pos >= len(trimmed) {
		return 0, 0, false, false, nil
	}

	// Hexadecimal integer or hexadecimal float (0x... / 0X...).
	if pos+1 < len(trimmed) && trimmed[pos] == '0' && (trimmed[pos+1] == 'x' || trimmed[pos+1] == 'X') {
		body := trimmed[pos+2:]
		var acc uint64
		empty := true
		isHexFloat := false
		for p := 0; p < len(body); p++ {
			if p > 0 && (p&(numeralCancelChunk-1)) == 0 {
				if cerr := cancelFn(); cerr != nil {
					return 0, 0, false, false, cerr
				}
			}
			c := body[p]
			if isNumXDigit(c) {
				acc = acc*16 + uint64(numHexVal(c))
				empty = false
				continue
			}
			if c == '.' || c == 'p' || c == 'P' {
				isHexFloat = true
				break
			}
			return 0, 0, false, false, nil
		}
		if !isHexFloat {
			if empty {
				return 0, 0, false, false, nil
			}
			if neg {
				acc = 0 - acc
			}
			return int64(acc), 0, true, true, nil
		}
		f, fok, ferr := parseHexFloatWithCancel(body, neg, cancelFn)
		if ferr != nil {
			return 0, 0, false, false, ferr
		}
		if !fok {
			return 0, 0, false, false, nil
		}
		return 0, f, false, true, nil
	}

	// Decimal integer or decimal float: validate syntax in one pass with cancel polling,
	// condensing arbitrarily many leading/trailing/insignificant digits before ParseFloat.
	const (
		maxIntBy10  = uint64(math.MaxInt64 / 10)
		maxIntLastD = uint64(math.MaxInt64 % 10)
		maxSigDec   = 800
		expClamp    = int64(1_000_000_000_000)
	)
	intLimit := maxIntLastD
	if neg {
		intLimit++
	}
	var intAcc uint64
	intOverflow := false
	hasDigits := false
	hasDot := false
	hasExp := false

	// Condensed decimal mantissa representation: sigDigits * 10^sigExp.
	var sigBuf [maxSigDec + 2]byte
	sigLen := 0
	sticky := false
	var sigExp int64 // exponent relative to sigBuf[:sigLen] as an integer

	p := pos
	tn := len(trimmed)
	for p < tn && isNumDigit(trimmed[p]) {
		c := trimmed[p]
		hasDigits = true
		d := uint64(c - '0')
		if !intOverflow {
			if intAcc >= maxIntBy10 && (intAcc > maxIntBy10 || d > intLimit) {
				intOverflow = true
			} else {
				intAcc = intAcc*10 + d
			}
		}
		if sigLen == 0 {
			if c != '0' {
				sigBuf[0] = c
				sigLen = 1
			}
		} else if sigLen < maxSigDec {
			sigBuf[sigLen] = c
			sigLen++
		} else {
			if sigExp < expClamp {
				sigExp++
			}
			if c != '0' {
				sticky = true
			}
		}
		p++
		if (p & (numeralCancelChunk - 1)) == 0 {
			if cerr := cancelFn(); cerr != nil {
				return 0, 0, false, false, cerr
			}
		}
	}
	if p < tn && trimmed[p] == '.' {
		hasDot = true
		p++
		for p < tn && isNumDigit(trimmed[p]) {
			c := trimmed[p]
			hasDigits = true
			if sigLen == 0 {
				if c != '0' {
					sigBuf[0] = c
					sigLen = 1
					if sigExp > -expClamp {
						sigExp--
					}
				} else if sigExp > -expClamp {
					sigExp--
				}
			} else if sigLen < maxSigDec {
				sigBuf[sigLen] = c
				sigLen++
				if sigExp > -expClamp {
					sigExp--
				}
			} else if c != '0' {
				sticky = true
			}
			p++
			if (p & (numeralCancelChunk - 1)) == 0 {
				if cerr := cancelFn(); cerr != nil {
					return 0, 0, false, false, cerr
				}
			}
		}
	}
	if !hasDigits {
		return 0, 0, false, false, nil
	}
	var expPart int64
	if p < tn && (trimmed[p] == 'e' || trimmed[p] == 'E') {
		hasExp = true
		p++
		expNeg := false
		if p < tn {
			if trimmed[p] == '-' {
				expNeg = true
				p++
			} else if trimmed[p] == '+' {
				p++
			}
		}
		expDigits := false
		for p < tn && isNumDigit(trimmed[p]) {
			expDigits = true
			if expPart < expClamp {
				expPart = expPart*10 + int64(trimmed[p]-'0')
			}
			p++
			if (p & (numeralCancelChunk - 1)) == 0 {
				if cerr := cancelFn(); cerr != nil {
					return 0, 0, false, false, cerr
				}
			}
		}
		if !expDigits {
			return 0, 0, false, false, nil
		}
		if expNeg {
			expPart = -expPart
		}
	}
	if p != tn {
		return 0, 0, false, false, nil
	}
	if !hasDot && !hasExp && !intOverflow {
		if neg {
			intAcc = 0 - intAcc
		}
		return int64(intAcc), 0, true, true, nil
	}
	if sigLen == 0 {
		z := 0.0
		if neg {
			z = math.Copysign(0, -1)
		}
		return 0, z, false, true, nil
	}
	if sticky {
		sigBuf[sigLen] = '1'
		sigLen++
		sigExp--
	}
	// Trim trailing zeros from sigBuf to keep compact form minimal.
	for sigLen > 1 && sigBuf[sigLen-1] == '0' {
		sigLen--
		sigExp++
	}
	totalExp := sigExp + expPart
	// Magnitude of value is in [10^(totalExp + sigLen - 1), 10^(totalExp + sigLen)).
	magExp := totalExp + int64(sigLen-1)
	if magExp > 310 {
		if neg {
			return 0, math.Inf(-1), false, true, nil
		}
		return 0, math.Inf(1), false, true, nil
	}
	if magExp < -330 {
		z := 0.0
		if neg {
			z = math.Copysign(0, -1)
		}
		return 0, z, false, true, nil
	}
	var compact [maxSigDec + 32]byte
	out := compact[:0]
	if neg {
		out = append(out, '-')
	}
	out = append(out, sigBuf[:sigLen]...)
	if totalExp != 0 {
		out = append(out, 'e')
		out = strconv.AppendInt(out, totalExp, 10)
	}
	f, perr := strconv.ParseFloat(string(out), 64)
	if perr != nil && !errors.Is(perr, strconv.ErrRange) {
		return 0, 0, false, false, nil
	}
	return 0, f, false, true, nil
}

func parseHexFloatWithCancel(s string, neg bool, cancelFn func() error) (float64, bool, error) {
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
		if p > 0 && (p&(numeralCancelChunk-1)) == 0 {
			if err := cancelFn(); err != nil {
				return 0, false, err
			}
		}
		c := s[p]
		if c == '.' {
			if hasDot {
				return 0, false, nil
			}
			hasDot = true
			p++
			continue
		}
		if !isNumXDigit(c) {
			break
		}
		hasDigits = true
		d := numHexVal(c)
		p++

		if sigBits == 0 {
			if d == 0 {
				if hasDot && leadingFracZeros < expClamp {
					leadingFracZeros++
				}
				continue
			}
			w := bits.Len8(d)
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
		return 0, false, nil
	}
	var expPart int64
	if p < n {
		if s[p] != 'p' && s[p] != 'P' {
			return 0, false, nil
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
		if p >= n || !isNumDigit(s[p]) {
			return 0, false, nil
		}
		for p < n && isNumDigit(s[p]) {
			if expPart < expClamp {
				expPart = expPart*10 + int64(s[p]-'0')
			}
			p++
			if (p & (numeralCancelChunk - 1)) == 0 {
				if err := cancelFn(); err != nil {
					return 0, false, err
				}
			}
		}
		if p != n {
			return 0, false, nil
		}
		if expNeg {
			expPart = -expPart
		}
	}
	if sigBits == 0 {
		return math.Float64frombits(signBit), true, nil
	}
	bitExp += expPart
	mant <<= (60 - sigBits)
	if bitExp > 1023 {
		return math.Float64frombits(signBit | (0x7FF << 52)), true, nil
	}
	if bitExp >= -1022 {
		sig := mant >> 7
		rem := mant & 0x7F
		if rem > 0x40 || (rem == 0x40 && (sticky || (sig&1) != 0)) {
			sig++
			if sig == (1 << 53) {
				sig >>= 1
				bitExp++
				if bitExp > 1023 {
					return math.Float64frombits(signBit | (0x7FF << 52)), true, nil
				}
			}
		}
		return math.Float64frombits(signBit | (uint64(bitExp+1023) << 52) | (sig & ((1 << 52) - 1))), true, nil
	}
	shift := (-1022 - bitExp) + 7
	if shift > 60 {
		return math.Float64frombits(signBit), true, nil
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
	return math.Float64frombits(signBit | sig), true, nil
}

// coerceFloat converts an int, float, or numeric string Value to float64 (matching lua_tonumberx),
// polling execution controls when parsing long numeral strings.
func (s *State) coerceFloat(v Value) (float64, bool, error) {
	if v.IsFloat() {
		return v.Float(), true, nil
	}
	if v.IsInt() {
		return float64(v.Int()), true, nil
	}
	if v.IsString() {
		iv, fv, isInt, ok, err := s.strToNum(s.asString(v).Str())
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, nil
		}
		if isInt {
			return float64(iv), true, nil
		}
		return fv, true, nil
	}
	return 0, false, nil
}

// coerceInt converts an int, exact-integer float, or numeric string Value to int64
// (matching lua_tointegerx), polling execution controls when parsing long numeral strings.
func (s *State) coerceInt(v Value) (int64, bool, error) {
	if v.IsInt() {
		return v.Int(), true, nil
	}
	if v.IsFloat() {
		i, ok := FloatToIntExact(v.Float())
		return i, ok, nil
	}
	if v.IsString() {
		iv, fv, isInt, ok, err := s.strToNum(s.asString(v).Str())
		if err != nil {
			return 0, false, err
		}
		if !ok {
			return 0, false, nil
		}
		if isInt {
			return iv, true, nil
		}
		i, ok := FloatToIntExact(fv)
		return i, ok, nil
	}
	return 0, false, nil
}

// coerceIntKind is like coerceInt, also reporting whether v was a numeric value/string
// without an exact integer representation.
func (s *State) coerceIntKind(v Value) (val int64, ok bool, nonIntNum bool, err error) {
	if v.IsInt() {
		return v.Int(), true, false, nil
	}
	if v.IsFloat() {
		i, iok := FloatToIntExact(v.Float())
		return i, iok, !iok, nil
	}
	if v.IsString() {
		iv, fv, isInt, nok, serr := s.strToNum(s.asString(v).Str())
		if serr != nil {
			return 0, false, false, serr
		}
		if !nok {
			return 0, false, false, nil
		}
		if isInt {
			return iv, true, false, nil
		}
		i, iok := FloatToIntExact(fv)
		return i, iok, !iok, nil
	}
	return 0, false, false, nil
}

// preCoerceFormatArgs pre-parses any long string argument consumed by a numeric string.format
// specifier using s.strToNum so execution controls are polled instead of running uncancellable
// luastdlib.StrToNum on huge strings. Zero cost unless execution controls are active and a
// string argument exceeds numeralCancelChunk.
func (s *State) preCoerceFormatArgs(fmtStr string, fargs []luastdlib.FormatArg) error {
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil {
		return nil
	}
	hasLongStr := false
	for i := range fargs {
		if fargs[i].Kind == luastdlib.ArgString && len(fargs[i].Str) > numeralCancelChunk {
			hasLongStr = true
			break
		}
	}
	if !hasLongStr {
		return nil
	}
	argIdx := 0
	for i := 0; i < len(fmtStr); i++ {
		if i > 0 && (i&(numeralCancelChunk-1)) == 0 {
			if err := cancelFn(); err != nil {
				return err
			}
		}
		if fmtStr[i] != '%' {
			continue
		}
		i++
		if i >= len(fmtStr) {
			return nil
		}
		if fmtStr[i] == '%' {
			continue
		}
		start := i
		for i < len(fmtStr) && i-start < 32 {
			c := fmtStr[i]
			if c == '-' || c == '+' || c == '#' || c == '0' || c == ' ' || (c >= '1' && c <= '9') || c == '.' {
				i++
				continue
			}
			break
		}
		if i >= len(fmtStr) {
			return nil
		}
		spec := fmtStr[i]
		if argIdx >= len(fargs) {
			return nil
		}
		idx := argIdx
		argIdx++
		switch spec {
		case 'd', 'i', 'o', 'u', 'x', 'X', 'c', 'a', 'A', 'e', 'E', 'f', 'g', 'G':
			if fargs[idx].Kind == luastdlib.ArgString && len(fargs[idx].Str) > numeralCancelChunk {
				iv, fv, isInt, ok, err := s.strToNum(fargs[idx].Str)
				if err != nil {
					return err
				}
				if !ok {
					fargs[idx].Str = "?"
				} else if isInt {
					fargs[idx] = luastdlib.IntArg(iv)
				} else {
					fargs[idx] = luastdlib.FloatArg(fv)
				}
			}
		case 'q', 'p', 's':
			// Non-numeric specifiers preserve original string/ToString semantics.
		default:
			// Stop before unknown specifiers so FormatWithControls reports the first syntax error.
			return nil
		}
	}
	return nil
}

// preCoercePackArgs pre-parses any long string argument consumed by a numeric string.pack
// option using s.strToNum so execution controls are polled on huge numeric strings.
func (s *State) preCoercePackArgs(fmtStr string, pargs []luastdlib.PackValue) error {
	cancelFn := s.nativeCancelFunc()
	if cancelFn == nil {
		return nil
	}
	hasLongStr := false
	for i := range pargs {
		if pargs[i].Kind == luastdlib.PackString && len(pargs[i].Str) > numeralCancelChunk {
			hasLongStr = true
			break
		}
	}
	if !hasLongStr {
		return nil
	}
	skipDigits := func(pos int) int {
		for pos < len(fmtStr) && fmtStr[pos] >= '0' && fmtStr[pos] <= '9' {
			pos++
		}
		return pos
	}
	argIdx := 0
	for i := 0; i < len(fmtStr); {
		if i > 0 && (i&(numeralCancelChunk-1)) == 0 {
			if err := cancelFn(); err != nil {
				return err
			}
		}
		opt := fmtStr[i]
		i++
		switch opt {
		case ' ', '<', '>', '=', 'x':
		case '!':
			i = skipDigits(i)
		case 'X':
			if i >= len(fmtStr) {
				return nil
			}
			al := fmtStr[i]
			i++
			if al == 'i' || al == 'I' || al == 's' {
				i = skipDigits(i)
			}
		case 'b', 'B', 'h', 'H', 'l', 'L', 'j', 'J', 'T', 'f', 'd', 'n', 'i', 'I':
			if opt == 'i' || opt == 'I' {
				i = skipDigits(i)
			}
			if argIdx >= len(pargs) {
				return nil
			}
			idx := argIdx
			argIdx++
			if pargs[idx].Kind == luastdlib.PackString && len(pargs[idx].Str) > numeralCancelChunk {
				iv, fv, isInt, ok, err := s.strToNum(pargs[idx].Str)
				if err != nil {
					return err
				}
				if !ok {
					pargs[idx] = luastdlib.PackStringVal("?")
				} else if isInt {
					pargs[idx] = luastdlib.PackIntVal(iv)
				} else {
					pargs[idx] = luastdlib.PackFloatVal(fv)
				}
			}
		case 'c', 's':
			i = skipDigits(i)
			argIdx++
		case 'z':
			argIdx++
		default:
			return nil
		}
	}
	return nil
}
