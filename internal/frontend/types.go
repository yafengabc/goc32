package frontend

import (
	"fmt"
	"strings"
)

// Type is the structured C type used by the front end (parser + checker).
//
// The code generator does not consume Type directly: it only cares whether a
// value rides in a GP register (integer class) or an XMM register (double), so
// Type.Class() collapses everything except double into the CType TInt. Pointers
// and all integer widths share that 8-byte integer slot on the toy stack model.
type TypeKind int

const (
	KVoid TypeKind = iota
	KInt
	KDouble
	KFloat
	KPtr
	KArr
	KFunc
	KStruct
	KUnion
	KBitInt // C23 _BitInt(N): arbitrary-width two's-complement integer
	KBool   // for _Bool type
	// KLongDouble is long double. goc spells it IEEE binary128 (16 bytes)
	// on every target and implements it in software rather than adopting
	// whatever the hardware offers (x87 80-bit on x86-64, binary128 on
	// aarch64): one format everywhere is what lets a long double program
	// produce identical bits under both back ends. See fp128.go.
	KLongDouble
)

// Member is a single field of a struct or union.
type Member struct {
	Name   string // field name (may be "" for an anonymous nested member)
	Type   *Type  // field type
	Offset int    // byte offset within the struct/union (0 for all union members)
	// BitWidth > 0 marks this member as a bit-field occupying BitWidth bits
	// starting at bit offset BitOff within its natural storage unit. Bit-fields
	// are handled later; for now only the data members are materialised.
	BitWidth int
	BitOff   int
	// AnonBase is non-nil for a sub-member promoted out of an anonymous
	// struct/union member (C11): Offset then holds the offset *within* the
	// anonymous member's own type, and computeLayout rewrites it to the
	// absolute position (AnonBase.Offset + Offset) once the anonymous shell
	// has been placed in the enclosing type.
	AnonBase *Member
}

type Type struct {
	Kind TypeKind
	Elem *Type // KPtr / KArr: element type
	Len  int   // KArr: number of elements (0 = incomplete)
	// VLALen is the length expression of a C99 variable-length array. It is
	// non-nil exactly for a VLA dimension, whose extent is only known when the
	// declaration is executed; Len stays 0 there and sizeOf() cannot be used
	// on the type (it would report 0). A VLA therefore has no compile-time
	// size at all -- sizeof on it is a run-time operation, and the storage for
	// a VLA object is a pointer to a run-time stack allocation.
	VLALen   Expr
	Params   []*Type // KFunc: parameter types
	Ret      *Type   // KFunc: return type
	Variadic bool    // KFunc: declared with a trailing "..."
	Width    int     // KInt: 1=char, 2=short, 4=int, 8=long
	Signed   bool    // KInt
	// PlainChar marks the type spelled bare "char". C11 6.2.5p15 makes char,
	// signed char and unsigned char three distinct types even when char's
	// representation matches one of the others, and _Generic (6.5.1.1) is the
	// only place standard C observes the difference. It is set for bare "char"
	// (and for types derived from it through a typedef) but never for an
	// explicitly "signed char" / "unsigned char" spelling. It deliberately
	// stays out of typesEqual so char and signed char remain assignable.
	PlainChar bool
	// LongLong carries the same idea for the other pair of standard types
	// that shares a representation but stays distinct in C: long vs long
	// long (C11 6.2.5p4). Like PlainChar it only ever affects _Generic
	// selection and the printed name, never assignability or codegen.
	// (long double needs no such flag: it is its own Kind, KLongDouble.)
	LongLong   bool      // KInt: spelled "long long"
	Members    []*Member // KStruct / KUnion: ordered fields
	Bits       int       // KBitInt: exact bit width N (1..4096)
	Size       int       // KStruct / KUnion: total size in bytes (aligned)
	Align      int       // KStruct / KUnion: required alignment (0 = not computed)
	Tag        string    // KStruct / KUnion: optional struct tag (named structs)
	Const      bool      // declared with a top-level "const" qualifier
	ConstExpr  bool      // declared with the C23 "constexpr" specifier
	AutoDeduce bool      // C23 "auto" placeholder: type to be inferred from the initialiser
	IsTLS      bool      // declared with _Thread_local / thread_local (C11 TLS)
	Atomic     bool      // C11 _Atomic: read-modify-writes on it carry a LOCK prefix
}

// --- constructors -----------------------------------------------------------

