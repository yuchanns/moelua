package luastdlib

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runLua55(t *testing.T, script string) string {
	t.Helper()
	bin := os.Getenv("LUA55_BIN")
	if bin == "" {
		bin = filepath.Join("..", "..", "build", "lua55_o2")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("reference lua55_o2 binary missing at %s (run `make tools LUA55_SRC=...`)", bin)
	}
	cmd := exec.Command(bin, "-")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("lua55_o2 failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	return stdout.String()
}

func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "\\x%02x", s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func encodeCaps(caps []CaptureVal) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		if c.IsPos {
			parts[i] = fmt.Sprintf("P:%d", c.Pos)
		} else {
			parts[i] = "S:" + hex.EncodeToString([]byte(c.Str))
		}
	}
	return strings.Join(parts, ",")
}

// =============================================================================
// 1. Pattern Matching Tests (pm.go)
// =============================================================================

func TestPMBasicAndOfficial(t *testing.T) {
	// Empty patterns and embedded NULs (pm.lua lines 22-40)
	s, e, caps, ok, err := Find("", "", 1, false)
	if err != nil || !ok || s != 1 || e != 0 || len(caps) != 0 {
		t.Fatalf("Find('', '') = (%d,%d,%v,%v,%v)", s, e, caps, ok, err)
	}
	s, e, _, ok, err = Find("alo", "", 1, false)
	if err != nil || !ok || s != 1 || e != 0 {
		t.Fatalf("Find('alo', '') = (%d,%d,%v,%v)", s, e, ok, err)
	}
	s, e, _, ok, err = Find("a\x00o a\x00o a\x00o", "a\x00o", 2, false)
	if err != nil || !ok || s != 5 || e != 7 {
		t.Fatalf("Find embedded nul = (%d,%d,%v,%v)", s, e, ok, err)
	}
	s, e, _, ok, err = Find("a\x00a\x00a\x00a\x00\x00ab", "\x00ab", 2, false)
	if err != nil || !ok || s != 9 || e != 11 {
		t.Fatalf("Find nul at end = (%d,%d,%v,%v)", s, e, ok, err)
	}
	_, _, _, ok, err = Find("a\x00a\x00a\x00a\x00\x00ab", "b\x00", 1, false)
	if err != nil || ok {
		t.Fatalf("Find('...ab', 'b\\0') expected not found")
	}
	_, _, _, ok, err = Find("", "\x00", 1, false)
	if err != nil || ok {
		t.Fatalf("Find('', '\\0') expected not found")
	}
	s, e, _, ok, err = Find("alo(.)alo", "(.)", 1, true)
	if err != nil || !ok || s != 4 || e != 6 {
		t.Fatalf("Find plain = (%d,%d,%v,%v)", s, e, ok, err)
	}
	s, _, _, ok, err = Find("abc\x00\x00", "\x00.", 1, false)
	if err != nil || !ok || s != 4 {
		t.Fatalf("Find magic char after nul = (%d,%v,%v)", s, ok, err)
	}
	s, _, _, ok, err = Find("b$a", "$\x00?", 1, false)
	if err != nil || !ok || s != 2 {
		t.Fatalf("Find $\\0? = (%d,%v,%v)", s, ok, err)
	}

	// Big strings without stack/complexity overflow (pm.lua lines 264-267)
	bigA := strings.Repeat("a", 300000)
	_, _, _, ok, err = Find(bigA, "^a*.?$", 1, false)
	if err != nil || !ok {
		t.Fatalf("big ^a*.?$ failed: ok=%v err=%v", ok, err)
	}
	_, _, _, ok, err = Find(bigA, "^a*.?b$", 1, false)
	if err != nil || ok {
		t.Fatalf("big ^a*.?b$ expected false: ok=%v err=%v", ok, err)
	}
	_, _, _, ok, err = Find(bigA, "^a-.?$", 1, false)
	if err != nil || !ok {
		t.Fatalf("big ^a-.?$ failed: ok=%v err=%v", ok, err)
	}

	// Recursion depth cap MAXCCALLS = 200 (cstack.lua lines 35-44)
	c80, ok, err := Match(strings.Repeat("a", 80), strings.Repeat(".?", 80), 1)
	if err != nil || !ok || len(c80) != 1 || len(c80[0].Str) != 80 {
		t.Fatalf("depth 80 failed: ok=%v err=%v", ok, err)
	}
	_, _, err = Match(strings.Repeat("a", 2000), strings.Repeat(".?", 2000), 1)
	if err == nil || !strings.Contains(err.Error(), "pattern too complex") {
		t.Fatalf("expected 'pattern too complex', got %v", err)
	}

	// GMatch repeated call after pattern-too-complex error (pm.lua lines 350-357)
	it := GMatch(strings.Repeat("a", 20000), strings.Repeat("a?", 20000), 1)
	_, _, err1 := it.Next()
	_, _, err2 := it.Next()
	if err1 == nil || !strings.Contains(err1.Error(), "too complex") ||
		err2 == nil || !strings.Contains(err2.Error(), "too complex") {
		t.Fatalf("expected repeated complexity errors, got %v / %v", err1, err2)
	}

	// MaxCaptures = 32 limit
	pat32 := strings.Repeat("(a)", 32)
	caps32, ok, err := Match(strings.Repeat("a", 32), pat32, 1)
	if err != nil || !ok || len(caps32) != 32 {
		t.Fatalf("32 captures failed: len=%d ok=%v err=%v", len(caps32), ok, err)
	}
	pat33 := strings.Repeat("(a)", 33)
	_, _, err = Match(strings.Repeat("a", 33), pat33, 1)
	if err == nil || !strings.Contains(err.Error(), "too many captures") {
		t.Fatalf("expected 'too many captures', got %v", err)
	}

	// Malformed patterns (pm.lua lines 389-406)
	malformed := []struct {
		pat string
		sub string
	}{
		{"(.", "unfinished capture"},
		{".)", "invalid pattern capture"},
		{"[a", "malformed"},
		{"[]", "malformed"},
		{"[^]", "malformed"},
		{"[a%]", "malformed"},
		{"[a%", "malformed"},
		{"%b", "malformed"},
		{"%ba", "malformed"},
		{"%", "malformed"},
		{"%f", "missing"},
		{"(%0)", "invalid capture index %0"},
		{"(%1)", "invalid capture index %1"},
	}
	for _, tc := range malformed {
		_, _, _, _, err := Find("a", tc.pat, 1, false)
		if err == nil || !strings.Contains(err.Error(), tc.sub) {
			t.Errorf("Find('a', %q): expected error containing %q, got %v", tc.pat, tc.sub, err)
		}
	}

	// GSub function and table replacements + maxRepl + unchanged optimization
	res, n, err := GSub("um (dois) tres (quatro)", "(%(%w+%))", func(caps []CaptureVal) (string, bool, error) {
		return strings.ToUpper(caps[0].Str), true, nil
	}, -1)
	if err != nil || res != "um (DOIS) tres (QUATRO)" || n != 2 {
		t.Fatalf("GSub func = (%q, %d, %v)", res, n, err)
	}

	// GSub with callback returning changed=false keeps original text while counting matches
	var seen []string
	res, n, err = GSub("first second word", "%w+", func(caps []CaptureVal) (string, bool, error) {
		seen = append(seen, caps[0].Str)
		return "", false, nil
	}, 2)
	if err != nil || res != "first second word" || n != 2 || strings.Join(seen, ",") != "first,second" {
		t.Fatalf("GSub unchanged = (%q, %d, %v, %v)", res, n, seen, err)
	}

	// GSub with maxRepl == 0 performs 0 replacements
	res, n, err = GSubString("abc", ".", "x", 0)
	if err != nil || res != "abc" || n != 0 {
		t.Fatalf("GSub maxRepl=0 = (%q, %d, %v)", res, n, err)
	}

	// GSubTable with position capture and substring capture (pm.lua lines 284-290)
	tbl := map[string]string{"a": "AA", "l": ""}
	res, n, err = GSubTable("alo alo", "(.)", func(key CaptureVal) (string, bool, error) {
		v, ok := tbl[key.Str]
		return v, ok, nil
	}, -1)
	if err != nil || res != "AAo AAo" || n != 7 {
		t.Fatalf("GSubTable = (%q, %d, %v)", res, n, err)
	}
	posTbl := map[int64]string{1: "x", 2: "yy", 3: "zzz"}
	res, _, err = GSubTable("alo alo", "().", func(key CaptureVal) (string, bool, error) {
		if !key.IsPos {
			t.Fatalf("expected position key")
		}
		v, ok := posTbl[key.Pos]
		return v, ok, nil
	}, -1)
	if err != nil || res != "xyyzzz alo" {
		t.Fatalf("GSubTable pos = (%q, %v)", res, err)
	}

	// GSubString errors (pm.lua lines 256-259)
	for _, tc := range []struct {
		pat, repl, wantErr string
	}{
		{".", "%2", "invalid capture index %2"},
		{".", "%x", "invalid use of '%'"},
		{"a", "%", "invalid use of '%'"},
		{"(a", "%1", "unfinished capture"},
	} {
		_, _, err := GSubString("alo", tc.pat, tc.repl, -1)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("GSubString(%q, %q): want err %q, got %v", tc.pat, tc.repl, tc.wantErr, err)
		}
	}
}

