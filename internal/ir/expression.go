package gocl

import "goc/frontend"

// Expressions, in LLVM IR.
//
// Two things distinguish this from the assembly path. First, there is no
// separate lvalue path in the C grammar sense: an expression yields either a
// value or a pointer to one, and load and store are explicit, so genLValue
// returns an address and rvalue loads from one. Second, C's implicit
// conversions -- integer promotion, the usual arithmetic conversions, pointer
// scaling -- have to be materialised, because IR will not do them for us.

// val is an expression's result: either a register holding a value, or a
// constant. Instructions are emitted as a side effect.
type val struct {
	op string         // an operand: "%t3", "42", "null"
	ty *frontend.Type // the C type of the value
}

// rvalue evaluates an expression to a value.
func (e *irEmitter) rvalue(x frontend.Expr) string {
	v := e.eval(x)
	return v.op
}

// eval is rvalue with the type attached.
func (e *irEmitter) eval(x frontend.Expr) val {
	switch n := x.(type) {
	case nil:
		return val{op: "0", ty: frontend.IntType()}
	case *frontend.NumLit:
		return e.numLit(n)
	case *frontend.StrLit:
		return e.strLit(n)
	case *frontend.Ident:
		return e.ident(n)
	case *frontend.Unary:
		return e.unary(n)
	case *frontend.Binary:
		return e.binary(n)
	case *frontend.AssignExpr:
		return e.assignExpr(n)
	case *frontend.AssignStmt:
		e.assignStmt(n)
		return val{op: "0", ty: frontend.IntType()}
	case *frontend.Call:
		return e.callExpr(n)
	case *frontend.IndirectCall:
		return e.indirectCall(n)
	case *frontend.Index:
		ty := e.tr.exprType(n)
		// An element that is itself an array decays to a pointer, exactly as
		// an array variable does in ident() and an array member does in
		// member(): "printf(\"%s\", rows[0])" passes the address of rows[0],
		// not the six bytes it holds. Loading it here handed the callee a
		// [6 x i8] where a ptr belonged -- a variadic callee reading a pointer
		// argument then took the array's own bytes as an address and faulted
		// inside the format scan on the first row it printed.
		if ty != nil && ty.Kind == frontend.KArr {
			return e.addressOf(n, ty)
		}
		return e.load(e.lvalue(n), ty)
	case *frontend.MemberExpr:
		return e.member(n)
	case *frontend.CastExpr:
		return e.cast(n)
	case *frontend.VaArgExpr:
		return e.vaArg(n)
	case *frontend.IncDecExpr:
		return e.incDec(n)
	case *frontend.CondExpr:
		return e.condExpr(n)
	case *frontend.CommaExpr:
		e.discard(n.Left)
		return e.eval(n.Right)
	case *frontend.SizeofExpr:
		t := n.Typ
		if t == nil && n.E != nil {
			t = e.tr.exprType(n.E)
		}
		// sizeof on a variable-length array is a run-time value -- and it is
		// the one sizeof whose operand is evaluated, because the extent lives
		// in the type rather than in any stored object. Multiply the dimension
		// by the element width; the element width is still a constant (a
		// second variable-length dimension is rejected by the checker).
		if t != nil && t.IsVLA() && t.Elem != nil && !t.Elem.HasVLA() {
			cnt := e.indexValue(t.VLALen)
			w := frontend.Sizeof(t.Elem)
			// The result is a 64-bit value, and it must be ANNOUNCED as one:
			// a conversion site (a cast, a return, an argument) turns it into
			// the type this val claims, and claiming the 4-byte unsigned that
			// a constant sizeof uses makes it emit `sext i32` on an i64 --
			// which LLVM rejects outright.
			sz64 := &frontend.Type{Kind: frontend.KInt, Width: 8, Signed: false}
			if w <= 1 {
				return val{op: cnt, ty: sz64}
			}
			tmp := e.newTmp()
			e.line("%s = mul i64 %s, %d", tmp, cnt, w)
			return val{op: tmp, ty: sz64}
		}
		return val{op: itoa(frontend.Sizeof(t)), ty: frontend.UnsignedType()}
	case *frontend.CompoundLit:
		return e.compoundLit(n)
	case *frontend.GenericExpr:
		if n.Chosen != nil {
			return e.eval(n.Chosen)
		}
		return val{op: "0", ty: frontend.IntType()}
	case *frontend.TmpLoad:
		// A temporary the native path parks in a frame slot; the IR path has
		// no such slots, so the node never appears here.
		return val{op: "0", ty: n.Typ}
	}
	return val{op: "0", ty: frontend.IntType()}
}

