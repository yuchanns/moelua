//go:build (!arm64 && !amd64) || moeluanojit || moeluafloatnil || moeluafaithful || moeluaregcheck

package vm

const jitLayoutSupported = false

func (s *State) runJIT(th *Thread, entryDepth int) error {
	return s.run(th, entryDepth)
}
