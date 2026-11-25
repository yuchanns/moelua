package vm

import (
	"fmt"
	"math"
	"strconv"
	"unsafe"

	"github.com/yuchanns/moelua/internal/luastdlib"
)

// Value is a 16-byte Go-GC-safe tagged Lua 5.5 value (zero-boxing layout).
// Exact size is statically asserted below.
//
// In default build (!moeluafloatnil):
//   ptr == nil            => full 64-bit int64 in bits (zero GC scan, zero write barrier on int->int)
//   ptr == &scalarTags[0] => nil (bits == 0)
//   ptr == &scalarTags[1] => false (bits == 0)
//   ptr == &scalarTags[2] => true (bits == 0)
//   ptr == &scalarTags[3] => full 64-bit float64 in bits (exact IEEE-754, zero NaN canonicalization)
//   ptr == heap pointer   => heap object discriminated by quiet-NaN-spaced kind tag in bits
//
// In -tags moeluafloatnil build:
//   ptr == nil            => full 64-bit float64 in bits
//   ptr == &scalarTags[3] => full 64-bit int64 in bits
type Value struct {
	ptr  unsafe.Pointer
	bits uint64
}

// Compile-time assertion: sizeof(Value) == 16 bytes.
const (
	_ = uint(16 - unsafe.Sizeof(Value{}))
	_ = uint(unsafe.Sizeof(Value{}) - 16)
)

// Non-zero-size static sentinel array in .noptrbss (never aliases runtime.zerobase).
// Layout: [0]=nil, [1]=false, [2]=true, [3]=altScalar (float in intnil, int in floatnil).
// Keeping [0]=nil and [1]=false adjacent makes IsFalsy a single SUB+CMP:
//   uintptr(v.ptr) - tagBase < 16
var scalarTags [4]uint64

var (
	tagNil   = unsafe.Pointer(&scalarTags[0])
	tagFalse = unsafe.Pointer(&scalarTags[1])
	tagTrue  = unsafe.Pointer(&scalarTags[2])
	tagAlt   = unsafe.Pointer(&scalarTags[3])
	tagBase  = uintptr(unsafe.Pointer(&scalarTags[0]))
)

// Quiet-NaN-spaced heap object kind tags stored in Value.bits when ptr is a Go heap pointer.
// Every Is<HeapKind> check verifies BOTH bits == kind AND ptr != nil && ptr != tagAlt,
// making type confusion impossible even against adversarial int64 or float64 bit patterns.
const (
	kindString   uint64 = 0xFFF9_0000_0000_0001
	kindTable    uint64 = 0xFFF9_0000_0000_0002
	kindClosure  uint64 = 0xFFF9_0000_0000_0003
	kindNative   uint64 = 0xFFF9_0000_0000_0004
	kindThread   uint64 = 0xFFF9_0000_0000_0005
	kindUserdata uint64 = 0xFFF9_0000_0000_0006
	kindLightUD  uint64 = 0xFFF9_0000_0000_0007
)

// Userdata represents a full Lua userdata with metatable and user values (Lua 5.4/5.5 nuvalue).
type Userdata struct {
	Data    any
	Meta    *Table
	UValues []Value
	gcMark  uint64
	gcFnzed bool
	gcInFnz bool
}

// LuaError wraps any Lua Value raised via error(v) so pcall/xpcall preserve non-string errors verbatim.
type LuaError struct {
	Val Value
}

func (e *LuaError) Error() string {
	return FormatValue(e.Val)
}

func NilValue() Value   { return Value{ptr: tagNil} }
func FalseValue() Value { return Value{ptr: tagFalse} }
func TrueValue() Value  { return Value{ptr: tagTrue} }
func BoolValue(b bool) Value {
	if b {
		return Value{ptr: tagTrue}
	}
	return Value{ptr: tagFalse}
}

func StringValue(s *LuaString) Value   { return Value{ptr: unsafe.Pointer(s), bits: kindString} }
func TableValue(t *Table) Value        { return Value{ptr: unsafe.Pointer(t), bits: kindTable} }
func ClosureValue(c *Closure) Value    { return Value{ptr: unsafe.Pointer(c), bits: kindClosure} }
func NativeValue(f *NativeFunc) Value  { return Value{ptr: unsafe.Pointer(f), bits: kindNative} }
func ThreadValue(th *Thread) Value     { return Value{ptr: unsafe.Pointer(th), bits: kindThread} }
func UserdataValue(u *Userdata) Value  { return Value{ptr: unsafe.Pointer(u), bits: kindUserdata} }
func LightUDValue(p unsafe.Pointer) Value {
	return Value{ptr: p, bits: kindLightUD}
}

