//go:build windows && (amd64 || arm64) && !moeluanojit

package execmem

import (
	"syscall"
	"unsafe"
)

const (
	memCommit       = 0x1000
	memReserve      = 0x2000
	memRelease      = 0x8000
	pageReadWrite   = 0x04
	pageExecuteRead = 0x20
)

var (
	modKernel32               = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc          = modKernel32.NewProc("VirtualAlloc")
	procVirtualProtect        = modKernel32.NewProc("VirtualProtect")
	procVirtualFree           = modKernel32.NewProc("VirtualFree")
	procFlushInstructionCache = modKernel32.NewProc("FlushInstructionCache")
	procGetCurrentProcess     = modKernel32.NewProc("GetCurrentProcess")
)

// Supported reports whether strict W^X executable code allocation is supported.
func Supported() bool { return true }

//go:nocheckptr
func allocRW(size int) ([]byte, uintptr, error) {
	addr, _, err := procVirtualAlloc.Call(
		0,
		uintptr(size),
		memCommit|memReserve,
		pageReadWrite,
	)
	if addr == 0 {
		if err != nil && err != syscall.Errno(0) {
			return nil, 0, err
		}
		return nil, 0, ErrUnsupported
	}
	// Construct a byte view of the freshly allocated OS virtual memory region.
	// Using unsafe.Add(nil, addr) satisfies go vet's unsafeptr check for OS-owned
	// VirtualAlloc addresses outside the Go heap (matching syscall.Mmap on Unix).
	mem := unsafe.Slice((*byte)(unsafe.Add(nil, addr)), size)
	return mem, addr, nil
}

func protectRX(_ []byte, addr uintptr, size int) error {
	var oldProt uint32
	ok, _, err := procVirtualProtect.Call(
		addr,
		uintptr(size),
		pageExecuteRead,
		uintptr(unsafe.Pointer(&oldProt)),
	)
	if ok == 0 {
		if err != nil && err != syscall.Errno(0) {
			return err
		}
		return ErrUnsupported
	}
	curProc, _, _ := procGetCurrentProcess.Call()
	_, _, _ = procFlushInstructionCache.Call(curProc, addr, uintptr(size))
	return nil
}

func freeMapping(_ []byte, addr uintptr, _ int) {
	if addr == 0 {
		return
	}
	_, _, _ = procVirtualFree.Call(addr, 0, memRelease)
}