// cond evaluates a controlling expression and reduces it to i1.
//
// vaArg lowers the va_arg builtin against the target's own va_list layout.
// llvm.va_start is target-defined and writes different things per ABI:
//
//	Windows x64 -- a single pointer to the caller's register save area, every
//	  variadic argument one eight-byte slot (general registers first, then the
//	  stack past them). Reading an argument is load cursor / load / advance 8.
//
//	x86-64 SysV (Linux) -- the 24-byte __va_list_tag { unsigned gp_offset,
//	  unsigned fp_offset; void *overflow_arg_area; void *reg_save_area; }.
//	  Integer and floating arguments live in *separate* halves of the register
//	  save area, each with its own cursor and its own limit (6 GP slots = 48
//	  bytes, 8 SSE slots = 128 bytes); past either limit the argument comes
//	  from the overflow area instead, one eight-byte slot at a time.
//
// The two cannot be shared: reading gp_offset as if it were the flat cursor
// makes every variadic call that consumes an argument crash on Linux, while the
// Windows shape (one pointer) is what the register-half arithmetic would read
// as garbage. So the read is dispatched on the target, and the comment on each
// branch records which ABI it implements.
//
// LLVM's own va_arg instruction is not an option here: it is a target-defined
// *instruction* ("vaarg %ap, i32") rather than a call, and the older intrinsic
// spelling ("@llvm.va_arg(ptr, [i32, i8*])") is rejected by LLVM 23 ("expected
// number in address space"). Spelling the read out also keeps the two back ends
// agreeing on where an argument lives, which is why goc does not defer to a
// second, target-specific opinion.
func (e *irEmitter) vaArg(n *frontend.VaArgExpr) val {
	// Every va_list -- a local `va_list ap;` or one that arrived as a
	// parameter -- is backed by a writable slot (vaListSlot returns its
	// address). That slot is what va_start fills in; writing back to it is what
	// makes a second va_arg read the next argument.
	ap := e.vaListSlot(n.Ap)
	ty := n.Typ
	if ty == nil {
		ty = frontend.IntType()
	}
	// A long double variadic argument was passed as ONE pointer slot naming
	// the 16 bytes (the aggregate hidden-pointer convention -- see call.go's
	// variadic marshalling, and goc's genVaArg for the other back end).
	// Reading it is therefore two steps: take a pointer out of the cursor
	// with the machinery for a pointer argument (which picks the right half
	// of a SysV register save area and steps by one slot on every target),
	// then load the 16 bytes it names. Handing the i128 IR type straight to
	// the readers would instead load 16 bytes OUT of the save area and step
	// the cursor by 16 -- a value from nowhere and every later argument off
	// by eight.
	if ty.Kind == frontend.KLongDouble {
		ptrTy := frontend.PtrType(frontend.CharType())
		pv := e.vaArg(&frontend.VaArgExpr{Ap: n.Ap, Typ: ptrTy})
		v := e.newTmp()
		e.line("%s = load i128, ptr %s, align 16", v, pv.op)
		return val{op: v, ty: ty}
	}
	lty := e.ty(ty)

	if e.vaListIsFlatCursor() {
		return e.vaArgFlat(ap, lty, ty)
	}
	if e.c.arch == "aarch64" {
		return e.vaArgAAPCS64(ap, lty, ty)
	}
	return e.vaArgSysV(ap, lty, ty)
}

