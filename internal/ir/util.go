package gocl

// Helpers shared by the IR expression and statement generators: literal
// classification, numeric conversions, string constants and the small type
// predicates the emitters ask for.

import (
	"goc/frontend"
	"math"
	"strconv"
)

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func f64bits(f float64) uint64 { return math.Float64bits(f) }
func f32bits(f float64) uint32 { return math.Float32bits(float32(f)) }

// floatOrDouble picks the C type a floating literal has: an unsuffixed literal
// is a double, an "f" suffixed one a float.
func floatOrDouble(n *frontend.NumLit) *frontend.Type {
	// IsFloat is the "f" suffix: it picks float, and everything else that
	// reached here is a double.
	if n.IsFloat {
		return frontend.FloatType()
	}
	return frontend.DoubleType()
}

// numLitType gives an integer literal its C type, which the suffix and the
// value's magnitude together decide.
func numLitType(n *frontend.NumLit) *frontend.Type {
	w := 4
	switch {
	case n.Long:
		w = 8
	case n.Unsig:
		w = 4
	}
	if !n.Long && !n.Unsig && n.Val > 0x7fffffff {
		w = 8 // a value too large for int promotes to long
	}
	t := &frontend.Type{Kind: frontend.KInt, Width: w, Signed: !n.Unsig}
	return t
}

func bytesToBytes(b []byte) []byte { return b }

// --- conversions ------------------------------------------------------------

// convert renders a value of type from as type to. LLVM has no implicit
// conversions, so every C conversion becomes an explicit instruction; one that
// changes nothing is left alone so the IR stays readable.
//
// This is the type-to-type form. convertTo is the same thing for a target that
// is already rendered as text, and the two share the pointer cases below --
// having them drift apart is how a `trunc ptr to i32` reaches LLVM, which it
// rejects because no such cast exists.
func (e *irEmitter) convert(op string, from, to *frontend.Type) string {
	// An array target decays the same way an array source does. A string
	// literal is a char array whose value is already a pointer to its first
	// element, so asking to convert it "to [1 x i8]" asked for a bitcast of a
	// pointer to an array, which has no valid opcode in LLVM.
	if to != nil && to.Kind == frontend.KArr {
		to = frontend.PtrType(to.Elem)
	}
	// long double is a runtime conversion in both directions, and it has to be
	// recognised before the float/integer test below: isFloatTy is false for
	// it (only float and double are), so "long double -> double" would read as
	// "integer -> float" and reach convertTo, where the i128 source counts as
	// an integer and gets truncated instead of converted.
	if isLongDouble(from) || isLongDouble(to) {
		if isLongDouble(from) && isLongDouble(to) {
			return op
		}
		if isLongDouble(from) {
			// The target's C signedness decides which runtime entry point
			// handles the conversion: goc_tf_to_ull saturates an out-of-range
			// value at ULLONG_MAX, while goc_tf_to_ll stops at LLONG_MAX. A
			// long double -> unsigned long long that forgets the unsigned flag
			// takes the signed path and hands back 0x7fff... instead of
			// 0xffff..., so the signedness must ride along from here.
			return e.tfFromLDTo(op, e.ty(to), to != nil && !to.Signed)
		}
		return e.tfToLD(op, from)
	}
	// Integer <-> floating point is a value conversion, not a reinterpretation,
	// and it has to be done here rather than in convertTo because the signedness
	// of the target is a C fact that the target's IR spelling ("i32") does not
	// carry. Left to convertTo it fell through to that function's
	// store-and-load fallback, which re-reads the stored bit pattern as the
	// other type: "f + 1" stored the integer 1 and loaded it back as a float,
	// so the add saw 1.4e-45 and every float expression with an integer operand
	// silently evaluated to 0. LLVM has opcodes for both directions.
	if isFloatTy(from) != isFloatTy(to) {
		ff, tf := isFloatTy(from), isFloatTy(to)
		intTy := func(t *frontend.Type) bool {
			return t != nil && (t.Kind == frontend.KInt || t.Kind == frontend.KBool)
		}
		if tf && intTy(from) {
			// An i1 source is neither signed nor unsigned: LLVM's i1 holds
			// 0 and -1, so sitofp turns true into -1.0 and "(double)b"
			// printed -1.000000 for a _Bool holding 1. Widen with zext
			// first -- the three-step clang uses, and what convertTo's copy
			// of this branch already does.
			src, f := op, e.ty(from)
			if f == "i1" {
				w := e.newTmp()
				e.line("%s = zext i1 %s to i64", w, op)
				src, f = w, "i64"
			}
			v := e.newTmp()
			opc := "uitofp"
			if from.Signed {
				opc = "sitofp"
			}
			e.line("%s = %s %s %s to %s", v, opc, f, src, e.ty(to))
			return v
		}
		if ff && intTy(to) {
			v := e.newTmp()
			opc := "fptoui"
			if to.Signed {
				opc = "fptosi"
			}
			e.line("%s = %s %s %s to %s", v, opc, e.ty(from), op, e.ty(to))
			return v
		}
	}
	return e.convertTo(op, from, e.ty(to))
}

