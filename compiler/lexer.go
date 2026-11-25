package compiler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const (
	eoz           = -1
	firstReserved = 256
)

// Token constants matching PUC-Rio Lua 5.5.1 llex.h (ORDER RESERVED).
const (
	tkAnd = firstReserved + iota
	tkBreak
	tkDo
	tkElse
	tkElseif
	tkEnd
	tkFalse
	tkFor
	tkFunction
	tkGlobal
	tkGoto
	tkIf
	tkIn
	tkLocal
	tkNil
	tkNot
	tkOr
	tkRepeat
	tkReturn
	tkThen
	tkTrue
	tkUntil
	tkWhile
	// other terminal symbols
	tkIdiv
	tkConcat
	tkDots
	tkEq
	tkGe
	tkLe
	tkNe
	tkShl
	tkShr
	tkDbColon
	tkEOS
	tkFlt
	tkInt
	tkName
	tkString
)

var tokenStrings = [...]string{
	"and", "break", "do", "else", "elseif",
	"end", "false", "for", "function", "global", "goto", "if",
	"in", "local", "nil", "not", "or", "repeat",
	"return", "then", "true", "until", "while",
	"//", "..", "...", "==", ">=", "<=", "~=",
	"<<", ">>", "::", "<eof>",
	"<number>", "<integer>", "<name>", "<string>",
}

var reservedWords = map[string]int{
	"and":      tkAnd,
	"break":    tkBreak,
	"do":       tkDo,
	"else":     tkElse,
	"elseif":   tkElseif,
	"end":      tkEnd,
	"false":    tkFalse,
	"for":      tkFor,
	"function": tkFunction,
	"global":   tkGlobal,
	"goto":     tkGoto,
	"if":       tkIf,
	"in":       tkIn,
	"local":    tkLocal,
	"nil":      tkNil,
	"not":      tkNot,
	"or":       tkOr,
	"repeat":   tkRepeat,
	"return":   tkReturn,
	"then":     tkThen,
	"true":     tkTrue,
	"until":    tkUntil,
	"while":    tkWhile,
}

type token struct {
	tok int
	r   float64
	i   int64
	s   string
}

type lexState struct {
	src        string // string fast path (r == nil)
	pos        int
	r          io.Reader // streaming ZIO path (nil on string fast path)
	rbuf       []byte    // 4 KiB chunk buffer when r != nil
	rlen       int       // valid bytes in rbuf[0:rlen]
	pendingErr error     // deferred non-EOF error from r after draining buffered bytes
	reof       bool      // true once r has returned io.EOF
	current    int       // current byte (0..255) or eoz (-1)
	linenumber int
	lastline   int
	t          token
	lookahead  token
	buff       []byte
	source     string

	compatGlobal bool
	maxRecursion int
	nCcalls      int
	checkMem     func(int64) error
	memUsed      int64

	fs  *funcState
	dyd dynData
}

func newLexState(name string, src string, cfg config) *lexState {
	ls := &lexState{
		src:          src,
		linenumber:   1,
		lastline:     1,
		source:       name,
		compatGlobal: cfg.compatGlobal,
		maxRecursion: cfg.maxRecursion,
		checkMem:     cfg.checkMem,
		buff:         make([]byte, 0, 64),
	}
	ls.lookahead.tok = tkEOS

	// Handle optional UTF-8 BOM and initial shebang / '#' comment line.
	allowBOM := true
	allowAnyHash := strings.HasPrefix(name, "@")
	allowShebang := true
	if cfg.hasSkipHeaderOpt {
		allowBOM = cfg.skipFileHeader
		allowAnyHash = cfg.skipFileHeader
		allowShebang = cfg.skipFileHeader
	}

	if allowBOM && strings.HasPrefix(ls.src, "\xef\xbb\xbf") {
		ls.pos = 3
	}
	if ls.pos < len(ls.src) && ls.src[ls.pos] == '#' {
		isShebang := ls.pos+1 < len(ls.src) && ls.src[ls.pos+1] == '!'
		if allowAnyHash || (allowShebang && isShebang) {
			for ls.pos < len(ls.src) && ls.src[ls.pos] != '\n' {
				ls.pos++
			}
			// Leave the '\n' for the lexer so line 2 stays line 2 (matching luaL_loadfilex).
		}
	}

	ls.nextChar()
	return ls
}

