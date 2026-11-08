package gocl

// The LLVM IR back end: a pure AST -> IR translator with no code-generator
// coupling.
//
// The type analysis, name resolution and scope handling are NOT reimplemented
// here. They live in typeResolver (src/llvmtype.go), which both this translator
// and the assembly generator draw on, so the two back ends never disagree about
// a program's meaning. The translator holds its own typeResolver (cg == nil)
// and changes only where the bytes go: instead of appending to the instruction
// stream that becomes assembly, it appends IR text.
//
// The result is that a function can move between the two back ends without
// either one having a different idea of what the program means.

import (
	"fmt"
	"goc/frontend"
	"strings"
)

// irEmitter renders one function as LLVM IR. It holds a *CG so the existing
// scope and type machinery is available; the ir* fields are the IR-side state.
type irEmitter struct {
	c  *irMod        // the module being written
	tr *typeResolver // the shared type/name resolver (no CG coupling)

	fname   string
	retTy   string         // LLVM type of the return value
	retType *frontend.Type // the C return type, for signedness-sensitive conversions (e.g. long double -> unsigned long long)
	// paramNames is the parameter list of the function being written. A va_list
	// is a char*, so `va_arg(ap, T)` needs the va_list itself when `ap` arrived
	// as a parameter -- it is already the pointer -- and the address of the
	// local when `ap` is a local `va_list ap;`. The two are told apart by which
	// list the name is in.
	paramNames map[string]bool
	// vaSlots maps a va_list variable's name to the 24-byte slot its va_start,
	// va_arg and va_end all share, so the front end's `char *` typedef and
	// llvm.va_start's structure agree on one object.
	vaSlots map[string]string
	// vaNames records every identifier that has been used as a va_list, keyed
	// by name. The front end types va_list as `char *`, which is
	// indistinguishable from an ordinary string pointer, so the type cannot
	// tell a caller's `va_list ap` argument from any other `char *`. Knowing
	// the name is what lets a call that passes one along hand over the right
	// thing: on Windows x64 the cursor pointer itself, on x86-64 SysV the
	// pointer to the caller's 24-byte __va_list_tag. Without it, passing a
	// SysV va_list on loads the first eight bytes of the tag -- two packed
	// cursor integers -- and hands that to the callee as an address.
	vaNames map[string]bool
	entry   strings.Builder
	body    strings.Builder
	tmp     int
	lab     int
	closed  bool // the current block already has its terminator
	// slots maps a local's uid to its alloca, because the generator tracks
	// locals by uid rather than by name.
	slots map[int]string
	// breakTo and continueTo hold the enclosing loop's labels, innermost last.
	breakTo    []string
	continueTo []string
	// fnHasVLA says the function allocates variable-length arrays at run time.
	// Only then do the loops need an llvm.stacksave/stackrestore bracket:
	// every VLA alloca stays on the stack until its restore point, so a loop
	// without one would grow the frame once per iteration until the stack
	// overflowed (0xC00000FD after enough iterations of a large VLA).
	fnHasVLA bool
	// userLabels maps a C goto label to the LLVM block carrying it.
	userLabels map[string]string
	// pendingLabels are C labels waiting for the block that follows.
	pendingLabels []string
	// lastLabel is the most recently opened block, which is where a pending
	// C label or a pending goto lands.
	lastLabel string
	// scratch is a reusable alloca for the conversions that have to move a
	// value through memory, and scratchSize is how many bytes it holds.
	scratch     string
	scratchSize int
	// ldVararg is the pool of entry allocas that long double variadic
	// arguments are stored into before being passed by address (call.go's
	// variadic marshalling). One slot per live argument of a single call is
	// enough: calls are serialised, so all call sites share the pool and it
	// only grows to the largest number of long double arguments any one call
	// in this function takes. ldVarargUsed is reset to zero at the start of
	// each call's marshalling.
	ldVararg     []string
	ldVarargUsed int
	// forwards records branches to a C label that had not been seen yet.
	forwards []forwardGoto
	// staticSites maps a static-local declaration node to the module
	// global minted for it. The parser hands back the same DeclStmt node
	// every time the enclosing block runs -- a loop body is walked once
	// per iteration -- so the first visit mints the object and later
	// visits find it here and reuse it. Without this, a static inside a
	// loop would be re-declared (and re-initialised) once per iteration,
	// which is exactly the lifetime the keyword exists to prevent.
	staticSites map[*frontend.DeclStmt]string
}

// forwardGoto is one branch written before its target label existed.
type forwardGoto struct {
	label       string
	at          int    // where the branch text starts in body
	placeholder string // the block name used until the target is known
}

// --- emission primitives ----------------------------------------------------

func (e *irEmitter) newTmp() string {
	e.tmp++
	return "%t" + itoa(e.tmp)
}

