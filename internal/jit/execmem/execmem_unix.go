//go:build (darwin || linux) && (arm64 || amd64) && !moeluanojit

package execmem

import (
	"syscall"
	"unsafe"
)

// Supported reports whether strict W^X executable code allocation is supported.
func Supported() bool { return true }

func allocRW(size int) ([]byte, uintptr, error) {
	mem, err := syscall.Mmap(
		-1,
		0,
		size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON,
	)
	if err != nil {
		return nil, 0, err
	}
	return mem, uintptr(unsafe.Pointer(unsafe.SliceData(mem))), nil
}

func protectRX(mem []byte, _ uintptr, _ int) error {
	return syscall.Mprotect(mem, syscall.PROT_READ|syscall.PROT_EXEC)
}

func freeMapping(mem []byte, _ uintptr, _ int) {
	if len(mem) == 0 {
		return
	}
	_ = syscall.Munmap(mem)
}