func newLexStateReader(name string, r io.Reader, cfg config) *lexState {
	ls := &lexState{
		r:            r,
		rbuf:         make([]byte, 4096),
		linenumber:   1,
		lastline:     1,
		source:       name,
		compatGlobal: cfg.compatGlobal,
		maxRecursion: cfg.maxRecursion,
		checkMem:     cfg.checkMem,
		buff:         make([]byte, 0, 64),
	}
	ls.lookahead.tok = tkEOS

	allowBOM := true
	allowAnyHash := strings.HasPrefix(name, "@")
	allowShebang := true
	if cfg.hasSkipHeaderOpt {
		allowBOM = cfg.skipFileHeader
		allowAnyHash = cfg.skipFileHeader
		allowShebang = cfg.skipFileHeader
	}

	if allowBOM {
		ls.ensureStreamBytes(3)
		if ls.rlen-ls.pos >= 3 && ls.rbuf[ls.pos] == 0xef && ls.rbuf[ls.pos+1] == 0xbb && ls.rbuf[ls.pos+2] == 0xbf {
			ls.pos += 3
		}
	}
	if allowAnyHash || allowShebang {
		ls.ensureStreamBytes(2)
		if ls.pos < ls.rlen && ls.rbuf[ls.pos] == '#' {
			isShebang := ls.pos+1 < ls.rlen && ls.rbuf[ls.pos+1] == '!'
			if allowAnyHash || (allowShebang && isShebang) {
				ls.nextChar()
				for ls.current != eoz && ls.current != '\n' {
					ls.nextChar()
				}
				return ls
			}
		}
	}

	ls.nextChar()
	return ls
}

func (ls *lexState) accountMem(delta int64) {
	if ls.checkMem == nil || delta <= 0 {
		return
	}
	if err := ls.checkMem(ls.memUsed + delta); err != nil {
		panic(compilePanic{err: err})
	}
	ls.memUsed += delta
}

// ensureStreamBytes fills ls.rbuf until at least need unread bytes are buffered or EOF is reached.
func (ls *lexState) ensureStreamBytes(need int) {
	emptyReads := 0
	for ls.rlen-ls.pos < need && !ls.reof {
		if ls.pos > 0 {
			rem := copy(ls.rbuf, ls.rbuf[ls.pos:ls.rlen])
			ls.pos = 0
			ls.rlen = rem
		}
		if ls.pendingErr != nil {
			if ls.rlen > 0 {
				return
			}
			err := ls.pendingErr
			ls.pendingErr = nil
			panic(compilePanic{err: err})
		}
		n, err := ls.r.Read(ls.rbuf[ls.rlen:])
		if n > 0 {
			ls.rlen += n
			emptyReads = 0
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				ls.reof = true
				return
			}
			if ls.rlen-ls.pos > 0 {
				ls.pendingErr = err
				return
			}
			panic(compilePanic{err: err})
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				panic(compilePanic{err: io.ErrNoProgress})
			}
		}
	}
}

func (ls *lexState) nextChar() int {
	if ls.r == nil {
		if ls.pos >= len(ls.src) {
			ls.current = eoz
		} else {
			ls.current = int(ls.src[ls.pos])
			ls.pos++
		}
		return ls.current
	}
	return ls.nextCharStream()
}