func (e *irEmitter) newLabel() string {
	e.lab++
	return "L" + itoa(e.lab)
}

// line appends one instruction, opening a block first if the previous one was
// terminated. LLVM requires every basic block to end in exactly one terminator,
// and a label may only be the first thing in a block; this is where that
// invariant is maintained.
func (e *irEmitter) line(format string, args ...interface{}) {
	if e.closed {
		l := e.newLabel()
		e.body.WriteString(l + ":\n")
		e.lastLabel = l
		e.closed = false
	}
	s := format
	if len(args) > 0 {
		s = fmt.Sprintf(format, args...)
	}
	e.body.WriteString("  " + s + "\n")
}

func (e *irEmitter) term(format string, args ...interface{}) {
	e.line(format, args...)
	e.closed = true
}

// blockLabel starts a fresh block, attaching any C labels that were waiting for
// this point. A C label sits in front of a statement, and the block that follows
// it is exactly where control must land, so the association is recorded here.
func (e *irEmitter) blockLabel(l string) {
	e.body.WriteString(l + ":\n")
	e.lastLabel = l
	e.closed = false
	for _, cl := range e.pendingLabels {
		e.userLabels[cl] = l
	}
	e.pendingLabels = nil
}

func (e *irEmitter) ty(t *frontend.Type) string { return e.c.llirType(t) }

// --- function ---------------------------------------------------------------

// genIRFunc renders one C function as an LLVM IR definition.
func genIRFunc(tr *typeResolver, m *irMod, f *frontend.FuncDecl) (string, error) {
	e := &irEmitter{
		c:          m,
		tr:         tr,
		fname:      f.Name,
		retTy:      m.llirType(f.Ret),
		retType:    f.Ret,
		slots:      map[int]string{},
		userLabels: map[string]string{},
	}
	// Reset the per-function emission state the way genFunc does, so name
	// resolution and type answers are the same on the IR path.
	tr.resetScope()
	tr.pushScope()

	paramNames := map[string]bool{}
	for _, pn := range f.Params {
		paramNames[pn] = true
	}
	e.paramNames = paramNames

	// Learn the va_list names before emitting anything. Passing a va_list to
	// another function has to know it is one (see irEmitter.vaNames), and that
	// question is asked while lowering a call -- which can come before the
	// va_start/va_arg that would otherwise have registered the name. A forward
	// reference is normal C (`void f(va_list); void g(va_list ap){ f(ap); }`),
	// so the whole body is swept first.
	e.vaNames = map[string]bool{}
	collectVaListNames(f.Body, e.vaNames)

	// Does this function ever allocate a VLA? Loops bracket their iterations
	// with llvm.stacksave/stackrestore only then, so a function with no
	// run-time stack allocation emits byte-identical IR to before.
	frontend.WalkStmts(f.Body, func(s frontend.Stmt) {
		if d, ok := s.(*frontend.DeclStmt); ok && d.Typ != nil && d.Typ.HasVLA() {
			e.fnHasVLA = true
		}
	})

	params := e.bindParams(f)

	e.stmt(f.Body)
	if !e.closed {
		// Falling off the end of a function returns zero, as C requires. The
		// spelling depends on the return type: a pointer's zero is the null
		// pointer, and "ret ptr 0" is rejected by LLVM ("integer/byte constant
		// must have integer/byte type").
		switch e.retTy {
		case "void":
			e.term("ret void")
		case "float":
			e.term("ret float 0.0")
		case "double":
			e.term("ret double 0.0")
		case "ptr":
			e.term("ret ptr null")
		default:
			e.term("ret %s 0", e.retTy)
		}
	}
	tr.popScope()

	// A `goto` whose target had not been reached yet branched to a placeholder
	// block. Now that every C label is bound, each placeholder is defined as a
	// trampoline: one block that branches on to the real target. Writing it here
	// is what keeps the emitter single-pass -- the branch text was emitted
	// before the target existed, and LLVM is happy for a block to be defined
	// after the first reference to it.
	fwd, err := e.resolveForwardGotos()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// irFuncSym, not f.Name: the definition has to carry the same spelling the
	// call sites use, and for the two libfuncs LLVM would rewrite that is not
	// the C name. See llvmMisrecognizedMaxMin.
	sig := "define " + m.dso() + e.retTy + " @" + irFuncSym(f.Name) + "(" + strings.Join(params, ", ")
	if f.Variadic {
		// The ellipsis is what makes this variadic to LLVM, and it has to come
		// after every named parameter. A function that takes a fixed count is not
		// variadic no matter what its body does: the caller would not set up the
		// register save area, and va_start would read whatever the registers
		// happened to hold.
		sig += ", ...)"
	} else {
		sig += ")"
	}
	b.WriteString(sig + " {\n")
	b.WriteString("entry:\n")
	b.WriteString(e.entry.String())
	b.WriteString(e.body.String())
	b.WriteString(fwd)
	b.WriteString("}\n")
	// The attribute goes on the definition, after the body -- which is where
	// LLVM reads it from. Marking every function is not a compromise: the
	// attribute is what tells the size pipeline it may trade speed for bytes in
	// this function, and a program is one unit for that decision.
	if e.c.optSize {
		b.WriteString("\nattributes #0 = { optsize }\n")
	}
	return b.String(), nil
}

