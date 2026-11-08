package gocl

// Compiler intrinsics the native back end lowers itself and LLVM spells
// differently: the marker builtins, GCC's overflow-checked arithmetic and the
// atomic family.
//
// All three are calls in the C source but none of them is a call in the output.
// Emitting them as calls to external symbols produces an object that links
// against names nothing defines, which is how gocl met Nim's generated C: the
// IR assembled, and the link then failed on __builtin_unreachable and friends.
//
// LLVM has an instruction or an intrinsic for each one, so the lowering is a
// direct translation rather than a reimplementation.

import (
	"fmt"

	"goc/frontend"
)

// overflowOpWord and atomicOpWord give the operation the name LLVM spells it
// with. C says "+", "-" and "*"; the with-overflow intrinsics and atomicrmw
// want words.
func overflowOpWord(op string) string {
	switch op {
	case "+":
		return "add"
	case "-":
		return "sub"
	case "*":
		return "mul"
	}
	return "add"
}

func atomicOpWord(op string) string {
	switch op {
	case "+":
		return "add"
	case "-":
		return "sub"
	}
	return op // "&", "|" and "^" are the same word in both languages
}

// markerCall lowers the builtins that carry optimiser information and no
// runtime work (see src/frontend/marker.go). It reports false when the name is
// not one of them.
func (e *irEmitter) markerCall(n *frontend.Call) (val, bool) {
	if _, ok := frontend.LookupMarkerBuiltin(n.Name); !ok {
		return val{}, false
	}
	switch n.Name {
	case "__builtin_trap":
		// The one marker with a runtime effect: the path is meant to be fatal.
		e.c.noteIntrinsic("llvm.trap", "void", nil)
		e.line("call void @llvm.trap()")
		return val{op: "0", ty: frontend.IntType()}, true
	case "__builtin_unreachable":
		// Terminates the block: nothing follows a path the program cannot take.
		// A switch arm that ends here therefore needs no branch to the join.
		e.term("unreachable")
		return val{op: "0", ty: frontend.IntType()}, true
	}
	// __builtin_assume and the branch hints: the value is either nothing or the
	// first argument, which still has to be evaluated for its side effects.
	if len(n.Args) == 0 {
		return val{op: "0", ty: frontend.IntType()}, true
	}
	return e.eval(n.Args[0]), true
}

// overflowCall lowers GCC's overflow-checked arithmetic (see
// src/frontend/overflow.go) to LLVM's with-overflow intrinsics, which return
// the wrapped result and the overflow flag as a pair -- exactly the two answers
// the C builtin produces.
func (e *irEmitter) overflowCall(n *frontend.Call) (val, bool) {
	ob, ok := frontend.LookupOverflowBuiltin(n.Name)
	if !ok || len(n.Args) != 3 {
		return val{}, false
	}
	// The width is part of the name (sadd is 32-bit, saddll is 64-bit); the
	// generic __builtin_add_overflow reads it off the result pointer.
	w := ob.Width
	signed := ob.Signed
	if w == 0 {
		pt := e.tr.exprType(n.Args[2])
		if pt == nil || pt.Elem == nil {
			return val{}, false
		}
		w = frontend.Sizeof(pt.Elem)
		signed = pt.Elem.Signed
	}
	if w != 1 && w != 2 && w != 4 && w != 8 {
		return val{}, false
	}
	// LLVM names the intrinsic after the operation, the signedness and the
	// width, and every operand has to be exactly that wide.
	ity := fmt.Sprintf("i%d", 8*w)
	sgn := "u"
	if signed {
		sgn = "s"
	}
	name := "llvm." + sgn + overflowOpWord(ob.Op) + ".with.overflow." + ity
	cty := &frontend.Type{Kind: frontend.KInt, Width: w, Signed: signed}
	e.c.noteIntrinsic(name, "{"+ity+", i1}", []string{ity, ity})

	a := e.coerce(e.eval(n.Args[0]), cty)
	b := e.coerce(e.eval(n.Args[1]), cty)
	p := e.rvalue(n.Args[2])
	pair := e.newTmp()
	e.line("%s = call {%s, i1} @%s(%s %s, %s %s)", pair, ity, name, ity, a.op, ity, b.op)
	rv := e.newTmp()
	e.line("%s = extractvalue {%s, i1} %s, 0", rv, ity, pair)
	ov := e.newTmp()
	e.line("%s = extractvalue {%s, i1} %s, 1", ov, ity, pair)
	e.store(p, val{op: rv, ty: cty})
	// The C builtin reports its flag as an int.
	flag := e.newTmp()
	e.line("%s = zext i1 %s to i32", flag, ov)
	return val{op: flag, ty: frontend.IntType()}, true
}

