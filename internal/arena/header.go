package arena

import "unsafe"

// ObjRef is a compact 32-bit handle addressing an 8-byte-aligned allocation inside an Arena.
// NullRef (0) represents the absence of an object.
//
// Encoding:
//   - Bit 31 == 0, ref != 0: small/medium slab object.
//     Bits 30..13 (18 bits): 1-based page ID (1..262,143 => up to ~16 GiB per State).
//     Bits 12..0  (13 bits): 8-byte word offset within the 64 KiB page (0..8191).
//   - Bit 31 == 1: dedicated large-object span (> MaxSmallSize).
//     Bits 30..0  (31 bits): 1-based large span index.
type ObjRef uint32

const (
	// NullRef is the invalid/nil arena reference.
	NullRef ObjRef = 0

	largeFlag ObjRef = 1 << 31
	largeMask ObjRef = ^largeFlag

	// PageSize is the fixed size (64 KiB) of each noscan arena slab page.
	PageSize   = 1 << pageShift
	pageShift  = 16
	alignShift = 3
	// Align is the minimum alignment (8 bytes) guaranteed for every arena allocation.
	Align     = 1 << alignShift
	wordBits  = pageShift - alignShift // 13 bits (8192 words per 64 KiB page)
	pageWords = 1 << wordBits
	wordMask  = pageWords - 1
	maxPageID = (1 << (31 - wordBits)) - 1 // 262,143 pages (~16 GiB)

	// LargeClass marks a GCHeader belonging to a dedicated large []byte span.
	LargeClass uint8 = 0xFF
)

// IsNull reports whether r is NullRef.
func (r ObjRef) IsNull() bool { return r == NullRef }

// IsLarge reports whether r refers to a dedicated large-object span.
func (r ObjRef) IsLarge() bool { return r&largeFlag != 0 }

func makePageRef(pid uint32, wordOff uint16) ObjRef {
	return ObjRef((pid << wordBits) | uint32(wordOff))
}

func splitPageRef(r ObjRef) (pid uint32, wordOff uint16) {
	return uint32(r >> wordBits), uint16(r & wordMask)
}

func makeLargeRef(idx uint32) ObjRef {
	return largeFlag | ObjRef(idx)
}

func largeRefIndex(r ObjRef) uint32 {
	return uint32(r & largeMask)
}

// Lua 5.5.1 GC mark and generational age bit layout (matching third_party/lua/lgc.h).
const (
	MarkWhite0    uint8 = 1 << 0 // object is white (type 0)
	MarkWhite1    uint8 = 1 << 1 // object is white (type 1)
	MarkBlack     uint8 = 1 << 2 // object is black (visited)
	MarkFinalized uint8 = 1 << 3 // object has been separated for finalization (FINALIZEDBIT)
	MarkWhiteBits uint8 = MarkWhite0 | MarkWhite1
	MarkColorMask uint8 = MarkWhiteBits | MarkBlack

	// Generational age bits stored in the upper 3 bits of GCHeader.Mark (bits 4..6).
	AgeShift uint8 = 4
	AgeMask  uint8 = 0x7 << AgeShift

	AgeNew      uint8 = 0 // created in current cycle (G_NEW)
	AgeSurvival uint8 = 1 // created in previous cycle (G_SURVIVAL)
	AgeOld0     uint8 = 2 // marked old by forward barrier in this cycle (G_OLD0)
	AgeOld1     uint8 = 3 // first full cycle as old (G_OLD1)
	AgeOld      uint8 = 4 // really old object, not to be visited in minor cycles (G_OLD)
	AgeTouched1 uint8 = 5 // old object touched this cycle (G_TOUCHED1)
	AgeTouched2 uint8 = 6 // old object touched in previous cycle (G_TOUCHED2)
)

// GCHeader is the 8-byte noscan object header placed at offset 0 of every GC-managed
// allocation in an Arena. It mirrors Lua 5.5.1 CommonHeader with zero Go pointers.
type GCHeader struct {
	Next  ObjRef // intrusive allgc / finobj / tobefnz list link
	Kind  uint8  // caller-defined object kind tag
	Mark  uint8  // color bits (bits 0..3) + generational age (bits 4..6)
	Class uint8  // size class index (0..NumClasses-1) or LargeClass
	Flags uint8  // per-kind flags
}

// Compile-time assertions: GCHeader is exactly 8 bytes and fits within minimum alignment.
const (
	_ = uint(8 - unsafe.Sizeof(GCHeader{}))
	_ = uint(unsafe.Sizeof(GCHeader{}) - 8)
)

// IsWhite reports whether the object is currently white (either white0 or white1).
func (h *GCHeader) IsWhite() bool { return h.Mark&MarkWhiteBits != 0 }

// IsBlack reports whether the object is currently black.
func (h *GCHeader) IsBlack() bool { return h.Mark&MarkBlack != 0 }

// IsGray reports whether the object is currently gray (neither white nor black).
func (h *GCHeader) IsGray() bool { return h.Mark&MarkColorMask == 0 }

// IsDead reports whether the object has the other (dead) white bit relative to currentWhite.
func (h *GCHeader) IsDead(otherWhite uint8) bool { return h.Mark&otherWhite != 0 }

// IsFinalized reports whether FINALIZEDBIT is set on this object.
func (h *GCHeader) IsFinalized() bool { return h.Mark&MarkFinalized != 0 }

// SetFinalized sets FINALIZEDBIT on this object.
func (h *GCHeader) SetFinalized() { h.Mark |= MarkFinalized }

// ClearFinalized clears FINALIZEDBIT on this object.
func (h *GCHeader) ClearFinalized() { h.Mark &^= MarkFinalized }

// SetGray transitions the object to gray while preserving FINALIZEDBIT and age.
func (h *GCHeader) SetGray() { h.Mark &^= MarkColorMask }

// SetBlack transitions the object to black while preserving FINALIZEDBIT and age.
func (h *GCHeader) SetBlack() { h.Mark = (h.Mark &^ MarkColorMask) | MarkBlack }

// SetWhite transitions the object to the specified white bit (MarkWhite0 or MarkWhite1)
// while preserving FINALIZEDBIT and age.
func (h *GCHeader) SetWhite(white uint8) { h.Mark = (h.Mark &^ MarkColorMask) | (white & MarkWhiteBits) }

// Age returns the generational age (AgeNew..AgeTouched2).
func (h *GCHeader) Age() uint8 { return (h.Mark & AgeMask) >> AgeShift }

// SetAge updates the generational age while preserving color and FINALIZEDBIT.
func (h *GCHeader) SetAge(age uint8) { h.Mark = (h.Mark &^ AgeMask) | ((age & 0x7) << AgeShift) }

// IsOld reports whether the object's age is beyond G_SURVIVAL (AgeOld0..AgeTouched2).
func (h *GCHeader) IsOld() bool { return h.Age() > AgeSurvival }
