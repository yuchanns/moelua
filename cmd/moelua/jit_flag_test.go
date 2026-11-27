package main

import "testing"

func TestCLICollectArgsJIT(t *testing.T) {
	f := collectArgs([]string{"moelua", "-jit", "-e", "print(42)"})
	if f.badOpt != "" || f.needArg != "" {
		t.Fatalf("unexpected error flags: %+v", f)
	}
	if !f.hasJIT || !f.hasE {
		t.Fatalf("expected hasJIT && hasE, got %+v", f)
	}

	// Unrecognized prefixes must still be rejected to match PUC-Rio lua.c.
	for _, bad := range []string{"-j", "-ji", "-jitx"} {
		fb := collectArgs([]string{"moelua", bad})
		if fb.badOpt != bad {
			t.Fatalf("collectArgs(%q).badOpt = %q, want %q", bad, fb.badOpt, bad)
		}
	}
}
