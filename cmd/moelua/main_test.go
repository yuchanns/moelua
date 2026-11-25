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
