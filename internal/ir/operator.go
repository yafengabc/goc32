package gocl

// Operators, calls and aggregates, in LLVM IR.

import (
	"fmt"
	"goc/frontend"
	"strconv"
)

// --- unary ------------------------------------------------------------------

func (e *irEmitter) unary(n *frontend.Unary) val {
	ty := e.tr.exprType(n)
	switch n.Op {
	case "&":
		// The address of an lvalue: genLValue already produces one. A
		// function is not an lvalue, but "&name" is still ordinary C and
		// names the same value the bare designator decays to. lvalue()
		// found no storage for a function name and answered "null", so the
		// pointer held null and the first indirect call jumped through it
		// -- which is how "fp = &mul2" took fnptrs down.
		if fd, ok := e.funcDesignator(n.E); ok {
			return fd
		}
		return val{op: e.lvalue(n.E), ty: frontend.PtrType(ty)}
	case "*":
		// Dereferencing a pointer to a function is the identity. C 6.5.2.3
		// says the operand of "*" that is a pointer to a function designates
		// the function, and a function designator converts straight back to
		// a pointer -- so "*fp" IS fp, not what fp points at. Loading
		// through it read the target's entry bytes as a pointer, and the
		// call that followed jumped through that garbage.
		if isFuncPtrTy(e.tr.exprType(n.E)) {
			// rvalue already loads the pointer out of its slot; handing
			// that back is the whole of the conversion.
			return val{op: e.rvalue(n.E), ty: e.tr.exprType(n.E)}
		}
		p := e.rvalue(n.E)
		return e.load(p, ty)
	case "!":
		if isFloatTy(ty) {
			// "oeq", not "ueq": !x means (x == 0), and a NaN is not equal to
			// zero, so !NaN is false. "ueq" would be true for unordered pairs
			// and answer the opposite. This branch is currently unreached --
			// exprType types a Unary node as int, so ! on a double goes through
			// cond() instead -- but it is the correct lowering and costs nothing.
			v := e.rvalue(n.E)
			t := e.newTmp()
			e.line("%s = fcmp oeq %s %s, 0.0", t, e.ty(ty), v)
			return val{op: t, ty: boolIr()}
		}
		c := e.cond(n.E)
		t := e.newTmp()
		e.line("%s = xor i1 %s, true", t, c)
		return val{op: t, ty: boolIr()}
	case "-":
		v := e.rvalue(n.E)
		// long double: negating the bit pattern would be "sub i128 0, x",
		// which is a two's-complement negation of the encoding and not of the
		// number. Flipping the sign bit is the runtime's job.
		if isLongDouble(ty) {
			return val{op: e.tfUn(v, "goc_tf_neg"), ty: ty}
		}
		t := e.newTmp()
		if isFloatTy(ty) {
			e.line("%s = fneg %s %s", t, e.ty(ty), v)
		} else {
			e.line("%s = sub %s 0, %s", t, e.ty(ty), v)
		}
		return val{op: t, ty: ty}
	case "~":
		v := e.rvalue(n.E)
		t := e.newTmp()
		e.line("%s = xor %s %s, -1", t, e.ty(ty), v)
		return val{op: t, ty: ty}
	}
	return e.eval(n.E)
}

// funcDesignator reports the value of a function designator appearing under
// "&", or false when the operand does not name a function.
//
// "&func" and a bare "func" in a value context are the same value in C, and
// in LLVM a function *is* its address: the symbol reference already is the
// pointer and no load is involved. See ident(), which spells out the same
// conversion for the bare-name case.
func (e *irEmitter) funcDesignator(x frontend.Expr) (val, bool) {
	id, ok := x.(*frontend.Ident)
	if !ok {
		return val{}, false
	}
	if !e.tr.isFuncName(id.Name) {
		return val{}, false
	}
	// irFuncSym, so the address names the symbol the function was defined
	// under; a renamed libfunc taken by pointer would otherwise reference a
	// symbol the module never defines.
	return val{op: "@" + irFuncSym(id.Name), ty: e.fnPtrTy(id.Name)}, true
}

// isFuncPtrTy reports whether t is a pointer to a function, the case where
// unary "*" must not load.
func isFuncPtrTy(t *frontend.Type) bool {
	return t != nil && t.Kind == frontend.KPtr && t.Elem != nil && t.Elem.Kind == frontend.KFunc
}

// --- binary -----------------------------------------------------------------

