//go:build (!arm64 && !amd64) || purego || moeluafloatnil || moeluafaithful || moeluaregcheck

package vm

const asmDispatchEnabled = false

// runFast falls back directly to the portable Go switch interpreter on architectures or
// build-tag configurations without an enabled Plan 9 direct-threaded interpreter.
func (s *State) runFast(th *Thread, entryDepth int) error {
	return s.run(th, entryDepth)
}
