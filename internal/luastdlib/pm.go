package luastdlib

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxCaptures matches LUA_MAXCAPTURES in Lua 5.5.1 lstrlib.c.
	MaxCaptures = 32
	// MaxCCalls matches MAXCCALLS in Lua 5.5.1 lstrlib.c.
	MaxCCalls = 200

	capUnfinished = -1
	capPosition   = -2

	lEsc     = '%'
	specials = "^$*+?.([%-"
)

var (
	errPatternTooComplex  = errors.New("pattern too complex")
	errTooManyCaptures    = errors.New("too many captures")
	errInvalidCapture     = errors.New("invalid pattern capture")
	errUnfinishedCapture  = errors.New("unfinished capture")
	errMalformedEndsEsc   = errors.New("malformed pattern (ends with '%')")
	errMalformedMissingBr = errors.New("malformed pattern (missing ']')")
	errMalformedBalance   = errors.New("malformed pattern (missing arguments to '%b')")
	errMalformedFrontier  = errors.New("missing '[' after '%f' in pattern")
	errInvalidReplEsc     = errors.New("invalid use of '%' in replacement string")
)

// CaptureVal represents a Lua pattern capture: either a substring capture
// or a 1-based byte position capture produced by "()".
type CaptureVal struct {
	IsPos bool
	Str   string
	Pos   int64
}

type pmCapture struct {
	init int
	len  int // >= 0 substring length, capUnfinished, or capPosition
}

type matchState struct {
	src         string
	pat         string
	matchDepth  int
	level       int
	steps       uint32
	checkCancel func() error
	capture     [MaxCaptures]pmCapture
}

func (ms *matchState) prep(src, pat string) {
	ms.src = src
	ms.pat = pat
}

func (ms *matchState) reprep() {
	ms.matchDepth = MaxCCalls
	ms.level = 0
}

// posrelatI translates a Lua 1-based relative initial position (negative counts
// back from end), clipping the result to [1, inf). Mirrors posrelatI in lstrlib.c.
func posrelatI(pos int64, l int) int {
	if pos > 0 {
		if pos > int64(l)+1 {
			return l + 2
		}
		return int(pos)
	}
	if pos == 0 {
		return 1
	}
	if pos < -int64(l) {
		return 1
	}
	return l + int(pos) + 1
}

func (ms *matchState) checkCapture(ch byte) (int, error) {
	l := int(ch) - '1'
	if l < 0 || l >= ms.level || ms.capture[l].len == capUnfinished {
		return 0, fmt.Errorf("invalid capture index %%%d", l+1)
	}
	return l, nil
}

func (ms *matchState) captureToClose() (int, error) {
	for level := ms.level - 1; level >= 0; level-- {
		if ms.capture[level].len == capUnfinished {
			return level, nil
		}
	}
	return 0, errInvalidCapture
}

func (ms *matchState) classEnd(p int) (int, error) {
	c := ms.pat[p]
	p++
	switch c {
	case lEsc:
		if p == len(ms.pat) {
			return 0, errMalformedEndsEsc
		}
		return p + 1, nil
	case '[':
		if p < len(ms.pat) && ms.pat[p] == '^' {
			p++
		}
		for {
			if p == len(ms.pat) {
				return 0, errMalformedMissingBr
			}
			ch := ms.pat[p]
			p++
			if ch == lEsc && p < len(ms.pat) {
				p++ // skip escapes (e.g. '%]')
			}
			if p < len(ms.pat) && ms.pat[p] == ']' {
				break
			}
		}
		return p + 1, nil
	default:
		return p, nil
	}
}

func matchClass(c, cl byte) bool {
	var res bool
	switch toLowerASCII(cl) {
	case 'a':
		res = isAlpha(c)
	case 'c':
		res = isCntrl(c)
	case 'd':
		res = isDigit(c)
	case 'g':
		res = isGraph(c)
	case 'l':
		res = isLower(c)
	case 'p':
		res = isPunct(c)
	case 's':
		res = isSpace(c)
	case 'u':
		res = isUpper(c)
	case 'w':
		res = isAlnum(c)
	case 'x':
		res = isXDigit(c)
	case 'z':
		res = (c == 0)
	default:
		return cl == c
	}
	if isLower(cl) {
		return res
	}
	return !res
}