// binary lowers a binary operator, applying the C conversions the operator
// implies. Both operands are brought to a common type first: the usual
// arithmetic conversions for arithmetic operators, and pointer scaling for "+"
// and "-" on a pointer.
func (e *irEmitter) binary(n *frontend.Binary) val {
	lty := e.tr.exprType(n.L)
	rty := e.tr.exprType(n.R)

	// The logical and short-circuit operators must not evaluate the right side
	// eagerly, so they get their own shape rather than going through here.
	switch n.Op {
	case "&&", "||":
		return e.logical(n, lty, rty)
	}

	l := e.eval(n.L)
	r := e.eval(n.R)

	// Pointer arithmetic: p + i scales the integer by the pointee's size.
	// An array local decays to a pointer when it is an operand ("tb + ti"),
	// so KArr counts as a pointer here exactly as it does for ptr - ptr.
	if n.Op == "+" || n.Op == "-" {
		if lty != nil && (lty.Kind == frontend.KPtr || lty.Kind == frontend.KArr) && rty != nil && rty.Kind == frontend.KInt {
			return e.ptrAdd(l, r, lty, n.Op == "-")
		}
		if rty != nil && (rty.Kind == frontend.KPtr || rty.Kind == frontend.KArr) && lty != nil && lty.Kind == frontend.KInt && n.Op == "+" {
			return e.ptrAdd(r, l, rty, false)
		}
		// ptr - ptr yields a count of elements, not of bytes. An array local
		// decays to a pointer in this context -- `char tb[320]; ... endp -
		// tb` -- so KArr counts as a pointer on either side, with the
		// element size taken from the side that is a declared pointer.
		if n.Op == "-" && lty != nil && rty != nil && lty.Kind == frontend.KPtr &&
			(rty.Kind == frontend.KPtr || rty.Kind == frontend.KArr) {
			esz := frontend.Sizeof(lty.Elem)
			if esz == 0 {
				esz = 1
			}
			lp := e.newTmp()
			rp := e.newTmp()
			e.line("%s = ptrtoint ptr %s to i64", lp, l.op)
			e.line("%s = ptrtoint ptr %s to i64", rp, r.op)
			d := e.newTmp()
			e.line("%s = sub i64 %s, %s", d, lp, rp)
			// The difference of two pointers is a ptrdiff_t, which is 64 bits on
			// this target. Tagging the result as int (i32) while the value is i64
			// made a later widening pass emit "sext i32 %v to i64" against an
			// already-64-bit operand.
			ptrdiff := &frontend.Type{Kind: frontend.KInt, Width: 8, Signed: true}
			if esz == 1 {
				return val{op: d, ty: ptrdiff}
			}
			t := e.newTmp()
			e.line("%s = sdiv i64 %s, %d", t, d, esz)
			return val{op: t, ty: ptrdiff}
		}
	}

	// Shifts do not promote their operands the way arithmetic does: each side is
	// converted independently, and the result has the left operand's type.
	if n.Op == "<<" || n.Op == ">>" {
		li := e.toInt(l, lty)
		ri := e.toInt(r, rty)
		// The shift's type is the left operand's; fall back to its evaluated
		// type when the front end left the node's own type nil (integer
		// constants have no recorded type).
		opTy := lty
		if opTy == nil {
			opTy = l.ty
		}
		// LLVM requires the shift amount to share the value's width, so a count
		// of a different integer width is converted to it before the shift.
		if r.ty != nil && opTy != nil && r.ty.Kind == frontend.KInt && opTy.Kind == frontend.KInt && r.ty.Width != opTy.Width {
			ri = e.convert(ri, r.ty, opTy)
		}
		// A count at or beyond the operand's width is undefined in C, and
		// LLVM's answer is worse than undefined: "shl i32 1, 32" is poison,
		// so every later pass is entitled to assume anything about the
		// result and the program prints whatever the optimizer left in the
		// register -- "1<<32" came out as a different garbage integer on
		// every build. gcc folds such a shift to 0 and the goldens were
		// generated with that, so name the zero here rather than emit an
		// instruction with a poison operand. A runtime count keeps the
		// instruction; only a literal can be judged.
		if w := irIntWidth(e.ty(opTy)); w > 0 {
			if cnt, err := strconv.ParseInt(ri, 10, 64); err == nil && (cnt < 0 || cnt >= int64(w)) {
				return val{op: "0", ty: opTy}
			}
		}
		t := e.newTmp()
		rop := "shl"
		if n.Op == ">>" {
			rop = "lshr"
			if (lty != nil && lty.Signed) || (opTy != nil && opTy.Signed) {
				rop = "ashr"
			}
		}
		e.line("%s = %s %s %s, %s", t, rop, e.ty(opTy), li, ri)
		return val{op: t, ty: opTy}
	}

	// The common type for the usual arithmetic conversions.
	ct, li, ri := e.usualArith(l, r, lty, rty)

	// A division whose DIVISOR is the literal zero is written out here rather
	// than left to fdiv, because LLVM's fdiv by zero is poison rather than the
	// IEEE result. C says 1.0/0.0 is +inf and 0.0/0.0 is a quiet NaN; emitting
	// the fdiv loses both, because a poison operand lets every later pass assume
	// the result is never NaN. That is how NAN -- which is exactly (0.0/0.0) --
	// turned into an ordinary number, and how fmaximum_num(NAN, 1) came to
	// return 1: after inlining, the constant-folded NaN fed an "fcmp une x, x"
	// that was entitled to answer false.
	//
	// Naming the value directly costs nothing and is what every other front end
	// does. Only a literal zero divisor is handled; a runtime division keeps the
	// generated instruction and its IEEE behaviour.
	if n.Op == "/" && isFloatTy(ct) && lty != nil && rty != nil {
		if bits, ok := litDivBits(n.L, n.R, ct); ok {
			t := e.newTmp()
			e.line("%s = bitcast %s %s to %s", t, e.ty(ct), floatConstLit(bits), e.ty(ct))
			return val{op: t, ty: ct}
		}
	}

	switch n.Op {
	case "+", "-", "*":
		return e.arith(val{op: li, ty: ct}, val{op: ri, ty: ct}, llirBin(n.Op), ct)
	case "/", "%":
		return e.arith(val{op: li, ty: ct}, val{op: ri, ty: ct}, llirBin(n.Op), ct)
	case "&", "|", "^":
		// A bitwise operand that is itself a comparison (or logical) result is
		// an i1, but C evaluates the bitwise operators on the promoted integer
		// type, so the instruction is "or i32 ...". usualArith converts each
		// operand by the front-end STATIC type, and the front end types every
		// comparison as int -- so the i1 arrives here still i1 and LLVM rejects
		// "or i32 15, %cmp" with "defined with type 'i1' but expected 'i32'".
		// Coerce the operands by their ACTUAL emitted type instead: that widens
		// the i1 to the common integer width and is a no-op for an operand that
		// already matches ct.
		return e.arith(e.coerce(l, ct), e.coerce(r, ct), llirBin(n.Op), ct)
	case "==", "!=", "<", ">", "<=", ">=":
		// A long double comparison is a runtime call. icmp over the raw i128
		// would order the encodings, which puts every negative number below
		// every positive one only by accident of the sign bit and gets NaN,
		// -0 and the subnormals wrong in ways that look plausible.
		if isLongDouble(ct) {
			return val{op: e.tfCmp(li, ri, n.Op), ty: boolIr()}
		}
		t := e.newTmp()
		// A comparison whose operand is itself a comparison result -- "(x >= 0)
		// == (y > x)" or "(a < b) == 1" -- carries an i1, not a number.
		// Usual arithmetic conversions leave the integer side untouched and tag
		// the whole expression with the integer type, so the i1 operand reaches
		// here still typed i1 while the other operand is i32/i64. Emitting a
		// plain integer icmp then feeds that i1 to an instruction that expects
		// the integer width, which LLVM rejects ("defined with type 'i1' but
		// expected 'i32'"). The integer side is brought to i1 the same way a
		// controlling expression would ("x != 0") and the two are compared as
		// booleans. This only fires when the common type is integral: a float
		// common type already widens the i1 through sitofp/uitofp in convert(),
		// so it must keep the float path below.
		if !isFloatTy(ct) && (e.ty(l.ty) == "i1" || e.ty(r.ty) == "i1") {
			lhs, rhs := li, ri
			if e.ty(l.ty) != "i1" {
				c := e.newTmp()
				e.line("%s = icmp ne %s %s, 0", c, e.ty(ct), li)
				lhs = c
			}
			if e.ty(r.ty) != "i1" {
				c := e.newTmp()
				e.line("%s = icmp ne %s %s, 0", c, e.ty(ct), ri)
				rhs = c
			}
			cmp := map[string]string{
				"==": "eq", "!=": "ne", "<": "ult", ">": "ugt",
				"<=": "ule", ">=": "uge",
			}[n.Op]
			e.line("%s = icmp %s i1 %s, %s", t, cmp, lhs, rhs)
			return val{op: t, ty: boolIr()}
		}
		if isFloatTy(ct) {
			// "one" is the bug that hides here. C reads "a != b" as true
			// whenever the two are not both ordered-and-equal, so a NaN is
			// unequal to itself: fcmp one, which demands ordering, folds
			// x != x to false and makes isnan() -- which is spelled (x) != (x)
			// in math.h -- report false for every value in the program. The
			// asymmetry is deliberate and matches C: "==" is oeq, false for a
			// NaN, and "!=" is une, true for one.
			//
			// The self-compare case is intercepted in cond(), which sees the
			// whole "x != x" before eval splits it into two loads.
			cmp := map[string]string{
				"==": "oeq", "!=": "une", "<": "olt", ">": "ogt",
				"<=": "ole", ">=": "oge",
			}[n.Op]
			e.line("%s = fcmp %s %s %s, %s", t, cmp, e.ty(ct), li, ri)
		} else {
			cmp := map[string]string{
				"==": "eq", "!=": "ne", "<": "slt", ">": "sgt",
				"<=": "sle", ">=": "sge",
			}[n.Op]
			if ct != nil && !ct.Signed {
				cmp = map[string]string{
					"==": "eq", "!=": "ne", "<": "ult", ">": "ugt",
					"<=": "ule", ">=": "uge",
				}[n.Op]
			}
			e.line("%s = icmp %s %s %s, %s", t, cmp, e.ty(ct), li, ri)
		}
		// A comparison is i1 in IR. Tagging it frontend.KBool -- which goc's front end
		// already treats as a one-byte boolean -- is what stops a later
		// controlling expression from trying to compare it against zero.
		return val{op: t, ty: boolIr()}
	}
	return l
}