// vaArgAAPCS64 reads one argument out of an AArch64 va_list.
//
// AAPCS64 is the one target here whose va_list is NOT a cursor. It is a
// five-field record
//
//	struct { void *__stack; void *__gr_top; void *__vr_top;
//	         int __gr_offs; int __vr_offs; }
//
// and the two halves of the argument list live in two different places: the
// prologue spills the eight general-purpose argument registers into a GP save
// area and the eight FP registers into a separate one, and an argument is in
// whichever save area still has room for it. Once a half is exhausted the
// argument comes off the stack instead.
//
// The offsets count UP from a negative start towards zero -- __gr_offs begins
// at -(number of vararg GP slots) * 8, so the test is `offs < 0`, and the
// address is __gr_top + offs, __gr_top pointing at the END of the save area.
// That is the opposite direction from x86-64 SysV's gp_offset, which counts up
// from zero to a positive limit; reading one as the other is why AArch64 used
// to print the same wrong number for every value.
//
// Treating this record as the flat cursor it is not -- loading the first eight
// bytes as a pointer -- yields __stack, the overflow area. That is where
// arguments past the eight registers live, so a program that passed everything
// on the stack would work; `printf("%d", 42)` does not, because 42 arrives in
// x1 and is spilled into the GP save area, which __stack never points at.
func (e *irEmitter) vaArgAAPCS64(ap, lty string, ty *frontend.Type) val {
	const (
		stackOff  = 0  // void *__stack
		grTopOff  = 8  // void *__gr_top
		vrTopOff  = 16 // void *__vr_top
		grOffsOff = 24 // int __gr_offs
		vrOffsOff = 28 // int __vr_offs
	)
	// A float or a double is in the FP save area, whose slots are 16 bytes
	// wide -- one whole q register each. Everything else is in the GP save
	// area, eight bytes a slot.
	isFP := lty == "float" || lty == "double"
	topOff, offsOff, step := grTopOff, grOffsOff, 8
	if isFP {
		topOff, offsOff, step = vrTopOff, vrOffsOff, 16
	}

	top := e.newTmp()
	e.line("%s = load ptr, ptr %s, align 8", top, e.gepStruct(ap, topOff))
	offs := e.newTmp()
	e.line("%s = load i32, ptr %s, align 4", offs, e.gepStruct(ap, offsOff))
	stack := e.newTmp()
	e.line("%s = load ptr, ptr %s, align 8", stack, e.gepStruct(ap, stackOff))

	useReg := e.newTmp()
	e.line("%s = icmp slt i32 %s, 0", useReg, offs)
	// The offset is signed and negative, so it has to be sign-extended before
	// it can index a GEP; zero-extending it would turn -56 into 4294967240.
	offs64 := e.newTmp()
	e.line("%s = sext i32 %s to i64", offs64, offs)
	regAddr := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %s", regAddr, top, offs64)
	addr := e.newTmp()
	e.line("%s = select i1 %s, ptr %s, ptr %s", addr, useReg, regAddr, stack)

	// Advance only the cursor that was used, and write the other back exactly
	// as it was read. Both stores go through the FIELD address: the record is
	// five independent fields packed into 32 bytes, so storing the advanced
	// __stack at ap+0 would land on top of __stack itself only by luck, and
	// elsewhere by design.
	advOffs := e.newTmp()
	e.line("%s = add i32 %s, %d", advOffs, offs, step)
	// Off the stack an argument occupies eight bytes whatever its type: the
	// stack half is packed, unlike the 16-byte FP register slots.
	advStack := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 8", advStack, stack)
	offsKept := e.newTmp()
	e.line("%s = select i1 %s, i32 %s, i32 %s", offsKept, useReg, advOffs, offs)
	e.line("store i32 %s, ptr %s, align 4", offsKept, e.gepStruct(ap, offsOff))
	stackKept := e.newTmp()
	e.line("%s = select i1 %s, ptr %s, ptr %s", stackKept, useReg, stack, advStack)
	e.line("store ptr %s, ptr %s, align 8", stackKept, e.gepStruct(ap, stackOff))

	slot := e.newTmp()
	switch lty {
	case "i1":
		raw := e.newTmp()
		e.line("%s = load i8, ptr %s, align 1", raw, addr)
		e.line("%s = trunc i8 %s to i1", slot, raw)
	case "float":
		bits := e.newTmp()
		e.line("%s = load i32, ptr %s, align 4", bits, addr)
		e.line("%s = bitcast i32 %s to float", slot, bits)
	case "double":
		bits := e.newTmp()
		e.line("%s = load i64, ptr %s, align 8", bits, addr)
		e.line("%s = bitcast i64 %s to double", slot, bits)
	default:
		e.line("%s = load %s, ptr %s, align %d", slot, lty, addr, alignOfIr(lty))
	}
	return val{op: slot, ty: ty}
}

// vaArgSysV reads one argument out of an x86-64 SysV va_list -- the
// __va_list_tag llvm.va_start writes on Linux:
//
//	struct { unsigned gp_offset, fp_offset; void *overflow_arg_area, *reg_save_area; }
//
// A double or float comes from the SSE half of the register save area while it
// is in range (fp_offset <= 176), otherwise from the overflow area; every other
// type comes from the GP half while gp_offset <= 48, otherwise from the overflow
// area. Either way the chosen cursor advances, so the next va_arg reads the next
// argument.
// vaListIsFlatCursor reports whether this target's va_list is a single cursor
// pointer -- the shape Windows x64 and the ARM AAPCS both use -- rather than
// the 24-byte __va_list_tag x86-64 SysV uses.
//
// The two are not interchangeable. Reading a flat cursor as if it were the tag
// makes the program take its offsets out of a stack address: a SysV read
// treats the first eight bytes as gp_offset, so an ARM variadic call compares a
// pointer against 48, decides the argument is "past the register save area",
// and loads it from wherever the pointer's low bits point. Nothing traps at
// link time; the program faults on the first variadic argument instead, which
// is why the two shapes have to be told apart explicitly rather than by
// "is this Linux".
//
// ARM's AAPCS has no register save area for variadic arguments at all: every
// variadic argument past the fixed ones is pushed onto the stack, and va_list is
// a bare `char *` cursor into that stack region (this is what
// `add r1, sp, #36; str r1, [sp]` lowers to). RISC-V's psABI says the same of
// va_list, so both are handled by vaArgFlat.
//
// AArch64 is deliberately NOT in that set, and it was once: AAPCS64's va_list
// is the five-field record in vaArgAAPCS64, and reading it as a cursor hands
// every integer argument the address of the stack overflow area instead of the
// GP save area it was spilled into.
func (e *irEmitter) vaListIsFlatCursor() bool {
	switch e.c.arch {
	case "arm", "armel", "riscv64", "riscv32":
		// The AAPCS and RISC-V shapes both hand va_start a single pointer and
		// let the callee walk it, so they share the flat-cursor reader.
		//
		// RISC-V earns its place here for a reason worth recording, because it
		// is not obvious from the psABI text: llvm.va_start is target-defined,
		// and what it writes on RISC-V is one pointer to a register save area
		// the prologue spilled the variadic registers into -- not the
		// {gp_offset, fp_offset, overflow, reg_save} four-part tag x86-64
		// uses. Reading that tag out of a RISC-V va_list therefore compares a
		// pointer's low half against 48, concludes the arguments are past the
		// register save area, and loads from whatever the following fields
		// happen to hold. The link succeeds; the first va_arg reads address 0.
		return true
	}
	return !e.c.linux // Windows x64
}

