//go:build !moeluafloatnil

package vm

import "math"

// PolarityName identifies the active Value polarity for benchmark reporting.
const PolarityName = "intnil"

const (
	tagInt   uint64 = 0
	tagFloat uint64 = 1
)

// IntValue constructs an unboxed full 64-bit integer Value (tag == 0).
func IntValue(i int64) Value { return Value{bits: uint64(i), tag: tagInt} }

// FloatValue constructs an unboxed full 64-bit float Value (tag == 1).
func FloatValue(f float64) Value { return Value{bits: math.Float64bits(f), tag: tagFloat} }

func (v Value) IsInt() bool   { return v.tag == tagInt }
func (v Value) IsFloat() bool { return v.tag == tagFloat }

func (v Value) Int() int64     { return int64(v.bits) }
func (v Value) Float() float64 { return math.Float64frombits(v.bits) }

// BothInt tests two operands in 1 OR + 1 JNZ.
func BothInt(a, b Value) bool {
	return (a.tag | b.tag) == 0
}

func BothFloat(a, b Value) bool {
	return a.tag == tagFloat && b.tag == tagFloat
}

// SetInt stores an int64 with zero branches and zero Go GC write barrier.
func (v *Value) SetInt(i int64) {
	v.bits = uint64(i)
	v.tag = tagInt
}

// SetIntBits stores an int64 into a slot known to already hold an int64.
func (v *Value) SetIntBits(i int64) {
	v.bits = uint64(i)
}

// SetFloat stores a float64 with zero branches and zero Go GC write barrier.
func (v *Value) SetFloat(f float64) {
	v.bits = math.Float64bits(f)
	v.tag = tagFloat
}

// SetFloatBits stores a float64 into a slot known to already hold a float64.
func (v *Value) SetFloatBits(f float64) {
	v.bits = math.Float64bits(f)
}
