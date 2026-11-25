package luastdlib

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

const (
	fmtFlagsF = "-+#0 "
	fmtFlagsX = "-#0"
	fmtFlagsI = "-+0 "
	fmtFlagsU = "-0"
	fmtFlagsC = "-"

	fmtScanChars = "-+#0 123456789."
	maxFormatLen = 32
)

// FormatArgKind identifies the Lua type of an argument passed to Format.
type FormatArgKind uint8

const (
	ArgNil FormatArgKind = iota
	ArgBool
	ArgInt
	ArgFloat
	ArgString
	ArgPointer
)

// FormatArg represents a typed Lua value for Format.
type FormatArg struct {
	Kind     FormatArgKind
	Bool     bool
	Int      int64
	Flt      float64
	Str      string
	Ptr      uintptr // 0 represents NULL ("(null)")
	ToString func() (string, error)
}

// Convenience constructors for FormatArg.
func NilArg() FormatArg              { return FormatArg{Kind: ArgNil} }
func BoolArg(b bool) FormatArg       { return FormatArg{Kind: ArgBool, Bool: b} }
func IntArg(v int64) FormatArg       { return FormatArg{Kind: ArgInt, Int: v} }
func FloatArg(v float64) FormatArg   { return FormatArg{Kind: ArgFloat, Flt: v} }
func StringArg(s string) FormatArg   { return FormatArg{Kind: ArgString, Str: s} }
func PointerArg(p uintptr) FormatArg { return FormatArg{Kind: ArgPointer, Ptr: p} }

type parsedFormat struct {
	flagMinus bool
	flagPlus  bool
	flagHash  bool
	flagZero  bool
	flagSpace bool
	width     int
	hasPrec   bool
	prec      int
	spec      byte
}

func get2Digits(s string, pos int) (int, int) {
	val := 0
	if pos < len(s) && isDigit(s[pos]) {
		val = int(s[pos] - '0')
		pos++
		if pos < len(s) && isDigit(s[pos]) {
			val = val*10 + int(s[pos]-'0')
			pos++
		}
	}
	return val, pos
}

func checkAndParseFormat(form string, allowedFlags string, allowPrec bool) (parsedFormat, error) {
	var pf parsedFormat
	i := 1 // skip '%'
	for i < len(form) && strings.IndexByte(allowedFlags, form[i]) >= 0 {
		switch form[i] {
		case '-':
			pf.flagMinus = true
		case '+':
			pf.flagPlus = true
		case '#':
			pf.flagHash = true
		case '0':
			pf.flagZero = true
		case ' ':
			pf.flagSpace = true
		}
		i++
	}
	if i < len(form) && form[i] != '0' {
		pf.width, i = get2Digits(form, i)
		if i < len(form) && form[i] == '.' && allowPrec {
			pf.hasPrec = true
			i++
			pf.prec, i = get2Digits(form, i)
		}
	}
	if i >= len(form) || !isAlpha(form[i]) || i != len(form)-1 {
		return pf, fmt.Errorf("invalid conversion specification: '%s'", form)
	}
	pf.spec = form[i]
	return pf, nil
}

func scanFormatSpec(fmtStr string, start int) (form string, nextPos int, err error) {
	// start points right after '%'
	p := start
	for p < len(fmtStr) && strings.IndexByte(fmtScanChars, fmtStr[p]) >= 0 {
		p++
	}
	// include the conversion specifier character (if present)
	if p < len(fmtStr) {
		p++
	} else {
		specLen := (p - start) + 1
		if specLen >= maxFormatLen-10 {
			return "", p, errors.New("invalid format (too long)")
		}
		return "%" + fmtStr[start:p], p, nil
	}
	specLen := p - start
	if specLen >= maxFormatLen-10 {
		return "", p, errors.New("invalid format (too long)")
	}
	return "%" + fmtStr[start:p], p, nil
}