func (ms *matchState) pollCancel() error {
	if ms.checkCancel != nil {
		ms.steps++
		if ms.steps&0x3ff == 0 {
			return ms.checkCancel()
		}
	}
	return nil
}

func (ms *matchState) matchBracketClass(c byte, p, ec int) (bool, error) {
	sig := true
	if ms.pat[p+1] == '^' {
		sig = false
		p++ // skip '^'
	}
	for p++; p < ec; p++ {
		if err := ms.pollCancel(); err != nil {
			return false, err
		}
		if ms.pat[p] == lEsc {
			p++
			if matchClass(c, ms.pat[p]) {
				return sig, nil
			}
		} else if p+2 < ec && ms.pat[p+1] == '-' {
			lo := ms.pat[p]
			hi := ms.pat[p+2]
			p += 2
			if lo <= c && c <= hi {
				return sig, nil
			}
		} else if ms.pat[p] == c {
			return sig, nil
		}
	}
	return !sig, nil
}

func (ms *matchState) singleMatch(s, p, ep int) (bool, error) {
	if s >= len(ms.src) {
		return false, nil
	}
	c := ms.src[s]
	switch ms.pat[p] {
	case '.':
		return true, nil
	case lEsc:
		return matchClass(c, ms.pat[p+1]), nil
	case '[':
		return ms.matchBracketClass(c, p, ep-1)
	default:
		return ms.pat[p] == c, nil
	}
}

func (ms *matchState) matchBalance(s, p int) (int, error) {
	if p >= len(ms.pat)-1 {
		return -1, errMalformedBalance
	}
	if s >= len(ms.src) || ms.src[s] != ms.pat[p] {
		return -1, nil
	}
	b := ms.pat[p]
	e := ms.pat[p+1]
	cont := 1
	for s++; s < len(ms.src); s++ {
		if err := ms.pollCancel(); err != nil {
			return -1, err
		}
		if ms.src[s] == e {
			cont--
			if cont == 0 {
				return s + 1, nil
			}
		} else if ms.src[s] == b {
			cont++
		}
	}
	return -1, nil
}

func (ms *matchState) maxExpand(s, p, ep int) (int, error) {
	i := 0
	for {
		if err := ms.pollCancel(); err != nil {
			return -1, err
		}
		ok, err := ms.singleMatch(s+i, p, ep)
		if err != nil {
			return -1, err
		}
		if !ok {
			break
		}
		i++
	}
	for i >= 0 {
		res, err := ms.match(s+i, ep+1)
		if err != nil {
			return -1, err
		}
		if res >= 0 {
			return res, nil
		}
		i--
	}
	return -1, nil
}

func (ms *matchState) minExpand(s, p, ep int) (int, error) {
	for {
		res, err := ms.match(s, ep+1)
		if err != nil {
			return -1, err
		}
		if res >= 0 {
			return res, nil
		}
		ok, err := ms.singleMatch(s, p, ep)
		if err != nil {
			return -1, err
		}
		if ok {
			s++
		} else {
			return -1, nil
		}
	}
}

func (ms *matchState) startCapture(s, p, what int) (int, error) {
	level := ms.level
	if level >= MaxCaptures {
		return -1, errTooManyCaptures
	}
	ms.capture[level].init = s
	ms.capture[level].len = what
	ms.level = level + 1
	res, err := ms.match(s, p)
	if err != nil {
		ms.level--
		return -1, err
	}
	if res < 0 {
		ms.level--
	}
	return res, nil
}

func (ms *matchState) endCapture(s, p int) (int, error) {
	l, err := ms.captureToClose()
	if err != nil {
		return -1, err
	}
	ms.capture[l].len = s - ms.capture[l].init
	res, err := ms.match(s, p)
	if err != nil {
		ms.capture[l].len = capUnfinished
		return -1, err
	}
	if res < 0 {
		ms.capture[l].len = capUnfinished
	}
	return res, nil
}

func (ms *matchState) matchCapture(s int, ch byte) (int, error) {
	l, err := ms.checkCapture(ch)
	if err != nil {
		return -1, err
	}
	capLen := ms.capture[l].len
	if capLen < 0 {
		// Position capture has CAP_POSITION (-2); in Lua C cast to size_t is huge so never matches.
		return -1, nil
	}
	init := ms.capture[l].init
	if len(ms.src)-s >= capLen && ms.src[init:init+capLen] == ms.src[s:s+capLen] {
		return s + capLen, nil
	}
	return -1, nil
}