// termOpen ends the current block with a branch, but only while the block is
// still open. A body that already jumped somewhere -- goto, return, break --
// carries its own terminator, and adding another would silently open a fresh
// unreachable block instead of falling through.
func (e *irEmitter) termOpen(format string, args ...interface{}) {
	if e.closed {
		return
	}
	e.term(format, args...)
}

// resolveForwardGotos defines the trampoline blocks that forward `goto`s
// branched to. Each one is the placeholder block the branch named, containing a
// single branch on to the block the C label turned out to belong to.
func (e *irEmitter) resolveForwardGotos() (string, error) {
	if len(e.forwards) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, fg := range e.forwards {
		l, ok := e.userLabels[fg.label]
		if !ok {
			// A goto to a label this function never defines. The front end
			// rejects that, so reaching here means the label was dropped on the
			// way into the IR -- which is worth a hard error rather than a
			// branch to nowhere.
			return "", fmt.Errorf("goto %s: label not emitted in %s", fg.label, e.fname)
		}
		b.WriteString(fg.placeholder + ":\n")
		b.WriteString("  br label %" + l + "\n")
	}
	return b.String(), nil
}

// bindParams turns the C parameters into LLVM block arguments and local slots.
// A parameter also gets an alloca because C allows its address to be taken;
// LLVM's mem2reg pass removes the slot again when nothing does.
func (e *irEmitter) bindParams(f *frontend.FuncDecl) []string {
	var out []string
	for i, pt := range f.ParamTypes {
		// A parameter declared as an array is a pointer: the caller passes the
		// address, and treating the slot as the whole array would load it as a
		// value and pass an [N x i32] where a pointer belongs.
		if pt != nil && pt.Kind == frontend.KArr {
			pt = frontend.PtrType(pt.Elem)
		}
		ty := e.ty(pt)
		name := "%p" + itoa(i)
		out = append(out, ty+" "+name)
		if i < len(f.Params) {
			uid := e.tr.declareVar(f.Params[i], varInfo{ty: pt})
			slot := e.newTmp()
			e.entry.WriteString("  " + slot + " = alloca " + ty + ", align " +
				itoa(alignOfIr(ty)) + "\n")
			e.slots[uid] = slot
			e.line("store %s %s, ptr %s, align %d", ty, name, slot, alignOfIr(ty))
		}
	}
	return out
}

// slotFor returns the alloca backing a local, creating it on first use. A
// variable declared inside a loop body is still one object for the whole
// activation, so the slot is created once and reused across iterations -- which
// is what distinguishes it from a temporary.
func (e *irEmitter) slotFor(uid int, ty string) string {
	if s, ok := e.slots[uid]; ok {
		return s
	}
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca " + ty + ", align " +
		itoa(alignOfIr(ty)) + "\n")
	e.slots[uid] = slot
	return slot
}

// vaListTy is the storage a va_list needs on this target.
//
// goc declares va_list as `char *` (parser.go: typedefs["va_list"] = frontend.PtrType(frontend.CharType())),
// which is eight bytes, and on Windows x64 the intrinsic llvm.va_start stores
// exactly one eight-byte pointer into it: the cursor into the caller's register
// save area. The slot is still widened so the intrinsic can never overwrite the
// locals that would otherwise follow an eight-byte object, whatever a future
// target's va_start writes. The pointer typedef stays as it is -- it is what
// the rest of the front end reasons about, and passing a va_list between
// functions passes this pointer. Only the storage is widened, which is exactly
// what a real stdarg.h does when __builtin_va_list is an array type.
//
// 24 bytes covers x86-64 SysV's __va_list_tag. AArch64 needs 32: AAPCS64's
// __va_list is five fields -- two pointers plus two offsets, then the stack
// cursor -- and an allocation one i64 short of that leaves __vr_offs in the
// next local. llvm.va_start writes all five, so the shortfall is silent: the
// last field lands on whatever follows and every floating-point va_arg reads
// it back as garbage.
func (e *irEmitter) vaListTy() string {
	if e.c.arch == "aarch64" {
		return "[4 x i64]"
	}
	return "[3 x i64]"
}

