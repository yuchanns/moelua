//go:build !moeluafloatnil

package vm

import "math"

// PolarityName identifies the active Value polarity for benchmark reporting.
const PolarityName = "intnil"

// IntValue constructs an unboxed full 64-bit integer Value (ptr == nil).
func IntValue(i int64) Value { return Value{bits: uint64(i)} }

// FloatValue constructs an unboxed full 64-bit float Value (ptr == tagAlt).
func FloatValue(f float64) Value { return Value{ptr: tagAlt, bits: math.Float64bits(f)} }

func (v Value) IsInt() bool   { return v.ptr == nil }
func (v Value) IsFloat() bool { return v.ptr == tagAlt }

func (v Value) Int() int64     { return int64(v.bits) }
func (v Value) Float() float64 { return math.Float64frombits(v.bits) }

// BothInt tests two operands in 1 OR + 1 JNZ.
func BothInt(a, b Value) bool {
	return (uintptr(a.ptr) | uintptr(b.ptr)) == 0
}

func BothFloat(a, b Value) bool {
	return a.ptr == tagAlt && b.ptr == tagAlt
}

// SetInt avoids the Go GC write barrier whenever *v already holds an int64.
func (v *Value) SetInt(i int64) {
	if v.ptr != nil {
		v.ptr = nil
	}
	v.bits = uint64(i)
}

// SetIntBits stores an int64 into a slot known to already hold an int64 (zero branch, zero barrier).
func (v *Value) SetIntBits(i int64) {
	v.bits = uint64(i)
}

// SetFloat avoids the Go GC write barrier whenever *v already holds a float64.
func (v *Value) SetFloat(f float64) {
	if v.ptr != tagAlt {
		v.ptr = tagAlt
	}
	v.bits = math.Float64bits(f)
}

// SetFloatBits stores a float64 into a slot known to already hold a float64 (zero branch, zero barrier).
func (v *Value) SetFloatBits(f float64) {
	v.bits = math.Float64bits(f)
}