func (ms *matchState) match(s, p int) (int, error) {
	if ms.matchDepth == 0 {
		return -1, errPatternTooComplex
	}
	ms.matchDepth--
	defer func() { ms.matchDepth++ }()

	for {
		if err := ms.pollCancel(); err != nil {
			return -1, err
		}
		if p == len(ms.pat) {
			return s, nil
		}
		switch ms.pat[p] {
		case '(':
			if p+1 < len(ms.pat) && ms.pat[p+1] == ')' {
				return ms.startCapture(s, p+2, capPosition)
			}
			return ms.startCapture(s, p+1, capUnfinished)
		case ')':
			return ms.endCapture(s, p+1)
		case '$':
			if p+1 != len(ms.pat) {
				goto dflt
			}
			if s == len(ms.src) {
				return s, nil
			}
			return -1, nil
		case lEsc:
			if p+1 == len(ms.pat) {
				goto dflt
			}
			switch ms.pat[p+1] {
			case 'b':
				ns, err := ms.matchBalance(s, p+2)
				if err != nil {
					return -1, err
				}
				if ns < 0 {
					return -1, nil
				}
				s = ns
				p += 4
				continue
			case 'f':
				p += 2
				if p >= len(ms.pat) || ms.pat[p] != '[' {
					return -1, errMalformedFrontier
				}
				ep, err := ms.classEnd(p)
				if err != nil {
					return -1, err
				}
				var prev, curr byte
				if s > 0 {
					prev = ms.src[s-1]
				}
				if s < len(ms.src) {
					curr = ms.src[s]
				}
				mPrev, err := ms.matchBracketClass(prev, p, ep-1)
				if err != nil {
					return -1, err
				}
				if !mPrev {
					mCurr, err := ms.matchBracketClass(curr, p, ep-1)
					if err != nil {
						return -1, err
					}
					if mCurr {
						p = ep
						continue
					}
				}
				return -1, nil
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				ns, err := ms.matchCapture(s, ms.pat[p+1])
				if err != nil {
					return -1, err
				}
				if ns < 0 {
					return -1, nil
				}
				s = ns
				p += 2
				continue
			default:
				goto dflt
			}
		}

	dflt:
		ep, err := ms.classEnd(p)
		if err != nil {
			return -1, err
		}
		var suffix byte
		if ep < len(ms.pat) {
			suffix = ms.pat[ep]
		}
		matched, err := ms.singleMatch(s, p, ep)
		if err != nil {
			return -1, err
		}
		if !matched {
			if suffix == '*' || suffix == '?' || suffix == '-' {
				p = ep + 1
				continue
			}
			return -1, nil
		}
		switch suffix {
		case '?':
			res, err := ms.match(s+1, ep+1)
			if err != nil {
				return -1, err
			}
			if res >= 0 {
				return res, nil
			}
			p = ep + 1
			continue
		case '+':
			return ms.maxExpand(s+1, p, ep)
		case '*':
			return ms.maxExpand(s, p, ep)
		case '-':
			return ms.minExpand(s, p, ep)
		default:
			s++
			p = ep
			continue
		}
	}
}

func (ms *matchState) getOneCapture(i, s, e int) (CaptureVal, error) {
	if i >= ms.level {
		if i != 0 {
			return CaptureVal{}, fmt.Errorf("invalid capture index %%%d", i+1)
		}
		return CaptureVal{Str: ms.src[s:e]}, nil
	}
	capLen := ms.capture[i].len
	init := ms.capture[i].init
	if capLen == capUnfinished {
		return CaptureVal{}, errUnfinishedCapture
	}
	if capLen == capPosition {
		return CaptureVal{IsPos: true, Pos: int64(init) + 1}, nil
	}
	return CaptureVal{Str: ms.src[init : init+capLen]}, nil
}

