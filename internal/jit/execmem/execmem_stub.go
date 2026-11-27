//go:build moeluanojit || (!darwin && !linux && !windows) || (!arm64 && !amd64)

package execmem

// Supported reports whether strict W^X executable code allocation is supported.
func Supported() bool { return false }

func allocRW(_ int) ([]byte, uintptr, error) {
	return nil, 0, ErrUnsupported
}

func protectRX(_ []byte, _ uintptr, _ int) error {
	return ErrUnsupported
}

func freeMapping(_ []byte, _ uintptr, _ int) {}