func floatBits(t *frontend.Type) (int, bool) {
	if t == nil {
		return 0, false
	}
	if t.Kind == frontend.KFloat {
		return 32, true
	}
	if t.Kind == frontend.KDouble {
		return 64, true
	}
	return 0, false
}

func isFloatTy(t *frontend.Type) bool {
	_, ok := floatBits(t)
	return ok
}

// widthOf returns a type's width in bits, counting pointers as 64.
func widthOf(t *frontend.Type) int {
	if t == nil {
		return 32
	}
	switch t.Kind {
	case frontend.KInt, frontend.KBool:
		return t.Width * 8
	case frontend.KFloat:
		return 32
	case frontend.KDouble:
		return 64
	case frontend.KPtr, frontend.KFunc, frontend.KArr:
		return 64
	}
	return 64
}

// scratchSlot returns an alloca of at least n bytes usable as a conversion
// staging buffer. The conversions that need one are rare and the slot is
// reused, so it is created once per function on first use -- and re-created,
// larger, when a later conversion needs more room. One fixed eight-byte slot
// was enough until a struct went through it: the store overwrote whatever
// followed, and the load read zeros for every member past the first eight
// bytes, so "S local = gs;" copied the length and lost the pointer.
func (e *irEmitter) scratchSlot(n int) string {
	if e.scratch != "" && e.scratchSize >= n {
		return e.scratch
	}
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca [" + itoa(n) + " x i8], align 16\n")
	e.scratch = slot
	e.scratchSize = n
	return slot
}

// ldVarargSlot hands out one of the shared entry allocas that a call stores a
// long double variadic argument into before passing its ADDRESS (call.go's
// variadic marshalling). The pool is per-function because the slots are entry
// allocas: emitting them in the call's own basic block would grow the stack on
// every loop iteration the call sits in. A call consumes the pool from index
// zero -- callExpr and indirectCall reset ldVarargUsed before marshalling -- so
// two arguments of one call get distinct slots while calls that run one after
// another reuse them.
func (e *irEmitter) ldVarargSlot() string {
	if e.ldVarargUsed < len(e.ldVararg) {
		s := e.ldVararg[e.ldVarargUsed]
		e.ldVarargUsed++
		return s
	}
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca i128, align 16\n")
	e.ldVararg = append(e.ldVararg, slot)
	e.ldVarargUsed++
	return slot
}

// storeBrace writes a braced initialiser into an object of type t at address p.
func (e *irEmitter) storeBrace(b *frontend.BraceInit, t *frontend.Type, p string) {
	e.storeInit(b, t, p, 0)
}