// vaArgFlat reads one argument from a single-cursor va_list: load the type at
// the cursor, then advance the cursor by that type's size. This is the ARM
// AAPCS and the RISC-V (ILP32 / LP64) shape -- the cursor is a pointer into a
// save area the callee's prologue spilled the variadic registers into, and the
// caller's own stack args follow it contiguously, so every argument sits at the
// position the cursor reaches by walking the slot sizes. The caller and callee
// both walk the same contiguous bytes, so the step (see vaArgStep) is the one
// thing that has to agree, and it does: an int and a pointer are a word, a
// double and an i64 are eight bytes, with nothing padded in between.
//
// The one wrinkle is the 8-byte case on a 32-bit target, handled inside: there
// the caller aligns a 64-bit argument to an even register pair, so the cursor
// has to be rounded up to eight before such a read. Rounding it up *relative to
// the save-area start for every argument* -- which is what an earlier version
// did -- walks past the low half of a double and made printf print 0.00 on
// every 32-bit target; the round-up belongs on the address of an 8-byte read
// only, and only where a word is four bytes.
func (e *irEmitter) vaArgFlat(ap, lty string, ty *frontend.Type) val {
	cur := e.newTmp()
	e.line("%s = load ptr, ptr %s, align 8", cur, ap)

	// A 64-bit argument starts at an EVEN register pair.
	//
	// Both the AAPCS (arm/armel) and the RV32 psABI place a 64-bit variadic
	// argument in an even-aligned register pair: the caller skips the odd
	// register and the value begins at the next 8-byte-aligned address, so a
	// `printf("%lld", 7LL)` passes its 7 in r2:r3 (a2:a3), never in r1:r2. The
	// cursor va_start hands over points at the next *free word*, which is that
	// skipped odd register whenever a 64-bit argument comes first. Loading
	// eight bytes there takes the skipped register as the low half and the
	// real low half as the high half: the value reads shifted left by 32
	// (`7` printed as 30064771072).
	//
	// So the cursor is rounded UP to eight before an 8-byte read -- on the
	// ADDRESS, not on the step: the step stays 8 and the next argument still
	// begins immediately after the value. Rounding the step instead (or
	// rounding unconditionally, on 64-bit targets where every slot is already
	// eight bytes) is what used to make every 32-bit target print 0.00.
	//
	// When the 64-bit argument is NOT the first one there is nothing to skip:
	// an earlier int has already consumed the odd register and the cursor is
	// aligned, which is why `printf("%d %lld", 42, 7LL)` was already correct
	// while `printf("%lld", 7LL)` was not.
	if (lty == "i64" || lty == "double") && e.vaArgStep("i32") == 4 {
		asInt := e.newTmp()
		e.line("%s = ptrtoint ptr %s to i64", asInt, cur)
		up := e.newTmp()
		e.line("%s = add i64 %s, 7", up, asInt)
		round := e.newTmp()
		e.line("%s = and i64 %s, -8", round, up)
		aligned := e.newTmp()
		e.line("%s = inttoptr i64 %s to ptr", aligned, round)
		cur = aligned
	}

	slot := e.newTmp()
	switch lty {
	case "i1":
		raw := e.newTmp()
		e.line("%s = load i8, ptr %s, align 1", raw, cur)
		e.line("%s = trunc i8 %s to i1", slot, raw)
	case "float":
		bits := e.newTmp()
		e.line("%s = load i32, ptr %s, align 4", bits, cur)
		e.line("%s = bitcast i32 %s to float", slot, bits)
	default:
		e.line("%s = load %s, ptr %s, align %d", slot, lty, cur, alignOfIr(lty))
	}
	next := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", next, cur, e.vaArgStep(lty))
	e.line("store ptr %s, ptr %s, align 8", next, ap)
	return val{op: slot, ty: ty}
}