// toInt brings a value to an integer form for a shift. A genuine pointer is
// turned into an integer with ptrtoint; an integer value -- or an untyped
// constant, whose type the front end leaves nil -- is already in integer form
// and must pass through untouched. Emitting ptrtoint against an integer, as the
// old nil-treating branch did for constants, is rejected by LLVM ("ptrtoint ptr
// 63" -- 63 is not a pointer).
func (e *irEmitter) toInt(v val, t *frontend.Type) string {
	if v.ty != nil && v.ty.Kind == frontend.KPtr {
		c := e.newTmp()
		e.line("%s = ptrtoint ptr %s to i64", c, v.op)
		return c
	}
	return v.op
}

func (e *irEmitter) ptrAdd(p val, i val, pt *frontend.Type, sub bool) val {
	esz := frontend.Sizeof(pt.Elem)
	if esz == 0 {
		esz = 1
	}
	// C's pointer arithmetic runs in the width of `ptrdiff_t`, which is 64 bits
	// here. The subscript, however, is plain `int` -- 32 bits -- so it must be
	// widened to i64 before the scaling and the add, or LLVM rejects the mix of
	// an i32 subscript with an i64 base. A signed subscript is sign-extended, an
	// unsigned one zero-extended; a pointer subscript (rare) becomes an integer
	// with ptrtoint.
	idx := e.toI64(i)
	scaled := idx
	if esz != 1 {
		scaled = e.newTmp()
		e.line("%s = mul i64 %s, %d", scaled, idx, esz)
	}
	// The base is widened to i64 to match; the result is then turned back into a
	// pointer.
	base := e.newTmp()
	e.line("%s = ptrtoint ptr %s to i64", base, p.op)
	sum := e.newTmp()
	if sub {
		e.line("%s = sub i64 %s, %s", sum, base, scaled)
	} else {
		e.line("%s = add i64 %s, %s", sum, base, scaled)
	}
	t := e.newTmp()
	e.line("%s = inttoptr i64 %s to ptr", t, sum)
	return val{op: t, ty: pt}
}