func (ms *matchState) collectCaptures(s, e int, includeWhole bool) ([]CaptureVal, error) {
	nlevels := ms.level
	if nlevels == 0 && includeWhole {
		nlevels = 1
	}
	if nlevels == 0 {
		return nil, nil
	}
	res := make([]CaptureVal, nlevels)
	for i := 0; i < nlevels; i++ {
		cv, err := ms.getOneCapture(i, s, e)
		if err != nil {
			return nil, err
		}
		res[i] = cv
	}
	return res, nil
}

// Find implements Lua 5.5.1 string.find(s, pattern [, init [, plain]]).
// init is a 1-based Lua index (negative counts from end; 0 clips to 1).
// Returns 1-based inclusive start and end byte positions, any explicit captures,
// whether a match was found, and any pattern error.
func Find(s, pattern string, init int, plain bool) (start, end int, captures []CaptureVal, found bool, err error) {
	return FindWithCancel(s, pattern, init, plain, nil)
}

// FindWithCancel is Find with an optional cooperative cancellation callback polled during backtracking.
func FindWithCancel(s, pattern string, init int, plain bool, checkCancel func() error) (start, end int, captures []CaptureVal, found bool, err error) {
	init0 := posrelatI(int64(init), len(s)) - 1
	if init0 > len(s) {
		return 0, 0, nil, false, nil
	}
	if plain || !strings.ContainsAny(pattern, specials) {
		idx := strings.Index(s[init0:], pattern)
		if idx < 0 {
			return 0, 0, nil, false, nil
		}
		st := init0 + idx
		return st + 1, st + len(pattern), nil, true, nil
	}
	var ms matchState
	ms.checkCancel = checkCancel
	anchor := len(pattern) > 0 && pattern[0] == '^'
	p := pattern
	if anchor {
		p = pattern[1:]
	}
	ms.prep(s, p)
	s1 := init0
	for {
		ms.reprep()
		res, merr := ms.match(s1, 0)
		if merr != nil {
			return 0, 0, nil, false, merr
		}
		if res >= 0 {
			caps, cerr := ms.collectCaptures(0, 0, false)
			if cerr != nil {
				return 0, 0, nil, false, cerr
			}
			return s1 + 1, res, caps, true, nil
		}
		if s1 >= len(s) || anchor {
			break
		}
		s1++
	}
	return 0, 0, nil, false, nil
}

// Match implements Lua 5.5.1 string.match(s, pattern [, init]).
// init is a 1-based Lua index (negative counts from end; 0 clips to 1).
// If the pattern specifies no captures, the single returned capture is the whole match.
func Match(s, pattern string, init int) (captures []CaptureVal, found bool, err error) {
	return MatchWithCancel(s, pattern, init, nil)
}

// MatchWithCancel is Match with an optional cooperative cancellation callback polled during backtracking.
func MatchWithCancel(s, pattern string, init int, checkCancel func() error) (captures []CaptureVal, found bool, err error) {
	init0 := posrelatI(int64(init), len(s)) - 1
	if init0 > len(s) {
		return nil, false, nil
	}
	var ms matchState
	ms.checkCancel = checkCancel
	anchor := len(pattern) > 0 && pattern[0] == '^'
	p := pattern
	if anchor {
		p = pattern[1:]
	}
	ms.prep(s, p)
	s1 := init0
	for {
		ms.reprep()
		res, merr := ms.match(s1, 0)
		if merr != nil {
			return nil, false, merr
		}
		if res >= 0 {
			caps, cerr := ms.collectCaptures(s1, res, true)
			if cerr != nil {
				return nil, false, cerr
			}
			return caps, true, nil
		}
		if s1 >= len(s) || anchor {
			break
		}
		s1++
	}
	return nil, false, nil
}

// GMatchIter iterates over all matches of a Lua pattern in a subject string.
type GMatchIter struct {
	srcPos    int
	lastMatch int
	ms        matchState
}

// GMatch implements Lua 5.5.1 string.gmatch(s, pattern [, init]).
// Note: like Lua 5.4/5.5, leading '^' is treated literally rather than as an anchor.
func GMatch(s, pattern string, init int) *GMatchIter {
	return GMatchWithCancel(s, pattern, init, nil)
}

