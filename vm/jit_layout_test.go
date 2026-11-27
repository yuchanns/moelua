//go:build (arm64 || amd64) && !moeluanojit && !moeluafloatnil && !moeluafaithful && !moeluaregcheck

package vm

import (
	"reflect"
	"testing"
)

func assertPointerFreeType(t *testing.T, name string, ft reflect.Type) {
	t.Helper()
	switch ft.Kind() {
	case reflect.Uintptr, reflect.Uint64, reflect.Int64, reflect.Uint32, reflect.Int32:
	case reflect.Array:
		assertPointerFreeType(t, name+"[]", ft.Elem())
	default:
		t.Fatalf("jitExecCtx field %s has non-scalar kind %v; context must remain pointer-free", name, ft.Kind())
	}
}

func TestJITExecCtxZeroPointersAndInvariants(t *testing.T) {
	rt := reflect.TypeFor[jitExecCtx]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		assertPointerFreeType(t, f.Name, f.Type)
	}
	if jitValSize != 16 || jitValBits != 0 || jitValTag != 8 {
		t.Fatalf("unexpected Value layout: size=%d bits=%d tag=%d", jitValSize, jitValBits, jitValTag)
	}
	if jitCtxSize < 160 || jitCtxSize%8 != 0 {
		t.Fatalf("unexpected jitExecCtx size: %d", jitCtxSize)
	}
}
