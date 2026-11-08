package gocl

// Calls, subscripting, member access and address-of.

import (
	"goc/common"
	"goc/frontend"
	"strings"
)

// lvalue renders an expression that must be an address, and returns it. This is
// the "genLValue" of the assembly path: everything that can appear on the left
// of an assignment or under & goes through here.
func (e *irEmitter) lvalue(x frontend.Expr) string {
	switch n := x.(type) {
	case *frontend.Ident:
		ty := e.tr.exprType(n)
		if vi, ok := e.tr.lookupVar(n.Name); ok {
			// A static local's object is the module global, not an
			// alloca: the symbol itself is its address.
			if vi.glob != "" {
				return vi.glob
			}
			return e.slotFor(vi.op, e.ty(ty))
		}
		// A thread-local variable lives in the linker-owned .tls section; its
		// address is the per-thread slot the __goc_tls_slot helper returns.
		if off, ok := e.c.tlsOffset(n.Name); ok {
			return e.tlsAddr(off)
		}
		if sym := e.c.globalSym(n.Name); sym != "" {
			return "@" + sym
		}
		// A name with no storage is an enum constant; it is not addressable,
		// but the caller only gets here for something the checker accepted.
		return "null"
	case *frontend.Unary:
		if n.Op == "*" {
			// Dereferencing: the operand already evaluated to the address.
			return e.rvalue(n.E)
		}
	case *frontend.Index:
		base, idx := e.indexOperands(n)
		// A[i] is *(a + i); the element address is what is wanted.
		ety := e.tr.exprType(n)
		esz := frontend.Sizeof(ety)
		if esz == 0 {
			esz = 1
		}
		scaled := idx
		if esz != 1 {
			scaled = e.newTmp()
			e.line("%s = mul i64 %s, %d", scaled, idx, esz)
		}
		bp := e.newTmp()
		e.line("%s = ptrtoint ptr %s to i64", bp, base)
		sum := e.newTmp()
		e.line("%s = add i64 %s, %s", sum, bp, scaled)
		r := e.newTmp()
		e.line("%s = inttoptr i64 %s to ptr", r, sum)
		return r
	case *frontend.MemberExpr:
		return e.memberAddr(n)
	case *frontend.CastExpr:
		return e.lvalue(n.E)
	case *frontend.CompoundLit:
		return e.compoundLit(n).op
	}
	// Anything else is not an lvalue; evaluating it is the best that can be done
	// and the checker will already have complained.
	return e.rvalue(x)
}

// isAddressable reports whether x names an object with an address, mirroring the
// node kinds lvalue() turns into a pointer. Everything else -- a call, a binary
// result, a conditional -- is a value produced without storage.
func isAddressable(x frontend.Expr) bool {
	switch x.(type) {
	case *frontend.Ident, *frontend.Index, *frontend.MemberExpr, *frontend.CastExpr, *frontend.CompoundLit:
		return true
	case *frontend.Unary:
		return x.(*frontend.Unary).Op == "*"
	}
	return false
}

// addressOf yields the address of an lvalue as a value, which is what an array
// decays to.
func (e *irEmitter) addressOf(x frontend.Expr, t *frontend.Type) val {
	p := e.lvalue(x)
	return val{op: p, ty: frontend.PtrType(t.Elem)}
}

// indexOperands evaluates the base and the subscript of an frontend.Index, applying C's
// array-to-pointer decay. It returns the base as an address and the index as an
// i64.
func (e *irEmitter) indexOperands(n *frontend.Index) (string, string) {
	bty := e.tr.exprType(n.Base)
	// A real array decays: the base is its address, not a load of it.
	if bty != nil && bty.Kind == frontend.KArr {
		return e.lvalue(n.Base), e.indexValue(n.Idx)
	}
	return e.rvalue(n.Base), e.indexValue(n.Idx)
}

func (e *irEmitter) indexValue(x frontend.Expr) string {
	v := e.eval(x)
	return e.convert(v.op, v.ty, &frontend.Type{Kind: frontend.KInt, Width: 8})
}

// --- calls ------------------------------------------------------------------