// vaListSlot returns the address of the object backing a va_list named by x.
// Every mention of the same variable -- va_start, each va_arg, va_end, va_copy
// -- must land on this one slot, so the mapping is kept by name for the
// duration of the function.
//
// A local `va_list ap;` gets a slot of its own, widened to 24 bytes so the
// intrinsics can write a whole target va_list into it whatever that turns out
// to be.
//
// A va_list that arrived as a PARAMETER is where the two ABIs genuinely
// differ, and getting it wrong is silent rather than fatal -- the callee walks
// a tag whose tail is garbage and simply prints nothing after the first
// argument:
//
//   - Windows x64: va_list is `char *`, so the parameter holds the cursor
//     itself. The slot bindParams made is already eight bytes wide and already
//     holds the right value, so its address is the answer.
//
//   - x86-64 SysV: va_list is `struct __va_list_tag[1]`, an array type, so a
//     parameter decays to a pointer to the tag -- 24 bytes that live in the
//     CALLER's frame. The pointer is what bindParams received; the address of
//     the eight-byte slot holding it is not. Handing the slot address to
//     llvm.va_arg would make every read past the first eight bytes garbage and
//     would write the advanced cursor into the local copy, leaving the caller's
//     tag untouched. So the pointer is loaded out first and IT is the address
//     of the va_list.
func (e *irEmitter) vaListSlot(x frontend.Expr) string {
	id, ok := x.(*frontend.Ident)
	if !ok {
		return e.lvalue(x)
	}
	if e.vaNames == nil {
		e.vaNames = map[string]bool{}
	}
	e.vaNames[id.Name] = true
	if e.paramNames[id.Name] {
		if uid, ok := e.tr.lookupUID(id.Name); ok {
			slot := e.slotFor(uid, "ptr")
			if e.vaListIsFlatCursor() {
				// Windows x64 and the ARM AAPCS: va_list is `char *`, so the
				// parameter holds the cursor itself and the slot bindParams made
				// is already the answer.
				return slot
			}
			// SysV: `slot` holds a pointer to the caller's 24-byte tag. Load
			// it so the intrinsics address the tag itself, and so a write-back
			// lands in the caller's list where the C semantics require.
			p := e.newTmp()
			e.line("%s = load ptr, ptr %s, align 8", p, slot)
			return p
		}
		return e.rvalue(x)
	}
	if e.vaSlots == nil {
		e.vaSlots = map[string]string{}
	}
	if s, seen := e.vaSlots[id.Name]; seen {
		return s
	}
	// A va_list local that the ordinary local machinery has already given a
	// slot keeps it -- but only where eight bytes IS the whole va_list.
	//
	// On Windows x64 stdarg.h defines va_copy as a plain pointer assignment, so
	// `va_list m; va_copy(m, ap);` leaves `m` an ordinary `char *` local: one
	// slot, written by the assignment. Allocating a second slot for it here
	// would give va_arg(m, T) a DIFFERENT object from the one the copy filled
	// -- the copy landed in the first, va_arg read and advanced the second, and
	// the second started life as whatever was on the stack. The result was a
	// plausible-looking garbage number rather than a diagnosable fault.
	//
	// On x86-64 SysV the same reuse is wrong in the opposite direction. There
	// va_copy is a compiler builtin and `m` is a real 24-byte __va_list_tag; the
	// eight-byte slot the declaration made is only the first third of it, so
	// reading a va_arg through that slot would lose overflow_arg_area and
	// reg_save_area (symptom: the first %d prints, the rest are garbage). So
	// the widened slot of its own is kept.
	if !e.c.linux {
		if uid, ok := e.tr.lookupUID(id.Name); ok {
			if s, exists := e.slots[uid]; exists {
				e.vaSlots[id.Name] = s
				return s
			}
		}
	}
	slot := e.newTmp()
	e.entry.WriteString("  " + slot + " = alloca " + e.vaListTy() + ", align 8\n")
	e.vaSlots[id.Name] = slot
	return slot
}

func alignOfIr(ty string) int {
	if len(ty) > 1 && ty[0] == 'i' {
		n := 0
		for i := 1; i < len(ty); i++ {
			if ty[i] < '0' || ty[i] > '9' {
				return 8
			}
			n = n*10 + int(ty[i]-'0')
		}
		if n/8 < 1 {
			return 1
		}
		return n / 8
	}
	// NOTE: "float" is 5 characters and "double" is 6, so the length test has
	// to be >= or these two branches are dead and every float and double ends
	// up with align 1 -- which is why loads and stores of them came out
	// misaligned. (This mirrors alignOfLlir in llvmmod.go; both must agree.)
	if len(ty) >= 5 && ty[:5] == "float" {
		return 4
	}
	if len(ty) >= 6 && ty[:6] == "double" {
		return 8
	}
	if ty == "ptr" {
		return 8
	}
	if len(ty) > 1 && ty[0] == '[' {
		// An array's alignment is its element's.
		if i := strings.IndexByte(ty, ' '); i > 0 {
			return alignOfIr(ty[i+1:])
		}
	}
	return 1
}
