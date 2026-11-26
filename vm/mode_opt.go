//go:build !moeluafaithful

package vm

// ModeName identifies the active optimization tier for benchmark reporting.
const ModeName = "opt"

// OptimizeEnabled enables Shapes, Inline Caches, homogeneous typed arrays, and call-site intrinsics.
const OptimizeEnabled = true