func TestPMDifferential(t *testing.T) {
	// Build all 256 bytes string for full character class equivalence
	var allBytesBuf [256]byte
	for i := 0; i < 256; i++ {
		allBytesBuf[i] = byte(i)
	}
	allBytes := string(allBytesBuf[:])

	type findCase struct {
		s, pat string
		init   int
		plain  bool
	}
	findCases := []findCase{
		{"", "", 1, false},
		{"", "", 2, false},
		{"alo", "", 1, false},
		{"alo123alo", "12", 1, false},
		{"alo123alo", "^12", 1, false},
		{"alo123alo", "alo", -3, false},
		{"alo123alo", "alo", 0, false},
		{"alo123alo", "alo", -100, false},
		{"1234567890123456789", ".45", -9, false},
		{"alo(.)alo", "(.)", 1, true},
		{"a\x00o a\x00o", "a\x00o", 2, false},
		{"a", "%f[a]", 1, false},
		{"a", "%f[^%z]", 1, false},
		{"a", "%f[^%l]", 1, false},
		{"aba", "%f[a%z]", 1, false},
		{"aba", "%f[%z]", 1, false},
		{"aba", "%f[%l%z]", 1, false},
		{"aba", "%f[^%l%z]", 1, false},
		{" alo aalo allo", "%f[%S].-%f[%s].-%f[%S]", 1, false},
		{"abc\x00efg", "%\x00", 1, false},
		{"b$a", "$\x00?", 1, false},
		{"hello (world)", "(%w+) %((%w+)%)", 1, false},
		{"0123456789", "(.+(.?)())", 1, false},
	}

	type matchCase struct {
		s, pat string
		init   int
	}
	matchCases := []matchCase{
		{"aaab", ".*b", 1},
		{"aaa", ".*a", 1},
		{"b", ".*b", 1},
		{"aaab", ".+b", 1},
		{"b", ".+b", 1},
		{"aaab", ".?b", 1},
		{"aa", "^aa?a?a", 1},
		{"aabaaabaaabaaaba", "b.*b", 1},
		{"aabaaabaaabaaaba", "b.-b", 1},
		{"alo alx 123 b\x00o b\x00o", "((..*) %2)", 1},
		{"axz123= 4= 4 34", "((.+)=(.*)=%3 %2)", 1},
		{"=======", "^(=*)=%1$", 1},
		{"==========", "^([=]*)=%1$", 1},
		{"0123456789", "(.+(.?)())", 1},
		{"a", "()%1", 1},
		{"ab\x00\x01\x02c", "[\x00-\x02]+", 1},
		{"abc\x00efg\x00\x01e\x01g", "%b\x00\x01", 1},
		{"alo (a (b) c) d", "%b()", 1},
		{" alo aalo allo", "%f[%S](.-%f[%s].-%f[%S])", 1},
		{"  \n\r*&\n\r   xuxu  \n\n", "%g%g%g+", 1},
		{"]]]ab", "[^]]+", 1},
	}

	type gmatchCase struct {
		s, pat string
		init   int
	}
	gmatchCases := []gmatchCase{
		{"abcde", "()", 1},
		{"first second word", "%w+", 1},
		{"xuxx uu ppar r", "()(.)%2", 1},
		{"13 14 10 = 11, 15= 16, 22=23", "(%d+)%s*=%s*(%d+)", 1},
		{"10 20 30", "%d+", 3},
		{"11 21 31", "%d+", -4},
		{"11 21 31", "%w*", 9},
		{"11 21 31", "%w*", 10},
		{"a  \nbc\t\td", "()%s*()", 1},
		{"alo alo th02 is 1hat", "()%f[%w%d]", 1},
		{"a^b^c", "^", 1},
	}

	type gsubCase struct {
		s, pat, repl string
		maxRepl      int // -1 means default/omitted
	}
	gsubCases := []gsubCase{
		{"a b cd", " *", "-", -1},
		{"  alo alo  ", "^%s*(.-)%s*$", "%1", -1},
		{"alo  alo  \n 123\n ", "%s+", " ", -1},
		{"alo alo", "()[al]", "%1", -1},
		{"abc=xyz", "(%w*)(%p)(%w+)", "%3%2%1-%0", -1},
		{"abc", "%w", "%1%0", -1},
		{"abc", "%w+", "%0%1", -1},
		{"abc", "$", "\x00xy", -1},
		{"", "^", "r", -1},
		{"", "$", "r", -1},
		{"abcde", ".", "%0@", 2},
		{"abcde", ".", "%0@", 0},
		{"aaa aa a aaa a", "%f[%w]a", "x", -1},
		{"[[]] [][] [[[[", "%f[[].", "x", -1},
		{"01abc45de3", "%f[%d]", ".", -1},
		{"01abc45 de3x", "%f[%D]%w", ".", -1},
		{"function", "%f[\x01-\xff]%w", ".", -1},
		{"function", "%f[^\x01-\xff]", ".", -1},
		{"alo 'oi' alo", "%b''", `"`, -1},
		{"a", "(a", "x", -1}, // unfinished capture ignored when template doesn't use %1
		// All character classes over all 256 bytes!
		{allBytes, "%a", "", -1},
		{allBytes, "%A", "", -1},
		{allBytes, "%c", "", -1},
		{allBytes, "%C", "", -1},
		{allBytes, "%d", "", -1},
		{allBytes, "%D", "", -1},
		{allBytes, "%g", "", -1},
		{allBytes, "%G", "", -1},
		{allBytes, "%l", "", -1},
		{allBytes, "%L", "", -1},
		{allBytes, "%p", "", -1},
		{allBytes, "%P", "", -1},
		{allBytes, "%s", "", -1},
		{allBytes, "%S", "", -1},
		{allBytes, "%u", "", -1},
		{allBytes, "%U", "", -1},
		{allBytes, "%w", "", -1},
		{allBytes, "%W", "", -1},
		{allBytes, "%x", "", -1},
		{allBytes, "%X", "", -1},
		{allBytes, "%z", "", -1},
		{allBytes, "%Z", "", -1},
		{allBytes, "[a-]", "", -1},
		{allBytes, "[]%%]", "", -1},
		{allBytes, "[a%-z]", "", -1},
		{allBytes, "[%^%[%-a%]%-b]", "", -1},
		{allBytes, "[\xc8-\xd2]", "", -1},
	}

	var script strings.Builder
	script.WriteString(`
local function tohex(s)
  return (s:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end
local function enc(t, start)
  local parts = {}
  for i = start, t.n do
    local v = t[i]
    if type(v) == "number" then parts[#parts+1] = "P:" .. tostring(v)
    else parts[#parts+1] = "S:" .. tohex(v) end
  end
  return table.concat(parts, ",")
end
`)
	for i, tc := range findCases {
		fmt.Fprintf(&script, "do local r=table.pack(string.find(%s,%s,%d,%v)); if not r[1] then print('F%d=NONE') else print('F%d='..r[1]..':'..r[2]..'|'..enc(r,3)) end end\n",
			luaQuote(tc.s), luaQuote(tc.pat), tc.init, tc.plain, i, i)
	}
	for i, tc := range matchCases {
		fmt.Fprintf(&script, "do local r=table.pack(string.match(%s,%s,%d)); if not r[1] then print('M%d=NONE') else print('M%d='..enc(r,1)) end end\n",
			luaQuote(tc.s), luaQuote(tc.pat), tc.init, i, i)
	}
	for i, tc := range gmatchCases {
		fmt.Fprintf(&script, "do local items={}; for a,b,c in string.gmatch(%s,%s,%d) do local r=table.pack(a,b,c); while r.n>0 and r[r.n]==nil do r.n=r.n-1 end; items[#items+1]=enc(r,1) end; print('G%d='..table.concat(items,';')) end\n",
			luaQuote(tc.s), luaQuote(tc.pat), tc.init, i)
	}
	for i, tc := range gsubCases {
		if tc.maxRepl < 0 {
			fmt.Fprintf(&script, "do local r,n=string.gsub(%s,%s,%s); print('S%d='..n..'|'..tohex(r)) end\n",
				luaQuote(tc.s), luaQuote(tc.pat), luaQuote(tc.repl), i)
		} else {
			fmt.Fprintf(&script, "do local r,n=string.gsub(%s,%s,%s,%d); print('S%d='..n..'|'..tohex(r)) end\n",
				luaQuote(tc.s), luaQuote(tc.pat), luaQuote(tc.repl), tc.maxRepl, i)
		}
	}

	out := runLua55(t, script.String())
	gotLua := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			gotLua[k] = v
		}
	}

	for i, tc := range findCases {
		st, en, caps, ok, err := Find(tc.s, tc.pat, tc.init, tc.plain)
		if err != nil {
			t.Fatalf("Find case %d unexpected err: %v", i, err)
		}
		var got string
		if !ok {
			got = "NONE"
		} else {
			got = fmt.Sprintf("%d:%d|%s", st, en, encodeCaps(caps))
		}
		want := gotLua[fmt.Sprintf("F%d", i)]
		if got != want {
			t.Errorf("Find[%d] (%q, %q, %d, %v): got %q, lua55 %q", i, tc.s, tc.pat, tc.init, tc.plain, got, want)
		}
	}

	for i, tc := range matchCases {
		caps, ok, err := Match(tc.s, tc.pat, tc.init)
		if err != nil {
			t.Fatalf("Match case %d unexpected err: %v", i, err)
		}
		var got string
		if !ok {
			got = "NONE"
		} else {
			got = encodeCaps(caps)
		}
		want := gotLua[fmt.Sprintf("M%d", i)]
		if got != want {
			t.Errorf("Match[%d] (%q, %q, %d): got %q, lua55 %q", i, tc.s, tc.pat, tc.init, got, want)
		}
	}

	for i, tc := range gmatchCases {
		it := GMatch(tc.s, tc.pat, tc.init)
		var items []string
		for {
			caps, ok, err := it.Next()
			if err != nil {
				t.Fatalf("GMatch case %d unexpected err: %v", i, err)
			}
			if !ok {
				break
			}
			items = append(items, encodeCaps(caps))
		}
		got := strings.Join(items, ";")
		want := gotLua[fmt.Sprintf("G%d", i)]
		if got != want {
			t.Errorf("GMatch[%d] (%q, %q, %d): got %q, lua55 %q", i, tc.s, tc.pat, tc.init, got, want)
		}
	}

	for i, tc := range gsubCases {
		r, n, err := GSubString(tc.s, tc.pat, tc.repl, tc.maxRepl)
		if err != nil {
			t.Fatalf("GSubString case %d unexpected err: %v", i, err)
		}
		got := fmt.Sprintf("%d|%s", n, hex.EncodeToString([]byte(r)))
		want := gotLua[fmt.Sprintf("S%d", i)]
		if got != want {
			t.Errorf("GSubString[%d] (pat=%q, repl=%q): mismatch\n  got:   %s\n  lua55: %s", i, tc.pat, tc.repl, got, want)
		}
	}
}

