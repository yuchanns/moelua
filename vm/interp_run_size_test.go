package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunIsNotBig ensures (*State).run and (*State).runHooked stay below Go's 'big' function
// inliner cutoff so hot Value accessors and intrinsics always inline into both dispatch loops.
func TestRunIsNotBig(t *testing.T) {
	cmd := exec.Command("go", "build", "-gcflags=-m=2", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build -gcflags=-m=2 failed: %v\n%s", err, out)
	}
	text := string(out)
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "(*State).run") && strings.Contains(line, "big") {
			t.Fatalf("(*State).run / (*State).runHooked crossed Go compiler big-function cliff:\n%s", line)
		}
		if strings.Contains(line, "interp_run.go:") && strings.Contains(line, "cannot inline") {
			t.Logf("inliner note: %s", line)
		}
	}
	// Verify key hot Value helpers are actually inlined inside BOTH interp_run.go and interp_run_hooked.go.
	mustInline := []string{
		"inlining call to (*Value).SetInt",
		"inlining call to (*Value).SetFloat",
		"inlining call to BothInt",
		"inlining call to nonNilWindow",
		"inlining call to codeWords.at",
	}
	for _, want := range mustInline {
		hasFast, hasHooked := false, false
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(line, want) {
				if strings.Contains(line, "interp_run.go:") {
					hasFast = true
				}
				if strings.Contains(line, "interp_run_hooked.go:") {
					hasHooked = true
				}
			}
		}
		if !hasFast {
			t.Errorf("expected %q inlined in interp_run.go", want)
		}
		if !hasHooked {
			t.Errorf("expected %q inlined in interp_run_hooked.go", want)
		}
	}
}

// TestGeneratedInterpFresh verifies interp_run_regcheck.go and interp_run_hooked.go
// match the exact output of internal/gen/interpgen from interp_run.go (zero drift).
func TestGeneratedInterpFresh(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	src, err := os.ReadFile(filepath.Join(wd, "interp_run.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "interp_run.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	genBin := filepath.Join(tmp, "interpgen")
	if runtime.GOOS == "windows" {
		genBin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", genBin, "../internal/gen/interpgen")
	buildCmd.Dir = wd
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build interpgen failed: %v\n%s", err, out)
	}
	cmd := exec.Command(genBin)
	cmd.Dir = tmp
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("interpgen failed: %v\n%s", err, out)
	}
	for _, name := range []string{"interp_run_regcheck.go", "interp_run_hooked.go"} {
		want, err := os.ReadFile(filepath.Join(tmp, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(wd, name))
		if err != nil {
			t.Fatal(err)
		}
		got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))
		want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
		if !bytes.Equal(got, want) {
			t.Fatalf("%s is out of sync with interp_run.go; run `go generate ./vm/...`", name)
		}
	}
}