func IntType() *Type                  { return &Type{Kind: KInt, Width: 4, Signed: true} }
func CharType() *Type                 { return &Type{Kind: KInt, Width: 1, Signed: true, PlainChar: true} }
func UnsignedType() *Type             { return &Type{Kind: KInt, Width: 4, Signed: false} }
func UnsignedCharType() *Type         { return &Type{Kind: KInt, Width: 1, Signed: false} }
func DoubleType() *Type               { return &Type{Kind: KDouble} }
func FloatType() *Type                { return &Type{Kind: KFloat} }
func LongDoubleType() *Type           { return &Type{Kind: KLongDouble, Size: 16, Align: 16} }
func VoidType() *Type                 { return &Type{Kind: KVoid} }
func PtrType(elem *Type) *Type        { return &Type{Kind: KPtr, Elem: elem} }
func ArrType(elem *Type, n int) *Type { return &Type{Kind: KArr, Elem: elem, Len: n} }
func FuncType(ret *Type, params []*Type) *Type {
	return &Type{Kind: KFunc, Ret: ret, Params: params}
}

// VLAArrType builds a C99 variable-length array type: n elements of elem,
// where n is an expression evaluated when the declaration is reached.
func VLAArrType(elem *Type, n Expr) *Type {
	return &Type{Kind: KArr, Elem: elem, Len: 0, VLALen: n}
}

// StructType builds an (initially incomplete) struct/union type carrying the
// supplied members; the caller must call computeLayout once the members are
// final.
func StructType(members []*Member) *Type {
	return &Type{Kind: KStruct, Members: members}
}

func UnionType(members []*Member) *Type {
	return &Type{Kind: KUnion, Members: members}
}

// --- layout -----------------------------------------------------------------

// alignOf returns the natural alignment of a type on the Win64 / System V x86-64
// ABIs: char=1, short=2, int/long=4/8, float=4, double/pointer=8, struct=its own
// Align.
func alignOf(t *Type) int {
	if t == nil {
		return 1
	}
	// An explicit alignment (from _Alignas) overrides the natural alignment of
	// any type, including scalars -- so _Alignof(aligned_var) reports it.
	if t.Align != 0 {
		return t.Align
	}
	switch t.Kind {
	case KInt:
		return t.Width // 1, 2, 4, or 8
	case KBool:
		return 1
	case KFloat:
		return 4
	case KLongDouble:
		return 16
	case KDouble, KPtr, KFunc:
		return 8
	case KArr:
		return alignOf(t.Elem)
	case KStruct, KUnion:
		if t.Align != 0 {
			return t.Align
		}
	case KBitInt:
		return 8 // word-aligned storage
	}
	return 8
}

// sizeOf returns the total byte size of a type (the storage a value occupies).
func sizeOf(t *Type) int {
	if t == nil {
		return 1
	}
	switch t.Kind {
	case KInt:
		return t.Width
	case KBool:
		return 1
	case KFloat:
		return 4
	case KLongDouble:
		return 16
	case KDouble, KPtr, KFunc:
		return 8
	case KArr:
		return sizeOf(t.Elem) * t.Len
	case KStruct, KUnion:
		if t.Size != 0 {
			return t.Size
		}
	case KBitInt:
		// Whole 64-bit words: little-endian word array, 8-byte aligned.
		w := (t.Bits + 63) / 64
		if w < 1 {
			w = 1
		}
		return w * 8
	}
	return 8
}

