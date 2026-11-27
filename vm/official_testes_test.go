package vm_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/yuchanns/moelua/compiler"
	"github.com/yuchanns/moelua/vm"
)

func testesDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("LUA55_TESTES")
	if dir == "" {
		dir = filepath.Join("..", "third_party", "lua", "testes")
	}
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	if _, err := os.Stat(filepath.Join(dir, "all.lua")); err != nil {
		t.Skipf("Lua 5.5.1 testes not found at %s (run `git submodule update --init` or set LUA55_TESTES): %v", dir, err)
	}
	return dir
}

// runOfficialTestSource executes Lua source of one official testes file inside MoeLua
// with the official portable _U=true globals (_soft=true, _port=true, _nomsg=true, T=nil).
// When hookCount > 0, installs a no-op debug count hook before executing the chunk so the
// entire file executes inside (*State).runHooked.
func runOfficialTestSource(t *testing.T, dir, file, src string, hookCount int) (bool, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	proto, err := compiler.Compile("@"+file, src, compiler.SkipFileHeader(true))
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}

	s := vm.NewState()
	defer s.Close()
	var out bytes.Buffer
	s.SetOutput(&out)
	s.SetErrOutput(&out)

	// Set official Lua 5.5.1 portable test profile (_U = true)
	s.SetGlobal("_U", vm.TrueValue())
	s.SetGlobal("_soft", vm.TrueValue())
	s.SetGlobal("_port", vm.TrueValue())
	s.SetGlobal("_nomsg", vm.TrueValue())
	pkg := s.AsTable(s.GetGlobal("package"))
	pkg.RawSetStr(s, s.Intern("path"), vm.StringValue(s.Intern(filepath.Join(dir, "?.lua")+";./?.lua")))

	cl, err := s.Load(proto)
	if err != nil {
		t.Fatalf("load %s: %v", file, err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s PANICKED: %v\n%s\n--- output tail ---\n%s", file, r, string(debug.Stack()), tailString(out.String(), 800))
		}
	}()
	// Run via a tiny Lua xpcall wrapper (non-tailcall!) so frameXPCall catches errors while full Lua stack is live!
	s.SetGlobal("__moelua_test_chunk", vm.ClosureValue(cl))
	harness := `local ok, err = xpcall(__moelua_test_chunk, debug.traceback); return ok, err`
	if hookCount > 0 {
		harness = `debug.sethook(function() end, "", 100000); local ok, err = xpcall(__moelua_test_chunk, debug.traceback); debug.sethook(); return ok, err`
	}
	wrapProto, err := compiler.Compile("=harness", harness)
	if err != nil {
		t.Fatalf("compile harness: %v", err)
	}
	wrapCl, err := s.Load(wrapProto)
	if err != nil {
		t.Fatalf("load harness: %v", err)
	}
	res, err := s.Call(vm.ClosureValue(wrapCl))
	if err != nil {
		return false, err.Error(), out.String()
	}
	if len(res) == 0 || res[0].IsFalsy() {
		var msg string
		if len(res) > 1 {
			msg = s.FormatValue(res[1])
		}
		return false, msg, out.String()
	}
	return true, "", out.String()
}

func runOfficialTestFile(t *testing.T, dir, file string) {
	t.Helper()
	srcBytes, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	ok, msg, out := runOfficialTestSource(t, dir, file, string(srcBytes), 0)
	if !ok {
		t.Fatalf("%s failed: %s\n--- output tail ---\n%s", file, msg, tailString(out, 1500))
	}
}

func hasWritableDevFull() bool {
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func tailString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}

var portableOfficialTestesFiles = []string{
	"bitwise.lua",
	"literals.lua",
	"tpack.lua",
	"utf8.lua",
	"pm.lua",
	"vararg.lua",
	"math.lua",
	"sort.lua",
	"constructs.lua",
	"goto.lua",
	"closure.lua",
	"strings.lua",
	"attrib.lua",
	"events.lua",
	"nextvar.lua",
	"calls.lua",
	"locals.lua",
	"coroutine.lua",
	"errors.lua",
	"db.lua",
	"gc.lua",
	"gengc.lua",
	"cstack.lua",
	"verybig.lua",
}

