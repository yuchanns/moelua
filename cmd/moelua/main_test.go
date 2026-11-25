package main_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

	// 8. Stdin script (`-`) consumes remaining bytes from the shared VM stdin buffer after `-e io.read()`.
	t.Run("stdin_script_shares_buffer_with_earlier_ioread", func(t *testing.T) {
		in := "HEADER\nprint('SCRIPT')\n"
		out, stderr, err := run("", in, nil, "-e", "print(io.read())", "-")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		lines := strings.Fields(strings.TrimSpace(out))
		if len(lines) != 2 || lines[0] != "HEADER" || lines[1] != "SCRIPT" {
			t.Fatalf("expected HEADER and SCRIPT, got %q", out)
		}
	})

	// 9. REPL continues reading stdin even after Lua code redirects default input via io.input(file).
	t.Run("repl_independent_of_io_input", func(t *testing.T) {
		dir := t.TempDir()
		dataPath := filepath.Join(dir, "data.txt")
		if err := os.WriteFile(dataPath, []byte("FROM_DATA_FILE\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		in := "io.input('data.txt')\nprint('DATA:' .. tostring(io.read()))\nprint('STILL_STDIN')\n"
		out, stderr, err := run(dir, in, nil, "-i")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if !strings.Contains(out, "DATA:FROM_DATA_FILE") || !strings.Contains(out, "STILL_STDIN") {
			t.Fatalf("expected DATA:FROM_DATA_FILE and STILL_STDIN, got %q", out)
		}
	})

	// 10. Script argument forwarding honors __len metamethod on `arg` (lua.c:pushargs).
	t.Run("script_args_honor_arg_len_metamethod", func(t *testing.T) {
		dir := t.TempDir()
		script := filepath.Join(dir, "echoargs.lua")
		if err := os.WriteFile(script, []byte(`print(select("#", ...), ...)`), 0o644); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := run(dir, "", nil, "-e", `setmetatable(arg, {__len = function() return 1 end})`, script, "first", "second")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "1\tfirst" {
			t.Fatalf("expected '1\\tfirst', got %q", out)
		}
	})

	// 11. -l honors _G's __index metamethod when resolving require.
	t.Run("library_load_honors_global_require_metamethod", func(t *testing.T) {
		out, stderr, err := run("", "", nil, "-E",
			"-e", `local r = require; require = nil; setmetatable(_G, {__index = {require = r}})`,
			"-l", "math",
			"-e", `print(math.floor(3.9))`,
		)
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "3" {
			t.Fatalf("expected '3', got %q", out)
		}
	})

	// 12. Explicitly empty script filename (`''` or `LUA_INIT=@`) fails to open rather than executing stdin.
	t.Run("empty_script_filename_does_not_read_stdin", func(t *testing.T) {
		out, stderr, err := run("", "print('EXECUTED_STDIN')\n", nil, "-E", "")
		if err == nil {
			t.Fatalf("expected error opening empty filename, got stdout=%q stderr=%q", out, stderr)
		}
		if strings.Contains(out, "EXECUTED_STDIN") {
			t.Fatalf("empty filename erroneously executed stdin: stdout=%q", out)
		}

		out2, stderr2, err2 := run("", "print('EXECUTED_STDIN')\n", []string{"LUA_INIT_5_5=@"}, "-e", "print('AFTER')")
		if err2 == nil {
			t.Fatalf("expected error for LUA_INIT_5_5=@, got stdout=%q stderr=%q", out2, stderr2)
		}
		if strings.Contains(out2, "EXECUTED_STDIN") {
			t.Fatalf("LUA_INIT_5_5=@ erroneously executed stdin: stdout=%q", out2)
		}
	})

	// 13. Second SIGINT terminates a script that caught the first SIGINT via pcall.
	if runtime.GOOS != "windows" {
		t.Run("repeated_sigint_terminates_pcall_loop", func(t *testing.T) {
			cmd := exec.Command(bin, "-E", "-e", `io.stdout:write("READY\n"); io.stdout:flush(); while true do pcall(function() while true do end end) end`)
			stdoutPipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Wait for the Lua script to print READY so signal handler is armed.
			buf := make([]byte, 16)
			if _, err := stdoutPipe.Read(buf); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("failed reading READY: %v", err)
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("first SIGINT failed: %v", err)
			}
			time.Sleep(150 * time.Millisecond)
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("second SIGINT failed: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("expected non-zero exit after two SIGINTs")
				}
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("process survived two SIGINTs spaced 150ms apart")
			}
		})
	}

	// 14. -l assigns nil when require returns zero values (matching lua.c:dolibrary).
	t.Run("library_nil_return_assigns_nil", func(t *testing.T) {
		out, stderr, err := run("", "", nil, "-E",
			"-e", `X = 123; require = function() end`,
			"-l", "X=foo",
			"-e", `print("X=" .. tostring(X))`,
		)
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if strings.TrimSpace(out) != "X=nil" {
			t.Fatalf("expected 'X=nil', got %q", out)
		}
	})

	// 15. Warnings enabled by LUA_INIT are reset off before CLI args unless -W is passed (lua.c:runargs).
	t.Run("lua_init_warnings_reset_before_runargs", func(t *testing.T) {
		_, stderr1, err1 := run("", "", []string{"LUA_INIT_5_5=warn('@on')"}, "-e", `warn("hidden_warn")`)
		if err1 != nil {
			t.Fatalf("err=%v stderr=%s", err1, stderr1)
		}
		if strings.Contains(stderr1, "hidden_warn") {
			t.Fatalf("expected LUA_INIT warnings to be reset off before -e, got stderr=%q", stderr1)
		}

		_, stderr2, err2 := run("", "", []string{"LUA_INIT_5_5=warn('@on')"}, "-W", "-e", `warn("shown_warn")`)
		if err2 != nil {
			t.Fatalf("err=%v stderr=%s", err2, stderr2)
		}
		if !strings.Contains(stderr2, "shown_warn") {
			t.Fatalf("expected -W to enable warnings, got stderr=%q", stderr2)
		}
	})

	// 16. Non-string error objects still include stack traceback (matching lua.c:msghandler).
	t.Run("non_string_error_preserves_traceback", func(t *testing.T) {
		_, stderr, err := run("", "", nil, "-E", "-e", `error({})`)
		if err == nil {
			t.Fatalf("expected error exit for error({})")
		}
		if !strings.Contains(stderr, "(error object is a table value)") || !strings.Contains(stderr, "stack traceback:") {
			t.Fatalf("expected '(error object is a table value)' with stack traceback, got stderr=%q", stderr)
		}

		_, stderr2, err2 := run("", "", nil, "-E", "-e", `error(setmetatable({}, {__tostring=function() return {} end}))`)
		if err2 == nil {
			t.Fatalf("expected error exit for non-string __tostring result")
		}
		if !strings.Contains(stderr2, "(error object is a table value)") || !strings.Contains(stderr2, "stack traceback:") {
			t.Fatalf("expected '(error object is a table value)' with stack traceback, got stderr=%q", stderr2)
		}
	})

	// 17. msgh looks up __tostring via rawget on the error object's metatable (matching luaL_getmetafield).
	t.Run("msgh_rawgets_tostring_metamethod", func(t *testing.T) {
		_, stderr, err := run("", "", nil, "-E", "-e", `local mt = setmetatable({}, {__index = function() error("unexpected metatable lookup") end}); error(setmetatable({}, mt))`)
		if err == nil {
			t.Fatalf("expected error exit")
		}
		if strings.Contains(stderr, "unexpected metatable lookup") {
			t.Fatalf("msgh erroneously invoked metatable __index: stderr=%q", stderr)
		}
		if !strings.Contains(stderr, "(error object is a table value)") || !strings.Contains(stderr, "stack traceback:") {
			t.Fatalf("expected original table error with traceback, got stderr=%q", stderr)
		}
	})

	// 18. REPL anchors expression results across metamethod-aware _G.print lookup.
	t.Run("repl_anchors_results_across_print_lookup", func(t *testing.T) {
		in := strings.Join([]string{
			`finalized = false`,
			`orig_print = print`,
			`print = nil`,
			`setmetatable(_G, { __index = function(_, k) if k == "print" then collectgarbage("collect"); return function(v) orig_print("FINALIZED=" .. tostring(finalized) .. " TAG=" .. tostring(v.tag)) end end end })`,
			`setmetatable({ tag = "alive" }, { __gc = function() finalized = true end })`,
			"",
		}, "\n")
		out, stderr, err := run("", in, nil, "-E", "-i")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		if !strings.Contains(out, "FINALIZED=false TAG=alive") {
			t.Fatalf("expected REPL result to remain anchored across _G.__index print lookup, got stdout=%q stderr=%q", out, stderr)
		}
	})

	// 19. -v banner matches Lua 5.5.1 copyright year (1994-2026).
	t.Run("version_banner_2026", func(t *testing.T) {
		out, stderr, err := run("", "", nil, "-v")
		if err != nil {
			t.Fatalf("err=%v stderr=%s", err, stderr)
		}
		const want = "Lua 5.5.1  Copyright (C) 1994-2026 Lua.org, PUC-Rio"
		if strings.TrimSpace(out) != want {
			t.Fatalf("expected %q, got %q", want, out)
		}
	})

	// 20. __tostring returning a string returns verbatim with NO traceback (lua.c:msghandler).
	t.Run("msgh_tostring_string_omits_traceback", func(t *testing.T) {
		_, stderr, err := run("", "", nil, "-E", "-e", `error(setmetatable({}, {__tostring=function() return "custom-msg" end}))`)
		if err == nil {
			t.Fatalf("expected non-zero exit")
		}
		if !strings.Contains(stderr, "custom-msg") {
			t.Fatalf("expected custom-msg in stderr, got %q", stderr)
		}
		if strings.Contains(stderr, "stack traceback:") {
			t.Fatalf("expected NO stack traceback when __tostring returns string, got stderr=%q", stderr)
		}
	})

	// 21. __tostring returning a number does NOT qualify as LUA_TSTRING; falls back with traceback (lua.c:msghandler).
	t.Run("msgh_tostring_number_falls_back_with_traceback", func(t *testing.T) {
		_, stderr, err := run("", "", nil, "-E", "-e", `error(setmetatable({}, {__tostring=function() return 123 end}))`)
		if err == nil {
			t.Fatalf("expected non-zero exit")
		}
		if !strings.Contains(stderr, "(error object is a table value)") || !strings.Contains(stderr, "stack traceback:") {
			t.Fatalf("expected '(error object is a table value)' with stack traceback, got stderr=%q", stderr)
		}
	})

	// 22. error() / error(nil) reports '(error object is a nil value)' with traceback (lua.c:msghandler + luaG_errormsg).
	t.Run("error_nil_reports_nil_object_with_traceback", func(t *testing.T) {
		for _, expr := range []string{`error()`, `error(nil)`} {
			_, stderr, err := run("", "", nil, "-E", "-e", expr)
			if err == nil {
				t.Fatalf("%s: expected non-zero exit", expr)
			}
			if !strings.Contains(stderr, "(error object is a nil value)") || !strings.Contains(stderr, "stack traceback:") {
				t.Fatalf("%s: expected '(error object is a nil value)' with stack traceback, got stderr=%q", expr, stderr)
			}
		}
	})

	// 23. SIGINT delivered to parent while os.execute runs a child process is ignored by parent.
	if runtime.GOOS != "windows" {
		t.Run("sigint_during_os_execute_ignored_by_parent", func(t *testing.T) {
			cmd := exec.Command(bin, "-E", "-e", `
				local ok = os.execute("printf 'CHILD_READY\\n'; sleep 0.35")
				io.stdout:write("AFTER_EXEC=" .. tostring(ok) .. "\n")
				io.stdout:flush()
			`)
			stdoutPipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 32)
			n, err := stdoutPipe.Read(buf)
			if err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("failed reading CHILD_READY: %v", err)
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				_ = cmd.Process.Kill()
				t.Fatalf("sending SIGINT to parent failed: %v", err)
			}
			rest, _ := io.ReadAll(stdoutPipe)
			out := string(buf[:n]) + string(rest)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("parent aborted on SIGINT during os.execute: err=%v stdout=%q stderr=%q", err, out, stderr.String())
			}
			if !strings.Contains(out, "AFTER_EXEC=true") {
				t.Fatalf("expected AFTER_EXEC=true, got stdout=%q stderr=%q", out, stderr.String())
			}
		})
	}
}

func TestCLIRejectsOptionAsSeparatedArgument(t *testing.T) {
	// Mirrors lua.c:collectargs: a separated -e/-l argument must exist and must not start with '-'.
	bin := filepath.Join(t.TempDir(), "moelua")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build cmd/moelua failed: %v\n%s", err, out)
	}
	script := filepath.Join(t.TempDir(), "script.lua")
	if err := os.WriteFile(script, []byte(`print("ran")`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args    []string
		needArg string
	}{
		{[]string{"-E", "-e", "--", script}, "-e"},
		{[]string{"-E", "-l", "-x", script}, "-l"},
		{[]string{"-E", "-e", "-"}, "-e"},
	} {
		cmd := exec.Command(bin, tc.args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil || strings.Contains(stdout.String(), "ran") || !strings.Contains(stderr.String(), "'"+tc.needArg+"' needs argument") {
			t.Errorf("args=%q: err=%v stdout=%q stderr=%q", tc.args, err, stdout.String(), stderr.String())
		}
	}
	out, err := exec.Command(bin, "-E", "-e--", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ran") {
		t.Errorf("concatenated -e-- should be accepted: err=%v out=%q", err, out)
	}
}
