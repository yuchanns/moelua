package execmem

import (
	"errors"
	"runtime"
	"testing"
)

func TestAllocSealFree(t *testing.T) {
	if !Supported() {
		if _, err := Alloc(64); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("expected ErrUnsupported on unsupported build, got %v", err)
		}
		return
	}

	if _, err := Alloc(0); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(0) = %v, want ErrInvalidSize", err)
	}
	if _, err := Alloc(-16); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(-16) = %v, want ErrInvalidSize", err)
	}
	if _, err := Alloc(maxRegionBytes + 1); !errors.Is(err, ErrInvalidSize) {
		t.Fatalf("Alloc(huge) = %v, want ErrInvalidSize", err)
	}

	r, err := Alloc(64)
	if err != nil {
		t.Fatalf("Alloc(64) failed: %v", err)
	}
	defer r.Free()

	if r.Size() < 64 {
		t.Fatalf("unexpected region size %d", r.Size())
	}
	buf := r.Bytes()
	if len(buf) != r.Size() {
		t.Fatalf("len(Bytes()) = %d, want %d", len(buf), r.Size())
	}
	buf[0] = 0x90

	addr, err := r.Seal()
	if err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	if addr == 0 || addr != r.Addr() {
		t.Fatalf("unexpected sealed addr=%#x r.Addr()=%#x", addr, r.Addr())
	}
	if !r.Sealed() {
		t.Fatalf("expected Sealed() == true")
	}
	if r.Bytes() != nil {
		t.Fatalf("Bytes() must return nil after Seal() to enforce W^X")
	}
	if _, err := r.Seal(); !errors.Is(err, ErrAlreadySealed) {
		t.Fatalf("second Seal() = %v, want ErrAlreadySealed", err)
	}

	r.Free()
	r.Free() // idempotent
	if r.Addr() != 0 || r.Sealed() || r.Bytes() != nil {
		t.Fatalf("expected zeroed state after Free()")
	}
}

func TestCleanupUnfreedRegion(t *testing.T) {
	if !Supported() {
		t.Skip("execmem unsupported on this platform")
	}
	func() {
		r, err := Alloc(64)
		if err != nil {
			t.Fatalf("Alloc failed: %v", err)
		}
		if _, err := r.Seal(); err != nil {
			t.Fatalf("Seal failed: %v", err)
		}
		// Intentionally drop without calling Free to exercise runtime.AddCleanup.
	}()
	runtime.GC()
	runtime.GC()
}