// callExpr lowers a direct call. Arguments are converted to the parameter types
// the front end resolved, because IR will not convert them.
func (e *irEmitter) callExpr(n *frontend.Call) val {
	// The same constant-format specialisation the native generator applies in
	// genCallExpr, from the one shared decision (see printfspec.go). Without
	// it a hello-world drags in the whole float exponent machine: printf
	// resolves to vfmt, vfmt's switch mentions every conversion the C library
	// defines, and the call-graph prune pulls vfmt's callees in with it.
	//
	// LLVM cannot do this for us. SimplifyLibCalls recognises calls to *known
	// libc symbols*, and this module has none: goclib's printf is emitted here
	// as a plain `define i32 @printf(ptr, ...)`, so from LLVM's point of view
	// it is a local function that happens to have a standard prototype.
	if repl := common.SpecializePrintfCall(n, e.printfQueries()); repl != nil {
		return e.callExpr(repl)
	}
	// Compiler intrinsics that exist only to carry information to the back end.
	// They are calls in the C source but instructions (or LLVM intrinsics) in
	// the output, so they have to be recognised before the ordinary call path
	// turns them into references to symbols no library defines. See builtin.go.
	if v, ok := e.markerCall(n); ok {
		return v
	}
	if v, ok := e.overflowCall(n); ok {
		return v
	}
	if v, ok := e.atomicCall(n); ok {
		return v
	}
	// va_start, va_end and va_copy are compiler built-ins in goc, recognised by
	// name. All three take the cursor's address: the intrinsics write through it.
	switch n.Name {
	case "va_start":
		if len(n.Args) == 0 {
			return val{op: "0", ty: frontend.IntType()}
		}
		// The operand is always a local `va_list ap;`. What llvm.va_start
		// stores into it on Windows x64 is a single pointer -- the cursor into
		// the caller's register save area -- so vaListSlot gives the local its
		// own widened slot and the intrinsic writes through it. Sharing the
		// ordinary pointer slot made va_start scribble over the locals that
		// followed it.
		ap := e.vaListSlot(n.Args[0])
		e.c.noteIntrinsic("llvm.va_start", "void", []string{"ptr"})
		e.line("call void @llvm.va_start(ptr %s)", ap)
		return val{op: "0", ty: frontend.IntType()}
	case "va_end":
		if len(n.Args) == 0 {
			return val{op: "0", ty: frontend.IntType()}
		}
		ap := e.vaListSlot(n.Args[0])
		e.c.noteIntrinsic("llvm.va_end", "void", []string{"ptr"})
		e.line("call void @llvm.va_end(ptr %s)", ap)
		return val{op: "0", ty: frontend.IntType()}
	case "va_copy":
		// C99 7.16.1.1: va_copy makes an independent copy of a va_list, so the
		// copy can be walked to the end without consuming the original. This is
		// what lets a formatter measure first and then write.
		//
		// The copy size is ABI-defined, which is why this is the one variadic
		// operation goclib/stdarg.h declines to define as a macro on Linux: on
		// x86-64 SysV a va_list is a 24-byte __va_list_tag carrying four
		// cursors, and copying only the first 8 bytes leaves the copy sharing
		// the original's register save area (and with a null overflow area, so
		// the first stack argument dereferences null). llvm.va_copy is
		// target-aware -- it copies the whole object on SysV and the single
		// pointer on Windows x64 -- so the same intrinsic serves both without a
		// compile-time branch here.
		if len(n.Args) < 2 {
			return val{op: "0", ty: frontend.IntType()}
		}
		dst := e.vaListSlot(n.Args[0])
		src := e.vaListSlot(n.Args[1])
		e.c.noteIntrinsic("llvm.va_copy", "void", []string{"ptr", "ptr"})
		e.line("call void @llvm.va_copy(ptr %s, ptr %s)", dst, src)
		return val{op: "0", ty: frontend.IntType()}
	}
	// A call through a function-pointer VARIABLE arrives here as a frontend.Call naming
	// the variable, not the function it points at. Emitting "call i32 @fn" for
	// a local such as "void (*fn)(void)" produces a reference to a symbol that
	// does not exist, and the link ends with "undefined symbol: fn". When the
	// name resolves to a pointer to function rather than to a function, lower
	// it as the indirect call it is.
	if _, _, ok := e.tr.fnPtrVar(n.Name); ok {
		return e.indirectCall(&frontend.IndirectCall{Fn: &frontend.Ident{Name: n.Name}, Args: n.Args})
	}
	// Resolve through the front end so a prototype supplies the parameter
	// types; without one the arguments keep their own types.
	paramTys, ret := e.calleeSig(n.Name, len(n.Args))
	e.c.noteExtern(n.Name, ret, paramTys)

	// Every argument carries its type explicitly. LLVM will infer them from a
	// declaration when there is one, but a function defined later in the same
	// module -- or one whose only definition this module does not have -- leaves
	// nothing to infer from, and the call is rejected with "invalid type for
	// function argument". Spelling the types out is always accepted.
	args := make([]string, 0, len(paramTys)+len(n.Args))
	// Long double variadic arguments draw their pass-by-address slots from a
	// shared per-function pool; a call consumes the pool from its start, so
	// reset the cursor here (see ldVarargSlot).
	e.ldVarargUsed = 0
	for i, a := range n.Args {
		// An argument that is a known va_list does not go through the ordinary
		// `char *` read, because on x86-64 SysV a va_list is a 24-byte
		// __va_list_tag and its first eight bytes are two packed cursor
		// integers, not a pointer. The callee wants the ADDRESS of that tag
		// (a SysV va_list parameter is an array type, so it decays to a
		// pointer), and advancing it must write back into the caller's tag --
		// which is what the C semantics require and what lets a callee consume
		// the list. On Windows x64 the value already is the cursor pointer, so
		// the ordinary read is exactly right and the slot's own address is not
		// wanted.
		//
		// The same is true of every flat-cursor Linux target -- ARM, AArch64
		// and RISC-V -- whose va_list is a bare `char *` walking a save area.
		// Passing the slot's address there hands the callee a pointer to the
		// cursor rather than the cursor, which is one level of indirection too
		// many: the callee's va_arg then loads the cursor's *value* and treats
		// it as the argument. printf with any conversion in it read a garbage
		// pointer, which is why the test programs printed "(null)" and a
		// nonsense integer on every one of those targets.
		if e.c.linux && !e.vaListIsFlatCursor() {
			if id, ok := a.(*frontend.Ident); ok && e.vaNames[id.Name] {
				args = append(args, "ptr "+e.vaListSlot(a))
				continue
			}
		}
		v := e.eval(a)
		if i < len(paramTys) {
			v = e.coerce(v, paramTys[i])
		} else {
			// A variadic argument keeps its own promoted type; a small
			// integer is widened to int, as C requires.
			v = e.defaultPromote(v)
			// ...and then widened again to fill its whole eight-byte slot.
			// A vararg occupies one eight-byte slot per argument (C 7.16.1.1,
			// and the SysV/Win64 register save areas are arrays of eight-byte
			// slots), while the callee reads the slot as a full register. For a
			// 32-bit int LLVM fills only the low half of the slot and leaves the
			// high half undefined, so a callee that reads it as a pointer --
			// which is exactly how goclib's own `printf_lite_with(int (*fmtfn)
			// (char *, long, const char *, va_list), const char *, va_list)` sees
			// its second and third parameters, the front end having typed
			// va_list as `char *` -- read whatever the register happened to hold
			// above the value.
			//
			// The symptom was every signed 32-bit variadic argument arriving as
			// its unsigned counterpart: `printf("%d", -8)` printed 4294967288,
			// because `sub i32 0, 8` only wrote edx and the stale upper half of
			// rdx was read back as part of the value. Clang, compiling this same
			// IR, widens with `xor %edx,%edx; sub $0x8,%edx` and is right.
			// Extending here is what makes the two agree -- and extending
			// rather than leaving it to the callee is the only place that can:
			// the callee is reached through a pointer whose signature already
			// says `ptr`, so the information about the argument's real width is
			// gone by then.
			v = e.widenVarargSlot(v)
		}
		// A long double VARIADIC argument is passed by address: the value is
		// stored into its own stack slot and ONE pointer rides the argument
		// list, exactly how goc marshals it (genCall's aggregate branch) and
		// how va_arg reads it back (expression.go's vaArg intercept). Passing
		// the i128 value instead would let LLVM split it across two argument
		// slots and desynchronise the cursor the callee walks.
		//
		// The check MUST be "beyond the prototype": a prototyped `long
		// double` parameter is still read by the callee as the i128 value
		// (function.go's parameter lowering, unchanged), so passing that
		// position by address hands the callee a pointer where it expects
		// the value itself -- every goc_tf_* call in every program crashed
		// this way before the position guard went in (found by win_regress:
		// longdouble [gocl] segfaulted with no output).
		if i >= len(paramTys) && v.ty != nil && v.ty.Kind == frontend.KLongDouble {
			slot := e.ldVarargSlot()
			e.line("store i128 %s, ptr %s, align 16", v.op, slot)
			args = append(args, "ptr "+slot)
			continue
		}
		args = append(args, e.ty(v.ty)+" "+v.op)
	}
	// A call may pass fewer arguments than the callee's prototype declares, and
	// the entry stub is exactly that case: `_start` calls `main()` with no
	// operands while the program defines `int main(int argc, char **argv)`.
	// LLVM rejects a call whose operand count does not match a defined function
	// ("call to function must have the correct number of arguments"), so the
	// missing parameters are filled with zero here.
	//
	// Zero is the right filler and not a guess: the kernel's argc/argv are not
	// threaded down to the C entry point on this path (a known limitation, the
	// same one the native generator's `call main` has), so a program that reads
	// them was never going to see real values. What matters is that the module
	// verifies and the program runs.
	for i := len(n.Args); i < len(paramTys); i++ {
		pt := paramTys[i]
		// An aggregate parameter has no single zero-valued operand to write --
		// a struct or an array needs an alloca plus a memcpy. Nothing in the
		// entry path passes one, so stop rather than invent a lowering.
		if pt == nil || pt.Kind == frontend.KStruct ||
			pt.Kind == frontend.KUnion || pt.Kind == frontend.KArr {
			break
		}
		args = append(args, e.ty(pt)+" "+e.c.zeroOf(pt))
	}
	argText := ""
	if len(args) > 0 {
		argText = strings.Join(args, ", ")
	}
	rty := e.c.externRet(n.Name)
	rs := e.ty(rty)
	// irFuncSym keeps the call spelling in step with the definition's; see
	// llvmMisrecognizedMaxMin for why the two cannot both use the C name.
	sym := irFuncSym(n.Name)
	call := e.newTmp()
	if rs == "void" {
		e.line("call void @%s(%s)", sym, argText)
		return val{op: "", ty: frontend.VoidType()}
	}
	e.line("%s = call %s @%s(%s)", call, rs, sym, argText)
	return val{op: call, ty: rty}
}