func formatArgToInt(arg FormatArg, argIdx int) (int64, error) {
	switch arg.Kind {
	case ArgInt:
		return arg.Int, nil
	case ArgFloat:
		if i, ok := floatToInt(arg.Flt); ok {
			return i, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'format' (number has no integer representation)", argIdx)
	case ArgString:
		if i, ok := StrToInt(arg.Str); ok {
			return i, nil
		}
		if f, ok := StrToFloat(arg.Str); ok {
			if i, ok := floatToInt(f); ok {
				return i, nil
			}
			return 0, fmt.Errorf("bad argument #%d to 'format' (number has no integer representation)", argIdx)
		}
		return 0, fmt.Errorf("bad argument #%d to 'format' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'format' (number expected)", argIdx)
	}
}

func formatArgToFloat(arg FormatArg, argIdx int) (float64, error) {
	switch arg.Kind {
	case ArgFloat:
		return arg.Flt, nil
	case ArgInt:
		return float64(arg.Int), nil
	case ArgString:
		if i, f, isInt, ok := StrToNum(arg.Str); ok {
			if isInt {
				return float64(i), nil
			}
			return f, nil
		}
		return 0, fmt.Errorf("bad argument #%d to 'format' (number expected)", argIdx)
	default:
		return 0, fmt.Errorf("bad argument #%d to 'format' (number expected)", argIdx)
	}
}

func formatArgToString(arg FormatArg) string {
	switch arg.Kind {
	case ArgString:
		return arg.Str
	case ArgInt:
		return strconv.FormatInt(arg.Int, 10)
	case ArgFloat:
		return FormatFloat(arg.Flt)
	case ArgBool:
		if arg.Bool {
			return "true"
		}
		return "false"
	case ArgNil:
		return "nil"
	case ArgPointer:
		if arg.Str != "" {
			return arg.Str
		}
		if arg.Ptr == 0 {
			return "(null)"
		}
		return "0x" + strconv.FormatUint(uint64(arg.Ptr), 16)
	default:
		return ""
	}
}

func writePadded(b *strings.Builder, prefix, body string, width int, flagMinus, flagZero bool) {
	total := len(prefix) + len(body)
	pad := width - total
	if pad <= 0 {
		b.WriteString(prefix)
		b.WriteString(body)
		return
	}
	if flagMinus {
		b.WriteString(prefix)
		b.WriteString(body)
		for i := 0; i < pad; i++ {
			b.WriteByte(' ')
		}
		return
	}
	if flagZero {
		b.WriteString(prefix)
		for i := 0; i < pad; i++ {
			b.WriteByte('0')
		}
		b.WriteString(body)
		return
	}
	for i := 0; i < pad; i++ {
		b.WriteByte(' ')
	}
	b.WriteString(prefix)
	b.WriteString(body)
}

func formatInteger(b *strings.Builder, pf parsedFormat, n int64) {
	var prefix string
	var digits string
	var mag uint64

	switch pf.spec {
	case 'd', 'i':
		if n < 0 {
			prefix = "-"
			mag = uint64(-n)
		} else {
			if pf.flagPlus {
				prefix = "+"
			} else if pf.flagSpace {
				prefix = " "
			}
			mag = uint64(n)
		}
		if mag == 0 && pf.hasPrec && pf.prec == 0 {
			digits = ""
		} else {
			digits = strconv.FormatUint(mag, 10)
		}
	case 'u':
		mag = uint64(n)
		if mag == 0 && pf.hasPrec && pf.prec == 0 {
			digits = ""
		} else {
			digits = strconv.FormatUint(mag, 10)
		}
	case 'o':
		mag = uint64(n)
		if mag == 0 && pf.hasPrec && pf.prec == 0 {
			digits = ""
		} else {
			digits = strconv.FormatUint(mag, 8)
		}
	case 'x', 'X':
		mag = uint64(n)
		if mag == 0 && pf.hasPrec && pf.prec == 0 {
			digits = ""
		} else {
			digits = strconv.FormatUint(mag, 16)
			if pf.spec == 'X' {
				digits = strings.ToUpper(digits)
			}
		}
		if pf.flagHash && mag != 0 {
			if pf.spec == 'X' {
				prefix = "0X"
			} else {
				prefix = "0x"
			}
		}
	}

	if pf.hasPrec && len(digits) < pf.prec {
		digits = strings.Repeat("0", pf.prec-len(digits)) + digits
	}

	// ISO C99: for %#o, increase precision if and only if necessary to force first digit to '0'.
	if pf.spec == 'o' && pf.flagHash {
		if len(digits) == 0 || digits[0] != '0' {
			digits = "0" + digits
		}
	}

	// ISO C99: '0' flag is ignored for integer conversions if precision is given.
	zeroPad := pf.flagZero && !pf.flagMinus && !pf.hasPrec
	writePadded(b, prefix, digits, pf.width, pf.flagMinus, zeroPad)
}

func formatSpecialFloat(b *strings.Builder, pf parsedFormat, f float64, upper bool) {
	var body string
	if math.IsNaN(f) {
		var prefix string
		if pf.flagPlus {
			prefix = "+"
		} else if pf.flagSpace {
			prefix = " "
		}
		if upper {
			body = "NAN"
		} else {
			body = "nan"
		}
		writePadded(b, prefix, body, pf.width, pf.flagMinus, false)
		return
	}
	var prefix string
	if math.Signbit(f) {
		prefix = "-"
	} else if pf.flagPlus {
		prefix = "+"
	} else if pf.flagSpace {
		prefix = " "
	}
	if upper {
		body = "INF"
	} else {
		body = "inf"
	}
	// ISO C99: inf/nan are never zero-padded.
	writePadded(b, prefix, body, pf.width, pf.flagMinus, false)
}

func formatHexFloat(b *strings.Builder, pf parsedFormat, f float64) {
	upper := pf.spec == 'A'
	if math.IsNaN(f) || math.IsInf(f, 0) {
		formatSpecialFloat(b, pf, f, upper)
		return
	}

	hexDigits := "0123456789abcdef"
	hexPrefix := "0x"
	expChar := byte('p')
	if upper {
		hexDigits = "0123456789ABCDEF"
		hexPrefix = "0X"
		expChar = 'P'
	}

	var sign string
	if math.Signbit(f) {
		sign = "-"
	} else if pf.flagPlus {
		sign = "+"
	} else if pf.flagSpace {
		sign = " "
	}
	prefix := sign + hexPrefix

	fbits := math.Float64bits(f) &^ (1 << 63)
	var body strings.Builder

	if fbits == 0 {
		body.WriteByte('0')
		if pf.hasPrec && pf.prec > 0 {
			body.WriteByte('.')
			for i := 0; i < pf.prec; i++ {
				body.WriteByte('0')
			}
		} else if pf.flagHash {
			body.WriteByte('.')
		}
		body.WriteByte(expChar)
		body.WriteString("+0")
		writePadded(b, prefix, body.String(), pf.width, pf.flagMinus, pf.flagZero && !pf.flagMinus)
		return
	}

	rawExp := int(fbits >> 52)
	rawFrac := fbits & ((1 << 52) - 1)
	var exp int
	var frac52 uint64
	if rawExp == 0 {
		// Subnormal: normalize leading 1 to bit 52 (per ISO C99 §7.21.6.1 ¶8, leading digit
		// for subnormals is implementation-defined; normalizing matches macOS/BSD C Lua 5.5.1
		// and Go's strconv.FormatFloat(..., 'x')).
		shift := bits.LeadingZeros64(rawFrac) - 11
		mant := rawFrac << shift
		exp = -1022 - shift
		frac52 = mant & ((1 << 52) - 1)
	} else {
		exp = rawExp - 1023
		frac52 = rawFrac
	}

	lead := byte(1)
	var nibbles [13]byte
	for k := 0; k < 13; k++ {
		nibbles[k] = byte((frac52 >> (48 - 4*k)) & 0xF)
	}

	var fracLen int
	var extraZeros int
	if !pf.hasPrec {
		fracLen = 13
		for fracLen > 0 && nibbles[fracLen-1] == 0 {
			fracLen--
		}
	} else if pf.prec >= 13 {
		fracLen = 13
		extraZeros = pf.prec - 13
	} else {
		fracLen = pf.prec
		remBits := uint(52 - 4*pf.prec)
		rem := frac52 & ((uint64(1) << remBits) - 1)
		half := uint64(1) << (remBits - 1)
		var lsb byte
		if pf.prec == 0 {
			lsb = lead & 1
		} else {
			lsb = nibbles[pf.prec-1] & 1
		}
		if rem > half || (rem == half && lsb != 0) {
			carry := byte(1)
			for k := pf.prec - 1; k >= 0 && carry > 0; k-- {
				v := nibbles[k] + carry
				if v >= 16 {
					nibbles[k] = 0
					carry = 1
				} else {
					nibbles[k] = v
					carry = 0
				}
			}
			if carry > 0 {
				lead++
			}
		}
	}

	body.WriteByte(hexDigits[lead])
	if fracLen > 0 || extraZeros > 0 || pf.flagHash {
		body.WriteByte('.')
		for k := 0; k < fracLen; k++ {
			body.WriteByte(hexDigits[nibbles[k]])
		}
		for k := 0; k < extraZeros; k++ {
			body.WriteByte('0')
		}
	}
	body.WriteByte(expChar)
	if exp >= 0 {
		body.WriteByte('+')
		body.WriteString(strconv.Itoa(exp))
	} else {
		body.WriteString(strconv.Itoa(exp))
	}

	writePadded(b, prefix, body.String(), pf.width, pf.flagMinus, pf.flagZero && !pf.flagMinus)
}

func formatDecFloat(b *strings.Builder, pf parsedFormat, f float64) {
	upper := pf.spec == 'E' || pf.spec == 'G'
	if math.IsNaN(f) || math.IsInf(f, 0) {
		formatSpecialFloat(b, pf, f, upper)
		return
	}

	var prefix string
	if math.Signbit(f) {
		prefix = "-"
		f = math.Copysign(f, 1)
	} else if pf.flagPlus {
		prefix = "+"
	} else if pf.flagSpace {
		prefix = " "
	}

	prec := 6
	if pf.hasPrec {
		prec = pf.prec
	}

	var body string
	switch pf.spec {
	case 'f':
		body = strconv.FormatFloat(f, 'f', prec, 64)
		if pf.flagHash && prec == 0 {
			body += "."
		}
	case 'e', 'E':
		body = strconv.FormatFloat(f, pf.spec, prec, 64)
		if pf.flagHash && prec == 0 {
			if idx := strings.IndexByte(body, pf.spec); idx >= 0 {
				body = body[:idx] + "." + body[idx:]
			}
		}
	case 'g', 'G':
		if prec == 0 {
			prec = 1
		}
		if !pf.flagHash {
			body = strconv.FormatFloat(f, pf.spec, prec, 64)
		} else {
			// Alternate form for %#g / %#G: keep trailing zeros and decimal point.
			eChar := byte('e')
			if pf.spec == 'G' {
				eChar = 'E'
			}
			// Format in %e with prec-1 fractional digits to determine rounded exponent.
			eForm := strconv.FormatFloat(f, eChar, prec-1, 64)
			eIdx := strings.IndexByte(eForm, eChar)
			expVal, _ := strconv.Atoi(eForm[eIdx+1:])
			if expVal < -4 || expVal >= prec {
				if prec == 1 {
					body = eForm[:eIdx] + "." + eForm[eIdx:]
				} else {
					body = eForm
				}
			} else {
				fPrec := prec - 1 - expVal
				body = strconv.FormatFloat(f, 'f', fPrec, 64)
				if fPrec == 0 {
					body += "."
				}
			}
		}
	}

	writePadded(b, prefix, body, pf.width, pf.flagMinus, pf.flagZero && !pf.flagMinus)
}

func quotedStringLen(s string) int {
	n := 2 // opening and closing quotes
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c == '\n' {
			n += 2
		} else if isCntrl(c) {
			if i+1 < len(s) && isDigit(s[i+1]) {
				n += 4
			} else if c >= 100 {
				n += 4
			} else if c >= 10 {
				n += 3
			} else {
				n += 2
			}
		} else {
			n++
		}
	}
	return n
}

