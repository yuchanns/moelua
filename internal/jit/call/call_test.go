package call

import (
	"encoding/binary"
	"runtime"
	"testing"
	"unsafe"

	"github.com/yuchanns/moelua/internal/jit/execmem"
)

func TestCallNativeCode(t *testing.T) {
	if !execmem.Supported() {
		t.Skip("execmem unsupported in this build")
	}

	// Emit a tiny native function:
	//   uint64 fn(uint64* ctx) { *ctx += 13; return *ctx + 29; }
	var code []byte
	switch runtime.GOARCH {
	case "arm64":
		// ldr  x1, [x0]        (0xf9400001)
		// add  x1, x1, #13     (0x91003421)
		// str  x1, [x0]        (0xf9000001)
		// add  x0, x1, #29     (0x91007420)
		// ret                  (0xd65f03c0)
		insns := []uint32{
			0xf9400001,
			0x91003421,
			0xf9000001,
			0x91007420,
			0xd65f03c0,
		}
		code = make([]byte, len(insns)*4)
		for i, w := range insns {
			binary.LittleEndian.PutUint32(code[i*4:], w)
		}
	case "amd64":
		// mov  rax, [rdi]      (48 8b 07)
		// add  rax, 13         (48 83 c0 0d)
		// mov  [rdi], rax      (48 89 07)
		// add  rax, 29         (48 83 c0 1d)
		// ret                  (c3)
		code = []byte{
			0x48, 0x8b, 0x07,
			0x48, 0x83, 0xc0, 0x0d,
			0x48, 0x89, 0x07,
			0x48, 0x83, 0xc0, 0x1d,
			0xc3,
		}
	default:
		t.Skipf("unsupported GOARCH %s", runtime.GOARCH)
	}

	reg, err := execmem.Publish(code)
	if err != nil {
		t.Fatalf("execmem.Publish failed: %v", err)
	}
	defer reg.Free()

	var val uint64 = 100
	got := Call(reg.Addr(), unsafe.Pointer(&val))
	if val != 113 {
		t.Fatalf("mutated ctx val = %d, want 113", val)
	}
	if got != 142 {
		t.Fatalf("Call return = %d, want 142", got)
	}
	runtime.KeepAlive(&val)
}
