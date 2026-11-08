package gocl

// long double in LLVM IR: the lowering, and the calls into goclib's software
// binary128 runtime.
//
// goc's long double is IEEE binary128 on every target, so the IR never asks
// LLVM to do arithmetic on it. It is carried as an opaque 128-bit integer --
// the bit pattern and nothing more -- and every operation becomes a call back
// into goclib/fp128.c. Two reasons:
//
//	1. LLVM's own fp128 would be legalised straight into __addtf3 &c, whose ABI
//	   is an fp128 in XMM0. goclib has no way to write that (it has to compile
//	   under goc, which has no such type), and a missing libcall is a link
//	   error with no diagnosis at the point that caused it.
//	2. i128 keeps the layout honest for free: LLVM gives it 16-byte alignment,
//	   so a struct member, an alloca and a global all land where C says a long
//	   double lands. A "{ i64, i64 }" would be 8-byte aligned and put
//	   "struct { char c; long double x; }"'s x at offset 8 instead of 16.
//
// The runtime's own entry points pass the value as two i64 halves in a
// two-member struct (goc_tf128), which is what a C caller of goclib sees and
// what the native back end will call in #48. Crossing between the two shapes
// is the whole of this file.

import (
	"goc/frontend"
	"math/big"
)

// tfIRType is the IR type of a long double value; tfPairType is the IR type of
// the runtime's argument and result.
const (
	tfIRType   = "i128"
	tfPairType = "%tf128"
)

// tfPairDecl emits the runtime's pair type once per module.
func (m *irMod) tfPairDecl() {
	if m.structs["tf128"] {
		return
	}
	m.structs["tf128"] = true
	m.typeLines = append(m.typeLines, "%tf128 = type { i64, i64 }")
}

// isLongDouble reports whether a C type is long double.
func isLongDouble(t *frontend.Type) bool {
	return t != nil && t.Kind == frontend.KLongDouble
}

// --- moving between the two shapes -----------------------------------------

// tfSplit breaks a long double value into its two 64-bit halves. Lo is bits
// 63..0, which is what sits at offset 0 of the little-endian bit pattern and
// therefore what goc_tf128 calls its first member.
func (e *irEmitter) tfSplit(op string) (lo, hi string) {
	lo = e.newTmp()
	e.line("%s = trunc %s %s to i64", lo, tfIRType, op)
	sh := e.newTmp()
	e.line("%s = lshr %s %s, 64", sh, tfIRType, op)
	hi = e.newTmp()
	e.line("%s = trunc %s %s to i64", hi, tfIRType, sh)
	return lo, hi
}

// tfJoin puts the two halves back into one i128.
func (e *irEmitter) tfJoin(lo, hi string) string {
	a := e.newTmp()
	e.line("%s = zext i64 %s to %s", a, hi, tfIRType)
	b := e.newTmp()
	e.line("%s = shl %s %s, 64", b, tfIRType, a)
	c := e.newTmp()
	e.line("%s = zext i64 %s to %s", c, lo, tfIRType)
	d := e.newTmp()
	e.line("%s = or %s %s, %s", d, tfIRType, b, c)
	return d
}

// tfPair builds the runtime's argument shape out of two halves. Member 0 is
// lo, matching the C declaration in goclib.h.
func (e *irEmitter) tfPair(lo, hi string) string {
	p := e.newTmp()
	e.line("%s = insertvalue %s undef, i64 %s, 0", p, tfPairType, lo)
	q := e.newTmp()
	e.line("%s = insertvalue %s %s, i64 %s, 1", q, tfPairType, p, hi)
	return q
}

// --- calls into the runtime -------------------------------------------------

// tfUn calls a one-operand runtime entry point (goc_tf_neg).
func (e *irEmitter) tfUn(a string, fn string) string {
	alo, ahi := e.tfSplit(a)
	r := e.newTmp()
	e.line("%s = call %s @%s(%s %s)", r, tfPairType, fn, tfPairType, e.tfPair(alo, ahi))
	return e.tfTake(r)
}

// tfBin calls a two-operand runtime entry point (goc_tf_add and friends).
func (e *irEmitter) tfBin(a, b string, fn string) string {
	alo, ahi := e.tfSplit(a)
	blo, bhi := e.tfSplit(b)
	r := e.newTmp()
	e.line("%s = call %s @%s(%s %s, %s %s)", r, tfPairType, fn,
		tfPairType, e.tfPair(alo, ahi), tfPairType, e.tfPair(blo, bhi))
	return e.tfTake(r)
}

// tfTake turns a runtime result back into an i128.
func (e *irEmitter) tfTake(r string) string {
	lo := e.newTmp()
	e.line("%s = extractvalue %s %s, 0", lo, tfPairType, r)
	hi := e.newTmp()
	e.line("%s = extractvalue %s %s, 1", hi, tfPairType, r)
	return e.tfJoin(lo, hi)
}

