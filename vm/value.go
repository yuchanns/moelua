package vm

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/yuchanns/moelua/internal/arena"
	"github.com/yuchanns/moelua/internal/luastdlib"
)

// Value is a 16-byte zero-pointer (noscan, ptrdata == 0) tagged Lua 5.5 value.
// Because Value contains zero Go pointers, Go's GC never scans Lua stacks, tables,
// closures, upvalues, or inline caches, and writing Values emits zero Go write barriers.
//
// Tag encoding:
//
//	0, 1: Int / Float (polarity selected by !moeluafloatnil vs moeluafloatnil) -> IsNumber: tag <= 1
//	2:    Nil         (bits == 0)
//	3:    False       (bits == 0) -> IsFalsy: tag-2 <= 1
//	4:    True        (bits == 0) -> IsBool:  tag-3 <= 1
//	5:    LightUD     (bits == opaque pointer address, 0 for nil light userdata)
//	6:    TokenLightUD(bits == non-owning debug.upvalueid token)
//	7:    StaticNative(bits == 1-based index into process-wide immutable staticNatives table)
//	8:    FrozenString(bits == arena.ObjRef in process-wide read-only stdTemplate.strArena)
//	--- Per-State Arena GC-managed ObjRefs (tag >= tagFirstGC && tag <= tagLastGC) ---
//	9:    String      (bits == arena.ObjRef in State.arena, gcKindShortStr / gcKindLongStr)
//	10:   Closure     (bits == arena.ObjRef in State.arena, gcKindClosure)
//	11:   Native      (bits == arena.ObjRef in State.arena, gcKindNative)
//	12:   Table       (bits == arena.ObjRef in State.arena, gcKindTable)
//	13:   Userdata    (bits == arena.ObjRef in State.arena, gcKindUserdata)
//	14:   Thread      (bits == arena.ObjRef in State.arena, gcKindThread)
//	15:   DeadKey     (internal swept hash-tombstone marker; never escapes to Lua)
type Value struct {
	bits uint64
	tag  uint64
}

// Compile-time assertion: sizeof(Value) == 16 bytes.
const (
	_ = uint(16 - unsafe.Sizeof(Value{}))
	_ = uint(unsafe.Sizeof(Value{}) - 16)
)

const (
	tagNil          uint64 = 2
	tagFalse        uint64 = 3
	tagTrue         uint64 = 4
	tagLightUD      uint64 = 5
	tagTokenLightUD uint64 = 6
	tagStaticNative uint64 = 7
	tagFrozenString uint64 = 8
	tagString       uint64 = 9
	tagClosure      uint64 = 10
	tagNative       uint64 = 11
	tagTable        uint64 = 12
	tagUserdata     uint64 = 13
	tagThread       uint64 = 14
	tagDeadKey      uint64 = 15

	tagFirstGC     = tagString
	tagFirstWeakGC = tagClosure
	tagLastGC      = tagThread
)

// GC object kind tags stored in arena.GCHeader.Kind for every Arena-managed VM object.
const (
	gcKindShortStr uint8 = 1
	gcKindLongStr  uint8 = 2
	gcKindTable    uint8 = 3
	gcKindShape    uint8 = 4
	gcKindClosure  uint8 = 5
	gcKindUpvalue  uint8 = 6
	gcKindNative   uint8 = 7
	gcKindUserdata uint8 = 8
	gcKindThread   uint8 = 9
	gcKindProto    uint8 = 10
)

type staticNativeKey struct {
	name      string
	fnPC      uintptr
	fastFnPC  uintptr
	intrinsic uint8
	numUpvals uint8
}

func makeStaticNativeKey(nf *NativeFunc) (staticNativeKey, bool) {
	if nf == nil || nf.Collectable || len(nf.upvals) != 0 {
		return staticNativeKey{}, false
	}
	var fnPC, fastPC uintptr
	if nf.Fn != nil {
		fnPC = reflect.ValueOf(nf.Fn).Pointer()
	}
	if nf.FastFn != nil {
		fastPC = reflect.ValueOf(nf.FastFn).Pointer()
	}
	return staticNativeKey{
		name:      nf.Name,
		fnPC:      fnPC,
		fastFnPC:  fastPC,
		intrinsic: nf.Intrinsic,
		numUpvals: nf.NumUpvals,
	}, true
}