// GMatchWithCancel is GMatch with an optional cooperative cancellation callback polled during backtracking.
func GMatchWithCancel(s, pattern string, init int, checkCancel func() error) *GMatchIter {
	init0 := posrelatI(int64(init), len(s)) - 1
	if init0 > len(s) {
		init0 = len(s) + 1
	}
	it := &GMatchIter{
		srcPos:    init0,
		lastMatch: -1,
	}
	it.ms.checkCancel = checkCancel
	it.ms.prep(s, pattern)
	return it
}

// Next advances the iterator to the next match.
func (it *GMatchIter) Next() (captures []CaptureVal, ok bool, err error) {
	for src := it.srcPos; src <= len(it.ms.src); src++ {
		it.ms.reprep()
		e, merr := it.ms.match(src, 0)
		if merr != nil {
			return nil, false, merr
		}
		if e >= 0 && e != it.lastMatch {
			it.srcPos = e
			it.lastMatch = e
			caps, cerr := it.ms.collectCaptures(src, e, true)
			if cerr != nil {
				return nil, false, cerr
			}
			return caps, true, nil
		}
	}
	it.srcPos = len(it.ms.src) + 1
	return nil, false, nil
}

// GSub implements Lua 5.5.1 string.gsub with a function replacement callback.
// For each match, repl receives all captures (or a single capture holding the
// whole match if the pattern has no captures). If repl returns changed=false,
// the original matched text is preserved.
// If maxRepl < 0, all matches are replaced (up to len(s)+1); if maxRepl == 0,
// no replacements are performed.
func GSub(s, pattern string, repl func(captures []CaptureVal) (string, bool, error), maxRepl int) (string, int, error) {
	return GSubWithCancel(s, pattern, repl, maxRepl, nil)
}

func GSubWithCancel(s, pattern string, repl func(captures []CaptureVal) (string, bool, error), maxRepl int, checkCancel func() error) (string, int, error) {
	return GSubWithLimits(s, pattern, repl, maxRepl, checkCancel, nil)
}

func GSubWithLimits(s, pattern string, repl func(captures []CaptureVal) (string, bool, error), maxRepl int, checkCancel func() error, checkSize func(int64) error) (string, int, error) {
	return gsubCore(s, pattern, maxRepl, checkCancel, checkSize, func(ms *matchState, b *strings.Builder, start, end int) (bool, error) {
		caps, err := ms.collectCaptures(start, end, true)
		if err != nil {
			return false, err
		}
		rep, ok, err := repl(caps)
		if err != nil {
			return false, err
		}
		if !ok {
			if err := writeCheckedString(b, s[start:end], checkSize); err != nil {
				return false, err
			}
			return false, nil
		}
		if err := writeCheckedString(b, rep, checkSize); err != nil {
			return false, err
		}
		return true, nil
	})
}

// GSubString implements Lua 5.5.1 string.gsub with a replacement string template
// supporting %0 (whole match), %1..%9 (captures), and %% (literal '%').
// If maxRepl < 0, all matches are replaced; if maxRepl == 0, no replacements occur.
func GSubString(s, pattern, repl string, maxRepl int) (string, int, error) {
	return GSubStringWithCancel(s, pattern, repl, maxRepl, nil)
}

func GSubStringWithCancel(s, pattern, repl string, maxRepl int, checkCancel func() error) (string, int, error) {
	return GSubStringWithLimits(s, pattern, repl, maxRepl, checkCancel, nil)
}

func GSubStringWithLimits(s, pattern, repl string, maxRepl int, checkCancel func() error, checkSize func(int64) error) (string, int, error) {
	return gsubCore(s, pattern, maxRepl, checkCancel, checkSize, func(ms *matchState, b *strings.Builder, start, end int) (bool, error) {
		if err := expandReplTemplate(ms, b, repl, start, end, checkSize); err != nil {
			return false, err
		}
		return true, nil
	})
}

// GSubTable implements Lua 5.5.1 string.gsub with table-style lookup.
// The first capture (or the whole match if there are no captures) is passed
// to lookup. If lookup returns ok=false, the original match text is preserved.
// If maxRepl < 0, all matches are replaced; if maxRepl == 0, no replacements occur.
func GSubTable(s, pattern string, lookup func(key CaptureVal) (string, bool, error), maxRepl int) (string, int, error) {
	return GSubTableWithCancel(s, pattern, lookup, maxRepl, nil)
}