func (ls *lexState) nextCharStream() int {
	if ls.pos < ls.rlen {
		ls.current = int(ls.rbuf[ls.pos])
		ls.pos++
		return ls.current
	}
	if ls.pendingErr != nil {
		err := ls.pendingErr
		ls.pendingErr = nil
		panic(compilePanic{err: err})
	}
	if ls.reof {
		ls.current = eoz
		return eoz
	}
	emptyReads := 0
	for {
		n, err := ls.r.Read(ls.rbuf)
		if n > 0 {
			ls.rlen = n
			ls.pos = 1
			ls.current = int(ls.rbuf[0])
			if err != nil {
				if errors.Is(err, io.EOF) {
					ls.reof = true
				} else {
					ls.pendingErr = err
				}
			}
			return ls.current
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				ls.reof = true
				ls.current = eoz
				return eoz
			}
			panic(compilePanic{err: err})
		}
		emptyReads++
		if emptyReads >= 100 {
			panic(compilePanic{err: io.ErrNoProgress})
		}
	}
}

func (ls *lexState) currIsNewline() bool {
	return ls.current == '\n' || ls.current == '\r'
}

func (ls *lexState) save(c int) {
	if ls.checkMem != nil && len(ls.buff) == cap(ls.buff) && cap(ls.buff) >= 4096 {
		if err := ls.checkMem(ls.memUsed + int64(cap(ls.buff))*2); err != nil {
			panic(compilePanic{err: err})
		}
	}
	ls.buff = append(ls.buff, byte(c))
}

func (ls *lexState) saveAndNext() {
	ls.save(ls.current)
	ls.nextChar()
}

func (ls *lexState) buffRemove(n int) {
	ls.buff = ls.buff[:len(ls.buff)-n]
}

func (ls *lexState) resetBuffer() {
	ls.buff = ls.buff[:0]
}

func (ls *lexState) token2Str(tok int) string {
	if tok < firstReserved {
		if isPrint(tok) {
			return fmt.Sprintf("'%c'", tok)
		}
		return fmt.Sprintf("'<\\%d>'", tok)
	}
	s := tokenStrings[tok-firstReserved]
	if tok < tkEOS {
		return fmt.Sprintf("'%s'", s)
	}
	return s
}

func (ls *lexState) txtToken(tok int) string {
	switch tok {
	case tkName, tkString, tkFlt, tkInt:
		b := ls.buff
		if idx := bytes.IndexByte(b, 0); idx >= 0 {
			b = b[:idx]
		}
		return fmt.Sprintf("'%s'", string(b))
	default:
		return ls.token2Str(tok)
	}
}

func (ls *lexState) lexError(msg string, tok int) {
	near := ""
	if tok != 0 {
		near = ls.txtToken(tok)
	}
	panic(compilePanic{err: &SyntaxError{
		Source: ls.source,
		Line:   ls.linenumber,
		Msg:    msg,
		Near:   near,
	}})
}

func (ls *lexState) syntaxError(msg string) {
	ls.lexError(msg, ls.t.tok)
}

func (ls *lexState) semError(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ls.t.tok = 0
	ls.linenumber = ls.lastline
	ls.syntaxError(msg)
}

func (ls *lexState) incLineNumber() {
	old := ls.current
	ls.nextChar()
	if ls.currIsNewline() && ls.current != old {
		ls.nextChar()
	}
	ls.linenumber++
	if ls.linenumber >= math.MaxInt32 {
		ls.lexError("chunk has too many lines", 0)
	}
}

func (ls *lexState) checkNext1(c int) bool {
	if ls.current == c {
		ls.nextChar()
		return true
	}
	return false
}

func (ls *lexState) checkNext2(a, b int) bool {
	if ls.current == a || ls.current == b {
		ls.saveAndNext()
		return true
	}
	return false
}

func (ls *lexState) readNumeral(tok *token) int {
	expoA, expoB := int('E'), int('e')
	first := ls.current
	ls.saveAndNext()
	if first == '0' && ls.checkNext2('x', 'X') {
		expoA, expoB = int('P'), int('p')
	}
	for {
		if ls.checkNext2(expoA, expoB) {
			ls.checkNext2('-', '+')
		} else if isXDigit(ls.current) || ls.current == '.' {
			ls.saveAndNext()
		} else {
			break
		}
	}
	if isAlpha(ls.current) {
		ls.saveAndNext() // force an error
	}
	iv, fv, isInt, ok := str2Num(string(ls.buff))
	if !ok {
		ls.lexError("malformed number", tkFlt)
	}
	if isInt {
		tok.i = iv
		return tkInt
	}
	tok.r = fv
	return tkFlt
}