// common.PrintfQueries adapts the IR path's resolver to the questions
// specializePrintfCall asks.
//
// userDefs, not funcDefs: the latter also holds the C runtime's own fwrite and
// printf_lite, so testing against it would report every library function as
// shadowed and the rewrite would never fire.
func (e *irEmitter) printfQueries() common.PrintfQueries {
	return common.PrintfQueries{
		UserDefines: func(name string) bool { return e.tr.userDefs[name] },
		ShadowedByVar: func(name string) bool {
			_, _, ok := e.tr.fnPtrVar(name)
			return ok
		},
	}
}

// calleeSig finds a callee's declared signature.
func (e *irEmitter) calleeSig(name string, nargs int) ([]*frontend.Type, *frontend.Type) {
	if p, ok := e.tr.calleeParams(name); ok {
		ret := e.tr.calleeRet(name)
		return p, ret
	}
	// No declaration in sight: treat every argument as an int, which is the
	// common case and keeps the module valid for the linker to check.
	ps := make([]*frontend.Type, nargs)
	for i := range ps {
		ps[i] = frontend.IntType()
	}
	return ps, frontend.IntType()
}

// defaultPromote applies the default argument promotions: a char or short
// becomes an int, and a float becomes a double.
//
// A _Bool is promoted too (C 6.3.1.1: the integer promotions convert it to
// int). Leaving it as i1 handed the variadic slot a one-byte value, which
// only worked by the luck of whatever LLVM's codegen left in the rest of the
// register; the promotion is what makes the slot carry a defined 0 or 1.
func (e *irEmitter) defaultPromote(v val) val {
	if v.ty == nil {
		return v
	}
	switch v.ty.Kind {
	case frontend.KFloat:
		return val{op: e.convert(v.op, v.ty, frontend.DoubleType()), ty: frontend.DoubleType()}
	case frontend.KInt, frontend.KBool:
		if v.ty.Width < 4 {
			return val{op: e.convert(v.op, v.ty, frontend.IntType()), ty: frontend.IntType()}
		}
	}
	return v
}