// =============================================================================
// 2. Binary Pack / Unpack / PackSize Tests (pack.go)
// =============================================================================

func TestPackOfficialAndEdgeCases(t *testing.T) {
	// LP64 platform sizes (tpack.lua lines 13-22)
	wantSizes := map[string]int64{
		"b": 1, "B": 1, "h": 2, "H": 2, "i": 4, "I": 4,
		"l": 8, "L": 8, "j": 8, "J": 8, "T": 8,
		"f": 4, "d": 8, "n": 8, "x": 1, "c0": 0, "c7": 7,
		"!xXi16": 8, "!8 xXi8": 8, "!8 xXi2": 2, "!2 xXi2": 2, "!2 xXi8": 2, "!16 xXi16": 16,
		" b b Xd b Xb x": 4,
		"<! c3":          3,
	}
	for f, want := range wantSizes {
		got, err := PackSize(f)
		if err != nil || got != want {
			t.Errorf("PackSize(%q) = (%d, %v), want %d", f, got, err, want)
		}
	}

	// Huge packsize / overflow checks (tpack.lua lines 135-144)
	maxSize := int64(math.MaxInt64)
	gotSz, err := PackSize(fmt.Sprintf("c%d", maxSize-9))
	if err != nil || gotSz != maxSize-9 {
		t.Fatalf("PackSize(c(max-9)) = (%d, %v)", gotSz, err)
	}
	if _, err := PackSize(fmt.Sprintf("c%dc10", maxSize-9)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected 'too large', got %v", err)
	}
	if _, err := Pack(fmt.Sprintf("xxxxxxxxxx c%d", maxSize-9), PackStringVal("")); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("expected 'too long', got %v", err)
	}
	if _, err := PackSize("c1" + strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "invalid format") {
		t.Fatalf("expected 'invalid format', got %v", err)
	}

	// Sign extension & arbitrary integer sizes 1..16 (tpack.lua lines 58-112)
	for i := 1; i <= 16; i++ {
		wantFF := strings.Repeat("\xff", i)
		p, err := Pack(fmt.Sprintf("i%d", i), PackIntVal(-1))
		if err != nil || p != wantFF {
			t.Fatalf("Pack(i%d, -1) = (%x, %v)", i, p, err)
		}
		vals, nextPos, err := Unpack(fmt.Sprintf("i%d", i), wantFF, 1)
		if err != nil || len(vals) != 1 || vals[0].Int != -1 || nextPos != int64(i+1) {
			t.Fatalf("Unpack(i%d, 0xff...) = (%v, %d, %v)", i, vals, nextPos, err)
		}

		wantAA := "\xaa" + strings.Repeat("\x00", i-1)
		pLE, err := Pack(fmt.Sprintf("<I%d", i), PackIntVal(0xAA))
		if err != nil || pLE != wantAA {
			t.Fatalf("Pack(<I%d, 0xAA) = (%x, %v)", i, pLE, err)
		}
		vals, _, err = Unpack(fmt.Sprintf("<I%d", i), wantAA, 1)
		if err != nil || vals[0].Int != 0xAA {
			t.Fatalf("Unpack(<I%d) = (%v, %v)", i, vals, err)
		}
		revAA := strings.Repeat("\x00", i-1) + "\xaa"
		pBE, err := Pack(fmt.Sprintf(">I%d", i), PackIntVal(0xAA))
		if err != nil || pBE != revAA {
			t.Fatalf("Pack(>I%d, 0xAA) = (%x, %v)", i, pBE, err)
		}
		vals, _, err = Unpack(fmt.Sprintf(">I%d", i), revAA, 1)
		if err != nil || vals[0].Int != 0xAA {
			t.Fatalf("Unpack(>I%d) = (%v, %v)", i, vals, err)
		}
	}

	// Pack integer overflow boundaries (tpack.lua lines 148-163)
	for i := 1; i <= 7; i++ {
		umax := int64((uint64(1) << (i * 8)) - 1)
		maxS := umax >> 1
		minS := ^maxS
		for _, tc := range []struct {
			f string
			v int64
		}{
			{fmt.Sprintf("<I%d", i), -1},
			{fmt.Sprintf("<I%d", i), minS},
			{fmt.Sprintf(">I%d", i), umax + 1},
			{fmt.Sprintf(">i%d", i), umax},
			{fmt.Sprintf(">i%d", i), maxS + 1},
			{fmt.Sprintf("<i%d", i), minS - 1},
		} {
			if _, err := Pack(tc.f, PackIntVal(tc.v)); err == nil || !strings.Contains(err.Error(), "overflow") {
				t.Errorf("Pack(%q, %d) expected overflow, got %v", tc.f, tc.v, err)
			}
		}
		// Exact boundary values succeed and roundtrip
		for _, tc := range []struct {
			f string
			v int64
		}{
			{fmt.Sprintf(">i%d", i), maxS},
			{fmt.Sprintf("<i%d", i), minS},
			{fmt.Sprintf(">I%d", i), umax},
		} {
			p, err := Pack(tc.f, PackIntVal(tc.v))
			if err != nil {
				t.Fatalf("Pack(%q, %d) failed: %v", tc.f, tc.v, err)
			}
			u, _, err := Unpack(tc.f, p, 1)
			if err != nil || u[0].Int != tc.v {
				t.Fatalf("Unpack(%q) = (%v, %v), want %d", tc.f, u, err, tc.v)
			}
		}
	}

	// Unpack >8-byte integer overflow & valid sign/zero extensions (tpack.lua lines 80-91)
	lnum := int64(0x0807060504030201)
	sNeg, err := Pack("<j", PackIntVal(-lnum))
	if err != nil {
		t.Fatal(err)
	}
	for i := 9; i <= 16; i++ {
		u, _, err := Unpack(fmt.Sprintf("<i%d", i), sNeg+strings.Repeat("\xff", i-8), 1)
		if err != nil || u[0].Int != -lnum {
			t.Fatalf("Unpack(<i%d, -lnum) = (%v, %v)", i, u, err)
		}
		u, _, err = Unpack(fmt.Sprintf("<I%d", i), sNeg+strings.Repeat("\x00", i-8), 1)
		if err != nil || u[0].Int != -lnum {
			t.Fatalf("Unpack(<I%d, -lnum) = (%v, %v)", i, u, err)
		}
		if _, _, err := Unpack(fmt.Sprintf("<I%d", i), strings.Repeat("\x00", i-1)+"\x01", 1); err == nil || !strings.Contains(err.Error(), "does not fit") {
			t.Fatalf("expected 'does not fit' for <I%d", i)
		}
		if _, _, err := Unpack(fmt.Sprintf(">i%d", i), "\x01"+strings.Repeat("\x00", i-1), 1); err == nil || !strings.Contains(err.Error(), "does not fit") {
			t.Fatalf("expected 'does not fit' for >i%d", i)
		}
	}

	// Invalid formats & runtime errors (tpack.lua lines 124-134, 203-290)
	packErrs := []struct {
		f    string
		args []PackValue
		want string
	}{
		{"i0", []PackValue{PackIntVal(0)}, "out of limits"},
		{"i17", []PackValue{PackIntVal(0)}, "out of limits"},
		{"!17", []PackValue{PackIntVal(0)}, "out of limits"},
		{"Xi17", nil, "(17) out of limits [1,16]"},
		{"i3r", []PackValue{PackIntVal(0)}, "invalid format option 'r'"},
		{"!4i3", []PackValue{PackIntVal(0)}, "not power of 2"},
		{"c", []PackValue{PackStringVal("")}, "missing size"},
		{"s1", []PackValue{PackStringVal(strings.Repeat("a", 300))}, "does not fit"},
		{"z", []PackValue{PackStringVal("alo\x00")}, "contains zeros"},
		{"s100", []PackValue{PackStringVal("alo")}, "out of limits"},
		{"c3", []PackValue{PackStringVal("1234")}, "longer than"},
		{"X", nil, "invalid next option"},
		{"Xc1", nil, "invalid next option"},
	}
	for _, tc := range packErrs {
		if _, err := Pack(tc.f, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Pack(%q) expected err %q, got %v", tc.f, tc.want, err)
		}
	}
	for _, f := range []string{"s", "z", "s4"} {
		if _, err := PackSize(f); err == nil || !strings.Contains(err.Error(), "variable-length format") {
			t.Errorf("PackSize(%q) expected variable-length error, got %v", f, err)
		}
	}
	unpackErrs := []struct {
		f, data string
		pos     int64
		want    string
	}{
		{"i16", strings.Repeat("\x03", 16), 1, "16-byte integer"},
		{"zc10000000", "alo", 1, "unfinished string"},
		{"c5", "abcd", 1, "too short"},
		{"XXi", "", 1, "invalid next option"},
		{"X i", "", 1, "invalid next option"},
		{"c0", "1234", 6, "out of string"},
	}
	for _, tc := range unpackErrs {
		if _, _, err := Unpack(tc.f, tc.data, tc.pos); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Unpack(%q) expected err %q, got %v", tc.f, tc.want, err)
		}
	}
}

