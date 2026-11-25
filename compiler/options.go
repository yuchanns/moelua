package compiler

import (
	"fmt"
	"io"
	"strings"

	"github.com/yuchanns/moelua/bytecode"
)

const (
	defaultMaxRecursion = 200
	luaIDSize           = 60
	maxShortStrLen      = 40
	luaEnv              = "_ENV"
)

type config struct {
	compatGlobal     bool
	maxRecursion     int
	skipFileHeader   bool
	hasSkipHeaderOpt bool
	checkMem         func(int64) error
}

// Option configures compilation behavior.
type Option func(*config)

// CompatGlobal controls whether "global" is context-sensitive (true, matching
// LUA_COMPAT_GLOBAL=1 default in Lua 5.5.1) or a strict reserved keyword (false).
func CompatGlobal(enable bool) Option {
	return func(c *config) {
		c.compatGlobal = enable
	}
}

// SkipFileHeader controls whether leading UTF-8 BOM and initial '#' comment
// lines are skipped. When not set, valid UTF-8 BOM and "#!" shebangs are
// skipped automatically, and any initial '#' line is skipped when name starts
// with '@' (matching luaL_loadfilex).
func SkipFileHeader(enable bool) Option {
	return func(c *config) {
		c.skipFileHeader = enable
		c.hasSkipHeaderOpt = true
	}
}

// MaxRecursionDepth sets the maximum syntactic nesting depth before failing
// with a syntax error instead of exhausting the Go stack. Defaults to 200.
func MaxRecursionDepth(depth int) Option {
	return func(c *config) {
		if depth > 0 {
			c.maxRecursion = depth
		}
	}
}

// WithMemoryCheck registers an optional callback invoked with estimated bytes
// allocated during compilation (e.g. token buffer, bytecode, and constant pools)
// so callers can enforce memory limits incrementally while streaming input.
func WithMemoryCheck(fn func(int64) error) Option {
	return func(c *config) {
		c.checkMem = fn
	}
}

// SyntaxError represents a Lua 5.5.1 lexical, syntactic, or semantic compile error.
type SyntaxError struct {
	Source string
	Line   int
	Msg    string
	Near   string
}

func (e *SyntaxError) Error() string {
	var base string
	if e.Source == "" {
		base = fmt.Sprintf("[string \"\"]:%d: %s", e.Line, e.Msg)
	} else {
		base = fmt.Sprintf("%s:%d: %s", chunkID(e.Source), e.Line, e.Msg)
	}
	if e.Near != "" {
		return fmt.Sprintf("%s near %s", base, e.Near)
	}
	return base
}

type compilePanic struct {
	err error
}

func defaultConfig(opts []Option) config {
	cfg := config{
		compatGlobal: true,
		maxRecursion: defaultMaxRecursion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// Compile compiles Lua 5.5.1 source code into a verified PUC-Rio Lua 5.5.1 Proto
// using the zero-copy string fast path.
func Compile(name string, src string, opts ...Option) (proto *bytecode.Proto, err error) {
	cfg := defaultConfig(opts)

	defer func() {
		if r := recover(); r != nil {
			if cp, ok := r.(compilePanic); ok {
				proto = nil
				err = cp.err
				return
			}
			panic(r)
		}
	}()

	ls := newLexState(name, src, cfg)
	p := ls.parseMain()
	if verr := bytecode.Verify(p); verr != nil {
		return nil, verr
	}
	return p, nil
}

// CompileReader streams Lua 5.5.1 source code from r into a verified PUC-Rio Lua 5.5.1
// Proto using a ZIO-style chunk buffer without reading the entire input into memory first.
func CompileReader(name string, r io.Reader, opts ...Option) (proto *bytecode.Proto, err error) {
	if r == nil {
		return nil, fmt.Errorf("compile: nil reader")
	}
	cfg := defaultConfig(opts)

	defer func() {
		if r := recover(); r != nil {
			if cp, ok := r.(compilePanic); ok {
				proto = nil
				err = cp.err
				return
			}
			panic(r)
		}
	}()

	ls := newLexStateReader(name, r, cfg)
	p := ls.parseMain()
	if verr := bytecode.Verify(p); verr != nil {
		return nil, verr
	}
	return p, nil
}

// chunkID formats a chunk source name identically to PUC-Rio lobject.c:luaO_chunkid.
func chunkID(source string) string {
	const (
		rets = "..."
		pre  = `[string "`
		pos  = `"]`
	)
	if len(source) == 0 {
		return pre + pos
	}
	bufflen := luaIDSize
	switch source[0] {
	case '=':
		rest := source[1:]
		if len(rest) < bufflen {
			return rest
		}
		return rest[:bufflen-1]
	case '@':
		rest := source[1:]
		if len(rest) < bufflen {
			return rest
		}
		keep := bufflen - len(rets) - 1
		if keep < 0 {
			keep = 0
		}
		return rets + rest[len(rest)-keep:]
	default:
		nl := strings.IndexByte(source, '\n')
		maxBody := bufflen - len(pre) - len(rets) - len(pos) - 1
		if len(source) < maxBody && nl < 0 {
			return pre + source + pos
		}
		body := source
		if nl >= 0 {
			body = body[:nl]
		}
		if len(body) > maxBody {
			body = body[:maxBody]
		}
		return pre + body + rets + pos
	}
}
