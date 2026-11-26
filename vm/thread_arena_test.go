package vm

import (
	"testing"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
)

func TestDeferredStackFreesScopedPerThread(t *testing.T) {
	s := NewState()
	defer s.Close()

	var (
		outerSawDeferred bool
		innerRan         bool
	)

	s.registerFunc(s.GlobalEnv(), "inner_native", 0, func(s *State, args []Value) ([]Value, error) {
		innerRan = true
		if len(args) != 1 {
			t.Errorf("inner_native: got %d args, want 1", len(args))
		}
		return []Value{IntValue(999)}, nil
	})

	s.registerFunc(s.GlobalEnv(), "outer_native", 0, func(s *State, args []Value) ([]Value, error) {
		if len(args) != 6 {
			t.Fatalf("outer_native: got %d args, want 6", len(args))
		}
		coVal := args[3]
		growFn := args[4]
		coResume := args[5]

		// Grow the parent thread's stack while outer_native borrows args from the old stack.
		if _, err := s.Call(growFn); err != nil {
			return nil, err
		}
		if len(s.deferredStackFrees) == 0 {
			t.Fatalf("expected parent old stack to be deferred while nativeArgsDepth > 0")
		}
		parentDeferredRef := s.deferredStackFrees[0].ref
		parentDeferredLen := s.deferredStackFrees[0].len
		parentThreadRef := s.curRef
		outerSawDeferred = true

		// Resume a second coroutine whose native call returns and drops its own nativeArgsDepth to 0.
		res, err := s.Call(coResume, coVal)
		if err != nil {
			return nil, err
		}
		if len(res) < 2 || res[0].IsFalsy() {
			t.Fatalf("coroutine.resume failed: %v", res)
		}
		if v, ok := res[1].ToInt(); !ok || v != 999 {
			t.Fatalf("coroutine.resume result = %v, want 999", res[1])
		}

		// Parent thread's deferred stack buffer must STILL be live and preserved in deferredStackFrees.
		found := false
		for _, df := range s.deferredStackFrees {
			if df.ref == parentDeferredRef && df.owner == parentThreadRef {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("parent deferred stack %v was prematurely removed by child coroutine native return", parentDeferredRef)
		}
		if !s.arena.IsLive(parentDeferredRef) {
			t.Fatalf("parent deferred stack %v was prematurely freed by child coroutine native return", parentDeferredRef)
		}

		// Allocate same-sized Arena buffers and overwrite them to catch any premature free/reuse.
		clobberSize := parentDeferredLen * int(unsafe.Sizeof(Value{}))
		var scratch []arena.ObjRef
		for range 8 {
			ref, aerr := s.arena.Alloc(clobberSize)
			if aerr != nil {
				t.Fatalf("clobber alloc failed: %v", aerr)
			}
			fill := arena.SliceFast[Value](s.arena, ref, 0, parentDeferredLen)
			for i := range fill {
				fill[i] = IntValue(-123456)
			}
			scratch = append(scratch, ref)
		}
		for _, ref := range scratch {
			s.arena.Free(ref)
		}

		// Borrowed args slice must still hold its original uncorrupted values, and returning a
		// subslice aliasing the deferred buffer must survive outer_native's finishNativeArgs.
		for i, want := range []int64{11, 22, 33} {
			got, ok := args[i].ToInt()
			if !ok || got != want {
				t.Fatalf("borrowed args[%d] corrupted: got %v, want %d", i, args[i], want)
			}
		}
		return args[:3], nil
	})

	cl, err := s.LoadChunk([]byte(`
		local function grow(n)
			if n <= 0 then return 0 end
			local a, b, c, d, e, f, g, h = 1, 2, 3, 4, 5, 6, 7, 8
			return grow(n - 1) + a - a + b - b + c - c + d - d + e - e + f - f + g - g + h - h
		end
		local child = coroutine.create(function()
			return inner_native(42)
		end)
		local parent = coroutine.create(function()
			local a, b, c = outer_native(11, 22, 33, child, function() return grow(60) end, coroutine.resume)
			return a, b, c
		end)
		local ok, r1, r2, r3 = coroutine.resume(parent)
		assert(ok, r1)
		return r1, r2, r3
	`), "=deferred_stack_per_thread")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(cl)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !outerSawDeferred || !innerRan {
		t.Fatalf("test did not exercise expected path: outerSawDeferred=%v innerRan=%v", outerSawDeferred, innerRan)
	}
	if len(s.deferredStackFrees) != 0 {
		t.Fatalf("deferredStackFrees leaked after parent native returned: len=%d", len(s.deferredStackFrees))
	}
	if len(res) != 3 {
		t.Fatalf("got %d return values, want 3", len(res))
	}
	for i, want := range []int64{11, 22, 33} {
		got, ok := res[i].ToInt()
		if !ok || got != want {
			t.Fatalf("res[%d] = %v, want %d", i, res[i], want)
		}
	}
}

func TestFindOrCreateUpvalOOMClearsOwnerRef(t *testing.T) {
	s := NewState()
	defer s.Close()

	th := s.mainThread()
	// Fill initial openUpvalsCap (4) and measure exact single-Upvalue Arena byte cost.
	for i := range 3 {
		if uv := th.findOrCreateUpval(s, i); uv == nil {
			t.Fatalf("findOrCreateUpval(%d) unexpectedly failed", i)
		}
	}
	before := s.arena.AllocBytes()
	if uv := th.findOrCreateUpval(s, 3); uv == nil {
		t.Fatalf("findOrCreateUpval(3) unexpectedly failed")
	}
	uvBytes := s.arena.AllocBytes() - before
	if uvBytes <= 0 {
		t.Fatalf("expected positive Upvalue alloc bytes, got %d", uvBytes)
	}

	// Set Arena limit so the 5th Upvalue allocation fits, but growing openUpvalsRef (cap 4 -> 8) fails.
	s.arena.SetLimit(s.arena.AllocBytes() + uvBytes)
	defer s.arena.SetLimit(0)

	if uv := th.findOrCreateUpval(s, 4); uv != nil {
		t.Fatalf("expected nil Upvalue on openUpvalsRef growth OOM, got %+v (ownerRef=%v)", uv, uv.ownerRef)
	}
	if !s.memExceeded {
		t.Fatalf("expected s.memExceeded=true after openUpvalsRef OOM")
	}
	if th.openUpvalsLen != 4 {
		t.Fatalf("openUpvalsLen = %d, want 4", th.openUpvalsLen)
	}

	// Lift limit, close remaining open upvalues, and run a full GC to verify zero dangling ownerRef.
	s.arena.SetLimit(0)
	s.memExceeded = false
	th.closeUpvals(s, 0)
	s.CollectFull()
}