func TestPackDifferential(t *testing.T) {
	type packCase struct {
		fmtStr string
		args   []PackValue
		luaExp string
	}
	cases := []packCase{
		{">i2 <i2", []PackValue{PackIntVal(10), PackIntVal(20)}, `string.pack(">i2 <i2", 10, 20)`},
		{"<b h b f d f n i", []PackValue{
			PackIntVal(1), PackIntVal(2), PackIntVal(3), PackFloatVal(4),
			PackFloatVal(5), PackFloatVal(6), PackFloatVal(7), PackIntVal(8),
		}, `string.pack("<b h b f d f n i", 1, 2, 3, 4, 5, 6, 7, 8)`},
		{">!8 b Xh i4 i8 c1 Xi8", []PackValue{
			PackIntVal(-12), PackIntVal(100), PackIntVal(200), PackStringVal("\xec"),
		}, `string.pack(">!8 b Xh i4 i8 c1 Xi8", -12, 100, 200, "\xEC")`},
		{">!4 c3 c4 c2 z i4 c5 c2 Xi4", []PackValue{
			PackStringVal("abc"), PackStringVal("abcd"), PackStringVal("xz"),
			PackStringVal("hello"), PackIntVal(5), PackStringVal("world"), PackStringVal("xy"),
		}, `string.pack(">!4 c3 c4 c2 z i4 c5 c2 Xi4", "abc", "abcd", "xz", "hello", 5, "world", "xy")`},
		{"<j >J <i12 >i12 <I12", []PackValue{
			PackIntVal(math.MinInt64), PackIntVal(-1), PackIntVal(-1234567890123),
			PackIntVal(-1234567890123), PackIntVal(-1),
		}, `string.pack("<j >J <i12 >i12 <I12", math.mininteger, -1, -1234567890123, -1234567890123, -1)`},
		{"<f >f <d >d <n >n", []PackValue{
			PackFloatVal(-1.5), PackFloatVal(math.Inf(1)), PackFloatVal(math.Pi),
			PackFloatVal(math.Copysign(0, -1)), PackFloatVal(1e-300), PackFloatVal(-1e200),
		}, `string.pack("<f >f <d >d <n >n", -1.5, 1/0, math.pi, -0.0, 1e-300, -1e200)`},
		{"s1 s2 s4 s8 s12 z c8", []PackValue{
			PackStringVal("hi"), PackStringVal("hello"), PackStringVal("world"),
			PackStringVal(""), PackStringVal("12bytes"), PackStringVal("zstr"), PackStringVal("123456"),
		}, `string.pack("s1 s2 s4 s8 s12 z c8", "hi", "hello", "world", "", "12bytes", "zstr", "123456")`},
		{" b b Xd b Xb x", []PackValue{
			PackIntVal(1), PackIntVal(2), PackIntVal(3),
		}, `string.pack(" b b Xd b Xb x", 1, 2, 3)`},
	}

	var script strings.Builder
	script.WriteString(`
local function tohex(s)
  return (s:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end
`)
	for i, tc := range cases {
		fmt.Fprintf(&script, "print('P%d='..tohex(%s))\n", i, tc.luaExp)
	}
	out := runLua55(t, script.String())
	luaMap := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			luaMap[k] = v
		}
	}

	for i, tc := range cases {
		packed, err := Pack(tc.fmtStr, tc.args...)
		if err != nil {
			t.Fatalf("Pack[%d] (%q) err: %v", i, tc.fmtStr, err)
		}
		gotHex := hex.EncodeToString([]byte(packed))
		wantHex := luaMap[fmt.Sprintf("P%d", i)]
		if gotHex != wantHex {
			t.Errorf("Pack[%d] (%q) mismatch:\n  got:   %s\n  lua55: %s", i, tc.fmtStr, gotHex, wantHex)
		}

		// Roundtrip Unpack check
		unpacked, nextPos, err := Unpack(tc.fmtStr, packed, 1)
		if err != nil {
			t.Fatalf("Unpack[%d] (%q) err: %v", i, tc.fmtStr, err)
		}
		if nextPos != int64(len(packed)+1) {
			t.Errorf("Unpack[%d] nextPos=%d, want %d", i, nextPos, len(packed)+1)
		}
		if len(unpacked) != len(tc.args) {
			t.Errorf("Unpack[%d] count=%d, want %d", i, len(unpacked), len(tc.args))
		}
	}

	// Unpack with non-default initial positions and alignment (tpack.lua lines 293-319)
	x, err := Pack("i4i4i4i4", PackIntVal(1), PackIntVal(2), PackIntVal(3), PackIntVal(4))
	if err != nil {
		t.Fatal(err)
	}
	for pos := int64(0); pos <= 12; pos++ {
		vals, np, err := Unpack("!4 i4", x, pos+1)
		wantVal := (pos+3)/4 + 1
		if err != nil || vals[0].Int != wantVal || np != wantVal*4+1 {
			t.Errorf("Unpack(!4 i4, pos=%d) = (%v, %d, %v), want (%d, %d)", pos+1, vals, np, err, wantVal, wantVal*4+1)
		}
	}
	vals, np, err := Unpack("!4 i4", x, -7)
	if err != nil || vals[0].Int != 4 || np != 17 {
		t.Errorf("Unpack(!4 i4, -7) = (%v, %d, %v)", vals, np, err)
	}
	vals, np, err = Unpack("c0", x, int64(len(x)+1))
	if err != nil || vals[0].Str != "" || np != int64(len(x)+1) {
		t.Errorf("Unpack(c0, #x+1) = (%v, %d, %v)", vals, np, err)
	}
}

// =============================================================================
// 3. UTF-8 Library Tests (utf8.go) — including Lua 5.5.1 two-return Offset
// =============================================================================