// vaArgStep is how far a va_arg cursor advances after reading an argument of
// the given IR type. The width is the *slot* width, not the C type's width,
// and the slot width follows the pointer size of the target, because that is
// also what widenVarargSlot makes the caller store: on a 64-bit target an int
// reaches the callee widened to i64 and occupies eight bytes in the save area,
// so a cursor that steps four reads the low half of one argument and then the
// zero above it -- printf("%d %d %d", 144, 7, 21) answers "144 0 7" on
// Windows x64, where va_list is this very flat cursor.
//
// The 32-bit targets -- AAPCS (arm/armel) and the RV32 psABI -- pack a
// variadic int into a word, so the step there is four. Widening it to eight on
// those targets is wrong in the direction that looks like a code-generation
// bug: the caller really did leave four bytes per argument and the cursor
// walked past every other one. (And widening the caller's operand there is
// independently wrong -- it changes how many registers the argument consumes --
// which is why widenVarargSlot skips 32-bit targets too.)
//
// A double and an i64 always occupy a full eight bytes; a pointer is one word,
// so it tracks the slot width with the ints.
func (e *irEmitter) vaArgStep(lty string) int64 {
	word := int64(8)
	switch e.c.arch {
	case "arm", "armel", "riscv32":
		// 32-bit targets: a word is four bytes and the save area is an array
		// of them.
		word = 4
	}
	switch lty {
	case "i1", "i8", "i16", "i32", "float", "ptr":
		return word
	case "i64", "double":
		return 8
	}
	// Anything wider (a vector, or an aggregate lowered to its own storage)
	// advances by its own natural size.
	return int64(alignOfIr(lty))
}

func (e *irEmitter) vaArgSysV(ap, lty string, ty *frontend.Type) val {
	// Field offsets within __va_list_tag: gp_offset and fp_offset are the two
	// leading unsigned ints (ap+0 and ap+4), the two pointers follow.
	const (
		gpOff  = 0
		fpOff  = 4
		ovfOff = 8
		rsvOff = 16
	)
	gpRegMax := 48  // 6 integer register slots
	fpRegMax := 176 // 8 SSE slots plus the 48-byte GP half (8 * 16 + 48)

	// isFP reports whether this argument type is fetched from the SSE half.
	// float and double promote to double when passed, and both are read back
	// from the 16-byte-per-slot SSE area, so a float argument occupies a whole
	// fp_offset step just like a double.
	isFP := lty == "float" || lty == "double"
	cursorOff := gpOff
	limit, step := gpRegMax, 8
	if isFP {
		cursorOff, limit, step = fpOff, fpRegMax, 16
	}

	// cur = load cursor field; ovf = load overflow_arg_area
	cur := e.newTmp()
	e.line("%s = load i32, ptr %s, align 4", cur, e.gepStruct(ap, cursorOff))
	ovf := e.newTmp()
	e.line("%s = load ptr, ptr %s, align 8", ovf, e.gepStruct(ap, ovfOff))
	rsv := e.newTmp()
	e.line("%s = load ptr, ptr %s, align 8", rsv, e.gepStruct(ap, rsvOff))

	// regAddr = reg_save_area + cur. The cursor field is an i32 in the
	// on-stack __va_list_tag, and a GEP index is i64, so it is widened first.
	cur64 := e.newTmp()
	e.line("%s = zext i32 %s to i64", cur64, cur)
	regAddr := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %s", regAddr, rsv, cur64)

	// addr = cur < limit ? regAddr : overflow_arg_area
	useReg := e.newTmp()
	e.line("%s = icmp ult i32 %s, %d", useReg, cur, limit)
	addr := e.newTmp()
	e.line("%s = select i1 %s, ptr %s, ptr %s", addr, useReg, regAddr, ovf)

	// Advance the cursor that was actually used -- and only that one. Both
	// stores go through the FIELD address, never the tag base: the tag is four
	// independent fields packed into 24 bytes, so storing the advanced
	// overflow_arg_area at ap+0 would land on top of gp_offset. The first
	// va_arg in a function would then return the right value and every later
	// one would read from a garbage cursor (symptom: the first %d prints and
	// the second is 0 or garbage).
	//
	// The advance is branch-shaped rather than unconditional on purpose: the
	// register cursor and the overflow cursor are alternatives, and moving both
	// would skip an argument on whichever path is used second. clang's IR is
	// the same shape -- a load/advance/store pair inside each arm of the
	// reg-vs-overflow branch, with the two candidate addresses rejoined by a
	// phi. Its bound is spelled `icmp ule 40` because the last GP slot starts at
	// 40, which is the same test as `ult 48` at eight bytes per slot.
	advCur := e.newTmp()
	e.line("%s = add i32 %s, %d", advCur, cur, step)
	advOvf := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 8", advOvf, ovf)
	// Writing the *unchanged* value on the arm not taken keeps this
	// branch-free: on the register path the overflow cursor is stored back
	// exactly as it was read, which is a no-op, and vice versa.
	curKept := e.newTmp()
	e.line("%s = select i1 %s, i32 %s, i32 %s", curKept, useReg, advCur, cur)
	e.line("store i32 %s, ptr %s, align 4", curKept, e.gepStruct(ap, cursorOff))
	ovfKept := e.newTmp()
	e.line("%s = select i1 %s, ptr %s, ptr %s", ovfKept, useReg, ovf, advOvf)
	e.line("store ptr %s, ptr %s, align 8", ovfKept, e.gepStruct(ap, ovfOff))

	// Load the value at the argument's own width from the same address.
	slot := e.newTmp()
	switch lty {
	case "i1":
		raw := e.newTmp()
		e.line("%s = load i8, ptr %s, align 1", raw, addr)
		e.line("%s = trunc i8 %s to i1", slot, raw)
	case "float":
		bits := e.newTmp()
		e.line("%s = load i32, ptr %s, align 4", bits, addr)
		e.line("%s = bitcast i32 %s to float", slot, bits)
	case "double":
		bits := e.newTmp()
		e.line("%s = load i64, ptr %s, align 8", bits, addr)
		e.line("%s = bitcast i64 %s to double", slot, bits)
	default:
		e.line("%s = load %s, ptr %s, align %d", slot, lty, addr, alignOfIr(lty))
	}
	return val{op: slot, ty: ty}
}