func GSubTableWithCancel(s, pattern string, lookup func(key CaptureVal) (string, bool, error), maxRepl int, checkCancel func() error) (string, int, error) {
	return GSubTableWithLimits(s, pattern, lookup, maxRepl, checkCancel, nil)
}

func GSubTableWithLimits(s, pattern string, lookup func(key CaptureVal) (string, bool, error), maxRepl int, checkCancel func() error, checkSize func(int64) error) (string, int, error) {
	return gsubCore(s, pattern, maxRepl, checkCancel, checkSize, func(ms *matchState, b *strings.Builder, start, end int) (bool, error) {
		key, err := ms.getOneCapture(0, start, end)
		if err != nil {
			return false, err
		}
		rep, ok, err := lookup(key)
		if err != nil {
			return false, err
		}
		if !ok {
			if err := writeCheckedString(b, s[start:end], checkSize); err != nil {
				return false, err
			}
			return false, nil
		}
		if err := writeCheckedString(b, rep, checkSize); err != nil {
			return false, err
		}
		return true, nil
	})
}

func writeCheckedString(b *strings.Builder, str string, checkSize func(int64) error) error {
	if checkSize != nil && len(str) > 0 {
		if err := checkSize(int64(b.Len()) + int64(len(str))); err != nil {
			return err
		}
	}
	b.WriteString(str)
	return nil
}

func writeCheckedByte(b *strings.Builder, c byte, checkSize func(int64) error) error {
	if checkSize != nil {
		if err := checkSize(int64(b.Len()) + 1); err != nil {
			return err
		}
	}
	b.WriteByte(c)
	return nil
}

func expandReplTemplate(ms *matchState, b *strings.Builder, news string, s, e int, checkSize func(int64) error) error {
	for len(news) > 0 {
		idx := strings.IndexByte(news, lEsc)
		if idx < 0 {
			return writeCheckedString(b, news, checkSize)
		}
		if err := writeCheckedString(b, news[:idx], checkSize); err != nil {
			return err
		}
		if idx+1 >= len(news) {
			return errInvalidReplEsc
		}
		ch := news[idx+1]
		switch {
		case ch == lEsc:
			if err := writeCheckedByte(b, lEsc, checkSize); err != nil {
				return err
			}
		case ch == '0':
			if err := writeCheckedString(b, ms.src[s:e], checkSize); err != nil {
				return err
			}
		case isDigit(ch):
			cv, err := ms.getOneCapture(int(ch-'1'), s, e)
			if err != nil {
				return err
			}
			if cv.IsPos {
				if err := writeCheckedString(b, strconv.FormatInt(cv.Pos, 10), checkSize); err != nil {
					return err
				}
			} else {
				if err := writeCheckedString(b, cv.Str, checkSize); err != nil {
					return err
				}
			}
		default:
			return errInvalidReplEsc
		}
		news = news[idx+2:]
	}
	return nil
}

func gsubCore(s, pattern string, maxRepl int, checkCancel func() error, checkSize func(int64) error, apply func(ms *matchState, b *strings.Builder, start, end int) (bool, error)) (string, int, error) {
	if maxRepl < 0 {
		maxRepl = len(s) + 1
	}
	anchor := len(pattern) > 0 && pattern[0] == '^'
	p := pattern
	if anchor {
		p = pattern[1:]
	}
	var ms matchState
	ms.checkCancel = checkCancel
	ms.prep(s, p)

	var b strings.Builder
	src := 0
	lastMatch := -1
	n := 0
	changed := false

	for n < maxRepl {
		ms.reprep()
		e, err := ms.match(src, 0)
		if err != nil {
			return "", 0, err
		}
		if e >= 0 && e != lastMatch {
			n++
			didChange, err := apply(&ms, &b, src, e)
			if err != nil {
				return "", 0, err
			}
			if didChange {
				changed = true
			}
			src = e
			lastMatch = e
		} else if src < len(s) {
			if err := writeCheckedByte(&b, s[src], checkSize); err != nil {
				return "", 0, err
			}
			src++
		} else {
			break
		}
		if anchor {
			break
		}
	}
	if !changed {
		return s, n, nil
	}
	if err := writeCheckedString(&b, s[src:], checkSize); err != nil {
		return "", 0, err
	}
	return b.String(), n, nil
}