func TestUTF8OfficialAndDifferential(t *testing.T) {
	// Strict vs lax Char checks
	if _, err := Char([]int64{0x7FFFFFFF + 1}, true); err == nil || !strings.Contains(err.Error(), "value out of range") {
		t.Fatalf("expected out of range for 0x80000000 lax=true, got %v", err)
	}
	if _, err := Char([]int64{-1}, true); err == nil || !strings.Contains(err.Error(), "value out of range") {
		t.Fatalf("expected out of range for -1, got %v", err)
	}
	if _, err := Char([]int64{0xD800}, false); err == nil || !strings.Contains(err.Error(), "value out of range") {
		t.Fatalf("expected out of range for surrogate with lax=false, got %v", err)
	}
	if _, err := Char([]int64{0x110000}, false); err == nil || !strings.Contains(err.Error(), "value out of range") {
		t.Fatalf("expected out of range for 0x110000 with lax=false, got %v", err)
	}
	if s, err := Char([]int64{0x4000000, 0x7FFFFFFF}, true); err != nil || s != "\xfc\x84\x80\x80\x80\x80\xfd\xbf\xbf\xbf\xbf\xbf" {
		t.Fatalf("Char extended UTF-8 = (%x, %v)", s, err)
	}

	// Verify CharPattern matches every single character of strict and extended strings
	samples := []struct {
		s   string
		cps []int64
		lax bool
	}{
		{"", []int64{}, false},
		{"hello World", []int64{'h', 'e', 'l', 'l', 'o', ' ', 'W', 'o', 'r', 'l', 'd'}, false},
		{"汉字/漢字", []int64{27721, 23383, 47, 28450, 23383}, false},
		{"日本語a-4\x00éó", []int64{26085, 26412, 35486, 97, 45, 52, 0, 233, 243}, false},
		{"𣲷𠜎𠱓𡁻𠵼ab𠺢", []int64{0x23CB7, 0x2070E, 0x20C53, 0x2107B, 0x20D7C, 0x61, 0x62, 0x20EA2}, false},
		{"\x00\x7f\xc2\x80\xdf\xbf\xe0\xa0\x80\xef\xbf\xbf\xf0\x90\x80\x80\xf4\x8f\xbf\xbf", []int64{0, 0x7F, 0x80, 0x7FF, 0x800, 0xFFFF, 0x10000, 0x10FFFF}, false},
		{"\xf0\x90\x80\x80\xf7\xbf\xbf\xbf\xf8\x88\x80\x80\x80\xfb\xbf\xbf\xbf\xbf\xfc\x84\x80\x80\x80\x80\xfd\xbf\xbf\xbf\xbf\xbf", []int64{0x10000, 0x1FFFFF, 0x200000, 0x3FFFFFF, 0x4000000, 0x7FFFFFFF}, true},
	}

	for _, tc := range samples {
		// Char roundtrip
		encoded, err := Char(tc.cps, true)
		if err != nil || encoded != tc.s {
			t.Fatalf("Char(%v) = (%x, %v), want %x", tc.cps, encoded, err, tc.s)
		}
		if !tc.lax {
			encodedStrict, err := Char(tc.cps, false)
			if err != nil || encodedStrict != tc.s {
				t.Fatalf("Char strict(%v) = (%x, %v)", tc.cps, encodedStrict, err)
			}
		}

		// Len
		cnt, errPos, err := Len(tc.s, 1, -1, tc.lax)
		if err != nil || errPos != 0 || cnt != int64(len(tc.cps)) {
			t.Fatalf("Len(%x) = (%d, %d, %v), want %d", tc.s, cnt, errPos, err, len(tc.cps))
		}

		// Codepoint
		gotCPs, err := Codepoint(tc.s, 1, -1, tc.lax)
		if err != nil || len(gotCPs) != len(tc.cps) {
			t.Fatalf("Codepoint(%x) = (%v, %v), want %v", tc.s, gotCPs, err, tc.cps)
		}
		for i := range tc.cps {
			if gotCPs[i] != tc.cps[i] {
				t.Fatalf("Codepoint[%d] = %d, want %d", i, gotCPs[i], tc.cps[i])
			}
		}

		// Next (utf8.codes)
		if err := ValidCodesInit(tc.s); err != nil {
			t.Fatalf("ValidCodesInit(%x) err: %v", tc.s, err)
		}
		var pos int64
		idx := 0
		for {
			np, cp, ok, err := Next(tc.s, pos, tc.lax)
			if err != nil {
				t.Fatalf("Next(%x, %d) err: %v", tc.s, pos, err)
			}
			if !ok {
				break
			}
			offStart, _, offOk, offErr := OffsetDefault(tc.s, int64(idx+1))
			if offErr != nil || !offOk || offStart != np || cp != tc.cps[idx] {
				t.Fatalf("Next mismatch at %d: np=%d cp=%d offStart=%d", idx, np, cp, offStart)
			}
			pos = np
			idx++
		}
		if idx != len(tc.cps) {
			t.Fatalf("Next yielded %d items, want %d", idx, len(tc.cps))
		}

		// CharPattern via GMatch
		it := GMatch(tc.s, CharPattern, 1)
		idx = 0
		for {
			caps, ok, err := it.Next()
			if err != nil {
				t.Fatalf("GMatch(CharPattern) err: %v", err)
			}
			if !ok {
				break
			}
			wantChar, _ := Char([]int64{tc.cps[idx]}, true)
			if caps[0].Str != wantChar {
				t.Fatalf("GMatch(CharPattern)[%d] = %x, want %x", idx, caps[0].Str, wantChar)
			}
			idx++
		}
		if idx != len(tc.cps) {
			t.Fatalf("GMatch(CharPattern) count = %d, want %d", idx, len(tc.cps))
		}

		// Lua 5.5.1 two-return Offset invariant (utf8.lua lines 56-85)
		expected := int64(1)
		for i := int64(1); i <= cnt+1; i++ {
			p, e, ok, err := OffsetDefault(tc.s, i)
			if err != nil || !ok || p != expected {
				t.Fatalf("OffsetDefault(%x, %d) = (%d,%d,%v,%v), want p=%d", tc.s, i, p, e, ok, err, expected)
			}
			if i <= cnt {
				for j := p; j <= e; j++ {
					o1, o2, ok0, err0 := Offset(tc.s, 0, j)
					if err0 != nil || !ok0 || o1 != p || o2 != e {
						t.Fatalf("Offset(%x, 0, %d) = (%d,%d,%v,%v), want (%d,%d)", tc.s, j, o1, o2, ok0, err0, p, e)
					}
				}
			}
			expected = e + 1
		}
		if expected-1 != int64(len(tc.s)+1) {
			t.Fatalf("final expected-1 = %d, want %d", expected-1, len(tc.s)+1)
		}
	}

	// Differential test against lua55_o2 for utf8.offset (2 returns!), utf8.len, utf8.codepoint, utf8.codes
	type offsetCase struct {
		s      string
		n, i   int64
		hasArg bool
	}
	offsetCases := []offsetCase{
		{"", 0, 1, true},
		{"", 1, 1, true},
		{"", -1, 1, true},
		{"", 2, 1, true},
		{"alo", 5, 1, true},
		{"alo", -4, 4, true},
		{"abc", 0, 4, true},
		{"abc", 1, 4, true},
		{"abc", 2, 4, true},
		{"abc", -1, 4, true},
		{"abc", -3, 4, true},
		{"abc", -4, 4, true},
		{"汉字/漢字", 1, 1, true},
		{"汉字/漢字", 2, 1, true},
		{"汉字/漢字", 3, 1, true},
		{"汉字/漢字", 5, 1, true},
		{"汉字/漢字", 6, 1, true},
		{"汉字/漢字", 7, 1, true},
		{"汉字/漢字", 0, 1, true},
		{"汉字/漢字", 0, 2, true},
		{"汉字/漢字", 0, 3, true},
		{"汉字/漢字", 0, 5, true},
		{"汉字/漢字", -1, 14, true},
		{"汉字/漢字", -2, -1, true},
		{"\xe0", 1, 1, true},
		{"\xe0\x9e", -1, 3, true},
		{"a\x80", 0, 2, true},
		{"\xe0\x80", 0, 2, true},
		{"\x80", 0, 1, true},
		{"\x80\x80", 0, 2, true},
		{"\x80", 1, 1, true},
		{"\x9c", -1, 2, true},
		{"𦧺", 1, 2, true},
		{"abc", 1, 5, true},
		{"abc", 1, -4, true},
		{"", 1, 2, true},
		{"", 1, -1, true},
	}

	type lenCase struct {
		s    string
		i, j int64
		lax  bool
	}
	lenCases := []lenCase{
		{"", 1, -1, false},
		{"abc", 4, 3, false},
		{"abc", 4, 4, false},
		{"abc", 0, 2, false},
		{"abc\xe3def", 1, -1, false},
		{"\xf4\x9f\xbf", 1, -1, false},
		{"\xf4\x9f\xbf\xbf", 1, -1, false},
		{"\xf4\x9f\xbf\xbf", 1, -1, true},
		{"汉字\x80", 1, -1, false},
		{"\x80hello", 1, -1, false},
		{"hel\x80lo", 1, -1, false},
		{"\xc0\x80", 1, -1, false},
		{"\xc1\xbf", 1, -1, false},
		{"\xe0\x9f\xbf", 1, -1, false},
		{"\xf0\x8f\xbf\xbf", 1, -1, false},
		{"\xed\xa0\x80", 1, -1, false}, // U+D800 surrogate
		{"\xed\xa0\x80", 1, -1, true},
		{"\xff\x8f\x8f\x8f\x8f\x8f\x8f\x8f", 1, -1, true},
	}

	var script strings.Builder
	for i, tc := range offsetCases {
		fmt.Fprintf(&script, "do local ok,a,b=pcall(utf8.offset,%s,%d,%d); if not ok then print('O%d=ERR') elseif a==nil then print('O%d=NIL') else print('O%d='..a..':'..b) end end\n",
			luaQuote(tc.s), tc.n, tc.i, i, i, i)
	}
	for i, tc := range lenCases {
		fmt.Fprintf(&script, "do local ok,a,b=pcall(utf8.len,%s,%d,%d,%v); if not ok then print('L%d=ERR') elseif a==nil then print('L%d=FAIL:'..b) else print('L%d=OK:'..a) end end\n",
			luaQuote(tc.s), tc.i, tc.j, tc.lax, i, i, i)
	}

	out := runLua55(t, script.String())
	luaMap := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			luaMap[k] = v
		}
	}

	for i, tc := range offsetCases {
		st, en, ok, err := Offset(tc.s, tc.n, tc.i)
		var got string
		if err != nil {
			got = "ERR"
		} else if !ok {
			got = "NIL"
		} else {
			got = fmt.Sprintf("%d:%d", st, en)
		}
		want := luaMap[fmt.Sprintf("O%d", i)]
		if got != want {
			t.Errorf("Offset[%d] (%x, n=%d, i=%d): got %q (err=%v), lua55 %q", i, tc.s, tc.n, tc.i, got, err, want)
		}
	}

	for i, tc := range lenCases {
		cnt, errPos, err := Len(tc.s, tc.i, tc.j, tc.lax)
		var got string
		if err != nil {
			got = "ERR"
		} else if errPos > 0 {
			got = fmt.Sprintf("FAIL:%d", errPos)
		} else {
			got = fmt.Sprintf("OK:%d", cnt)
		}
		want := luaMap[fmt.Sprintf("L%d", i)]
		if got != want {
			t.Errorf("Len[%d] (%x, %d, %d, lax=%v): got %q (err=%v), lua55 %q", i, tc.s, tc.i, tc.j, tc.lax, got, err, want)
		}
	}

	// Next edge cases (utf8.lua lines 125-145)
	if err := ValidCodesInit("\x80"); err == nil {
		t.Fatalf("ValidCodesInit('\\x80') expected error")
	}
	if _, _, _, err := Next("a\x80", 0, false); err == nil || !strings.Contains(err.Error(), "invalid UTF-8 code") {
		t.Fatalf("Next('a\\x80', 0) expected invalid UTF-8 error, got %v", err)
	}
	for _, badPos := range []int64{2, -1, math.MinInt64} {
		_, _, ok, err := Next("", badPos, false)
		if err != nil || ok {
			t.Fatalf("Next('', %d) = (%v, %v), want (false, nil)", badPos, ok, err)
		}
	}
}