// Process-wide immutable registry of non-collectable stdlib builtins (1-based index).
// Backed by an atomic copy-on-write slice snapshot so concurrent lookupStaticNative calls
// are lock-free and race-free while deduplicating identical stdlib builtin descriptors.
var (
	staticNativeMu    sync.Mutex
	staticNatives     atomic.Pointer[[]*NativeFunc]
	staticNativeBy    = map[*NativeFunc]uint32{}
	staticNativeByKey = map[staticNativeKey]uint32{}
	staticMathSqrtIdx atomic.Uint32
	staticMathAbsIdx  atomic.Uint32
)

func init() {
	initSlice := []*NativeFunc{nil} // index 0 is unused sentinel
	staticNatives.Store(&initSlice)
}

func registerStaticNative(nf *NativeFunc) uint32 {
	if nf == nil {
		return 0
	}
	staticNativeMu.Lock()
	defer staticNativeMu.Unlock()
	if idx, ok := staticNativeBy[nf]; ok {
		return idx
	}
	key, hasKey := makeStaticNativeKey(nf)
	if hasKey {
		if idx, ok := staticNativeByKey[key]; ok {
			return idx
		}
	}
	cur := *staticNatives.Load()
	idx := uint32(len(cur))
	next := make([]*NativeFunc, len(cur)+1)
	copy(next, cur)
	next[idx] = nf
	staticNatives.Store(&next)
	staticNativeBy[nf] = idx
	if hasKey {
		staticNativeByKey[key] = idx
	}
	switch nf.Intrinsic {
	case intrMathSqrt:
		staticMathSqrtIdx.CompareAndSwap(0, idx)
	case intrMathAbs:
		staticMathAbsIdx.CompareAndSwap(0, idx)
	}
	return idx
}

func staticNativeValue(idx uint32) Value {
	return Value{bits: uint64(idx), tag: tagStaticNative}
}

func lookupStaticNative(idx uint32) *NativeFunc {
	if p := staticNatives.Load(); p != nil {
		list := *p
		if int(idx) < len(list) {
			return list[idx]
		}
	}
	return nil
}

// Userdata is a 48-byte noscan Lua full userdata stored inside State.arena.
// Its Go host payload is bridged via dataRef in State.goRefs and released automatically
// when Lua's Arena GC sweeps the userdata.
type Userdata struct {
	hdr       arena.GCHeader // 8B: Kind=gcKindUserdata
	dataRef   arena.GoHandle // 8B: GoRefs handle for Go `any` payload (NullGoHandle if nil)
	self      arena.ObjRef   // 4B: own ObjRef inside State.arena
	metaRef   arena.ObjRef   // 4B: metatable Table ObjRef (NullRef if nil)
	uvalsRef  arena.ObjRef   // 4B: raw Arena []Value for Lua user values (NullRef if nUValues==0)
	gcRefsRef arena.ObjRef   // 4B: raw Arena []Value for internal GC-traced refs (NullRef if nGCRefs==0)
	nUValues  uint32         // 4B
	nGCRefs   uint32         // 4B
	_         uint64         // 8B pad -> 48B exact
}

const (
	_ = uint(48 - unsafe.Sizeof(Userdata{}))
	_ = uint(unsafe.Sizeof(Userdata{}) - 48)
)

const (
	natFlagCollectable uint8 = 1 << 0
)

// ArenaNative is a 32-byte noscan Lua C-closure / dynamic native function stored in State.arena.
// The Go *NativeFunc callback descriptor is bridged via goRef in State.goRefs and released
// when Lua's Arena GC sweeps this ArenaNative.
type ArenaNative struct {
	hdr         arena.GCHeader // 8B: Kind=gcKindNative, Flags=natFlag*
	goRef       arena.GoHandle // 8B: GoRefs handle for *NativeFunc
	self        arena.ObjRef   // 4B: own ObjRef inside State.arena
	upvalsRef   arena.ObjRef   // 4B: raw Arena []Value of captured C upvalues
	upvalIDsRef arena.ObjRef   // 4B: raw Arena []uint64 of lazy debug.upvalueid tokens
	nUpvals     uint16         // 2B: actual captured upvals length
	numUpvals   uint8          // 1B: logical C closure upvalue count
	intrinsic   uint8          // 1B: cached Intrinsic ID
}

const (
	_ = uint(32 - unsafe.Sizeof(ArenaNative{}))
	_ = uint(unsafe.Sizeof(ArenaNative{}) - 32)
)

func (an *ArenaNative) isCollectable() bool {
	return an != nil && an.hdr.Flags&natFlagCollectable != 0
}