func TestOfficialLua55Testes(t *testing.T) {
	dir := testesDir(t)
	for _, f := range portableOfficialTestesFiles {
		t.Run(f, func(t *testing.T) {
			runOfficialTestFile(t, dir, f)
		})
	}
	t.Run("files.lua", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("files.lua exercises POSIX devices (/dev/null, /dev/full) and shell utilities (cat, ls)")
		}
		srcBytes, err := os.ReadFile(filepath.Join(dir, "files.lua"))
		if err != nil {
			t.Fatal(err)
		}
		src := string(srcBytes)
		if hasWritableDevFull() {
			ok, msg, out := runOfficialTestSource(t, dir, "files.lua", src, 0)
			if !ok {
				t.Fatalf("files.lua failed: %s\n--- output tail ---\n%s", msg, tailString(out, 1500))
			}
			return
		}
		// macOS has no writable /dev/full (even C lua55_o2 fails at files.lua:474).
		// 1. Verify unmodified files.lua reaches the exact same /dev/full failure at line 474.
		ok, msg, out := runOfficialTestSource(t, dir, "files.lua", src, 0)
		if ok || !strings.Contains(msg, "files.lua:474:") || !strings.Contains(msg, "/dev/full") {
			t.Fatalf("unmodified files.lua expected macOS /dev/full failure at line 474, got ok=%v msg=%s\n--- output tail ---\n%s",
				ok, msg, tailString(out, 1500))
		}
		// 2. Also execute the ENTIRE rest of files.lua (lines 483-1004) with only the 6-line /dev/full block skipped!
		const devFullBlock = `  local f = io.output("/dev/full")
  assert(f:write("abcd"))   -- write to buffer
  assert(not f:flush())     -- cannot write to device
  assert(f:write("abcd"))   -- write to buffer
  assert(not io.flush())    -- cannot write to device
  assert(f:close())`
		patched := strings.Replace(src, devFullBlock, "  -- /dev/full unavailable on host OS", 1)
		if patched == src {
			t.Fatalf("could not locate /dev/full block in files.lua")
		}
		ok2, msg2, out2 := runOfficialTestSource(t, dir, "files.lua", patched, 0)
		if !ok2 {
			t.Fatalf("files.lua (post-/dev/full) failed: %s\n--- output tail ---\n%s", msg2, tailString(out2, 1500))
		}
	})
	t.Run("big.lua", func(t *testing.T) {
		// big.lua is designed to run inside a coroutine (yields 'b', then returns 'a', matching all.lua).
		bigPath := filepath.Join(dir, "big.lua")
		src := fmt.Sprintf(`
			_soft = nil
			local fn, err = loadfile(%q)
			assert(fn, err)
			local co = coroutine.create(fn)
			local ok1, v1 = coroutine.resume(co)
			assert(ok1, tostring(v1))
			assert(v1 == 'b', "expected yield 'b', got " .. tostring(v1))
			local ok2, v2 = coroutine.resume(co)
			assert(ok2, tostring(v2))
			assert(v2 == 'a', "expected return 'a', got " .. tostring(v2))
		`, bigPath)
		ok, msg, out := runOfficialTestSource(t, dir, "big_driver.lua", src, 0)
		if !ok {
			t.Fatalf("big.lua failed: %s\n--- output tail ---\n%s", msg, tailString(out, 1500))
		}
	})
}

// TestOfficialLua55TestesHooked verifies the generated (*State).runHooked dispatch loop
// against the official Lua 5.5.1 portable test suite with a live no-op debug count hook.
func TestOfficialLua55TestesHooked(t *testing.T) {
	dir := testesDir(t)
	for _, f := range portableOfficialTestesFiles {
		// db.lua asserts debug.gethook() == nil on entry and exercises hooks exhaustively itself.
		if f == "db.lua" {
			continue
		}
		t.Run(f, func(t *testing.T) {
			srcBytes, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			ok, msg, out := runOfficialTestSource(t, dir, f, string(srcBytes), 100000)
			if !ok {
				t.Fatalf("%s (hooked) failed: %s\n--- output tail ---\n%s", f, msg, tailString(out, 1500))
			}
		})
	}
}