// widenVarargSlot extends a variadic argument to the full eight bytes of its
// slot.
//
// Only a narrow integer needs it. A double is already 64 bits and a pointer is
// already 64 bits, and an i64 integer occupies the whole slot on its own -- so
// in every case but the 1/2/4-byte integers the value is already the width the
// callee will read. Those are exactly the ones LLVM stores in the low half of
// the register and leaves the high half undefined.
//
// The extension follows the type's own signedness, not its width: `sext` for a
// signed int reproduces C's integer promotion, and `zext` for unsigned (and for
// _Bool, whose values are 0 and 1 and must not become 0xFFFFFFFF and 0) keeps
// the value non-negative. Getting this backwards is the bug being fixed, only
// with the sign bit set in the other half.
//
// The eight-byte slot is the x86-64 / Win64 shape, where the register save area
// and the overflow area are both arrays of eight-byte slots. 32-bit ARM's AAPCS
// does not work that way: a variadic argument is promoted to int or double and
// then pushed as a *word*, and the callee's va_arg steps the cursor by 4 for an
// int. Widening to i64 here therefore does not merely waste space -- it moves
// every following argument by an extra 4 bytes, so `addall(4, 10, 20, 30, 40)`
// read 10, 20, 30 and then whatever sits past the end (the sum came out 70,
// having read 10, 20, 30, 10). So the widening is skipped there and the
// promoted 32-bit value is passed as-is.
//
// AArch64's AAPCS64 rounds the slot *up* to eight bytes, so it keeps the
// widened form even though its va_list is the same flat cursor as 32-bit ARM's.
// The two differ in slot width, not in cursor shape, so the test is on the
// architecture rather than on the cursor kind.
func (e *irEmitter) widenVarargSlot(v val) val {
	switch e.c.arch {
	case "arm", "armel", "riscv32":
		// AAPCS and the RISC-V 32-bit ABI: the slot is a word, and vaArgFlat
		// steps by 4 for an int. Widening to i64 here pushes each following
		// argument 4 bytes further along, so addall(4, 10, 20, 30, 40) reads
		// 10, 20, 30 and then whatever follows -- 70 instead of 100.
		//
		// (Measured: making these targets eight-byte slots too, caller and
		// callee together, answers 40. The four-byte step is not a
		// simplification here, it is what the caller's layout actually is.)
		return v
	}
	if v.ty == nil || v.ty.Kind != frontend.KInt {
		// A pointer, a double, or a type the emitter did not classify: already
		// a full slot.
		return v
	}
	lty := e.ty(v.ty)
	switch lty {
	case "i8", "i16", "i32":
	default:
		// i64 and anything wider already fills the slot.
		return v
	}
	out := e.newTmp()
	opc := "zext"
	if v.ty.Signed {
		opc = "sext"
	}
	e.line("%s = %s %s %s to i64", out, opc, lty, v.op)
	// The widened value is a long as far as the IR is concerned: eight bytes
	// carrying the argument, which is what the callee's eight-byte read wants.
	// The original type stays for the caller's own arithmetic on the result.
	return val{op: out, ty: &frontend.Type{Kind: frontend.KInt, Width: 8, Signed: true}}
}