// tfArith lowers a long double arithmetic operator. "%" is not reachable: the
// front end rejects it, as C does, because both operands are floating.
func (e *irEmitter) tfArith(a, b, op string) string {
	switch op {
	case "add":
		return e.tfBin(a, b, "goc_tf_add")
	case "sub":
		return e.tfBin(a, b, "goc_tf_sub")
	case "mul":
		return e.tfBin(a, b, "goc_tf_mul")
	case "div":
		return e.tfBin(a, b, "goc_tf_div")
	}
	return e.tfBin(a, b, "goc_tf_add")
}

// tfZero is the i128 spelling of +0.0, which is the all-zero bit pattern.
const tfZero = "0"

// tfCmp lowers a long double comparison. The runtime answers -1, 0 or 1 for
// ordered pairs and 2 for an unordered one, which is the one number C's six
// operators disagree about: every comparison involving a NaN is false except
// "!=", and comparing the answer against a bound is what says so.
//
//	<   c == -1        <=  c <= 0   (2, unordered, is excluded)
//	>   c == 1         >=  c <  2 unsigned  (-1 is a huge unsigned, excluded)
//	==  c == 0         !=  c != 0
func (e *irEmitter) tfCmp(a, b, op string) string {
	c := e.newTmp()
	e.line("%s = call i32 @goc_tf_cmp(%s %s, %s %s)", c,
		tfPairType, e.tfPairFrom(a), tfPairType, e.tfPairFrom(b))
	t := e.newTmp()
	switch op {
	case "==":
		e.line("%s = icmp eq i32 %s, 0", t, c)
	case "!=":
		e.line("%s = icmp ne i32 %s, 0", t, c)
	case "<":
		e.line("%s = icmp eq i32 %s, -1", t, c)
	case ">":
		e.line("%s = icmp eq i32 %s, 1", t, c)
	case "<=":
		e.line("%s = icmp sle i32 %s, 0", t, c)
	case ">=":
		e.line("%s = icmp ult i32 %s, 2", t, c)
	default:
		e.line("%s = icmp eq i32 %s, 0", t, c)
	}
	return t
}

// tfPairFrom is tfSplit followed by tfPair; the two-operand helpers take the
// halves separately so they can avoid building a pair twice.
func (e *irEmitter) tfPairFrom(v string) string {
	lo, hi := e.tfSplit(v)
	return e.tfPair(lo, hi)
}

// tfTruth lowers a long double used as a condition: it is false only when it
// is a zero, and a NaN is not. Comparing against +0 through the runtime is
// what keeps a value like 1e-4000 (whose integer conversion is 0) truthy.
func (e *irEmitter) tfTruth(v string) string {
	c := e.newTmp()
	e.line("%s = call i32 @goc_tf_cmp(%s %s, %s %s)", c,
		tfPairType, e.tfPairFrom(v), tfPairType, e.tfPair(tfZero, tfZero))
	t := e.newTmp()
	e.line("%s = icmp ne i32 %s, 0", t, c)
	return t
}

// --- conversions ------------------------------------------------------------

// tfFromLD converts a long double to another IR type. Every one of these is a
// runtime call: the float and double forms hand back the BIT PATTERN of the
// narrower float, so they are bitcast rather than a numeric reinterpretation
// (a bitcast of the i128 itself would be a 128-bit reinterpretation, which is
// not what "convert to double" means).
func (e *irEmitter) tfFromLDTo(op string, toIR string, toUnsigned bool) string {
	p := e.tfPairFrom(op)
	switch toIR {
	case "double":
		b := e.newTmp()
		e.line("%s = call i64 @goc_tf_to_double(%s %s)", b, tfPairType, p)
		v := e.newTmp()
		e.line("%s = bitcast i64 %s to double", v, b)
		return v
	case "float":
		b := e.newTmp()
		e.line("%s = call i32 @goc_tf_to_float(%s %s)", b, tfPairType, p)
		v := e.newTmp()
		e.line("%s = bitcast i32 %s to float", v, b)
		return v
	}
	// Integer targets go through the 64-bit conversion, which is the only
	// width the runtime offers for the integer direction; narrower ones are
	// then truncated. An i1 is "is this nonzero", not a truncation.
	//
	// An unsigned target takes the unsigned entry point: the two do not merely
	// differ in how they truncate, they disagree about a negative value, which
	// one saturates at 0 and the other does not.
	fn := "goc_tf_to_ll"
	if toUnsigned {
		fn = "goc_tf_to_ull"
	}
	v := e.newTmp()
	e.line("%s = call i64 @%s(%s %s)", v, fn, tfPairType, p)
	if toIR == "i1" {
		t := e.newTmp()
		e.line("%s = icmp ne i64 %s, 0", t, v)
		return t
	}
	if toIR == "i64" {
		return v
	}
	t := e.newTmp()
	e.line("%s = trunc i64 %s to %s", t, v, toIR)
	return t
}

// tfFromLD converts a long double to a type known only by its IR spelling.
// The signedness of an integer target is lost on this path, so it takes the
// signed conversion; convert() supplies the C type and is the path that cares.
func (e *irEmitter) tfFromLD(op string, toIR string) string {
	return e.tfFromLDTo(op, toIR, false)
}