func addQuotedString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c == '\n' {
			b.WriteByte('\\')
			b.WriteByte(c)
		} else if isCntrl(c) {
			b.WriteByte('\\')
			if i+1 < len(s) && isDigit(s[i+1]) {
				b.WriteByte('0' + c/100)
				b.WriteByte('0' + (c/10)%10)
				b.WriteByte('0' + c%10)
			} else {
				b.WriteString(strconv.Itoa(int(c)))
			}
		} else {
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

func addLiteral(b *strings.Builder, arg FormatArg, argIdx int) error {
	switch arg.Kind {
	case ArgString:
		addQuotedString(b, arg.Str)
		return nil
	case ArgInt:
		if arg.Int == math.MinInt64 {
			b.WriteString("0x8000000000000000")
		} else {
			b.WriteString(strconv.FormatInt(arg.Int, 10))
		}
		return nil
	case ArgFloat:
		f := arg.Flt
		switch {
		case math.IsInf(f, 1):
			b.WriteString("1e9999")
		case math.IsInf(f, -1):
			b.WriteString("-1e9999")
		case math.IsNaN(f):
			b.WriteString("(0/0)")
		default:
			formatHexFloat(b, parsedFormat{spec: 'a'}, f)
		}
		return nil
	case ArgBool:
		if arg.Bool {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case ArgNil:
		b.WriteString("nil")
		return nil
	default:
		return fmt.Errorf("bad argument #%d to 'format' (value has no literal form)", argIdx)
	}
}

// Format implements Lua 5.5.1 string.format(fmtStr, args...).
func Format(fmtStr string, args []FormatArg) (string, error) {
	return FormatWithLimits(fmtStr, args, nil)
}

// FormatWithLimits implements Lua 5.5.1 string.format(fmtStr, args...) while invoking
// checkSize(newLen) before growing the output buffer so callers can enforce memory limits
// before allocating large formatted results.
func FormatWithLimits(fmtStr string, args []FormatArg, checkSize func(int64) error) (string, error) {
	return FormatWithControls(fmtStr, args, checkSize, nil)
}

// FormatWithControls is like FormatWithLimits, additionally polling cancelFn periodically
// while scanning the format string.
func FormatWithControls(fmtStr string, args []FormatArg, checkSize func(int64) error, cancelFn func() error) (string, error) {
	var b strings.Builder
	checkGrow := func(add int) error {
		if checkSize == nil || add <= 0 {
			return nil
		}
		return checkSize(int64(b.Len()) + int64(add))
	}
	argIdx := 0
	i := 0
	var steps int64
	for i < len(fmtStr) {
		steps++
		if cancelFn != nil && (steps&1023) == 0 {
			if err := cancelFn(); err != nil {
				return "", err
			}
		}
		if fmtStr[i] != lEsc {
			start := i
			if cancelFn == nil {
				for i < len(fmtStr) && fmtStr[i] != lEsc {
					i++
				}
			} else {
				const litChunk = 65536
				limit := min(len(fmtStr), i+litChunk)
				for i < limit && fmtStr[i] != lEsc {
					i++
				}
				if i == limit && i < len(fmtStr) && fmtStr[i] != lEsc {
					if err := cancelFn(); err != nil {
						return "", err
					}
				}
			}
			if err := checkGrow(i - start); err != nil {
				return "", err
			}
			b.WriteString(fmtStr[start:i])
			continue
		}
		i++ // skip '%'
		if i < len(fmtStr) && fmtStr[i] == lEsc {
			if err := checkGrow(1); err != nil {
				return "", err
			}
			b.WriteByte(lEsc)
			i++
			continue
		}
		if argIdx >= len(args) {
			return "", fmt.Errorf("bad argument #%d to 'format' (no value)", argIdx+2)
		}
		arg := args[argIdx]
		argNum := argIdx + 2
		argIdx++

		form, nextPos, err := scanFormatSpec(fmtStr, i)
		if err != nil {
			return "", err
		}
		i = nextPos
		if len(form) < 2 {
			return "", fmt.Errorf("invalid conversion '%s' to 'format'", form)
		}
		spec := form[len(form)-1]

		switch spec {
		case 'c':
			pf, err := checkAndParseFormat(form, fmtFlagsC, false)
			if err != nil {
				return "", err
			}
			n, err := formatArgToInt(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(1, pf.width)); err != nil {
				return "", err
			}
			writePadded(&b, "", string([]byte{byte(n)}), pf.width, pf.flagMinus, false)

		case 'd', 'i':
			pf, err := checkAndParseFormat(form, fmtFlagsI, true)
			if err != nil {
				return "", err
			}
			n, err := formatArgToInt(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(32, max(pf.width, pf.prec+4))); err != nil {
				return "", err
			}
			formatInteger(&b, pf, n)

		case 'u':
			pf, err := checkAndParseFormat(form, fmtFlagsU, true)
			if err != nil {
				return "", err
			}
			n, err := formatArgToInt(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(32, max(pf.width, pf.prec+4))); err != nil {
				return "", err
			}
			formatInteger(&b, pf, n)

		case 'o', 'x', 'X':
			pf, err := checkAndParseFormat(form, fmtFlagsX, true)
			if err != nil {
				return "", err
			}
			n, err := formatArgToInt(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(32, max(pf.width, pf.prec+4))); err != nil {
				return "", err
			}
			formatInteger(&b, pf, n)

		case 'a', 'A':
			pf, err := checkAndParseFormat(form, fmtFlagsF, true)
			if err != nil {
				return "", err
			}
			f, err := formatArgToFloat(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(64, max(pf.width, pf.prec+32))); err != nil {
				return "", err
			}
			formatHexFloat(&b, pf, f)

		case 'f', 'e', 'E', 'g', 'G':
			pf, err := checkAndParseFormat(form, fmtFlagsF, true)
			if err != nil {
				return "", err
			}
			f, err := formatArgToFloat(arg, argNum)
			if err != nil {
				return "", err
			}
			if err := checkGrow(max(64, max(pf.width, pf.prec+32))); err != nil {
				return "", err
			}
			formatDecFloat(&b, pf, f)
			if checkSize != nil {
				if err := checkSize(int64(b.Len())); err != nil {
					return "", err
				}
			}

		case 'p':
			pf, err := checkAndParseFormat(form, fmtFlagsC, false)
			if err != nil {
				return "", err
			}
			var ps string
			if (arg.Kind == ArgPointer || arg.Kind == ArgString) && arg.Ptr != 0 {
				ps = "0x" + strconv.FormatUint(uint64(arg.Ptr), 16)
			} else {
				ps = "(null)"
			}
			if err := checkGrow(max(len(ps), pf.width)); err != nil {
				return "", err
			}
			writePadded(&b, "", ps, pf.width, pf.flagMinus, false)

		case 'q':
			if len(form) != 2 {
				return "", errors.New("specifier '%q' cannot have modifiers")
			}
			if checkSize != nil {
				est := 32
				if arg.Kind == ArgString {
					est = quotedStringLen(arg.Str)
				}
				if err := checkGrow(est); err != nil {
					return "", err
				}
			}
			if err := addLiteral(&b, arg, argNum); err != nil {
				return "", err
			}

		case 's':
			var s string
			if arg.ToString != nil {
				var err error
				s, err = arg.ToString()
				if err != nil {
					return "", err
				}
			} else {
				s = formatArgToString(arg)
			}
			if len(form) == 2 {
				if err := checkGrow(len(s)); err != nil {
					return "", err
				}
				b.WriteString(s)
			} else {
				if strings.IndexByte(s, 0) >= 0 {
					return "", fmt.Errorf("bad argument #%d to 'format' (string contains zeros)", argNum)
				}
				pf, err := checkAndParseFormat(form, fmtFlagsC, true)
				if err != nil {
					return "", err
				}
				if pf.hasPrec && pf.prec < len(s) {
					s = s[:pf.prec]
				}
				if err := checkGrow(max(len(s), pf.width)); err != nil {
					return "", err
				}
				writePadded(&b, "", s, pf.width, pf.flagMinus, false)
			}

		default:
			return "", fmt.Errorf("invalid conversion '%s' to 'format'", form)
		}
	}
	return b.String(), nil
}