// toI64 widens a value to i64 for pointer arithmetic: a signed integer is
// sign-extended, an unsigned one zero-extended, and a pointer becomes an
// integer through ptrtoint. Anything already i64 comes back unchanged.
func (e *irEmitter) toI64(v val) string {
	if v.ty != nil {
		switch v.ty.Kind {
		case frontend.KPtr:
			c := e.newTmp()
			e.line("%s = ptrtoint ptr %s to i64", c, v.op)
			return c
		case frontend.KInt:
			if v.ty.Width*8 == 64 {
				return v.op
			}
			c := e.newTmp()
			if v.ty.Signed {
				e.line("%s = sext %s %s to i64", c, e.ty(v.ty), v.op)
			} else {
				e.line("%s = zext %s %s to i64", c, e.ty(v.ty), v.op)
			}
			return c
		}
	}
	return v.op
}

// usualArith applies C's usual arithmetic conversions and returns the common
// type together with the two operands converted to it.
func (e *irEmitter) usualArith(l, r val, lty, rty *frontend.Type) (*frontend.Type, string, string) {
	ct := arithCommon(lty, rty)
	if ct == nil {
		return lty, l.op, r.op
	}
	return ct, e.convert(l.op, lty, ct), e.convert(r.op, rty, ct)
}

// arithCommon is C's usual arithmetic conversion: the higher rank wins, a
// float beats an integer of any width, and two same-width integers take the
// unsigned one.
//
// Before that rule runs, each operand passes through integer promotion: any
// integer type narrower than int (the C rank-below-int cases -- _Bool, char,
// short and their unsigned variants) is raised to int. Without it,
// "unsigned char" - "unsigned char" is evaluated in a one-byte register and the
// result then zero-extended, so 'a' - 'b' yields 255 instead of -1 and any
// signed difference over a narrow operand is lost. (Signed narrow types hid the
// same defect for years: an i8 subtraction that wraps and is then sign-extended
// happens to match the int result, but the unsigned one does not.) This is the
// bug that made qsort a no-op -- its comparator returns strcmp's value, which
// is an unsigned-char difference, so every "less than" test read positive.
func arithCommon(a, b *frontend.Type) *frontend.Type {
	a = promoteInt(a)
	b = promoteInt(b)
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// long double outranks both of the narrower floating types, whichever side
	// it came from -- it is binary128, so mixing it with a double has to widen
	// the double rather than narrow the quad.
	if a.Kind == frontend.KLongDouble || b.Kind == frontend.KLongDouble {
		return frontend.LongDoubleType()
	}
	if a.Kind == frontend.KDouble || b.Kind == frontend.KDouble {
		return frontend.DoubleType()
	}
	if a.Kind == frontend.KFloat || b.Kind == frontend.KFloat {
		return frontend.FloatType()
	}
	// A pointer operand makes the whole expression a pointer.
	if a.Kind == frontend.KPtr {
		return a
	}
	if b.Kind == frontend.KPtr {
		return b
	}
	if a.Width > b.Width {
		return a
	}
	if b.Width > a.Width {
		return b
	}
	if !a.Signed {
		return a
	}
	if !b.Signed {
		return b
	}
	return a
}