// gepStruct returns a pointer to the field `off` bytes into the byte-addressed
// object at `p`. The va_list slot is an opaque 24-byte allocation whose real
// layout is the target's, so it is addressed as bytes rather than through a
// struct type the emitter would have to know per target.
func (e *irEmitter) gepStruct(p string, off int) string {
	g := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr %s, i64 %d", g, p, off)
	return g
}

func (e *irEmitter) cond(x frontend.Expr) string {
	if x == nil {
		return "true"
	}
	// "x != x" -- how math.h spells isnan(x) -- is answered here, before eval
	// splits it into two loads. Lowered the ordinary way it becomes
	// "fcmp une %t17, %t18" over two temporaries, and LLVM rewrites that
	// self-compare to "fcmp uno x, 0.0" during InstCombine -- a rewrite that
	// does not survive being carried into the phi that a short-circuit || or
	// if produces. Measured with opt default<O1>, the identical function
	// returns 1 when the two tests are combined with "or i1" and 0 when they
	// are combined with the phi: fmaximum_num(NAN, 1) answered 1.0 and
	// isnan() was false for every value a program could name.
	//
	// Naming the shape here leaves nothing to rewrite, which is what clang
	// does for isnan too. Only the exact shape -- the same variable on both
	// sides of "!=" -- is redirected; a genuine "a != b" is left alone.
	if id, ok := floatSelfNe(x); ok {
		v := e.rvalue(id)
		t := e.newTmp()
		e.line("%s = fcmp uno %s %s, 0.0", t, e.ty(e.tr.exprType(id)), v)
		return t
	}
	// The value's IR type decides, not the C type: a comparison already yields
	// an i1 even though C types it as int, and re-comparing that against zero
	// would both be redundant and type-wrong.
	v := e.eval(x)
	ty := e.ty(v.ty)
	if ty == "i1" {
		return v.op
	}
	if v.ty != nil && (v.ty.Kind == frontend.KFloat || v.ty.Kind == frontend.KDouble) {
		t := e.newTmp()
		e.line("%s = fcmp une %s %s, 0.0", t, ty, v.op)
		return t
	}
	// A long double condition is a runtime comparison against zero. Truncating
	// it to an integer first would make 1e-4000 -- whose integer conversion is
	// 0 -- a false condition, and C says only a zero is false.
	if v.ty != nil && v.ty.Kind == frontend.KLongDouble {
		return e.tfTruth(v.op)
	}
	t := e.newTmp()
	if ty == "ptr" {
		// A pointer condition is "non-null"; compare against the null pointer
		// constant, not the integer 0, or LLVM rejects the mixed types.
		e.line("%s = icmp ne ptr %s, null", t, v.op)
	} else {
		e.line("%s = icmp ne %s %s, 0", t, ty, v.op)
	}
	return t
}

// discard evaluates an expression for its effects only.
func (e *irEmitter) discard(x frontend.Expr) {
	switch n := x.(type) {
	case nil:
		return
	case *frontend.Call:
		e.callExpr(n)
	case *frontend.IndirectCall:
		e.indirectCall(n)
	case *frontend.CommaExpr:
		e.discard(n.Left)
		e.discard(n.Right)
	case *frontend.AssignExpr:
		e.assignExpr(n)
	case *frontend.AssignStmt:
		e.assignStmt(n)
	default:
		// A pure expression needs no code; the operand it produced is unused.
		e.eval(x)
	}
}

// --- leaves -----------------------------------------------------------------

