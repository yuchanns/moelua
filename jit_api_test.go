package moelua_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuchanns/moelua"
)

func TestWithJITOptInAndClose(t *testing.T) {
	// Default State must NOT enable JIT (zero overhead for noJIT callers).
	sDefault := moelua.New()
	defer sDefault.Close()
	if sDefault.JITEnabled() {
		t.Fatalf("expected default State to have JITEnabled() == false")
	}

	// Opt-in State activates JIT when supported by build/OS and runs Lua identically.
	var out bytes.Buffer
	sJIT := moelua.New(moelua.WithJIT(), moelua.WithOutput(&out))
	if moelua.JITSupported() && !sJIT.JITEnabled() {
		t.Logf("JITSupported() is true but runtime W^X probe declined PROT_EXEC; fell back cleanly")
	}
	if !moelua.JITSupported() && sJIT.JITEnabled() {
		t.Fatalf("JITEnabled() must be false when JITSupported() is false")
	}
	if _, err := sJIT.DoString(`local sum = 0; for i = 1, 100 do sum = sum + i end; print(sum)`); err != nil {
		t.Fatalf("DoString with WithJIT failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "5050" {
		t.Fatalf("unexpected output: %q, want 5050", got)
	}
	sJIT.Close()
	if sJIT.JITEnabled() {
		t.Fatalf("expected JITEnabled() == false after Close()")
	}
	sJIT.Close() // idempotent
}