// indirectCall lowers a call through a computed callee.
//
// The callee's signature is NOT necessarily unknown: a call through a
// function-pointer variable, a function-pointer parameter or any expression
// whose static type is `pointer to function` all carry one, and the front end
// has already resolved it. Using it is not an optimisation -- it is what makes
// the call correct.
//
// Without it every argument keeps its own type, and a literal `0` passed where
// the prototype says `char *` or `long` is emitted as `i32 0`. LLVM accepts
// that (an indirect call is not type-checked against anything at IR level),
// and on x86-64 it even works by accident: every argument goes in a 64-bit
// register and a 32-bit zero leaves the upper half zero. On 32-bit targets it
// does not. RISC-V passes an i64 in an even-aligned register pair, so a callee
// reading `long limit` from a slot the caller filled with a single i32 picks up
// the *next* argument as its high half: printf_lite_with's measuring pass
// `fmtfn(0, 0, fmt, ap)` handed vfmt_i a limit of 0x9e3000000000, the guard
// `n < limit` became vacuously true, and the "measure only" pass stored into
// the null buffer -- a fault at the first byte on every 32-bit target, and
// scrambled output on the 64-bit ones.
func (e *irEmitter) indirectCall(n *frontend.IndirectCall) val {
	if n.UFCS != nil {
		// A method call resolved by the checker: the receiver is prepended and
		// the function is called directly.
		return e.callExpr(n.UFCS)
	}
	fn := e.rvalue(n.Fn)
	paramTys, ret := e.indirectCalleeSig(n.Fn)
	var args []string
	// Same shared pool as callExpr's variadic marshalling (see ldVarargSlot).
	e.ldVarargUsed = 0
	for i, a := range n.Args {
		// A va_list argument goes over as the address of the caller's tag on
		// SysV, for the same reason as in callExpr: the callee's parameter is a
		// decayed array, and the ordinary `char *` read would hand it the tag's
		// first eight bytes -- two packed cursors -- instead of an address.
		// Flat-cursor targets pass the cursor by value instead; see callExpr.
		if e.c.linux && !e.vaListIsFlatCursor() {
			if id, ok := a.(*frontend.Ident); ok && e.vaNames[id.Name] {
				args = append(args, "ptr "+e.vaListSlot(a))
				continue
			}
		}
		v := e.eval(a)
		if i < len(paramTys) && paramTys[i] != nil && !isAggregateTy(paramTys[i]) {
			// A declared parameter type is authoritative: the argument is
			// converted to it, exactly as in a direct call.
			v = e.coerce(v, paramTys[i])
		} else {
			v = e.defaultPromote(v)
			// Same eight-byte slot rule as a direct variadic call (see
			// callExpr): a narrow integer would otherwise leave the upper half
			// of the slot undefined for the callee to read. Every argument of
			// an indirect call is variadic in the sense that matters here --
			// beyond the declared parameters the callee's signature says
			// nothing, so nothing else will widen them.
			v = e.widenVarargSlot(v)
		}
		// A long double VARIADIC argument rides one pointer slot, exactly as
		// in callExpr above (the two paths must agree, or an indirect printf
		// and a direct one would disagree about where argument two lives).
		// Beyond-prototype only, for the same callee-shape reason as there.
		if i >= len(paramTys) && v.ty != nil && v.ty.Kind == frontend.KLongDouble {
			slot := e.ldVarargSlot()
			e.line("store i128 %s, ptr %s, align 16", v.op, slot)
			args = append(args, "ptr "+slot)
			continue
		}
		args = append(args, e.ty(v.ty)+" "+v.op)
	}
	// A call through a pointer with a known arity that passes fewer arguments
	// than the prototype declares gets the same zero fill as a direct call:
	// LLVM rejects the arity mismatch outright otherwise.
	for i := len(n.Args); i < len(paramTys); i++ {
		pt := paramTys[i]
		if pt == nil || isAggregateTy(pt) {
			break
		}
		args = append(args, e.ty(pt)+" "+e.c.zeroOf(pt))
	}
	argText := ""
	if len(args) > 0 {
		argText = strings.Join(args, ", ")
	}
	rs := "i32"
	if ret != nil {
		if t := e.ty(ret); t != "" {
			rs = t
		}
	}
	t := e.newTmp()
	if rs == "void" {
		e.line("call void %s(%s)", fn, argText)
		return val{op: "", ty: frontend.VoidType()}
	}
	e.line("%s = call %s %s(%s)", t, rs, fn, argText)
	if ret != nil {
		return val{op: t, ty: ret}
	}
	return val{op: t, ty: frontend.IntType()}
}

