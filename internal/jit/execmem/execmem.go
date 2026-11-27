// Package execmem provides a strict W^X executable code memory allocator with zero
// CGo and zero third-party dependencies.
//
// Regions are always allocated read-write (never RWX), populated by the compiler, and
// sealed to read-execute via Seal (including ARM64 instruction-cache maintenance).
// Unsupported platforms (e.g. iOS, WASM, 32-bit) and builds with -tags moeluanojit
// report Supported() == false and return ErrUnsupported so callers gracefully fall back
// to the interpreter.
package execmem

import (
	"errors"
	"os"
	"runtime"
	"sync/atomic"
)

// ErrUnsupported is returned when executable JIT memory is unavailable on the current
// OS/architecture or disabled via -tags moeluanojit.
var ErrUnsupported = errors.New("jit: executable memory unsupported on this platform")

// ErrInvalidSize is returned when Alloc is called with a non-positive or excessive size.
var ErrInvalidSize = errors.New("jit: invalid executable allocation size")

// ErrAlreadySealed is returned when Seal is called more than once or after Free.
var ErrAlreadySealed = errors.New("jit: region already sealed or freed")

const maxRegionBytes = 64 << 20 // 64 MiB hard cap per single executable mapping

type regionHandle struct {
	mem  []byte
	addr uintptr
	size int
}

// Region owns one page-aligned OS virtual memory mapping that transitions once from
// writable (RW) to immutable executable (RX) via Seal, and is released via Free.
type Region struct {
	mem     []byte
	addr    uintptr
	size    int
	sealed  atomic.Bool
	freed   atomic.Bool
	cleanup runtime.Cleanup
}

func roundUpPage(n int) int {
	ps := os.Getpagesize()
	if ps <= 0 {
		ps = 4096
	}
	rem := n % ps
	if rem == 0 {
		return n
	}
	return n + (ps - rem)
}

// Alloc reserves a page-aligned read-write memory region of at least minBytes.
// Callers write machine code into r.Bytes(), then invoke r.Seal() to flip the region
// to read-execute and flush instruction caches. Always call r.Free() when finished.
func Alloc(minBytes int) (*Region, error) {
	if !Supported() {
		return nil, ErrUnsupported
	}
	if minBytes <= 0 || minBytes > maxRegionBytes {
		return nil, ErrInvalidSize
	}
	size := roundUpPage(minBytes)
	if size <= 0 || size > maxRegionBytes {
		return nil, ErrInvalidSize
	}
	mem, addr, err := allocRW(size)
	if err != nil {
		return nil, err
	}
	r := &Region{
		mem:  mem,
		addr: addr,
		size: size,
	}
	r.cleanup = runtime.AddCleanup(r, func(h regionHandle) {
		freeMapping(h.mem, h.addr, h.size)
	}, regionHandle{mem: mem, addr: addr, size: size})
	return r, nil
}

// Publish allocates a W^X Region, copies code into it, and seals it to read-execute.
// If Seal fails (for example when OS policy denies PROT_EXEC), the mapping is freed
// immediately before returning the error.
func Publish(code []byte) (*Region, error) {
	if len(code) == 0 {
		return nil, ErrInvalidSize
	}
	r, err := Alloc(len(code))
	if err != nil {
		return nil, err
	}
	copy(r.Bytes(), code)
	if _, err := r.Seal(); err != nil {
		r.Free()
		return nil, err
	}
	return r, nil
}

// Bytes returns the writable backing slice while the Region is unsealed and live.
// Returns nil once Seal or Free has been called so callers cannot write into RX memory.
func (r *Region) Bytes() []byte {
	if r == nil || r.sealed.Load() || r.freed.Load() {
		return nil
	}
	return r.mem
}

// Seal transitions the Region from read-write to immutable read-execute, performs
// architecture-specific instruction cache coherency maintenance, and returns its base
// executable address.
func (r *Region) Seal() (uintptr, error) {
	if r == nil || r.freed.Load() {
		return 0, ErrAlreadySealed
	}
	if !r.sealed.CompareAndSwap(false, true) {
		return 0, ErrAlreadySealed
	}
	if err := protectRX(r.mem, r.addr, r.size); err != nil {
		return 0, err
	}
	flushICache(r.addr, uintptr(r.size))
	return r.addr, nil
}

// Addr returns the base address of the Region (0 once freed).
func (r *Region) Addr() uintptr {
	if r == nil || r.freed.Load() {
		return 0
	}
	return r.addr
}

// Size returns the allocated page-aligned byte length of the Region.
func (r *Region) Size() int {
	if r == nil {
		return 0
	}
	return r.size
}

// Sealed reports whether the Region has been transitioned to read-execute.
func (r *Region) Sealed() bool {
	return r != nil && r.sealed.Load() && !r.freed.Load()
}

// Free unmaps the underlying OS memory region. Safe to call multiple times or on nil.
func (r *Region) Free() {
	if r == nil || !r.freed.CompareAndSwap(false, true) {
		return
	}
	r.cleanup.Stop()
	mem := r.mem
	addr := r.addr
	size := r.size
	r.mem = nil
	r.addr = 0
	freeMapping(mem, addr, size)
}