// promoteInt raises an integer type narrower than int to int, per C's integer
// promotions. Pointers, floats, bit-precise integers and nil are left alone:
// only the rank-below-int integer types (width under int's) get promoted, and
// the target int can represent every value of each, so the promotion is always
// to signed int.
//
// _Bool is one of those types and has to be listed: it is KBool, not KInt, so
// testing only KInt let it through unpromoted and "_Bool + _Bool" was evaluated
// in a one-bit register -- "t + t" for t == 1 answered 0, because i1 addition
// wraps, and "f - t" answered 1 rather than -1.
func promoteInt(t *frontend.Type) *frontend.Type {
	if t == nil {
		return nil
	}
	if (t.Kind == frontend.KInt || t.Kind == frontend.KBool) && t.Width < 4 {
		return frontend.IntType()
	}
	return t
}

// logical lowers && and ||, which short-circuit and therefore cannot be built
// from the eager binary path.
func (e *irEmitter) logical(n *frontend.Binary, lty, rty *frontend.Type) val {
	rhs := e.newLabel()
	shortBlk := e.newLabel()
	done := e.newLabel()
	// Evaluating the left operand may itself be a short-circuit expression,
	// which opens and closes blocks of its own, so the left value is known in
	// whatever block is current afterwards rather than in the one this
	// expression started in.
	lc := e.cond(n.L)
	// The result goes through memory rather than a phi. A phi is the obvious
	// spelling and it was what this used to do, but LLVM's constant
	// propagation through a phi gets a float comparison wrong: measured with
	// opt default<O1>, a function that tests its arguments with
	// "fcmp uno x, 0.0" and joins the two tests with a phi answers 0 for a NaN
	// argument, and the same function joining them with "or i1" answers 1 --
	// correct. Nothing in the IR marks it as unusual and no diagnostic fires;
	// fmaximum_num(NAN, 1) just returned 1.0 and isnan() was false for every
	// value a program could name.
	//
	// The slot is allocated in the ENTRY block, which is what lets both paths
	// store into it and the join load from it: an alloca in a block that
	// dominates only one path would not be addressable from the other. Short
	// circuiting is unchanged -- the right operand is still reached only on the
	// path that needs it.
	slot := e.entryAlloca("i1")
	// "&&" evaluates the right side only when the left was true, and yields
	// false on the short path; "||" evaluates it only when the left was false,
	// and yields the left's value there.
	short := "false"
	if n.Op == "||" {
		short = lc
		e.term("br i1 %s, label %%%s, label %%%s", lc, shortBlk, rhs)
	} else {
		e.term("br i1 %s, label %%%s, label %%%s", lc, rhs, shortBlk)
	}
	e.blockLabel(shortBlk)
	e.line("store i1 %s, ptr %s, align 1", short, slot)
	e.term("br label %%%s", done)
	e.blockLabel(rhs)
	rc := e.cond(n.R)
	e.line("store i1 %s, ptr %s, align 1", rc, slot)
	e.term("br label %%%s", done)
	e.blockLabel(done)
	res := e.newTmp()
	e.line("%s = load i1, ptr %s, align 1", res, slot)
	return val{op: res, ty: boolIr()}
}

