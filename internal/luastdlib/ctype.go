package luastdlib

// ASCII C-locale character classification bitmask table.
// Matches C locale ctype.h functions used by Lua 5.5.1's lstrlib.c,
// lobject.c, and lctype.c without depending on OS locale or Unicode tables.
const (
	cAlpha  uint16 = 1 << iota // A-Z a-z
	cCntrl                     // 0x00-0x1F, 0x7F
	cDigit                     // 0-9
	cGraph                     // 0x21-0x7E
	cLower                     // a-z
	cPunct                     // printable non-space non-alnum
	cSpace                     // \t \n \v \f \r ' '
	cUpper                     // A-Z
	cAlnum                     // A-Z a-z 0-9
	cXDigit                    // 0-9 A-F a-f
	cPrint                     // 0x20-0x7E
)

var asciiTable [256]uint16

func init() {
	for i := 0; i < 256; i++ {
		var m uint16
		c := byte(i)
		if c <= 0x1F || c == 0x7F {
			m |= cCntrl
		}
		if c == ' ' || (c >= '\t' && c <= '\r') {
			m |= cSpace
		}
		if c >= 'A' && c <= 'Z' {
			m |= cUpper | cAlpha | cAlnum
		}
		if c >= 'a' && c <= 'z' {
			m |= cLower | cAlpha | cAlnum
		}
		if c >= '0' && c <= '9' {
			m |= cDigit | cAlnum | cXDigit
		}
		if (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f') {
			m |= cXDigit
		}
		if c >= 0x20 && c <= 0x7E {
			m |= cPrint
		}
		if c >= 0x21 && c <= 0x7E {
			m |= cGraph
			if (m & cAlnum) == 0 {
				m |= cPunct
			}
		}
		asciiTable[i] = m
	}
}

func isAlpha(c byte) bool  { return (asciiTable[c] & cAlpha) != 0 }
func isCntrl(c byte) bool  { return (asciiTable[c] & cCntrl) != 0 }
func isDigit(c byte) bool  { return (asciiTable[c] & cDigit) != 0 }
func isGraph(c byte) bool  { return (asciiTable[c] & cGraph) != 0 }
func isLower(c byte) bool  { return (asciiTable[c] & cLower) != 0 }
func isPunct(c byte) bool  { return (asciiTable[c] & cPunct) != 0 }
func isSpace(c byte) bool  { return (asciiTable[c] & cSpace) != 0 }
func isUpper(c byte) bool  { return (asciiTable[c] & cUpper) != 0 }
func isAlnum(c byte) bool  { return (asciiTable[c] & cAlnum) != 0 }
func isXDigit(c byte) bool { return (asciiTable[c] & cXDigit) != 0 }

func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func hexValue(c byte) byte {
	if c >= '0' && c <= '9' {
		return c - '0'
	}
	return toLowerASCII(c) - 'a' + 10
}