// computeLayout fills Size and Align for a struct or union using MSVC x64
// packing rules (each member aligned to its natural alignment; the struct is
// padded to a multiple of its maximum member alignment). Union members all
// share offset 0 and the union size is the largest member size.
func (t *Type) computeLayout() {
	if t.Kind != KStruct && t.Kind != KUnion {
		return
	}
	align := 1
	if t.Kind == KUnion {
		maxSize := 0
		for _, m := range t.Members {
			if m.AnonBase != nil {
				continue // promoted sub-member: placed in the final pass below
			}
			a := alignOf(m.Type)
			if a > align {
				align = a
			}
			m.Offset = 0
			sz := sizeOf(m.Type)
			if sz > maxSize {
				maxSize = sz
			}
		}
		t.Align = align
		t.Size = maxSize
		return
	}
	off := 0
	// Bit-fields are allocated inside the storage unit of their declared type
	// (int => a 4-byte unit, char => 1 byte, ...), packing low-bit-first from
	// the start of each unit. A bit-field that would cross its unit's boundary
	// is pushed into the next aligned unit instead (MSVC rule: bit-fields never
	// straddle their base type). A zero-width unnamed bit-field occupies no
	// storage but forces the following member into a fresh unit. `end` tracks
	// the furthest byte actually occupied so a trailing bit-field unit is
	// counted in the struct's size.
	end := 0
	bitOff := 0
	unitSize := 0 // byte size of the open bit-field storage unit (0 = none open)
	for _, m := range t.Members {
		if m.AnonBase != nil {
			continue // promoted sub-member: placed in the final pass below
		}
		a := alignOf(m.Type)
		if a > align {
			align = a
		}
		if m.BitWidth > 0 {
			u := sizeOf(m.Type) // storage-unit size in bytes (int=4, char=1, ...)
			ub := u * 8
			// A bit-field starts a fresh storage unit when none is open, when
			// it would straddle the current unit, or when its declared base
			// type's storage size differs from the open unit's (each base type
			// keeps its own allocation unit -- MSVC rule).
			if bitOff != 0 && (bitOff+m.BitWidth > ub || u != unitSize) {
				off += unitSize
				bitOff = 0
			}
			if bitOff == 0 {
				if off%a != 0 {
					off += a - (off % a)
				}
				unitSize = u
			}
			m.Offset = off
			m.BitOff = bitOff
			bitOff += m.BitWidth
			if off+u > end {
				end = off + u
			}
			continue
		}
		// A zero-width unnamed bit-field (e.g. "int : 0;") occupies no storage
		// but forces the following member into a fresh storage unit of its own
		// base type (MSVC: "beginning of the next allocation unit"). An
		// anonymous struct/union shell (Name=="", aggregate type) is NOT this:
		// it takes the ordinary-member path below.
		if m.BitWidth == 0 && m.Name == "" && m.Type.Kind != KStruct && m.Type.Kind != KUnion {
			if bitOff != 0 {
				off += unitSize
				bitOff = 0
			}
			u := sizeOf(m.Type)
			if off%u != 0 {
				off += u - (off % u)
			}
			unitSize = 0
			m.Offset = off
			m.BitOff = 0
			continue
		}
		// Ordinary member: normal alignment, and it closes any open bit-field
		// unit (the unit's bytes are part of the layout).
		if bitOff != 0 {
			off += unitSize
			bitOff = 0
			unitSize = 0
		}
		if off%a != 0 {
			off += a - (off % a)
		}
		m.Offset = off
		off += sizeOf(m.Type)
		if off > end {
			end = off
		}
	}
	t.Align = align
	size := end
	if align != 0 && size%align != 0 {
		size += align - (size % align)
	}
	t.Size = size
	// Final pass: promoted sub-members of anonymous struct/union members get
	// their absolute offset (the anonymous shell's position plus the offset
	// the sub-member had inside the anonymous type). Runs for both struct and
	// union parents; for a union parent AnonBase.Offset is 0, so this only
	// restores the relative offset preserved above.
	for _, m := range t.Members {
		if m.AnonBase != nil {
			m.Offset += m.AnonBase.Offset
		}
	}
}

// bigArithResult computes the result type of an arithmetic / bitwise / shift
// operation with at least one _BitInt operand (the simplified C23 usual
// arithmetic conversions): the wider bit-precise type wins, a tie between two
// bitints of equal width goes to the unsigned one, and an int-class operand
// converts to the bitint side. Comparisons yield int. The callers must verify
// that at least one operand is a _BitInt and that the other is an integer
// class (floating/pointer mixes are reported as errors before this runs).
func bigArithResult(op string, lt0, rt0 *Type) *Type {
	// exprType reports nil for numeric literals (no type table); on a
	// _BitInt operation that missing type is the plain int it actually is.
	lt, rt := lt0, rt0
	if lt == nil {
		lt = IntType()
	}
	if rt == nil {
		rt = IntType()
	}
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		return IntType()
	case "<<", ">>":
		if lt.Kind == KBitInt {
			return lt
		}
		return rt
	}
	lb, rb := lt.Kind == KBitInt, rt.Kind == KBitInt
	if lb && rb {
		if lt.Bits != rt.Bits {
			if lt.Bits > rt.Bits {
				return lt
			}
			return rt
		}
		if !lt.Signed {
			return lt
		}
		return rt
	}
	if lb {
		return lt
	}
	return rt
}