// LuaError wraps any Lua Value raised via error(v) so pcall/xpcall preserve non-string errors verbatim.
type LuaError struct {
	Val       Value
	msg       string // pre-formatted Go string cached when escaping to Go host (safe across GC / State.Close)
	hasMsg    bool
	xpHandled bool // true once an enclosing xpcall message handler has already run for this error
	isMemErr  bool // true for LUA_ERRMEM allocation failures (bypass xpcall message handlers per Lua 5.5 §2.3)
}

func (e *LuaError) Error() string {
	if e.hasMsg {
		return e.msg
	}
	return FormatValue(e.Val)
}

func NilValue() Value   { return Value{tag: tagNil} }
func FalseValue() Value { return Value{tag: tagFalse} }
func TrueValue() Value  { return Value{tag: tagTrue} }
func BoolValue(b bool) Value {
	if b {
		return Value{tag: tagTrue}
	}
	return Value{tag: tagFalse}
}

func deadKeyValue() Value { return Value{tag: tagDeadKey} }

func StringValue(ls *LuaString) Value {
	if ls == nil {
		return NilValue()
	}
	if ls.IsFixed() {
		return Value{bits: uint64(ls.self), tag: tagFrozenString}
	}
	return Value{bits: uint64(ls.self), tag: tagString}
}

func TableValue(t *Table) Value {
	if t == nil {
		return NilValue()
	}
	return Value{bits: uint64(t.self), tag: tagTable}
}

func ClosureValue(c *Closure) Value {
	if c == nil {
		return NilValue()
	}
	return Value{bits: uint64(c.self), tag: tagClosure}
}

func ArenaNativeValue(an *ArenaNative) Value {
	if an == nil {
		return NilValue()
	}
	return Value{bits: uint64(an.self), tag: tagNative}
}

func ThreadValue(th *Thread) Value {
	if th == nil {
		return NilValue()
	}
	return Value{bits: uint64(th.self), tag: tagThread}
}

func UserdataValue(u *Userdata) Value {
	if u == nil {
		return NilValue()
	}
	return Value{bits: uint64(u.self), tag: tagUserdata}
}

func tableRefValue(ref arena.ObjRef) Value {
	if ref == arena.NullRef {
		return NilValue()
	}
	return Value{bits: uint64(ref), tag: tagTable}
}

func closureRefValue(ref arena.ObjRef) Value {
	if ref == arena.NullRef {
		return NilValue()
	}
	return Value{bits: uint64(ref), tag: tagClosure}
}

func threadRefValue(ref arena.ObjRef) Value {
	if ref == arena.NullRef {
		return NilValue()
	}
	return Value{bits: uint64(ref), tag: tagThread}
}

func userdataRefValue(ref arena.ObjRef) Value {
	if ref == arena.NullRef {
		return NilValue()
	}
	return Value{bits: uint64(ref), tag: tagUserdata}
}

func stringRefValue(ref arena.ObjRef, frozen bool) Value {
	if ref == arena.NullRef {
		return NilValue()
	}
	if frozen {
		return Value{bits: uint64(ref), tag: tagFrozenString}
	}
	return Value{bits: uint64(ref), tag: tagString}
}

func LightUDValue(p unsafe.Pointer) Value {
	return Value{bits: uint64(uintptr(p)), tag: tagLightUD}
}

// LightUDTokenValue constructs a non-owning light userdata identified by an opaque uint64 token
// without pinning any Go heap pointer (used by debug.upvalueid).
func LightUDTokenValue(id uint64) Value {
	return Value{bits: id, tag: tagTokenLightUD}
}

func (v Value) IsNil() bool   { return v.tag == tagNil }
func (v Value) IsFalse() bool { return v.tag == tagFalse }
func (v Value) IsTrue() bool  { return v.tag == tagTrue }
func (v Value) IsBool() bool  { return v.tag-tagFalse <= 1 }

// IsFalsy reports whether v is nil or false in 1 SUB + 1 CMP.
func (v Value) IsFalsy() bool {
	return v.tag-tagNil <= 1
}

// IsNumber reports whether v is an int64 or float64 in 1 CMP.
func (v Value) IsNumber() bool {
	return v.tag <= 1
}