// storeInit writes the elements of b into the aggregate at p, starting at
// byte offset off within it.
func (e *irEmitter) storeInit(b *frontend.BraceInit, t *frontend.Type, p string, off int) {
	if b == nil || t == nil {
		return
	}
	switch t.Kind {
	case frontend.KArr:
		esz := frontend.Sizeof(t.Elem)
		for i, el := range b.Elems {
			idx := i
			if el.DesigIdx >= 0 {
				idx = el.DesigIdx
			}
			slot := e.gep(p, esz, int64(idx))
			if nested, ok := el.E.(*frontend.BraceInit); ok {
				e.storeInit(nested, t.Elem, slot, 0)
				continue
			}
			e.storeScalar(slot, el.E, t.Elem)
		}
	case frontend.KStruct:
		// Positional initialisation walks the members C can name, which is
		// not the whole member list: an anonymous struct/union member keeps
		// a shell entry for size/offset accounting, and that shell must not
		// consume an initialiser -- its promoted sub-members are separate
		// entries and pair with the initialisers directly (C11's
		// transparent-initialisation rule; frontend.PosMembers). Counting the
		// shell shifted every later field by one: {1,2,3,10,20,'Z'} put 2
		// into the shell and 'Z' into `a`.
		vis := frontend.PosMembers(t)
		for i, el := range b.Elems {
			if i >= len(vis) {
				break
			}
			mem := vis[i]
			if el.Desig != "" {
				if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 {
					mem = t.Members[mi]
				}
			}
			if mem.Type == nil {
				continue
			}
			slot := e.gep(p, 1, int64(off+mem.Offset))
			if nested, ok := el.E.(*frontend.BraceInit); ok {
				e.storeInit(nested, mem.Type, slot, 0)
				continue
			}
			e.storeScalar(slot, el.E, mem.Type)
		}
	case frontend.KUnion:
		// A union initialiser names at most one member: the first visible
		// one, or the one a designator picks. Everything after the first
		// element is ignored -- writing it would clobber the member that was
		// just initialised, since every union member shares offset 0.
		if len(b.Elems) == 0 {
			return
		}
		el := b.Elems[0]
		var mem *frontend.Member
		if vis := frontend.PosMembers(t); len(vis) > 0 {
			mem = vis[0]
		}
		if el.Desig != "" {
			if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 {
				mem = t.Members[mi]
			}
		}
		if mem == nil || mem.Type == nil {
			return
		}
		slot := e.gep(p, 1, int64(off+mem.Offset))
		if nested, ok := el.E.(*frontend.BraceInit); ok {
			e.storeInit(nested, mem.Type, slot, 0)
			return
		}
		e.storeScalar(slot, el.E, mem.Type)
	default:
		// A braced scalar: "{ 5 }" is just 5.
		if len(b.Elems) > 0 {
			e.storeScalar(p, b.Elems[0].E, t)
		}
	}
	_ = off
}

// storeScalar evaluates e and stores it into the object of type t at p.
func (e *irEmitter) storeScalar(p string, x frontend.Expr, t *frontend.Type) {
	if x == nil {
		return
	}
	// A string literal initialising a char array copies its bytes into the
	// array ("struct { char name[8]; } m = {\"hi\", ...}"). Evaluating the
	// literal would decay it to a pointer and coerce() would then be a
	// pointer-to-pointer no-op, so the initialiser came out "store [8 x i8]
	// <ptr>", which LLVM rejects. storeString copies the bytes and zero-fills
	// the tail, which is what the initialiser means.
	if sl, ok := x.(*frontend.StrLit); ok && t != nil && t.Kind == frontend.KArr {
		e.storeString(sl, t, p)
		return
	}
	if nested, ok := x.(*frontend.BraceInit); ok {
		e.storeInit(nested, t, p, 0)
		return
	}
	v := e.eval(x)
	e.store(p, e.coerce(v, t))
}

// coerce converts a value to a target type where the assignment requires it.
func (e *irEmitter) coerce(v val, to *frontend.Type) val {
	if to == nil || v.ty == nil {
		return v
	}
	if e.ty(v.ty) == e.ty(to) {
		return val{op: v.op, ty: to}
	}
	return val{op: e.convert(v.op, v.ty, to), ty: to}
}

// gep builds a getelementptr for an array of elements of size esz.
func (e *irEmitter) gep(p string, esz int, idx int64) string {
	v := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", v, p, idx*int64(esz))
	return v
}

// storeString copies a string literal's bytes into a char array. A C initialiser
// of the form `char s[] = "x"` copies; assigning the same literal to a char*
// takes its address instead, and the two are different instructions.
func (e *irEmitter) storeString(sl *frontend.StrLit, t *frontend.Type, slot string) {
	n := t.Len
	if n <= 0 {
		n = len(sl.Bytes) + 1
	}
	// Copying literal bytes as i8 stores: LLVM has no memcpy here, and a byte
	// at a time is what the initialiser means.
	for i := 0; i < n; i++ {
		var b byte
		if i < len(sl.Bytes) {
			b = sl.Bytes[i]
		}
		dst := e.newTmp()
		e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", dst, slot, i)
		e.line("store i8 %d, ptr %s, align 1", int(b), dst)
	}
}

// entryAlloca allocates a typed slot in the function's ENTRY block and returns
// its name.
//
// The entry block is the only one every path executes, so a slot placed there
// is addressable from anywhere in the body. That is what lets a value be
// written on one branch of a short-circuit operator and read back after the
// join -- see logical, which needs that because a phi's constant propagation
// gets a float comparison wrong.
func (e *irEmitter) entryAlloca(irTy string) string {
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca " + irTy + ", align 8\n")
	return slot
}
