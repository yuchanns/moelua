//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yuchanns/moelua/bytecode"
	"github.com/yuchanns/moelua/compiler"
)

// TestJITCompilesAllBenchScripts ensures that every proto and nested sub-proto across
// all benchmark scripts compiles cleanly with zero assembler/encoder errors.
func TestJITCompilesAllBenchScripts(t *testing.T) {
	scriptsDir := filepath.Join("..", "bench", "scripts")
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		t.Fatalf("read bench/scripts: %v", err)
	}
	s := NewState()
	defer s.Close()
	compiled := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(scriptsDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		root, err := compiler.Compile("@"+entry.Name(), string(src))
		if err != nil {
			t.Fatalf("parse/compile %s: %v", entry.Name(), err)
		}
		var walk func(p *bytecode.Proto)
		walk = func(p *bytecode.Proto) {
			_, bp, err := s.bindProto(p)
			if err != nil {
				t.Errorf("%s proto bindProto failed: %v", entry.Name(), err)
				return
			}
			jf, err := compileBaselineNative(bp, nil)
			if err != nil {
				t.Errorf("%s proto (lines %d-%d, %d insns) failed JIT compile: %v",
					entry.Name(), p.LineDefined, p.LastLineDefined, len(p.Code), err)
				return
			}
			if jf == nil || len(jf.pcEntries) != len(p.Code) {
				t.Errorf("%s proto produced invalid jitFunc (len=%d, want %d)",
					entry.Name(), len(jf.pcEntries), len(p.Code))
			} else {
				compiled++
			}
			jf.free()
			for _, sub := range p.Protos {
				walk(sub)
			}
		}
		walk(root)
	}
	if compiled == 0 {
		t.Fatal("expected to compile at least one benchmark proto")
	}
	t.Logf("compiled %d benchmark protos/sub-protos cleanly", compiled)
}