func (v Value) IsString() bool       { return v.tag-tagFrozenString <= 1 }
func (v Value) IsFrozenString() bool { return v.tag == tagFrozenString }
func (v Value) IsTable() bool        { return v.tag == tagTable }
func (v Value) IsClosure() bool      { return v.tag == tagClosure }
func (v Value) IsStaticNative() bool { return v.tag == tagStaticNative }
func (v Value) IsDynNative() bool    { return v.tag == tagNative }
func (v Value) IsNative() bool       { return v.tag == tagStaticNative || v.tag == tagNative }
func (v Value) IsThread() bool       { return v.tag == tagThread }
func (v Value) IsUserdata() bool     { return v.tag == tagUserdata }
func (v Value) IsLightUD() bool      { return v.tag-tagLightUD <= 1 }
func (v Value) IsFunction() bool {
	return v.tag == tagClosure || v.tag == tagStaticNative || v.tag == tagNative
}
func (v Value) IsDeadKey() bool { return v.tag == tagDeadKey }

// IsGCObject reports whether v holds a per-State Arena ObjRef managed by State.collector
// (dynamic String, Closure, DynNative, Table, Userdata, or Thread).
func (v Value) IsGCObject() bool {
	return v.tag >= tagFirstGC && v.tag <= tagLastGC
}

// Ref returns the underlying arena.ObjRef stored in v (only meaningful for Arena-backed tags).
func (v Value) Ref() arena.ObjRef {
	return arena.ObjRef(v.bits)
}

func (v Value) staticNativeIdx() uint32 {
	return uint32(v.bits)
}

// AsLightUD returns nil for null/token light userdata.
// Non-nil light userdata identity addresses are exposed via LightUDAddr.
func (v Value) AsLightUD() unsafe.Pointer {
	return nil
}

// LightUDAddr returns the opaque pointer address or token ID of a light userdata Value.
func (v Value) LightUDAddr() uintptr {
	return uintptr(v.bits)
}

func (v Value) formatLightUDAddr() string {
	return fmt.Sprintf("0x%x", v.bits)
}

func (v *Value) SetNil() {
	v.bits = 0
	v.tag = tagNil
}

