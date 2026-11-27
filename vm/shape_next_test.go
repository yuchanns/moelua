package vm

import (
	"fmt"
	"testing"
)

func TestTableNextShapeOrderDeletionAndMixedPhases(t *testing.T) {
	s := NewState()
	defer s.Close()

	// Build tables across small (<=8) and indexed (>8 up to maxShapeProps=64) shapes,
	// verify exact insertion order, mid-iteration deletions, mixed array+shape+hash,
	// and invalid key error parity with Lua 5.5.1.
	for _, nProps := range []int{1, 4, 8, 9, 24, 64} {
		tbl := s.NewTable(2, nProps)
		tbl.RawSetInt(s, 1, IntValue(101))
		tbl.RawSetInt(s, 2, IntValue(202))

		wantKeys := make([]string, nProps)
		wantVals := make(map[string]int64, nProps)
		for i := range nProps {
			k := fmt.Sprintf("k%02d", i)
			wantKeys[i] = k
			wantVals[k] = int64(i + 1000)
			tbl.RawSetStr(s, s.Intern(k), IntValue(int64(i+1000)))
		}

		// 1. Full traversal via Next() must visit array keys 1..2 then all shape keys in slot order.
		var gotInt []int64
		var gotStr []string
		k := NilValue()
		for {
			nk, nv, err := tbl.Next(s, k)
			if err != nil {
				t.Fatalf("nProps=%d Next failed: %v", nProps, err)
			}
			if nk.IsNil() {
				break
			}
			if nk.IsInt() {
				gotInt = append(gotInt, nk.Int())
			} else if nk.IsString() {
				ks := s.AsString(nk).Str()
				gotStr = append(gotStr, ks)
				wantVal, ok := wantVals[ks]
				if !ok || nv.Int() != wantVal {
					t.Fatalf("nProps=%d key %q value=%d want=%d", nProps, ks, nv.Int(), wantVal)
				}
			} else {
				t.Fatalf("nProps=%d unexpected key type: %v", nProps, nk)
			}
			k = nk
		}
		if len(gotInt) != 2 || gotInt[0] != 1 || gotInt[1] != 2 {
			t.Fatalf("nProps=%d array keys=%v want [1 2]", nProps, gotInt)
		}
		if len(gotStr) != nProps {
			t.Fatalf("nProps=%d visited %d string keys, want %d", nProps, len(gotStr), nProps)
		}
		for i, wk := range wantKeys {
			if OptimizeEnabled && gotStr[i] != wk {
				t.Fatalf("nProps=%d slot %d got %q want %q", nProps, i, gotStr[i], wk)
			}
		}

		// 2. Deleting the current key during traversal must succeed without skipping remaining keys.
		seen := make(map[string]bool, nProps)
		k = NilValue()
		for {
			nk, _, err := tbl.Next(s, k)
			if err != nil {
				t.Fatalf("nProps=%d Next during deletion failed: %v", nProps, err)
			}
			if nk.IsNil() {
				break
			}
			if nk.IsString() {
				ks := s.AsString(nk).Str()
				if seen[ks] {
					t.Fatalf("nProps=%d duplicate key %q during deletion", nProps, ks)
				}
				seen[ks] = true
				tbl.RawSetStr(s, s.AsString(nk), NilValue())
			}
			k = nk
		}
		if len(seen) != nProps {
			t.Fatalf("nProps=%d deleted traversal visited %d keys, want %d", nProps, len(seen), nProps)
		}

		// 3. Invalid key must still raise "invalid key to 'next'".
		missing := StringValue(s.Intern("definitely_absent"))
		if _, _, err := tbl.Next(s, missing); err == nil {
			t.Fatalf("nProps=%d expected error for absent key in Next()", nProps)
		}
	}
}

func TestTableNextShapeZeroAllocs(t *testing.T) {
	if !OptimizeEnabled {
		t.Skip("shape tables disabled under moeluafaithful build tag")
	}
	s := NewState()
	defer s.Close()

	tbl := s.NewTable(0, 32)
	for i := range 32 {
		tbl.RawSetStr(s, s.Intern(fmt.Sprintf("prop_%02d", i)), IntValue(int64(i)))
	}
	// Warm lazy shape index (built on 2nd lookup when shape length > 8).
	for range 2 {
		k := NilValue()
		for {
			nk, _, err := tbl.Next(s, k)
			if err != nil {
				t.Fatal(err)
			}
			if nk.IsNil() {
				break
			}
			k = nk
		}
	}

	allocs := testing.AllocsPerRun(500, func() {
		k := NilValue()
		for {
			nk, _, err := tbl.Next(s, k)
			if err != nil {
				t.Fatal(err)
			}
			if nk.IsNil() {
				break
			}
			k = nk
		}
	})
	if allocs != 0 {
		t.Fatalf("expected 0 Go heap allocs iterating 32-key Shape table via Next(), got %.2f", allocs)
	}
}

func BenchmarkTableNextShape(b *testing.B) {
	for _, nProps := range []int{8, 32} {
		b.Run(fmt.Sprintf("props_%d", nProps), func(b *testing.B) {
			s := NewState()
			defer s.Close()

			tbl := s.NewTable(0, nProps)
			for i := range nProps {
				tbl.RawSetStr(s, s.Intern(fmt.Sprintf("field_%02d", i)), IntValue(int64(i)))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				k := NilValue()
				for {
					nk, _, err := tbl.Next(s, k)
					if err != nil {
						b.Fatal(err)
					}
					if nk.IsNil() {
						break
					}
					k = nk
				}
			}
		})
	}
}