// tfToLD converts another IR type to long double. Signedness is a C fact the
// IR string does not carry, so the caller passes the source type; a pointer
// has no signedness and is widened the way an address is.
func (e *irEmitter) tfToLD(op string, from *frontend.Type) string {
	if from != nil && from.Kind == frontend.KLongDouble {
		return op
	}
	switch e.ty(from) {
	case "double":
		b := e.newTmp()
		e.line("%s = bitcast double %s to i64", b, op)
		return e.tfFromBits("goc_tf_from_double", "i64", b)
	case "float":
		b := e.newTmp()
		e.line("%s = bitcast float %s to i32", b, op)
		return e.tfFromBits("goc_tf_from_float", "i32", b)
	}
	// Integers: the runtime wants a 64-bit value, and which of the two entry
	// points it is depends on the C signedness. An i1 carries 0 and -1, so it
	// has to be widened as unsigned or it becomes -1.0.
	fn := "goc_tf_from_ll"
	if from == nil || !from.Signed || e.ty(from) == "i1" {
		fn = "goc_tf_from_ull"
	}
	src := op
	if e.ty(from) != "i64" {
		w := e.newTmp()
		opc := "sext"
		if fn == "goc_tf_from_ull" {
			opc = "zext"
		}
		e.line("%s = %s %s %s to i64", w, opc, e.ty(from), op)
		src = w
	}
	return e.tfFromBits(fn, "i64", src)
}

// tfFromBits calls a one-argument widening entry point and takes its result.
func (e *irEmitter) tfFromBits(fn, argIR, arg string) string {
	r := e.newTmp()
	e.line("%s = call %s @%s(%s %s)", r, tfPairType, fn, argIR, arg)
	return e.tfTake(r)
}

// --- constants --------------------------------------------------------------

// tfRuntime names every goclib entry point this file can emit a call to.
//
// They are reached from generated code, not from the source, so the
// reachability sweep that prunes the C runtime cannot see them -- the same
// reason the soft-float helpers (__adddf3 & co) are named by hand in
// llvmRoots. Unlike those, which a target either needs or does not, these are
// pulled in only for a program that actually mentions long double: 16 small
// functions in every binary would be a size regression for the far more
// common program that never says the words.
var tfRuntime = []string{
	"goc_tf_add", "goc_tf_sub", "goc_tf_mul", "goc_tf_div", "goc_tf_neg",
	"goc_tf_cmp",
	"goc_tf_from_double", "goc_tf_from_float",
	"goc_tf_to_double", "goc_tf_to_float",
	"goc_tf_from_ll", "goc_tf_from_ull",
	"goc_tf_to_ll", "goc_tf_to_ull", "goc_tf_to_int", "goc_tf_to_uint",
}

// usesLongDouble reports whether a program mentions long double: a
// declaration, a parameter, a return type, a cast, or an l/L-suffixed literal.
//
// A pointer to long double counts, because what is interesting is not the
// pointer but the value it is dereferenced into, and a literal counts because
// "double d = 7.5L;" is a long double constant converted to double -- the
// conversion is a runtime call even though no declaration says long double.
func usesLongDouble(prog *frontend.Program) bool {
	if !frontend.EnableLongDouble {
		return false
	}
	var inType func(t *frontend.Type) bool
	inType = func(t *frontend.Type) bool {
		if t == nil {
			return false
		}
		if t.Kind == frontend.KLongDouble {
			return true
		}
		if t.Elem != nil && inType(t.Elem) {
			return true
		}
		for i := range t.Members {
			if inType(t.Members[i].Type) {
				return true
			}
		}
		return false
	}
	for _, g := range prog.Globals {
		if inType(g.Typ) {
			return true
		}
	}
	for _, f := range prog.Funcs {
		if inType(f.Ret) {
			return true
		}
		for _, pt := range f.ParamTypes {
			if inType(pt) {
				return true
			}
		}
		found := false
		frontend.WalkStmts(f.Body, func(s frontend.Stmt) {
			if found {
				return
			}
			// A declaration is where the type is named: "long double x;" is a
			// DeclStmt, and a local's type appears nowhere else.
			if d, ok := s.(*frontend.DeclStmt); ok && inType(d.Typ) {
				found = true
				return
			}
			for _, e := range stmtExprs(s) {
				walkExpr(e, func(n frontend.Expr) {
					if found {
						return
					}
					switch x := n.(type) {
					case *frontend.CastExpr:
						if inType(x.Typ) {
							found = true
						}
					case *frontend.NumLit:
						if x.Kind == frontend.TF128 {
							found = true
						}
					}
				})
			}
		})
		if found {
			return true
		}
	}
	return false
}

// tfConst spells a binary128 encoding as an i128 literal: the 128-bit pattern
// is one unsigned integer, written in decimal because that is the only base
// LLVM accepts for an arbitrary-width integer constant.
func tfConst(hi, lo uint64) string {
	n := new(big.Int).Lsh(new(big.Int).SetUint64(hi), 64)
	n.Or(n, new(big.Int).SetUint64(lo))
	return n.String()
}