// posMembers returns the members that participate in positional brace
// initialisation. Anonymous struct/union shells (C11) are skipped: they do
// not consume an initialiser themselves -- their promoted sub-members are
// separate entries in the member list and pair with the initialisers
// directly, exactly as C11's transparent-initialisation rule requires.
func posMembers(t *Type) []*Member {
	out := make([]*Member, 0, len(t.Members))
	for _, m := range t.Members {
		if m.Name == "" && (m.Type.Kind == KStruct || m.Type.Kind == KUnion) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// --- predicates -------------------------------------------------------------

// Class returns the low-level codegen scalar class for this type. Everything
// that is not floating point is an 8-byte integer-class value (pointers
// included). float shares the double class: a float scalar is always widened to
// a double in registers/temporaries and only narrowed back to 4 bytes at real
// float storage (array elements, struct members, float parameters, returns).
func (t *Type) Class() CType {
	if t.Kind == KDouble || t.Kind == KFloat {
		return TDouble
	}
	// long double is 16 bytes, so it rides in neither the 8-byte integer
	// slot nor a single XMM register: codegen treats it as its own class
	// (a 16-byte aggregate on goc, LLVM fp128 on gocl).
	if t.Kind == KLongDouble {
		return TF128
	}
	return TInt
}

func (t *Type) IsArith() bool {
	return t.Kind == KInt || t.Kind == KDouble || t.Kind == KFloat ||
		t.Kind == KLongDouble || t.Kind == KBool
}
func (t *Type) IsIntClass() bool { return t.Kind == KInt || t.Kind == KBool }
func (t *Type) IsScalar() bool   { return t.IsArith() || t.Kind == KPtr || t.Kind == KBool }
func (t *Type) IsVoid() bool     { return t.Kind == KVoid }
func (t *Type) IsPtr() bool      { return t.Kind == KPtr }
func (t *Type) IsFloat() bool    { return t.Kind == KFloat }

// IsFloating reports whether the type is a real floating-point type (float or
// double), as opposed to the codegen scalar class TDouble which float also
// rides in.
func (t *Type) IsFloating() bool {
	return t.Kind == KDouble || t.Kind == KFloat || t.Kind == KLongDouble
}
func (t *Type) IsLongDouble() bool { return t.Kind == KLongDouble }
func (t *Type) IsArray() bool      { return t.Kind == KArr }
func (t *Type) IsFunc() bool       { return t.Kind == KFunc }
func (t *Type) IsStruct() bool     { return t.Kind == KStruct }
func (t *Type) IsUnion() bool      { return t.Kind == KUnion }

// IsVLA reports whether t is a C99 variable-length array: an array whose
// extent is an expression rather than a constant, so it has no size until the
// declaration that created it has run.
func (t *Type) IsVLA() bool { return t != nil && t.Kind == KArr && t.VLALen != nil }

// HasVLA reports whether any dimension in t's array chain is variable-length
// ("int a[3][n]"). It deliberately does not descend through a pointer:
// "int (*p)[n]" is a pointer, and sizeof(p) is 8 whatever the pointee is.
func (t *Type) HasVLA() bool {
	for t != nil && t.Kind == KArr {
		if t.VLALen != nil {
			return true
		}
		t = t.Elem
	}
	return false
}

// StorageSize is the bytes a variable of type t occupies in the frame. It is
// sizeOf for everything except a type with a variable-length dimension, whose
// object is a *pointer* to a run-time stack allocation rather than the
// elements themselves -- so its storage is the 8-byte pointer, while its
// sizeof is a run-time value. HasVLA rather than IsVLA: "int a[3][n]" is not
// itself a VLA object but is stored exactly like one.
func StorageSize(t *Type) int {
	if t.HasVLA() {
		return 8
	}
	return sizeOf(t)
}

// IsChar reports whether t is a char type. In this dialect char is a 1-byte
// int (signed or unsigned); it is the element type a string literal can
// initialise an array of.
func (t *Type) IsChar() bool { return t != nil && t.Kind == KInt && t.Width == 1 }
func (t *Type) IsBool() bool { return t != nil && t.Kind == KBool }

// PtrElem returns the element type of a pointer/array, or nil.
func (t *Type) PtrElem() *Type {
	if t.Kind == KPtr || t.Kind == KArr {
		return t.Elem
	}
	return nil
}

// String renders a type for diagnostics.
func (t *Type) String() string {
	switch t.Kind {
	case KVoid:
		return "void"
	case KBitInt:
		s := ""
		if !t.Signed {
			s = "unsigned "
		}
		return fmt.Sprintf("%s_BitInt(%d)", s, t.Bits)
	case KBool:
		return "bool"
	case KDouble:
		return "double"
	case KFloat:
		return "float"
	case KLongDouble:
		return "long double"
	case KInt:
		s := ""
		if !t.Signed {
			s = "unsigned "
		}
		switch t.Width {
		case 1:
			return s + "char"
		case 2:
			return s + "short"
		case 4:
			return s + "int"
		default:
			if t.LongLong {
				return s + "long long"
			}
			return s + "long"
		}
	case KPtr:
		if t.Elem == nil {
			return "void*"
		}
		return t.Elem.String() + "*"
	case KArr:
		if t.Elem == nil {
			return "void[]"
		}
		return t.Elem.String() + "[]"
	case KStruct:
		if t.Tag != "" {
			return "struct " + t.Tag
		}
		return "struct{}"
	case KUnion:
		if t.Tag != "" {
			return "union " + t.Tag
		}
		return "union{}"
	case KFunc:
		ret := "void"
		if t.Ret != nil {
			ret = t.Ret.String()
		}
		ps := make([]string, 0, len(t.Params))
		for _, p := range t.Params {
			if p == nil {
				ps = append(ps, "?")
				continue
			}
			ps = append(ps, p.String())
		}
		if t.Variadic {
			ps = append(ps, "...")
		}
		return ret + "(" + strings.Join(ps, ", ") + ")"
	}
	return "?"
}