func (v Value) IsNil() bool   { return v.ptr == tagNil }
func (v Value) IsFalse() bool { return v.ptr == tagFalse }
func (v Value) IsTrue() bool  { return v.ptr == tagTrue }
func (v Value) IsBool() bool  { return v.ptr == tagFalse || v.ptr == tagTrue }

// IsFalsy reports whether v is nil or false in 1 SUB + 1 CMP.
func (v Value) IsFalsy() bool {
	return uintptr(v.ptr)-tagBase < 16
}

func (v Value) IsNumber() bool {
	return v.ptr == nil || v.ptr == tagAlt
}

func (v Value) IsString() bool   { return v.bits == kindString && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsTable() bool    { return v.bits == kindTable && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsClosure() bool  { return v.bits == kindClosure && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsNative() bool   { return v.bits == kindNative && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsThread() bool   { return v.bits == kindThread && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsUserdata() bool { return v.bits == kindUserdata && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsLightUD() bool  { return v.bits == kindLightUD && v.ptr != nil && v.ptr != tagAlt }
func (v Value) IsFunction() bool { return v.IsClosure() || v.IsNative() }

func (v Value) AsString() *LuaString  { return (*LuaString)(v.ptr) }
func (v Value) AsTable() *Table       { return (*Table)(v.ptr) }
func (v Value) AsClosure() *Closure   { return (*Closure)(v.ptr) }
func (v Value) AsNative() *NativeFunc { return (*NativeFunc)(v.ptr) }
func (v Value) AsThread() *Thread     { return (*Thread)(v.ptr) }
func (v Value) AsUserdata() *Userdata { return (*Userdata)(v.ptr) }
func (v Value) AsLightUD() unsafe.Pointer {
	return v.ptr
}

func (v *Value) SetNil() {
	if v.ptr != tagNil {
		v.ptr = tagNil
	}
	v.bits = 0
}

func (v *Value) SetBool(b bool) {
	want := tagFalse
	if b {
		want = tagTrue
	}
	if v.ptr != want {
		v.ptr = want
	}
	v.bits = 0
}

// ToFloat converts an int or float Value to float64.
func (v Value) ToFloat() (float64, bool) {
	if v.IsFloat() {
		return v.Float(), true
	}
	if v.IsInt() {
		return float64(v.Int()), true
	}
	return 0, false
}

// NumFloat returns v as float64 when v.IsNumber() is already known to be true.
func (v Value) NumFloat() float64 {
	if v.IsFloat() {
		return v.Float()
	}
	return float64(v.Int())
}

// ToInt converts an int or exact-integer float Value to int64.
func (v Value) ToInt() (int64, bool) {
	if v.IsInt() {
		return v.Int(), true
	}
	if v.IsFloat() {
		return FloatToIntExact(v.Float())
	}
	return 0, false
}

// CoerceFloat converts an int, float, or numeric string Value to float64 (matching lua_tonumberx).
func (v Value) CoerceFloat() (float64, bool) {
	if v.IsFloat() {
		return v.Float(), true
	}
	if v.IsInt() {
		return float64(v.Int()), true
	}
	if v.IsString() {
		if i, f, isInt, ok := luastdlib.StrToNum(v.AsString().S); ok {
			if isInt {
				return float64(i), true
			}
			return f, true
		}
	}
	return 0, false
}

// CoerceInt converts an int, exact-integer float, or numeric string Value to int64 (matching lua_tointegerx).
func (v Value) CoerceInt() (int64, bool) {
	if v.IsInt() {
		return v.Int(), true
	}
	if v.IsFloat() {
		return FloatToIntExact(v.Float())
	}
	if v.IsString() {
		if i, f, isInt, ok := luastdlib.StrToNum(v.AsString().S); ok {
			if isInt {
				return i, true
			}
			return FloatToIntExact(f)
		}
	}
	return 0, false
}

// TypeTag returns the Lua 5.5 numeric type tag (0..8 matching lua.h LUA_T*).
func (v Value) TypeTag() int {
	switch {
	case v.IsNil():
		return 0 // LUA_TNIL
	case v.IsBool():
		return 1 // LUA_TBOOLEAN
	case v.IsLightUD():
		return 2 // LUA_TLIGHTUSERDATA
	case v.IsNumber():
		return 3 // LUA_TNUMBER
	case v.IsString():
		return 4 // LUA_TSTRING
	case v.IsTable():
		return 5 // LUA_TTABLE
	case v.IsClosure(), v.IsNative():
		return 6 // LUA_TFUNCTION
	case v.IsUserdata():
		return 7 // LUA_TUSERDATA
	case v.IsThread():
		return 8 // LUA_TTHREAD
	default:
		return 7
	}
}

// TypeName returns the Lua 5.5 type(v) string.
func (v Value) TypeName() string {
	switch {
	case v.IsNil():
		return "nil"
	case v.IsBool():
		return "boolean"
	case v.IsNumber():
		return "number"
	case v.IsString():
		return "string"
	case v.IsTable():
		return "table"
	case v.IsClosure(), v.IsNative():
		return "function"
	case v.IsThread():
		return "thread"
	default:
		return "userdata"
	}
}

// RawEqual implements Lua 5.5 rawequal(a, b) (exact int/float cross-equality, NaN != NaN).
func RawEqual(a, b Value) bool {
	if a.ptr == b.ptr {
		if a.IsNil() || a.IsBool() {
			return true
		}
		if a.IsInt() {
			return a.bits == b.bits
		}
		if a.IsFloat() {
			return a.Float() == b.Float()
		}
		// Heap objects with identical pointer (including interned short strings) are equal.
		return true
	}
	// Cross int/float comparison
	if a.IsInt() && b.IsFloat() {
		return eqIntFloat(a.Int(), b.Float())
	}
	if a.IsFloat() && b.IsInt() {
		return eqIntFloat(b.Int(), a.Float())
	}
	// Two long strings with different pointers may still have equal contents.
	if a.IsString() && b.IsString() {
		sa, sb := a.AsString(), b.AsString()
		if sa.Short && sb.Short {
			return false // short strings are interned per State
		}
		return sa.S == sb.S
	}
	return false
}

// Exact Lua 5.5 numeric bounds for int64 <-> float64 conversion (matching llimits.h / lvm.c).
const (
	minIntAsFloat = -9223372036854775808.0 // -2^63, exact in float64
	maxIntPlusOne = 9223372036854775808.0  // +2^63, exact in float64 (1 past MaxInt64)
)

// FloatToIntExact converts f to int64 iff f has an exact integer value in [MinInt64, MaxInt64].
func FloatToIntExact(f float64) (int64, bool) {
	if math.IsNaN(f) || f < minIntAsFloat || f >= maxIntPlusOne {
		return 0, false
	}
	fl := math.Floor(f)
	if fl != f {
		return 0, false
	}
	return int64(fl), true
}

// FloatToIntFloor converts math.Floor(f) to int64 if in range.
func FloatToIntFloor(f float64) (int64, bool) {
	fl := math.Floor(f)
	if math.IsNaN(fl) || fl < minIntAsFloat || fl >= maxIntPlusOne {
		return 0, false
	}
	return int64(fl), true
}

// FloatToIntCeil converts math.Ceil(f) to int64 if in range.
func FloatToIntCeil(f float64) (int64, bool) {
	cl := math.Ceil(f)
	if math.IsNaN(cl) || cl < minIntAsFloat || cl >= maxIntPlusOne {
		return 0, false
	}
	return int64(cl), true
}

func eqIntFloat(i int64, f float64) bool {
	fi, ok := FloatToIntExact(f)
	return ok && i == fi
}

// LTIntFloat implements exact Lua 5.5 LTintfloat (lvm.c) without precision loss for 64-bit ints.
func LTIntFloat(i int64, f float64) bool {
	if math.IsNaN(f) {
		return false
	}
	if f >= maxIntPlusOne {
		return true
	}
	if f < minIntAsFloat {
		return false
	}
	// f is in [-2^63, 2^63): floor(f) fits in int64.
	fl := math.Floor(f)
	fi := int64(fl)
	if i < fi {
		return true
	}
	if i > fi {
		return false
	}
	return f > fl // i == floor(f) < f iff f had a fractional part
}

// LEIntFloat implements exact Lua 5.5 LEintfloat (lvm.c).
func LEIntFloat(i int64, f float64) bool {
	if math.IsNaN(f) {
		return false
	}
	if f >= maxIntPlusOne {
		return true
	}
	if f < minIntAsFloat {
		return false
	}
	fl := math.Floor(f)
	fi := int64(fl)
	return i <= fi
}

// LTFloatInt implements exact Lua 5.5 LTfloatint (lvm.c): f < i.
func LTFloatInt(f float64, i int64) bool {
	if math.IsNaN(f) {
		return false
	}
	if f < minIntAsFloat {
		return true
	}
	if f >= maxIntPlusOne {
		return false
	}
	cl := math.Ceil(f)
	if cl >= maxIntPlusOne {
		return false
	}
	return int64(cl) < i || (int64(cl) == i && f < cl)
}

// LEFloatInt implements exact Lua 5.5 LEfloatint (lvm.c): f <= i.
func LEFloatInt(f float64, i int64) bool {
	if math.IsNaN(f) {
		return false
	}
	if f < minIntAsFloat {
		return true
	}
	if f >= maxIntPlusOne {
		return false
	}
	cl := math.Ceil(f)
	if cl >= maxIntPlusOne {
		return false
	}
	return int64(cl) <= i
}

// IntDiv implements Lua 5.5 floor integer division (//) with explicit div-by-zero check.
func IntDiv(m, n int64) (int64, error) {
	if n == 0 {
		return 0, fmt.Errorf("attempt to divide by zero")
	}
	if n == -1 {
		return -m, nil // handles MinInt64 // -1 == MinInt64 wraparound
	}
	q := m / n
	if (m^n) < 0 && m%n != 0 {
		q--
	}
	return q, nil
}

// IntMod implements Lua 5.5 floor integer modulo (%) with explicit mod-by-zero check.
func IntMod(m, n int64) (int64, error) {
	if n == 0 {
		return 0, fmt.Errorf("attempt to perform 'n%%0'")
	}
	if n == -1 {
		return 0, nil
	}
	r := m % n
	if r != 0 && (r^n) < 0 {
		r += n
	}
	return r, nil
}

// FloatMod implements Lua 5.5 float modulo: a - floor(a/b)*b.
func FloatMod(a, b float64) float64 {
	m := math.Mod(a, b)
	if m != 0 && (m > 0) != (b > 0) {
		m += b
	}
	return m
}

// ShiftLeft implements Lua 5.5 bitwise left shift (handles negative and >=64 counts safely).
func ShiftLeft(x, y int64) int64 {
	if y < 0 {
		if y <= -64 {
			return 0
		}
		return int64(uint64(x) >> uint(-y))
	}
	if y >= 64 {
		return 0
	}
	return int64(uint64(x) << uint(y))
}

// ShiftRight implements Lua 5.5 bitwise logical right shift.
func ShiftRight(x, y int64) int64 {
	if y < 0 {
		if y <= -64 {
			return 0
		}
		return int64(uint64(x) << uint(-y))
	}
	if y >= 64 {
		return 0
	}
	return int64(uint64(x) >> uint(y))
}

// FormatFloat implements Lua 5.5 round-trip float formatting (lobject.c:tostringbuffFloat).
func FormatFloat(f float64) string {
	return luastdlib.FormatFloat(f)
}

// FormatValue formats a Lua Value for tostring() / print().
func FormatValue(v Value) string {
	switch {
	case v.IsNil():
		return "nil"
	case v.IsFalse():
		return "false"
	case v.IsTrue():
		return "true"
	case v.IsInt():
		return strconv.FormatInt(v.Int(), 10)
	case v.IsFloat():
		return FormatFloat(v.Float())
	case v.IsString():
		return v.AsString().S
	case v.IsTable():
		return fmt.Sprintf("table: %p", v.AsTable())
	case v.IsClosure():
		return fmt.Sprintf("function: %p", v.AsClosure())
	case v.IsNative():
		return fmt.Sprintf("function: %p", v.AsNative())
	case v.IsThread():
		return fmt.Sprintf("thread: %p", v.AsThread())
	default:
		return fmt.Sprintf("userdata: %p", v.ptr)
	}
}