// =============================================================================
// 4. Number Parsing & FormatFloat Tests (numstr.go)
// =============================================================================

func TestNumStrDifferential(t *testing.T) {
	numInputs := []string{
		"0", "-0", "+0", "42", " -2 ", "\t\n\v\f\r 123 \r\n\t",
		"9223372036854775807", "-9223372036854775808",
		"9223372036854775808", "-9223372036854775809",
		"1" + strings.Repeat("0", 30), "-1" + strings.Repeat("0", 30),
		"0x0", "-0x0", "+0x2", " -0xaA ", "0xffffffffffffffff", "-0xffffffffffffffff",
		"0x1" + strings.Repeat("0", 30), "-0x8000000000000000", "0x8000000000000000",
		"0.0", "-0.0", "+0.01", "+.01", ".01", "-1.", "+1.", "0e12", ".2e2", "2.E-1",
		"-1.2e2", "+1.23E18", "1e10000", "-1e10000", "1e-10000", "-1e-10000",
		"0x1.fp-3", "  0x2.5  ", "  -0x2.5  ", "  +0x0.51p+8  ", "0x.FfffFFFF",
		"0xA.a", "0xa.aP4", "0x4P-2", "0x1.", "+0x.1", "0Xabcdef.0", "0x.ABCDEFp+24",
		"0x0p12", "-0x0p12", "0x.0p-3", "0x1p10000", "-0x1p10000", "0x1p-10000", "-0x1p-10000",
		"0x1p999999999999999", "0x1p-999999999999999", "0x0p999999999999999",
		"0x1p-1022", "0x1p-1023", "0x1p-1074", "0x1p-1075", "0x1.0000000000001p-1075",
		"0x1.fffffffffffffp+1023", "0x1.fffffffffffff8p+1023",
		"0x1.00000000000008p0", "0x1.00000000000018p0", "0x1.00000000000008000001p0",
		// Very long hex floats (math.lua lines 442-454)
		"0x" + strings.Repeat("f", 13) + ".0",
		"0x" + strings.Repeat("f", 150) + ".0",
		"0x" + strings.Repeat("f", 300) + ".0",
		"0x3." + strings.Repeat("0", 1000),
		"0x" + strings.Repeat("0", 1000) + "a",
		"0x0." + strings.Repeat("0", 13) + "1",
		"0x0." + strings.Repeat("0", 150) + "1",
		"0x0." + strings.Repeat("0", 300) + "1",
		"0xe03" + strings.Repeat("0", 1000) + "p-4000",
		"0x." + strings.Repeat("0", 1000) + "74p4004",
		// Invalid inputs (must ALL fail!)
		"", "  ", "-", "+", ".", "+ 0.01", "+.e1", "1e", "1.0e+", "1_000", "0x1_0", "0x1_0p0",
		"inf", "+inf", "-inf", "INF", "infinity", "Infinity", "nan", "NaN", "-nan", "+nan",
		"0x", " -0x ", "x", "x3", "0x3.3.3", "00x2", "0x 2", "0 x2", "23x", "- 0xaa",
		"-0xaaP ", "0x0.51p", "0x5p+-2", "0x.p1", "1  a", "1\x00", "1 \x00", "1\x00 ",
		"e1", "e  1", " 3.4.5 ",
	}

	var script strings.Builder
	script.WriteString(`
local function encnum(v)
  if v == nil then return "NIL" end
  if math.type(v) == "integer" then return "I:" .. tostring(v) end
  local b = string.pack("<d", v)
  return "F:" .. (b:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end
`)
	for i, s := range numInputs {
		fmt.Fprintf(&script, "print('N%d='..encnum(tonumber(%s)))\n", i, luaQuote(s))
	}

	// FormatFloat differential cases
	floatCases := []struct {
		v   float64
		lua string
	}{
		{0.0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1.0, "1.0"},
		{-1.0, "-1.0"},
		{1.1, "1.1"},
		{0.1, "0.1"},
		{0.3, "0.3"},
		{1.0 / 3.0, "1/3"},
		{math.Pi, "math.pi"},
		{1203.125, "1203.125"},
		{-1203.0, "-1203.0"},
		{1e13, "1e13"},
		{1e14, "1e14"},
		{1e15, "1e15"},
		{1e-4, "1e-4"},
		{1e-5, "1e-5"},
		{math.MaxFloat64, "1.7976931348623157e308"},
		{2.2250738585072014e-308, "2.2250738585072014e-308"},
		{math.SmallestNonzeroFloat64, "5e-324"},
		{math.Inf(1), "1/0"},
		{math.Inf(-1), "-1/0"},
		{math.NaN(), "0/0"},
	}
	for i, fc := range floatCases {
		fmt.Fprintf(&script, "print('T%d='..tostring(%s))\n", i, fc.lua)
	}

	// StrToIntBase differential cases (math.lua lines 421-438, 467-474)
	baseCases := []struct {
		s    string
		base int
	}{
		{"  001010  ", 2},
		{"  -1010  ", 2},
		{"10", 36},
		{"  -10  ", 36},
		{"  +1Z  ", 36},
		{"  -1z  ", 36},
		{"-fFfa", 16},
		{"ffffFFFF", 16},
		{"-0ffffffFFFF", 16},
		{"\t10000000000\t", 10},
		{"fFfa", 15},
		{"099", 8},
		{"1\x00", 2},
		{"", 8},
		{"  ", 9},
		{"0xf", 10},
		{"1  a", 2},
	}
	for i, bc := range baseCases {
		fmt.Fprintf(&script, "print('B%d='..encnum(tonumber(%s,%d)))\n", i, luaQuote(bc.s), bc.base)
	}

	out := runLua55(t, script.String())
	luaMap := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			luaMap[k] = v
		}
	}

	for i, s := range numInputs {
		iv, fv, isInt, ok := StrToNum(s)
		var got string
		if !ok {
			got = "NIL"
		} else if isInt {
			got = fmt.Sprintf("I:%d", iv)
		} else {
			p, _ := Pack("<d", PackFloatVal(fv))
			got = "F:" + hex.EncodeToString([]byte(p))
		}
		want := luaMap[fmt.Sprintf("N%d", i)]
		if got != want {
			t.Errorf("StrToNum[%d] (%q): got %s, lua55 %s", i, s, got, want)
		}
	}

	for i, fc := range floatCases {
		got := FormatFloat(fc.v)
		want := luaMap[fmt.Sprintf("T%d", i)]
		if got != want {
			t.Errorf("FormatFloat[%d] (%v): got %q, lua55 %q", i, fc.v, got, want)
		}
	}

	for i, bc := range baseCases {
		iv, ok := StrToIntBase(bc.s, bc.base)
		got := "NIL"
		if ok {
			got = fmt.Sprintf("I:%d", iv)
		}
		want := luaMap[fmt.Sprintf("B%d", i)]
		if got != want {
			t.Errorf("StrToIntBase[%d] (%q, %d): got %s, lua55 %s", i, bc.s, bc.base, got, want)
		}
	}
}

// =============================================================================
// 5. string.format Tests (format.go)
// =============================================================================