func (e *irEmitter) numLit(n *frontend.NumLit) val {
	// Kind == frontend.TDouble marks the literal as floating point; IsFloat then only
	// says whether it is float or double ("1.5f" versus "1.5"). Testing IsFloat
	// alone made every unsuffixed literal -- "1.0", "0.0", and so the NAN and
	// INFINITY macros -- look like an integer, so a division of them was
	// emitted as "sdiv i32 0, 0" and the surrounding double arithmetic lost its
	// type.
	if n.Kind == frontend.TF128 {
		// A long double literal is written as its full 128-bit pattern. There
		// is no fp128 constant spelling to reach for and no float64 in the
		// path: n.F128 is the encoding the lexer produced from the exact
		// decimal, and losing it to a double would lose 60 bits of mantissa.
		return val{op: tfConst(n.F128.Hi, n.F128.Lo), ty: frontend.LongDoubleType()}
	}
	if n.Kind == frontend.TDouble {
		// A float literal lives in an i32 on this path (the same choice the
		// native generator makes), so it is written as a bit pattern.
		t := e.newTmp()
		// The 16-digit form is what LLVM reads for float as well as double,
		// and it must be exactly representable in the type: for float that is
		// the bit pattern of the double the float widens to, so 5.0f is
		// 0x4014000000000000 and not the 32-bit pattern 0x40a00000 padded
		// out, which lands in the subnormals and is rejected.
		if n.IsFloat {
			e.line("%s = bitcast float 0x%016x to float", t,
				f64bits(float64(float32(n.Fval))))
		} else {
			e.line("%s = bitcast double 0x%016x to double", t, f64bits(n.Fval))
		}
		return val{op: t, ty: floatOrDouble(n)}
	}
	if n.BigWords != nil {
		// _BitInt: the value is a little-endian word array. The caller keeps
		// such functions on the native path, but rendering the low word keeps
		// the module valid if one ever arrives.
		if len(n.BigWords) > 0 {
			return val{op: itoa64(int64(n.BigWords[0])), ty: &frontend.Type{Kind: frontend.KBitInt, Bits: n.BigBits}}
		}
		return val{op: "0", ty: &frontend.Type{Kind: frontend.KBitInt, Bits: n.BigBits}}
	}
	ty := numLitType(n)
	return val{op: itoa64(n.Val), ty: ty}
}

func (e *irEmitter) strLit(n *frontend.StrLit) val {
	// A C string literal carries a trailing NUL; the lexer keeps only the
	// quoted bytes, so append the terminator here. Without it the literal is
	// an unterminated [N x i8] and the runtime reads straight past its end
	// into the next global (e.g. vfmt formatting "x" then walking into the
	// "assertion \"%s\"..." template and hitting the %s branch).
	//
	// The terminator is one ELEMENT wide: an L"..." literal's Bytes hold
	// UTF-16LE code units, so its NUL is two zero bytes. One byte there left
	// the last wchar_t half-initialised and %ls walked into the next
	// constant ("wide" printed "wide", a NUL, then whatever followed).
	b := bytesToBytes(n.Bytes)
	b = append(b, 0)
	if n.Wide {
		b = append(b, 0)
	}
	name := e.c.addString(b)
	t := e.newTmp()
	e.line("%s = getelementptr inbounds i8, ptr @%s, i64 0", t, name)
	// The literal's value is its address: a char array decays to a pointer.
	return val{op: t, ty: frontend.PtrType(frontend.CharType())}
}