// --- assignment -------------------------------------------------------------

func (e *irEmitter) assignStmt(n *frontend.AssignStmt) {
	p := e.lvalue(n.Lhs)
	v := e.eval(n.Rhs)
	e.store(p, e.coerce(v, e.tr.exprType(n.Lhs)))
}

func (e *irEmitter) assignExpr(n *frontend.AssignExpr) val {
	t := e.tr.exprType(n.Lhs)
	if n.Op == "" {
		p := e.lvalue(n.Lhs)
		v := e.coerce(e.eval(n.Rhs), t)
		e.store(p, v)
		return v
	}
	// A compound assignment evaluates its left operand once. Reading it here and
	// writing the result back is what keeps "a[i++] += 10" from advancing i
	// twice, which is why the parser stopped desugaring this.
	p := e.lvalue(n.Lhs)
	old := e.load(p, t)
	cur := e.coerce(old, t)
	// C performs the operation in the common type of the two operands and then
	// converts the result back to the left operand's type -- "short s; s += 1"
	// adds as int and truncates on the way back. Doing it in the left operand's
	// width instead left the right operand widened to the common type and
	// produced "add i32 %a, %b" with %b an i64, which LLVM rejects.
	//
	// Shifts are the exception: C promotes each operand on its own and the
	// result keeps the left operand's type, which is also what LLVM demands
	// (both operands of shl/lshr/ashr carry the same width).
	rt := e.tr.exprType(n.Rhs)
	shift := n.Op == "<<" || n.Op == ">>"
	ct := cur.ty
	if !shift && cur.ty != nil && cur.ty.Kind != frontend.KPtr &&
		rt != nil && rt.Kind != frontend.KPtr {
		ct = arithCommon(cur.ty, rt)
	}
	lc := e.coerce(cur, ct)
	rhs := e.coerce(e.eval(n.Rhs), ct)
	var res val
	switch n.Op {
	case "+":
		if ct != nil && ct.Kind == frontend.KPtr {
			res = e.ptrAdd(lc, rhs, ct, false)
		} else {
			res = e.arith(lc, rhs, "add", ct)
		}
	case "-":
		if ct != nil && ct.Kind == frontend.KPtr {
			res = e.ptrAdd(lc, rhs, ct, true)
		} else {
			res = e.arith(lc, rhs, "sub", ct)
		}
	case "*":
		res = e.arith(lc, rhs, "mul", ct)
	case "/":
		res = e.arith(lc, rhs, "div", ct)
	case "%":
		res = e.arith(lc, rhs, "rem", ct)
	case "&":
		res = e.arith(lc, rhs, "and", ct)
	case "|":
		res = e.arith(lc, rhs, "or", ct)
	case "^":
		res = e.arith(lc, rhs, "xor", ct)
	case "<<", ">>":
		op := "shl"
		if n.Op == ">>" {
			op = "lshr"
			if ct != nil && ct.Signed {
				op = "ashr"
			}
		}
		r := e.newTmp()
		e.line("%s = %s %s %s, %s", r, op, e.ty(ct), lc.op, rhs.op)
		res = val{op: r, ty: ct}
	default:
		res = lc
	}
	res = e.coerce(res, t)
	e.store(p, res)
	return res
}

// foperand adapts one operand of a floating-point instruction.
//
// LLVM will not silently promote: "fadd float %x, 1" is rejected with
// "integer/byte constant must have integer/byte type", and an integer register
// has to be converted explicitly. An integer constant is rewritten as the
// equivalent float constant, which keeps the common "x + 1" on a float down to
// a single instruction; an integer value gets a sitofp/uitofp.
func (e *irEmitter) foperand(v val, t *frontend.Type) val {
	if v.ty == nil || isFloatTy(v.ty) {
		return v
	}
	if _, err := strconv.ParseInt(v.op, 10, 64); err == nil {
		return val{op: v.op + ".0", ty: t}
	}
	conv := "sitofp"
	if !v.ty.Signed {
		conv = "uitofp"
	}
	r := e.newTmp()
	e.line("%s = %s %s %s to %s", r, conv, e.ty(v.ty), v.op, e.ty(t))
	return val{op: r, ty: t}
}

