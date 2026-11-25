package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfficialMainLuaCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("official Lua 5.5.1 testes/main.lua requires POSIX shell and signals")
	}
	testesDir := os.Getenv("LUA55_TESTES")
	if testesDir == "" {
		testesDir = filepath.Join("..", "..", "third_party", "lua", "testes")
	}
	if abs, err := filepath.Abs(testesDir); err == nil {
		testesDir = abs
	}
	mainLuaPath := filepath.Join(testesDir, "main.lua")
	if _, err := os.Stat(mainLuaPath); err != nil {
		t.Skipf("official Lua 5.5.1 testes/main.lua not found at %s (run `git submodule update --init` or set LUA55_TESTES)", mainLuaPath)
	}

	bin := filepath.Join(t.TempDir(), "moelua")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/moelua failed: %v\n%s", err, out)
	}

	srcBytes, err := os.ReadFile(mainLuaPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(srcBytes)

	// Pure-Go MoeLua does not load C shared libraries (.so via dlopen); omit only the
	// 4-line C .so test (lines 250-253: lib2-v2.so) by exact content match.
	const cSharedLibBlock = `-- test module names with version suffix ("libs/lib2-v2")
RUN("env LUA_CPATH='./libs/?.so' lua -l lib2-v2 -e 'print(lib2.id())' > %s",
    out)
checkout("true\n")`
	patched := strings.Replace(src, cSharedLibBlock, "-- C .so dlopen omitted in pure-Go CGO_ENABLED=0 build", 1)
	if patched == src {
		t.Fatalf("could not locate C shared library block in main.lua")
	}

	patchedPath := filepath.Join(t.TempDir(), "main_purego.lua")
	if err := os.WriteFile(patchedPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, patchedPath)
	cmd.Dir = testesDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("official main.lua failed: %v\n--- output ---\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("main.lua did not print OK:\n%s", out.String())
	}
}

func TestCLIFlagsREPLAndStdinParity(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "moelua")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/moelua failed: %v\n%s", err, out)
	}

	run := func(dir, stdin string, env []string, args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		if dir != "" {
			cmd.Dir = dir
		}
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	// 1. arg[0] keeps executable name when no script is supplied.
	t.Run("arg0_without_script", func(t *testing.T) {
		out, stderr, err := run("", "", nil, "-e", "print(arg[0], arg[1])")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if !strings.Contains(out, bin) || !strings.Contains(out, "-e") {
			t.Fatalf("expected executable in arg[0] and -e in arg[1], got %q", out)
		}
	})

	// 2. Literal '-' filename after '--' reads file instead of stdin.
	t.Run("double_dash_literal_dash_file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "-"), []byte(`print("FILE")`), 0o644); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := run(dir, `print("STDIN")`+"\n", nil, "--", "-")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "FILE" {
			t.Fatalf("expected FILE, got %q", out)
		}
	})

	// 3. -l does not suppress stdin execution.
	t.Run("library_load_allows_stdin", func(t *testing.T) {
		out, stderr, err := run("", `print("STDIN-RAN", type(math.sin))`+"\n", nil, "-l", "math")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "STDIN-RAN\tfunction" {
			t.Fatalf("expected STDIN-RAN, got %q", out)
		}
	})

	// 4. -l mod-v2 strips version suffix from implicit global name.
	t.Run("library_version_suffix_stripped", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "mod-v2.lua"), []byte(`return { v = 77 }`), 0o644); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := run(dir, "", nil, "-l", "mod-v2", "-e", "print(mod.v)")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "77" {
			t.Fatalf("expected 77, got %q", out)
		}
	})

	// 5. -W processed sequentially in runArgs.
	t.Run("warnings_sequential_order", func(t *testing.T) {
		_, stderr, err := run("", "", nil, "-W", "-e", `warn("@off")`, "-W", "-e", `warn("SHOULD-PRINT")`)
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if !strings.Contains(stderr, "SHOULD-PRINT") {
			t.Fatalf("expected SHOULD-PRINT warning on stderr, got %q", stderr)
		}
	})

	// 6. REPL and Lua io.read() share stdin buffering.
	t.Run("repl_shares_stdin_with_ioread", func(t *testing.T) {
		in := "print(\"GOT:\" .. tostring(io.read()))\nDATA-LINE\n"
		out, stderr, err := run("", in, nil, "-i")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if !strings.Contains(out, "GOT:DATA-LINE") {
			t.Fatalf("expected GOT:DATA-LINE in REPL output, got %q", out)
		}
	})

	// 7. /dev/null is not treated as an interactive terminal.
	if runtime.GOOS != "windows" {
		t.Run("dev_null_not_terminal", func(t *testing.T) {
			devNull, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer devNull.Close()
			cmd := exec.Command(bin)
			cmd.Stdin = devNull
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("err=%v stderr=%s", err, stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("expected empty output for </dev/null, got stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}