func (ls *lexState) skipSep() int {
	count := 0
	s := ls.current
	ls.saveAndNext()
	for ls.current == '=' {
		ls.saveAndNext()
		count++
	}
	if ls.current == s {
		return count + 2
	}
	if count == 0 {
		return 1
	}
	return 0
}

func (ls *lexState) readLongString(tok *token, sep int) {
	line := ls.linenumber
	ls.saveAndNext() // skip 2nd '['
	if ls.currIsNewline() {
		ls.incLineNumber()
	}
	for {
		switch ls.current {
		case eoz:
			what := "comment"
			if tok != nil {
				what = "string"
			}
			ls.lexError(fmt.Sprintf("unfinished long %s (starting at line %d)", what, line), tkEOS)
		case ']':
			if ls.skipSep() == sep {
				ls.saveAndNext() // skip 2nd ']'
				if tok != nil {
					tok.s = string(ls.buff[sep : len(ls.buff)-sep])
				}
				return
			}
		case '\n', '\r':
			ls.save('\n')
			ls.incLineNumber()
			if tok == nil {
				ls.resetBuffer()
			}
		default:
			if tok != nil {
				ls.saveAndNext()
			} else {
				ls.nextChar()
			}
		}
	}
}

func (ls *lexState) escCheck(cond bool, msg string) {
	if !cond {
		if ls.current != eoz {
			ls.saveAndNext()
		}
		ls.lexError(msg, tkString)
	}
}

func (ls *lexState) getHexa() int {
	ls.saveAndNext()
	ls.escCheck(isXDigit(ls.current), "hexadecimal digit expected")
	return hexValue(ls.current)
}

func (ls *lexState) readHexaEsc() int {
	r := ls.getHexa()
	r = (r << 4) + ls.getHexa()
	ls.buffRemove(2)
	return r
}

func (ls *lexState) readUTF8Esc() uint32 {
	i := 4
	ls.saveAndNext() // skip 'u'
	ls.escCheck(ls.current == '{', "missing '{'")
	r := uint32(ls.getHexa())
	for {
		ls.saveAndNext()
		if !isXDigit(ls.current) {
			break
		}
		i++
		ls.escCheck(r <= (0x7FFFFFFF>>4), "UTF-8 value too large")
		r = (r << 4) + uint32(hexValue(ls.current))
	}
	ls.escCheck(ls.current == '}', "missing '}'")
	ls.nextChar() // skip '}'
	ls.buffRemove(i)
	return r
}

func (ls *lexState) utf8Esc() {
	bytes, _ := utf8Esc(ls.readUTF8Esc())
	for _, b := range bytes {
		ls.save(int(b))
	}
}

func (ls *lexState) readDecEsc() int {
	r := 0
	i := 0
	for ; i < 3 && isDigit(ls.current); i++ {
		r = 10*r + (ls.current - '0')
		ls.saveAndNext()
	}
	ls.escCheck(r <= 255, "decimal escape too large")
	ls.buffRemove(i)
	return r
}