// llirBin maps a C binary operator to the name LLVM gives the instruction. They
// are not the same words -- C says "+", LLVM says "add" -- and passing the C
// spelling through produces a module LLVM rejects with "expected instruction
// opcode".
func llirBin(op string) string {
	switch op {
	case "+":
		return "add"
	case "-":
		return "sub"
	case "*":
		return "mul"
	case "/":
		return "div"
	case "%":
		return "rem"
	case "&":
		return "and"
	case "|":
		return "or"
	case "^":
		return "xor"
	case "<<":
		return "shl"
	case ">>":
		return "shr"
	}
	return op
}

func (e *irEmitter) arith(a, b val, op string, t *frontend.Type) val {
	// long double never reaches an LLVM arithmetic instruction: every operator
	// on it is a call into the software runtime.
	if isLongDouble(t) {
		return val{op: e.tfArith(a.op, b.op, op), ty: t}
	}
	r := e.newTmp()
	if isFloatTy(t) {
		a, b = e.foperand(a, t), e.foperand(b, t)
		switch op {
		case "div":
			e.line("%s = fdiv %s %s, %s", r, e.ty(t), a.op, b.op)
		case "rem":
			// C's % on floats is fmod, which LLVM spells frem.
			e.line("%s = frem %s %s, %s", r, e.ty(t), a.op, b.op)
		default:
			e.line("%s = f%s %s %s, %s", r, op, e.ty(t), a.op, b.op)
		}
		return val{op: r, ty: t}
	}
	if op == "div" || op == "rem" {
		kind := "sdiv"
		if t != nil && !t.Signed {
			kind = "udiv"
		}
		if op == "rem" {
			kind = "srem"
			if t != nil && !t.Signed {
				kind = "urem"
			}
		}
		e.line("%s = %s %s %s, %s", r, kind, e.ty(t), a.op, b.op)
		return val{op: r, ty: t}
	}
	e.line("%s = %s %s %s, %s", r, op, e.ty(t), a.op, b.op)
	return val{op: r, ty: t}
}

// floatConstLit spells a 64-bit pattern the way the float-literal path spells
// one: the SOURCE of the bitcast names the float type and carries the pattern,
// which is the spelling LLVMParseIRInContext accepts. (Naming an integer source
// -- "bitcast i64 0x7FF8... to double" -- is read as a float constant of the
// wrong type and rejected.)
func floatConstLit(bits uint64) string {
	return fmt.Sprintf("0x%016X", bits)
}

// litDivBits gives the bit pattern of `num / den` for two floating literals
// where den is zero, and reports false in every other case -- a non-literal
// operand, an integer division, or a divisor that is not zero. Those go through
// the fdiv the arithmetic path emits.
//
// The pattern is a string because a float and a double need different widths,
// and both callers want to drop it straight into a bitcast.
func litDivBits(num, den frontend.Expr, ct *frontend.Type) (uint64, bool) {
	if ct == nil || ct.Kind != frontend.KDouble {
		return 0, false
	}
	d, ok := litFloat(den)
	if !ok || d != 0 {
		return 0, false
	}
	n, ok := litFloat(num)
	if !ok {
		return 0, false
	}
	switch {
	case n == 0:
		// 0.0/0.0 is a quiet NaN. The sign is positive: IEEE 754 leaves it
		// unspecified and every compiler that is asked picks the positive one.
		return 0x7FF8000000000000, true
	case n < 0:
		return 0xFFF0000000000000, true
	default:
		return 0x7FF0000000000000, true
	}
}

// litFloat reads a floating NumLit. An int literal is accepted, because C's
// usual arithmetic conversions turn "1.0/0" into a floating division and the
// divisor's zero is what matters here, not how it was spelled.
//
// The float test is on Kind, not on IsFloat: the lexer sets IsFloat only for
// an f/F suffix ("1.0f" is type float), so a plain "1.0" is TDouble with
// IsFloat false and its value in Fval. Testing IsFloat reads every double
// literal as the integer zero -- which turned 1.0/0.0 into a quiet NaN.
func litFloat(x frontend.Expr) (float64, bool) {
	n, ok := x.(*frontend.NumLit)
	if !ok {
		return 0, false
	}
	if n.Kind == frontend.TDouble {
		return n.Fval, true
	}
	return float64(n.Val), true
}

// --- inc/dec ----------------------------------------------------------------