func (v *Value) SetBool(b bool) {
	v.bits = 0
	if b {
		v.tag = tagTrue
	} else {
		v.tag = tagFalse
	}
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
func (s *State) CoerceFloat(v Value) (float64, bool) {
	if v.IsFloat() {
		return v.Float(), true
	}
	if v.IsInt() {
		return float64(v.Int()), true
	}
	if v.IsString() {
		if i, f, isInt, ok := luastdlib.StrToNum(s.asString(v).Str()); ok {
			if isInt {
				return float64(i), true
			}
			return f, true
		}
	}
	return 0, false
}

// CoerceInt converts an int, exact-integer float, or numeric string Value to int64 (matching lua_tointegerx).
func (s *State) CoerceInt(v Value) (int64, bool) {
	if v.IsInt() {
		return v.Int(), true
	}
	if v.IsFloat() {
		return FloatToIntExact(v.Float())
	}
	if v.IsString() {
		if i, f, isInt, ok := luastdlib.StrToNum(s.asString(v).Str()); ok {
			if isInt {
				return i, true
			}
			return FloatToIntExact(f)
		}
	}
	return 0, false
}

var valTypeTags = [16]int{
	0:               3, // LUA_TNUMBER (Int/Float)
	1:               3, // LUA_TNUMBER (Float/Int)
	tagNil:          0, // LUA_TNIL
	tagFalse:        1, // LUA_TBOOLEAN
	tagTrue:         1, // LUA_TBOOLEAN
	tagLightUD:      2, // LUA_TLIGHTUSERDATA
	tagTokenLightUD: 2, // LUA_TLIGHTUSERDATA
	tagStaticNative: 6, // LUA_TFUNCTION
	tagFrozenString: 4, // LUA_TSTRING
	tagString:       4, // LUA_TSTRING
	tagClosure:      6, // LUA_TFUNCTION
	tagNative:       6, // LUA_TFUNCTION
	tagTable:        5, // LUA_TTABLE
	tagUserdata:     7, // LUA_TUSERDATA
	tagThread:       8, // LUA_TTHREAD
	tagDeadKey:      0,
}

// TypeTag returns the Lua 5.5 numeric type tag (0..8 matching lua.h LUA_T*).
func (v Value) TypeTag() int {
	if v.tag < uint64(len(valTypeTags)) {
		return valTypeTags[v.tag]
	}
	return 7
}

var valTypeNames = [16]string{
	0:               "number",
	1:               "number",
	tagNil:          "nil",
	tagFalse:        "boolean",
	tagTrue:         "boolean",
	tagLightUD:      "userdata",
	tagTokenLightUD: "userdata",
	tagStaticNative: "function",
	tagFrozenString: "string",
	tagString:       "string",
	tagClosure:      "function",
	tagNative:       "function",
	tagTable:        "table",
	tagUserdata:     "userdata",
	tagThread:       "thread",
	tagDeadKey:      "nil",
}

// TypeName returns the Lua 5.5 type(v) string.
func (v Value) TypeName() string {
	if v.tag < uint64(len(valTypeNames)) {
		return valTypeNames[v.tag]
	}
	return "userdata"
}

// RawEqualScalar compares two Values without dereferencing Arena long strings.
// For dynamic long strings, use (*State).RawEqual.
func RawEqual(a, b Value) bool {
	if a.tag == b.tag {
		if a.IsFloat() {
			return a.Float() == b.Float()
		}
		return a.bits == b.bits
	}
	if a.IsInt() && b.IsFloat() {
		return eqIntFloat(a.Int(), b.Float())
	}
	if a.IsFloat() && b.IsInt() {
		return eqIntFloat(b.Int(), a.Float())
	}
	return false
}

// RawEqual implements Lua 5.5 rawequal(a, b) for Values owned by this State
// (exact int/float cross-equality, NaN != NaN, and content equality for long strings).
func (s *State) RawEqual(a, b Value) bool {
	if a.tag == b.tag {
		if a.IsFloat() {
			return a.Float() == b.Float()
		}
		if a.bits == b.bits {
			return true
		}
		if a.tag == tagString {
			sa, sb := s.asString(a), s.asString(b)
			if sa.IsShort() || sb.IsShort() {
				return false
			}
			return sa.Len == sb.Len && sa.Str() == sb.Str()
		}
		return false
	}
	if a.IsInt() && b.IsFloat() {
		return eqIntFloat(a.Int(), b.Float())
	}
	if a.IsFloat() && b.IsInt() {
		return eqIntFloat(b.Int(), a.Float())
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
	fl := math.Floor(f)
	fi := int64(fl)
	if i < fi {
		return true
	}
	if i > fi {
		return false
	}
	return f > fl
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
		return -m, nil
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

// FormatValue formats a scalar or non-string Lua Value without a State context.
// For string Values (stored in per-State Arena storage), use (*State).FormatValue(v).
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
		return fmt.Sprintf("string: 0x%x", v.bits)
	case v.IsTable():
		return fmt.Sprintf("table: 0x%x", v.bits)
	case v.IsClosure(), v.IsNative():
		return fmt.Sprintf("function: 0x%x", v.bits)
	case v.IsThread():
		return fmt.Sprintf("thread: 0x%x", v.bits)
	case v.IsLightUD():
		return "userdata: " + v.formatLightUDAddr()
	default:
		return fmt.Sprintf("userdata: 0x%x", v.bits)
	}
}

// valueAddr returns a deterministic unique pointer-like uintptr for %p / tostring() formatting.
func (s *State) valueAddr(v Value) uintptr {
	switch v.tag {
	case tagLightUD, tagTokenLightUD:
		return uintptr(v.bits)
	case tagStaticNative:
		return uintptr(unsafe.Pointer(lookupStaticNative(v.staticNativeIdx())))
	case tagFrozenString:
		if s.intern.frozenArena != nil {
			return uintptr(s.intern.frozenArena.PtrFast(v.Ref()))
		}
		return uintptr(v.bits)
	case tagString, tagClosure, tagNative, tagTable, tagUserdata, tagThread:
		if s.arena != nil {
			return uintptr(s.arena.PtrFast(v.Ref()))
		}
		return uintptr(v.bits)
	default:
		return 0
	}
}

// FormatValue formats a Lua Value owned by this State for tostring() / print() / host diagnostics.
// Returned strings are safely cloned copies independent of Arena lifetime.
func (s *State) FormatValue(v Value) string {
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
		return strings.Clone(s.asString(v).Str())
	case v.IsTable():
		return fmt.Sprintf("table: 0x%x", s.valueAddr(v))
	case v.IsClosure(), v.IsNative():
		return fmt.Sprintf("function: 0x%x", s.valueAddr(v))
	case v.IsThread():
		return fmt.Sprintf("thread: 0x%x", s.valueAddr(v))
	case v.IsLightUD():
		return "userdata: " + v.formatLightUDAddr()
	default:
		return fmt.Sprintf("userdata: 0x%x", s.valueAddr(v))
	}
}