func TestFormatDifferential(t *testing.T) {
	type fmtCase struct {
		fmtStr string
		args   []FormatArg
		luaExp string
	}

	longPct := strings.Repeat("%", 2000)
	cases := []fmtCase{
		{"", nil, `string.format("")`},
		{"%%%d %010d", []FormatArg{IntArg(10), IntArg(23)}, `string.format("%%%d %010d", 10, 23)`},
		{"%#12o", []FormatArg{IntArg(10)}, `string.format("%#12o", 10)`},
		{"%#10x", []FormatArg{IntArg(100)}, `string.format("%#10x", 100)`},
		{"%#-17X", []FormatArg{IntArg(100)}, `string.format("%#-17X", 100)`},
		{"%013i", []FormatArg{IntArg(-100)}, `string.format("%013i", -100)`},
		{"%2.5d", []FormatArg{IntArg(-100)}, `string.format("%2.5d", -100)`},
		{"%.u", []FormatArg{IntArg(0)}, `string.format("%.u", 0)`},
		{"%.0d", []FormatArg{IntArg(0)}, `string.format("%.0d", 0)`},
		{"%#.0o", []FormatArg{IntArg(0)}, `string.format("%#.0o", 0)`},
		{"%#o", []FormatArg{IntArg(0)}, `string.format("%#o", 0)`},
		{"%#.4o", []FormatArg{IntArg(8)}, `string.format("%#.4o", 8)`},
		{"%#x", []FormatArg{IntArg(0)}, `string.format("%#x", 0)`},
		{"%#.0x", []FormatArg{IntArg(0)}, `string.format("%#.0x", 0)`},
		{"%05.3d", []FormatArg{IntArg(42)}, `string.format("%05.3d", 42)`},
		{"%05.0d", []FormatArg{IntArg(0)}, `string.format("%05.0d", 0)`},
		{"%+05.3d", []FormatArg{IntArg(42)}, `string.format("%+05.3d", 42)`},
		{"% 05d", []FormatArg{IntArg(42)}, `string.format("% 05d", 42)`},
		{"%#08x", []FormatArg{IntArg(42)}, `string.format("%#08x", 42)`},
		{"%+08d", []FormatArg{IntArg(31501)}, `string.format("%+08d", 31501)`},
		{"%+08d", []FormatArg{IntArg(-30927)}, `string.format("%+08d", -30927)`},
		{"%d %d %u %x %X", []FormatArg{
			IntArg(math.MaxInt64), IntArg(math.MinInt64), IntArg(-1), IntArg(math.MinInt64), IntArg(0x8f000003),
		}, `string.format("%d %d %u %x %X", math.maxinteger, math.mininteger, -1, math.mininteger, 0x8f000003)`},
		{"%x %02x", []FormatArg{FloatArg(0.0), FloatArg(0.0)}, `string.format("%x %02x", 0.0, 0.0)`},
		{"%d", []FormatArg{FloatArg(float64(int64(1) << 53))}, `string.format("%d", 2^53)`},
		{"%+#014.0f", []FormatArg{FloatArg(100)}, `string.format("%+#014.0f", 100)`},
		{"%+.3G", []FormatArg{FloatArg(1.5)}, `string.format("%+.3G", 1.5)`},
		{"% 1.0E", []FormatArg{FloatArg(100)}, `string.format("% 1.0E", 100)`},
		{"% .1g", []FormatArg{FloatArg(1024)}, `string.format("% .1g", 2^10)`},
		{"%#g|%#g|%#.0g|%#.3g|%.0g|%#.0g|%#g|%#g", []FormatArg{
			FloatArg(1.0), FloatArg(100.0), FloatArg(1.0), FloatArg(1.0),
			FloatArg(0.0), FloatArg(0.0), FloatArg(1e6), FloatArg(1e-5),
		}, `string.format("%#g|%#g|%#.0g|%#.3g|%.0g|%#.0g|%#g|%#g", 1.0, 100.0, 1.0, 1.0, 0.0, 0.0, 1e6, 1e-5)`},
		{"%f|%f|%f|%E|%G|%010f|%+10f", []FormatArg{
			FloatArg(math.Inf(1)), FloatArg(math.Inf(-1)), FloatArg(math.NaN()),
			FloatArg(math.Inf(1)), FloatArg(math.NaN()), FloatArg(math.Inf(1)), FloatArg(math.Inf(1)),
		}, `string.format("%f|%f|%f|%E|%G|%010f|%+10f", 1/0, -1/0, 0/0, 1/0, 0/0, 1/0, 1/0)`},
		{"%.99f", []FormatArg{FloatArg(-1e308)}, `string.format("%.99f", -1e308)`},
		{"%.0e|%#.0e", []FormatArg{FloatArg(1.5), FloatArg(1.5)}, `string.format("%.0e|%#.0e", 1.5, 1.5)`},
		// %a / %A (exact hex float formatting)
		{"%a|%A|%a|%A", []FormatArg{FloatArg(0.0), FloatArg(0.0), FloatArg(math.Copysign(0, -1)), FloatArg(math.Copysign(0, -1))},
			`string.format("%a|%A|%a|%A", 0.0, 0.0, -0.0, -0.0)`},
		{"%a|%A|%a|%A", []FormatArg{FloatArg(0.1), FloatArg(-0.1), FloatArg(1.0 / 3.0), FloatArg(math.Pi)},
			`string.format("%a|%A|%a|%A", 0.1, -0.1, 1/3, math.pi)`},
		{"%a|%a|%a", []FormatArg{FloatArg(2.2250738585072014e-308), FloatArg(1.1125369292536007e-308), FloatArg(5e-324)},
			`string.format("%a|%a|%a", 2^-1022, 2^-1023, 5e-324)`},
		{"%+.2A|%.4A|%010a|%-10a|%#a|%#.0a|% a", []FormatArg{
			FloatArg(12.0), FloatArg(-12.0), FloatArg(-1.5), FloatArg(1.5), FloatArg(1.0), FloatArg(1.0), FloatArg(1.5),
		}, `string.format("%+.2A|%.4A|%010a|%-10a|%#a|%#.0a|% a", 12, -12, -1.5, 1.5, 1.0, 1.0, 1.5)`},
		{"%a|%A|%a|%010a|%+10a|%010A", []FormatArg{
			FloatArg(math.Inf(1)), FloatArg(math.Inf(-1)), FloatArg(math.NaN()),
			FloatArg(math.Inf(1)), FloatArg(math.Inf(1)), FloatArg(math.NaN()),
		}, `string.format("%a|%A|%a|%010a|%+10a|%010A", 1/0, -1/0, 0/0, 1/0, 1/0, 0/0)`},
		// %c
		{"\x00%c\x00%c%x\x00", []FormatArg{IntArg(0xe4), IntArg('b'), IntArg(140)},
			`string.format("\0%c\0%c%x\0", 0xe4, string.byte("b"), 140)`},
		{"%-16c|%5c|%5c", []FormatArg{IntArg(97), IntArg(65), IntArg(0)},
			`string.format("%-16c|%5c|%5c", 97, 65, 0)`},
		// %s
		{"%s %s %.4s %.3s %.0s %.s", []FormatArg{
			NilArg(), BoolArg(true), BoolArg(false), BoolArg(true), StringArg("alo"), StringArg("alo"),
		}, `string.format("%s %s %.4s %.3s %.0s %.s", nil, true, false, true, "alo", "alo")`},
		{"\x00%s\x00", []FormatArg{StringArg("\x00\x00\x01")}, `string.format("\0%s\0", "\0\0\1")`},
		{`"%-50s"`, []FormatArg{StringArg("a")}, `string.format('"%-50s"', 'a')`},
		{"-%.20s.20s", []FormatArg{StringArg(longPct)}, `string.format("-%.20s.20s", string.rep("%", 2000))`},
		{`"-%20s.20s"`, []FormatArg{StringArg(longPct)}, `string.format('"-%20s.20s"', string.rep("%", 2000))`},
		// %p
		{"%p|%10p|%-12p", []FormatArg{NilArg(), BoolArg(false), FloatArg(1.5)},
			`string.format("%p|%10p|%-12p", nil, false, 1.5)`},
		// %q
		{"%q%s", []FormatArg{StringArg("\"ílo\"\n\\"), StringArg("\"ílo\"\n\\")},
			`string.format('%q%s', '"ílo"\n\\', '"ílo"\n\\')`},
		{"%q|%q|%q", []FormatArg{
			StringArg("\x00"),
			StringArg("\x00\x01\x023\x05\x009\x7f0\x7fa\r\t\n\"\\"),
			StringArg("\x00\x00\x01\xff\u0234"),
		}, `string.format("%q|%q|%q", "\0", "\0\1\0023\5\0009\1270\127a\r\t\n\"\\", "\0\0\1\255\u{234}")`},
		{"%q|%q|%q|%q|%q|%q|%q|%q|%q|%q", []FormatArg{
			IntArg(math.MaxInt64), IntArg(math.MinInt64), IntArg(0),
			FloatArg(math.Pi), FloatArg(0.1), FloatArg(math.Inf(1)), FloatArg(math.Inf(-1)), FloatArg(math.NaN()),
			BoolArg(true), NilArg(),
		}, `string.format("%q|%q|%q|%q|%q|%q|%q|%q|%q|%q", math.maxinteger, math.mininteger, 0, math.pi, 0.1, math.huge, -math.huge, 0/0, true, nil)`},
	}

	var script strings.Builder
	script.WriteString(`
local function tohex(s)
  return (s:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end
`)
	for i, tc := range cases {
		fmt.Fprintf(&script, "print('F%d='..tohex(%s))\n", i, tc.luaExp)
	}
	out := runLua55(t, script.String())
	luaMap := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			luaMap[k] = v
		}
	}

	for i, tc := range cases {
		got, err := Format(tc.fmtStr, tc.args)
		if err != nil {
			t.Fatalf("Format[%d] (%q) err: %v", i, tc.fmtStr, err)
		}
		gotHex := hex.EncodeToString([]byte(got))
		wantHex := luaMap[fmt.Sprintf("F%d", i)]
		if gotHex != wantHex {
			wantBytes, _ := hex.DecodeString(wantHex)
			t.Errorf("Format[%d] (%q) mismatch:\n  got:   %q\n  lua55: %q", i, tc.fmtStr, got, string(wantBytes))
		}
	}

	// Non-null %p formatting check
	pRes, err := Format("%20p|%-20p", []FormatArg{PointerArg(0x1234abcd), PointerArg(0x1234abcd)})
	if err != nil || pRes != "          0x1234abcd|0x1234abcd          " {
		t.Fatalf("Format(%%p non-null) = (%q, %v)", pRes, err)
	}

	// Exact ISO C99 round-to-nearest-even for %a precision rounding
	hexRoundCases := []struct {
		fmtStr string
		val    float64
		want   string
	}{
		{"%.0a", 1.5, "0x2p+0"},        // 0x1.8p+0 -> ties to even 2
		{"%.0a", 1.4999999, "0x1p+0"},  // < 1.5 -> 1
		{"%.0a", 1.5000001, "0x2p+0"},  // > 1.5 -> 2
		{"%.1a", 0x1.ffp0, "0x2.0p+0"}, // carry into leading digit
		{"%.0a", 0x1.ffp0, "0x2p+0"},
		{"%.1a", 0x1.08p0, "0x1.0p+0"}, // ties to even (0 is even)
		{"%.1a", 0x1.18p0, "0x1.2p+0"}, // ties to even (1 is odd -> 2)
		{"%.1a", 0x1.080001p0, "0x1.1p+0"},
	}
	for _, tc := range hexRoundCases {
		got, err := Format(tc.fmtStr, []FormatArg{FloatArg(tc.val)})
		if err != nil || got != tc.want {
			t.Errorf("Format(%q, %x) = (%q, %v), want %q", tc.fmtStr, tc.val, got, err, tc.want)
		}
	}

	// Verify all %q outputs roundtrip via lua55_o2 load("return " .. s)()
	var qCheck strings.Builder
	qArgs := []FormatArg{
		StringArg("\x00\x00\x01\xff\u0234\n\"\\\r\t0123"),
		IntArg(math.MaxInt64),
		IntArg(math.MinInt64),
		FloatArg(math.Pi),
		FloatArg(0.1),
		FloatArg(math.Copysign(0, -1)),
		FloatArg(5e-324),
		FloatArg(math.Inf(1)),
		FloatArg(math.Inf(-1)),
		BoolArg(true),
		BoolArg(false),
		NilArg(),
	}
	for i, a := range qArgs {
		s, err := Format("%q", []FormatArg{a})
		if err != nil {
			t.Fatalf("Format(%%q) err: %v", err)
		}
		fmt.Fprintf(&qCheck, "local v%d = assert(load('return ' .. %s))()\n", i, luaQuote(s))
	}
	qCheck.WriteString("assert(v1 == math.maxinteger and math.type(v1) == 'integer')\n")
	qCheck.WriteString("assert(v2 == math.mininteger and math.type(v2) == 'integer')\n")
	qCheck.WriteString("assert(v3 == math.pi and v4 == 0.1 and 1/v5 == -math.huge and v6 == 5e-324)\n")
	qCheck.WriteString("assert(v7 == math.huge and v8 == -math.huge and v9 == true and v10 == false and v11 == nil)\n")
	qCheck.WriteString("print('OK')\n")
	if strings.TrimSpace(runLua55(t, qCheck.String())) != "OK" {
		t.Fatalf("%%q roundtrip in lua55_o2 failed")
	}

	// All error cases from strings.lua lines 370-392 & 242, 246
	aux := strings.Repeat("0", 600)
	errCases := []struct {
		fmtStr string
		args   []FormatArg
		want   string
	}{
		{"%100.3d", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%1" + aux + ".3d", []FormatArg{IntArg(10)}, "too long"},
		{"%1.100d", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%10.1" + aux + "004d", []FormatArg{IntArg(10)}, "too long"},
		{"%t", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%" + aux + "d", []FormatArg{IntArg(10)}, "too long"},
		{"%d %d", []FormatArg{IntArg(10)}, "no value"},
		{"%010c", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%.10c", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%0.34s", []FormatArg{StringArg("a")}, "invalid conversion"},
		{"%#i", []FormatArg{IntArg(10)}, "invalid conversion"},
		{"%3.1p", []FormatArg{NilArg()}, "invalid conversion"},
		{"%0.s", []FormatArg{StringArg("a")}, "invalid conversion"},
		{"%10q", []FormatArg{IntArg(10)}, "cannot have modifiers"},
		{"%F", []FormatArg{FloatArg(10)}, "invalid conversion"},
		{"%q", []FormatArg{PointerArg(1)}, "no literal"},
		{"%10s", []FormatArg{StringArg("\x00")}, "contains zeros"},
		{"%", []FormatArg{IntArg(1)}, "invalid conversion"},
		{"%d", []FormatArg{FloatArg(1.5)}, "no integer representation"},
		{"%d", []FormatArg{StringArg("notanumber")}, "number expected"},
	}
	for _, tc := range errCases {
		if _, err := Format(tc.fmtStr, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Format(%q): expected error containing %q, got %v", tc.fmtStr, tc.want, err)
		}
	}
}

func TestCoercionsAndEdgePaths(t *testing.T) {
	// Pack coercions: float->int, string->int, int->float, string->float, int/float->string, missing args
	s, err := Pack("=i4 f d c5 z",
		PackFloatVal(42.0),
		PackIntVal(3),
		PackStringVal("2.5"),
		PackIntVal(12345),
		PackFloatVal(1.5),
	)
	if err != nil {
		t.Fatalf("Pack coercions failed: %v", err)
	}
	vals, _, err := Unpack("=i4 f d c5 z", s, 1)
	if err != nil || vals[0].Int != 42 || vals[1].Flt != 3 || vals[2].Flt != 2.5 || vals[3].Str != "12345" || vals[4].Str != "1.5" {
		t.Fatalf("Unpack coerced values = (%v, %v)", vals, err)
	}
	s2, err := Pack("i2 i2 f", PackStringVal("10"), PackStringVal("20.0"), PackStringVal("0x1.8p+1"))
	if err != nil {
		t.Fatalf("Pack string-to-num failed: %v", err)
	}
	vals2, _, err := Unpack("i2 i2 f", s2, 1)
	if err != nil || vals2[0].Int != 10 || vals2[1].Int != 20 || vals2[2].Flt != 3.0 {
		t.Fatalf("Unpack string-to-num = (%v, %v)", vals2, err)
	}
	for _, tc := range []struct {
		f    string
		args []PackValue
		want string
	}{
		{"i4", nil, "value expected"},
		{"i4", []PackValue{PackFloatVal(1.5)}, "no integer representation"},
		{"i4", []PackValue{PackStringVal("1.5")}, "no integer representation"},
		{"i4", []PackValue{PackStringVal("abc")}, "number expected"},
		{"f", []PackValue{PackStringVal("abc")}, "number expected"},
		{"d", []PackValue{PackStringVal("abc")}, "number expected"},
	} {
		if _, err := Pack(tc.f, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Pack(%q) want err %q, got %v", tc.f, tc.want, err)
		}
	}

	// Format coercions & edge cases
	out, err := Format("%d|%d|%f|%f|%f|%s|%s|%s|%s|%#.0G|%020a", []FormatArg{
		StringArg("123"), StringArg("45.0"),
		IntArg(7), StringArg("8"), StringArg("0x1.8p+0"),
		IntArg(99), FloatArg(1.5), PointerArg(0xab), PointerArg(0),
		FloatArg(1e6), FloatArg(1.0),
	})
	if err != nil {
		t.Fatalf("Format coercions err: %v", err)
	}
	want := "123|45|7.000000|8.000000|1.500000|99|1.5|0xab|(null)|1.E+06|0x000000000000001p+0"
	if out != want {
		t.Fatalf("Format coercions:\n  got:  %q\n  want: %q", out, want)
	}
	if _, err := Format("%d", []FormatArg{StringArg("1.5")}); err == nil || !strings.Contains(err.Error(), "no integer representation") {
		t.Fatalf("Format string float to %%d: got %v", err)
	}
	if _, err := Format("%f", []FormatArg{StringArg("nope")}); err == nil || !strings.Contains(err.Error(), "number expected") {
		t.Fatalf("Format bad string to %%f: got %v", err)
	}
	if _, err := Format("%d", []FormatArg{NilArg()}); err == nil || !strings.Contains(err.Error(), "number expected") {
		t.Fatalf("Format nil to %%d: got %v", err)
	}
	if _, err := Format("%f", []FormatArg{NilArg()}); err == nil || !strings.Contains(err.Error(), "number expected") {
		t.Fatalf("Format nil to %%f: got %v", err)
	}

	// UTF-8 Codepoint bounds, empty interval, invalid UTF-8, OffsetDefault negative n
	if cps, err := Codepoint("abc", 3, 2, false); err != nil || len(cps) != 0 {
		t.Fatalf("Codepoint empty interval = (%v, %v)", cps, err)
	}
	if _, err := Codepoint("abc", -4, 1, false); err == nil || !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("Codepoint low bound: got %v", err)
	}
	if _, err := Codepoint("abc", 1, 4, false); err == nil || !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("Codepoint high bound: got %v", err)
	}
	if _, err := Codepoint("\x80", 1, 1, false); err == nil || !strings.Contains(err.Error(), "invalid UTF-8 code") {
		t.Fatalf("Codepoint invalid utf8: got %v", err)
	}
	st, en, ok, err := OffsetDefault("汉字", -1)
	if err != nil || !ok || st != 4 || en != 6 {
		t.Fatalf("OffsetDefault('汉字', -1) = (%d, %d, %v, %v)", st, en, ok, err)
	}

	// PM callback error propagation and Match init > len
	sentinel := fmt.Errorf("boom")
	if _, _, err := GSub("abc", ".", func([]CaptureVal) (string, bool, error) { return "", false, sentinel }, -1); err != sentinel {
		t.Fatalf("GSub err propagation = %v", err)
	}
	if _, _, err := GSubTable("abc", ".", func(CaptureVal) (string, bool, error) { return "", false, sentinel }, -1); err != sentinel {
		t.Fatalf("GSubTable err propagation = %v", err)
	}
	if _, ok, err := Match("abc", "a", 10); err != nil || ok {
		t.Fatalf("Match init>len = (%v, %v)", ok, err)
	}
	if _, ok := StrToIntBase("10", 1); ok {
		t.Fatalf("StrToIntBase invalid base 1 should fail")
	}
	if _, ok := StrToIntBase("10", 37); ok {
		t.Fatalf("StrToIntBase invalid base 37 should fail")
	}
}