// atomicCall lowers the <stdatomic.h> family and GCC's __atomic_* spellings
// (see src/frontend/atomic.go) to LLVM's atomic instructions. It reports false
// when the name is not one of them.
func (e *irEmitter) atomicCall(n *frontend.Call) (val, bool) {
	ab, ok := frontend.LookupAtomicBuiltin(n.Name)
	if !ok {
		return val{}, false
	}
	// The GCC spelling appends memory-order arguments the lowering does not
	// use: every access below is emitted with a real atomic instruction, which
	// is at least as strong as the order asked for.
	want := 2
	if ab.CAS {
		want = 3
	}
	if ab.LoadOnly {
		want = 1
	}
	if len(n.Args) < want {
		return val{}, false
	}
	args := n.Args[:want]
	p := e.rvalue(args[0])
	pt := e.tr.exprType(args[0])
	if pt == nil || pt.Elem == nil {
		return val{}, false
	}
	// The locked access is as wide as the object, never as wide as the operand.
	et := *pt.Elem
	et.Atomic = false
	ty := e.ty(&et)

	if ab.LoadOnly {
		v := e.newTmp()
		e.line("%s = load %s, ptr %s, align %d", v, ty, p, alignOfIr(ty))
		return val{op: v, ty: &et}, true
	}
	if ab.CAS {
		// (object, expected, desired) -> bool. cmpxchg answers both at once:
		// the value the object held and whether it matched.
		if len(args) < 3 {
			return val{}, false
		}
		ep := e.rvalue(args[1])
		cmp := e.newTmp()
		e.line("%s = load %s, ptr %s, align %d", cmp, ty, ep, alignOfIr(ty))
		des := e.rvalue(args[2])
		pair := e.newTmp()
		e.line("%s = cmpxchg ptr %s, %s %s, %s %s monotonic monotonic",
			pair, p, ty, cmp, ty, des)
		old := e.newTmp()
		e.line("%s = extractvalue {%s, i1} %s, 0", old, ty, pair)
		okv := e.newTmp()
		e.line("%s = extractvalue {%s, i1} %s, 1", okv, ty, pair)
		// C writes the value observed back through `expected` on failure. On
		// success the observed value is the one already there, so storing it
		// unconditionally costs a store and changes nothing.
		e.store(ep, val{op: old, ty: &et})
		return val{op: okv, ty: boolIr()}, true
	}
	// The fetch family and exchange: one locked read-modify-write.
	opnd := e.coerce(e.eval(args[1]), &et)
	if ab.Op == "" {
		// atomic_exchange: xchg hands back the previous contents.
		r := e.newTmp()
		e.line("%s = atomicrmw xchg ptr %s, %s %s monotonic", r, p, ty, opnd.op)
		return val{op: r, ty: &et}, true
	}
	r := e.newTmp()
	e.line("%s = atomicrmw %s ptr %s, %s %s monotonic", r, atomicOpWord(ab.Op), p, ty, opnd.op)
	if ab.NoResult {
		// __atomic_store_n returns void; the value it writes is the operand.
		return val{op: "0", ty: frontend.IntType()}, true
	}
	return val{op: r, ty: &et}, true
}