func (ls *lexState) readString(del int, tok *token) {
	ls.saveAndNext() // keep delimiter
	for ls.current != del {
		switch ls.current {
		case eoz:
			ls.lexError("unfinished string", tkEOS)
		case '\n', '\r':
			ls.lexError("unfinished string", tkString)
		case '\\':
			var c int
			ls.saveAndNext() // keep '\\'
			switch ls.current {
			case 'a':
				c = '\a'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'b':
				c = '\b'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'f':
				c = '\f'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'n':
				c = '\n'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'r':
				c = '\r'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 't':
				c = '\t'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'v':
				c = '\v'
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'x':
				c = ls.readHexaEsc()
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case 'u':
				ls.utf8Esc()
			case '\n', '\r':
				ls.incLineNumber()
				c = '\n'
				ls.buffRemove(1)
				ls.save(c)
			case '\\', '"', '\'':
				c = ls.current
				ls.nextChar()
				ls.buffRemove(1)
				ls.save(c)
			case eoz:
				// will raise error on next loop iteration
			case 'z':
				ls.buffRemove(1) // remove '\\'
				ls.nextChar()    // skip 'z'
				for isSpace(ls.current) {
					if ls.currIsNewline() {
						ls.incLineNumber()
					} else {
						ls.nextChar()
					}
				}
			default:
				ls.escCheck(isDigit(ls.current), "invalid escape sequence")
				c = ls.readDecEsc()
				ls.buffRemove(1)
				ls.save(c)
			}
		default:
			ls.saveAndNext()
		}
	}
	ls.saveAndNext() // skip closing delimiter
	tok.s = string(ls.buff[1 : len(ls.buff)-1])
}

func (ls *lexState) llex(tok *token) int {
	ls.resetBuffer()
	for {
		switch ls.current {
		case '\n', '\r':
			ls.incLineNumber()
		case ' ', '\f', '\t', '\v':
			ls.nextChar()
		case '-':
			ls.nextChar()
			if ls.current != '-' {
				return '-'
			}
			// comment
			ls.nextChar()
			if ls.current == '[' {
				sep := ls.skipSep()
				ls.resetBuffer()
				if sep >= 2 {
					ls.readLongString(nil, sep)
					ls.resetBuffer()
					continue
				}
			}
			// short comment
			for !ls.currIsNewline() && ls.current != eoz {
				ls.nextChar()
			}
		case '[':
			sep := ls.skipSep()
			if sep >= 2 {
				ls.readLongString(tok, sep)
				return tkString
			} else if sep == 0 {
				ls.lexError("invalid long string delimiter", tkString)
			}
			return '['
		case '=':
			ls.nextChar()
			if ls.checkNext1('=') {
				return tkEq
			}
			return '='
		case '<':
			ls.nextChar()
			if ls.checkNext1('=') {
				return tkLe
			}
			if ls.checkNext1('<') {
				return tkShl
			}
			return '<'
		case '>':
			ls.nextChar()
			if ls.checkNext1('=') {
				return tkGe
			}
			if ls.checkNext1('>') {
				return tkShr
			}
			return '>'
		case '/':
			ls.nextChar()
			if ls.checkNext1('/') {
				return tkIdiv
			}
			return '/'
		case '~':
			ls.nextChar()
			if ls.checkNext1('=') {
				return tkNe
			}
			return '~'
		case ':':
			ls.nextChar()
			if ls.checkNext1(':') {
				return tkDbColon
			}
			return ':'
		case '"', '\'':
			ls.readString(ls.current, tok)
			return tkString
		case '.':
			ls.saveAndNext()
			if ls.checkNext1('.') {
				if ls.checkNext1('.') {
					return tkDots
				}
				return tkConcat
			}
			if !isDigit(ls.current) {
				return '.'
			}
			return ls.readNumeral(tok)
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return ls.readNumeral(tok)
		case eoz:
			return tkEOS
		default:
			if isAlpha(ls.current) {
				for {
					ls.saveAndNext()
					if !isAlnum(ls.current) {
						break
					}
				}
				name := string(ls.buff)
				if kw, ok := reservedWords[name]; ok {
					if kw != tkGlobal || !ls.compatGlobal {
						return kw
					}
				}
				tok.s = name
				return tkName
			}
			c := ls.current
			ls.nextChar()
			return c
		}
	}
}

func (ls *lexState) next() {
	ls.lastline = ls.linenumber
	if ls.lookahead.tok != tkEOS {
		ls.t = ls.lookahead
		ls.lookahead.tok = tkEOS
	} else {
		ls.t.tok = ls.llex(&ls.t)
	}
}

func (ls *lexState) lookAhead() int {
	ls.lookahead.tok = ls.llex(&ls.lookahead)
	return ls.lookahead.tok
}