// indirectCalleeSig recovers the declared signature behind a computed callee,
// or two nils when the front end cannot see one.
//
// A call through a bare identifier is the common case (`fmtfn(...)` where
// fmtfn is a parameter or a local of function-pointer type); anything else --
// `(*fp)(x)`, `tbl[i](y)` -- is resolved from the expression's static type.
func (e *irEmitter) indirectCalleeSig(fn frontend.Expr) ([]*frontend.Type, *frontend.Type) {
	var ft *frontend.Type
	if id, ok := fn.(*frontend.Ident); ok {
		_, ft, _ = e.tr.fnPtrVar(id.Name)
	}
	if ft == nil {
		if t := e.tr.exprType(fn); t != nil {
			ft = frontend.FuncTypeOf(t)
		}
	}
	if ft == nil {
		return nil, nil
	}
	return ft.Params, ft.Ret
}

// isAggregateTy reports whether a parameter type has no single-operand
// lowering, so its argument must be passed exactly as the front end produced
// it rather than converted.
func isAggregateTy(t *frontend.Type) bool {
	return t != nil && (t.Kind == frontend.KStruct || t.Kind == frontend.KUnion || t.Kind == frontend.KArr)
}

// --- members ----------------------------------------------------------------

// memberAddr returns the address of a struct member.
func (e *irEmitter) memberAddr(n *frontend.MemberExpr) string {
	sty := e.tr.exprType(n.Base)
	// In the arrow form the base is already a pointer to the struct; in the dot
	// form it is the struct itself, whose address is taken.
	var base string
	if n.Arrow {
		base = e.rvalue(n.Base)
	} else if isAddressable(n.Base) {
		base = e.lvalue(n.Base)
	} else {
		// The base is a struct/union produced by value with no address of its
		// own -- a call such as "mkpt(1,2).x" or a function-pointer call
		// "mp(5,6).y". lvalue() would hand back the returned value, and a
		// getelementptr over that value is a type error LLVM rejects
		// ("defined with type '{ i32, i32 }' but expected 'ptr'"). Spill the
		// value into a temporary slot in the entry block and address that.
		v := e.eval(n.Base)
		if v.ty != nil {
			slot := e.entryAlloca(e.ty(v.ty))
			e.store(slot, v)
			base = slot
		} else {
			base = v.op
		}
	}
	off, ok := e.tr.memberOffset(n, sty)
	if !ok {
		return base
	}
	if off == 0 {
		return base
	}
	r := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", r, base, off)
	return r
}

func (e *irEmitter) member(n *frontend.MemberExpr) val {
	ty := e.tr.exprType(n)
	p := e.memberAddr(n)
	// An array member decays to a pointer to its first element, exactly as an
	// array variable does in ident(). `stat(e->d_name, &st)` passes that
	// address; it does not pass the 256 bytes it points at. Loading it here
	// handed the callee a value of type `[256 x i8]` where a ptr was expected,
	// which LLVM rejected with "'%t28' defined with type '[256 x i8]' but
	// expected 'ptr'" -- so every program that passed a struct's char array to
	// a function failed to compile on this back end while compiling fine on
	// the native one. Deciding it here is what C means by array-to-pointer
	// conversion, and a subscript of the member still reads one element
	// because Index asks for the base's address, which is the same thing.
	if ty != nil && ty.Kind == frontend.KArr {
		return val{op: p, ty: frontend.PtrType(ty.Elem)}
	}
	return e.load(p, ty)
}
