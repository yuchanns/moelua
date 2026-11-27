package arena

import "testing"

func TestGoRefsCloneIsolation(t *testing.T) {
	if empty := (*GoRefs)(nil).Clone(); empty == nil || empty.LiveCount() != 0 {
		t.Fatalf("nil Clone = %v (live=%d), want non-nil empty", empty, empty.LiveCount())
	}

	orig := NewGoRefs()
	h1 := orig.Alloc("alpha")
	h2 := orig.Alloc("beta")
	h3 := orig.Alloc("gamma")
	orig.Free(h2) // creates a free-list hole with bumped generation

	c1 := orig.Clone()
	c2 := orig.Clone()

	for name, g := range map[string]*GoRefs{"orig": orig, "c1": c1, "c2": c2} {
		if g.LiveCount() != 2 {
			t.Fatalf("%s LiveCount = %d, want 2", name, g.LiveCount())
		}
		if v, ok := g.Get(h1); !ok || v != "alpha" {
			t.Fatalf("%s Get(h1) = (%v, %v), want (alpha, true)", name, v, ok)
		}
		if v, ok := g.Get(h2); ok || v != nil {
			t.Fatalf("%s Get(freed h2) = (%v, %v), want (nil, false)", name, v, ok)
		}
		if v, ok := g.Get(h3); !ok || v != "gamma" {
			t.Fatalf("%s Get(h3) = (%v, %v), want (gamma, true)", name, v, ok)
		}
	}

	// Mutating c1 (freeing h1 and allocating into the recycled slot) must not affect orig or c2.
	c1.Free(h1)
	h4 := c1.Alloc("delta")
	if v, ok := c1.Get(h4); !ok || v != "delta" {
		t.Fatalf("c1 Get(h4) = (%v, %v), want (delta, true)", v, ok)
	}
	if _, ok := c1.Get(h1); ok {
		t.Fatalf("c1 Get(freed h1) unexpectedly succeeded")
	}

	for name, g := range map[string]*GoRefs{"orig": orig, "c2": c2} {
		if g.LiveCount() != 2 {
			t.Fatalf("%s LiveCount after c1 mutation = %d, want 2", name, g.LiveCount())
		}
		if v, ok := g.Get(h1); !ok || v != "alpha" {
			t.Fatalf("%s Get(h1) after c1 mutation = (%v, %v), want (alpha, true)", name, v, ok)
		}
		if _, ok := g.Get(h4); ok {
			t.Fatalf("%s Get(c1's h4) unexpectedly succeeded", name)
		}
	}
}