func (e *irEmitter) incDec(n *frontend.IncDecExpr) val {
	t := e.tr.exprType(n.E)
	p := e.lvalue(n.E)
	old := e.load(p, t)
	one := val{op: "1", ty: frontend.IntType()}
	step := "add"
	if n.Op == "--" {
		step = "sub"
	}
	var next val
	if t != nil && t.Kind == frontend.KPtr {
		next = e.ptrAdd(old, one, t, n.Op == "--")
	} else {
		ct := arithCommon(t, frontend.IntType())
		ctv := e.coerce(old, ct)
		next = e.coerce(e.arith(ctv, one, step, ct), t)
	}
	e.store(p, next)
	if n.Prefix {
		return next
	}
	return old
}

// --- casts and conditionals -------------------------------------------------

func (e *irEmitter) cast(n *frontend.CastExpr) val {
	v := e.eval(n.E)
	if n.Typ == nil {
		return v
	}
	// A cast to void -- "(void)expr", how a statement discards a value -- has
	// no value to produce. There is no void operand in IR, so converting one
	// would ask for a "load void" off the operand's address, which LLVM rejects
	// ("void type only allowed for function results"). The operand is still
	// evaluated, for its side effects.
	if n.Typ.Kind == frontend.KVoid {
		return val{op: "0", ty: frontend.IntType()}
	}
	return val{op: e.convert(v.op, v.ty, n.Typ), ty: n.Typ}
}

func (e *irEmitter) condExpr(n *frontend.CondExpr) val {
	c := e.cond(n.Cond)
	thenL := e.newLabel()
	elseL := e.newLabel()
	doneL := e.newLabel()
	// The arms' common type is known from the front end, so each arm can
	// convert its value before leaving its own block. It has to be done there:
	// a conversion emitted in the join block would define a value the phi's
	// incoming edge does not dominate ("instruction does not dominate all
	// uses"), and leaving the arms unconverted put a plain 0 into a "phi ptr".
	ct := arithCommon(e.tr.exprType(n.Then), e.tr.exprType(n.Else))
	if ct == nil {
		ct = e.tr.exprType(n.Then)
	}
	// An array arm is already a pointer to its first element, so the phi names
	// a pointer type. Naming the array type instead asked for a
	// "phi [1 x i8]" over pointer operands, which has no valid form.
	if ct != nil && ct.Kind == frontend.KArr {
		ct = frontend.PtrType(ct.Elem)
	}
	e.term("br i1 %s, label %%%s, label %%%s", c, thenL, elseL)

	// "a ? f() : g()" with two void arms is a statement, not a value: C allows
	// it, and Nim's generated C uses exactly that shape to call a closure
	// through one of two spellings. There is nothing to join -- naming void
	// would emit "phi void", which LLVM rejects -- so the arms are run for
	// their side effects and the result is a dummy.
	if ct != nil && ct.Kind == frontend.KVoid {
		e.blockLabel(thenL)
		e.discard(n.Then)
		e.termOpen("br label %%%s", doneL)
		e.blockLabel(elseL)
		e.discard(n.Else)
		e.termOpen("br label %%%s", doneL)
		e.blockLabel(doneL)
		return val{op: "0", ty: frontend.IntType()}
	}

	e.blockLabel(thenL)
	tv := e.coerce(e.eval(n.Then), ct)
	// The arm's value may have been produced in a block of its own -- a nested
	// ternary or a short-circuit expression opens and closes blocks -- so the
	// predecessor the phi names is whichever block is current now, not the one
	// just opened.
	tb := e.currentBlock(thenL)
	e.term("br label %%%s", doneL)

	e.blockLabel(elseL)
	ev := e.coerce(e.eval(n.Else), ct)
	eb := e.currentBlock(elseL)
	e.term("br label %%%s", doneL)

	e.blockLabel(doneL)
	r := e.newTmp()
	e.line("%s = phi %s [ %s, %%%s ], [ %s, %%%s ]", r, e.ty(ct), tv.op, tb, ev.op, eb)
	return val{op: r, ty: ct}
}

// currentBlock is the block instructions are being emitted into, falling back
// to the one just opened when the emitter has not opened any (the first block
// of a function is "entry" and carries no label of its own).
func (e *irEmitter) currentBlock(fallback string) string {
	if e.lastLabel != "" {
		return e.lastLabel
	}
	if fallback == "" {
		return "entry"
	}
	return fallback
}

// compoundLit materialises a C99 compound literal and yields its address.
func (e *irEmitter) compoundLit(n *frontend.CompoundLit) val {
	ty := n.Typ
	tyStr := e.ty(ty)
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca " + tyStr + ", align " +
		itoa(alignOfIr(tyStr)) + "\n")
	if n.Init != nil {
		e.storeInit(n.Init, ty, slot, 0)
	}
	return val{op: slot, ty: ty}
}