func (e *irEmitter) ident(n *frontend.Ident) val {
	ty := e.tr.exprType(n)
	// A local is a slot; a global is a symbol. Both are addresses, and the
	// value is a load from it -- except for an array, which decays to its
	// address. Deciding that here rather than at every use is what C means by
	// "an array is converted to a pointer", and it is why a[j] on an array
	// parameter does not try to load the whole array as a value.
	if ty != nil && ty.Kind == frontend.KArr {
		return e.addressOf(n, ty)
	}
	// A va_list that va_start has already given its real storage reads as the
	// address of that storage -- the cursor the intrinsic wrote -- not as the
	// address of the slot holding it. Without the load, vfprintf was handed the
	// slot's address instead of the register save area, so it dereferenced one
	// level short and every variadic call crashed while a program that merely
	// linked printf ran fine.
	if s, ok := e.vaSlots[n.Name]; ok {
		t := e.newTmp()
		e.line("%s = load ptr, ptr %s, align 8", t, s)
		return val{op: t, ty: frontend.PtrType(frontend.CharType())}
	}
	if vi, ok := e.tr.lookupVar(n.Name); ok {
		// A static local reads from its module global -- the same object
		// every call sees -- and an array of it decays to that global's
		// address, exactly as a file-scope array does.
		if vi.glob != "" {
			if ty != nil && ty.Kind == frontend.KArr {
				return val{op: vi.glob, ty: frontend.PtrType(ty.Elem)}
			}
			return e.load(vi.glob, ty)
		}
		slot := e.slotFor(vi.op, e.ty(ty))
		return e.load(slot, ty)
	}
	// A thread-local global is reached through the linker-provided
	// __goc_tls_slot helper, which returns its per-thread address from the
	// offset ComputeTLSLayout assigned. The array case below routes through
	// lvalue (and therefore through tlsAddr too), so handling the scalar here
	// and letting arrays fall through is enough.
	if off, ok := e.c.tlsOffset(n.Name); ok {
		addr := e.tlsAddr(off)
		return e.load(addr, ty)
	}
	if sym := e.c.globalSym(n.Name); sym != "" {
		if ty != nil && ty.Kind == frontend.KArr {
			return val{op: "@" + sym, ty: frontend.PtrType(ty.Elem)}
		}
		return e.load("@"+sym, ty)
	}
	// A name with no storage is a constant (an enum member) or a function used
	// as a value. The constant case is what the checker leaves behind.
	if v, ok := e.tr.constValue(n.Name); ok {
		return val{op: itoa64(v), ty: ty}
	}
	// A function used as a value decays to its own address. C spells this
	// "function designator conversion"; in LLVM a function *is* its address, so
	// the symbol reference is already the pointer and no load is involved --
	// loading would read the instruction bytes at the entry point instead.
	//
	// This is what makes `int (*fp)(int) = twice;` and `apply(twice, 21)` work.
	// Without it they silently produced a null pointer, and the program crashed
	// at the first indirect call -- which is how goclib's own
	// `printf_lite_with(vfmt_i, ...)` took the program down: the formatter was
	// handed a null function pointer and called through it.
	if e.tr.isFuncName(n.Name) {
		// irFuncSym, so the address names the symbol the function was defined
		// under; a renamed libfunc taken by pointer would otherwise decay to a
		// reference to a symbol the module never defines.
		return val{op: "@" + irFuncSym(n.Name), ty: frontend.PtrType(e.fnPtrTy(n.Name))}
	}
	// A name with no storage at all. The checker's own view is that this can
	// only be reached when the operand is an integer (an enum member whose
	// value the front end did not fold) or a pointer to nothing, so the zero
	// value is rendered for the operand's own type. Emitting "null"
	// unconditionally put a pointer constant where a long was expected, and
	// LLVM rejected the comparison "icmp sle i64 null, %v".
	return val{op: e.zeroLiteral(ty), ty: ty}
}

// zeroLiteral renders the zero value of a type as an operand of that type.
func (e *irEmitter) zeroLiteral(t *frontend.Type) string {
	if t == nil {
		return "0"
	}
	switch t.Kind {
	case frontend.KPtr, frontend.KFunc, frontend.KArr, frontend.KStruct, frontend.KUnion:
		return "null"
	case frontend.KFloat, frontend.KDouble:
		return "0.0"
	}
	return "0"
}

// load reads a value of type t from the address p.
func (e *irEmitter) load(p string, t *frontend.Type) val {
	ty := e.ty(t)
	v := e.newTmp()
	e.line("%s = load %s, ptr %s, align %d", v, ty, p, alignOfIr(ty))
	return val{op: v, ty: t}
}

// tlsAddr returns the per-thread address of a thread-local variable given its
// .tls offset. The address comes from __goc_tls_slot, a small assembly helper
// the entry stub defines: on Windows it indexes the TEB's ThreadLocalStorage
// pointer with the loader-filled TLS index; on Linux it adds the offset to the
// .tls base. Computing the address in one place keeps the per-platform segment
// dance out of the IR.
func (e *irEmitter) tlsAddr(off int64) string {
	a := e.newTmp()
	e.line("%s = call ptr @__goc_tls_slot(i64 %d)", a, off)
	return a
}

// store writes value v of type t to the address p.
func (e *irEmitter) store(p string, v val) {
	ty := e.ty(v.ty)
	e.line("store %s %s, ptr %s, align %d", ty, v.op, p, alignOfIr(ty))
}

// floatSelfNe recognises "x != x" on a floating operand -- the shape math.h
// gives isnan() -- and hands back the variable being tested.
//
// Only the exact shape counts: the same variable spelled the same way on both
// sides of a float "!=". A genuine "a != b", a comparison between two different
// variables, and an integer self-compare all return false and are lowered the
// ordinary way.
func floatSelfNe(x frontend.Expr) (frontend.Expr, bool) {
	b, ok := x.(*frontend.Binary)
	if !ok || b.Op != "!=" || b.L == nil || b.R == nil {
		return nil, false
	}
	// Ident nodes are distinct objects even when they name the same variable,
	// so the names have to be compared. Any other node kind is compared by
	// pointer, which is exactly "the same expression written twice".
	li, lok := b.L.(*frontend.Ident)
	ri, rok := b.R.(*frontend.Ident)
	if lok && rok {
		if li.Name != ri.Name {
			return nil, false
		}
	} else if b.L != b.R {
		return nil, false
	}
	return b.L, true
}
