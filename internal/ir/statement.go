package gocl

// Statements and expressions, in LLVM IR.
//
// C's grammar and LLVM's are close but not aligned: a C statement list has no
// notion of basic blocks, while every LLVM block must end in a terminator. The
// translation therefore has to invent blocks wherever control can arrive from
// somewhere else, and it has to be careful that a block is never left open -- an
// unterminated block is rejected by LLVM with a message that points at the block
// rather than at the construct responsible.
//
// The one rule that keeps this manageable: every path out of a statement leaves
// the current block closed, so appending the next statement simply opens a fresh
// block. That is what e.line does when it sees e.closed.

import (
	"goc/frontend"
	"strings"
)

// --- statements -------------------------------------------------------------

func (e *irEmitter) stmt(s frontend.Stmt) {
	switch n := s.(type) {
	case nil:
		return
	case *frontend.Block:
		e.tr.pushScope()
		for _, x := range n.Stmts {
			e.stmt(x)
		}
		e.tr.popScope()
	case *frontend.DeclList:
		for _, d := range n.Decls {
			e.stmt(d)
		}
	case *frontend.DeclStmt:
		e.localDecl(n)
	case *frontend.ExprStmt:
		e.discard(n.E)
	case *frontend.ReturnStmt:
		e.doReturn(n)
	case *frontend.IfStmt:
		e.doIf(n)
	case *frontend.WhileStmt:
		e.doWhile(n)
	case *frontend.DoWhileStmt:
		e.doDoWhile(n)
	case *frontend.ForStmt:
		e.doFor(n)
	case *frontend.BreakStmt:
		if n := len(e.breakTo); n > 0 {
			e.term("br label %%%s", e.breakTo[n-1])
		}
	case *frontend.ContinueStmt:
		if n := len(e.continueTo); n > 0 {
			e.term("br label %%%s", e.continueTo[n-1])
		}
	case *frontend.LabelStmt:
		e.doLabel(n)
	case *frontend.GotoStmt:
		e.doGoto(n)
	case *frontend.SwitchStmt:
		e.doSwitch(n)
	case *frontend.AsmStmt:
		// Inline assembly is goa assembler text with no IR equivalent; the
		// eligibility check keeps such functions off this path.
	}
}

// vlaDecl lowers a C99 variable-length array declaration "T a[n];".
//
// The array's extent is a run-time value, so the object cannot be an alloca of
// a fixed array type -- which is what a declaration whose size is known gets.
// LLVM's alloca takes its element count as a VALUE ("alloca i32, i64 %n"),
// which is exactly C's semantics, and it yields a plain element pointer. Every
// other site -- decay, subscripting, pointer arithmetic -- already works in
// terms of opaque pointers and a separately computed element size, so handing
// them that pointer needs no change at all.
//
// The alloca is emitted where the declaration is, not in the entry block: the
// length expression can only be evaluated here. A VLA inside a loop body
// therefore allocates once per iteration, which is the same growth C accepts.
func (e *irEmitter) vlaDecl(d *frontend.DeclStmt, ty *frontend.Type, uid int) {
	e.tr.declUID[d] = uid
	count := e.indexValue(ty.VLALen)
	ety := e.ty(ty.Elem)
	slot := e.newTmp()
	e.line("%s = alloca %s, i64 %s, align %d", slot, ety, count, alignOfIr(ety))
	e.slots[uid] = slot
}

// localDecl gives a local its stack slot and runs the initialiser.
func (e *irEmitter) localDecl(d *frontend.DeclStmt) {
	if d.Name == "" {
		return // a bare "struct S;" declares nothing
	}
	ty := d.Typ
	if ty == nil {
		return
	}
	// An extern local declares no object: it names one defined elsewhere --
	// here, the file-scope global of the same name. Binding it to a fresh
	// alloca instead would shadow that global with an uninitialised stack
	// slot, so "extern int g; g = g + 100;" would read and write the slot and
	// leave the global untouched. Leaving the name unbound sends every later
	// reference down the global-symbol path, which is the object C means. The
	// assembly back end skips these the same way.
	if d.Storage == "extern" {
		return
	}
	// A static local is the one declaration whose storage outlives the
	// activation: it becomes a module global, initialised once, and the
	// name resolves to that symbol. Everything else is an alloca.
	//
	// A thread-local static local is excluded: per-thread storage comes
	// from the linker's .tls section, whose layout (ComputeTLSLayout)
	// walks file-scope declarations only, so a function-local one has no
	// offset to be reached through. It keeps the alloca it had before,
	// which is per-activation rather than per-thread -- wrong in theory,
	// but the section machinery to do better does not exist yet and a
	// silently shared module global would be wrong in a harder-to-see
	// way.
	if d.Storage == "static" && !d.IsTLS {
		e.staticLocalDecl(d, ty)
		return
	}
	uid := e.tr.declareVar(d.Name, varInfo{ty: ty})
	// A C99 variable-length array: its extent is not known until this
	// declaration actually runs, so it cannot be the entry-block alloca of a
	// fixed array type that an ordinary local gets.
	if ty.HasVLA() {
		e.vlaDecl(d, ty, uid)
		return
	}
	lty := e.ty(ty)
	slot := e.slotFor(uid, lty)
	e.tr.declUID[d] = uid
	if d.Init != nil {
		if bi, ok := d.Init.(*frontend.BraceInit); ok {
			e.storeBrace(bi, ty, slot)
			return
		}
		// A string initialising a char array copies the bytes; one
		// initialising a char pointer stores the address. goc's parser already
		// built the right node for each case, but they arrive here as the same
		// expression, so the target type decides.
		if sl, ok := d.Init.(*frontend.StrLit); ok && ty.Kind == frontend.KArr {
			e.storeString(sl, ty, slot)
			return
		}
		v := e.eval(d.Init)
		e.store(slot, e.coerce(v, ty))
	}
}

// staticLocalDecl lowers "static T x = init;" written inside a function.
//
// A static local has static storage duration: one object for the whole
// program, initialised once before main runs. That is not what an alloca
// gives -- an alloca is created on entry and its initialiser runs every
// time the declaration is reached, which is what made a static counter
// return 1 on every call. The IR instead holds the object as a module
// global carrying a constant initialiser, exactly like a file-scope
// global, and the name resolves to that symbol for the rest of the
// function.
//
// The label is minted per declaration SITE, not per visit: the parser
// hands back the same DeclStmt node every time the block runs, so a
// static inside a loop mints once and reuses afterwards. Two functions
// may each declare "static int s" -- the minted labels differ, so the
// two objects are distinct, which is what C requires and what a flat
// name-keyed table would have gotten wrong.
//
// The initialiser of a static must be a constant in C; one that is not
// is a constraint violation the front end does not diagnose, so a
// constInit that declines falls back to zeroinitializer -- the same
// object semantics a C compiler gives a tentative definition.
func (e *irEmitter) staticLocalDecl(d *frontend.DeclStmt, ty *frontend.Type) {
	if e.staticSites == nil {
		e.staticSites = map[*frontend.DeclStmt]string{}
	}
	label, minted := e.staticSites[d]
	if !minted {
		gty := ty
		if gty.Kind == frontend.KArr && gty.Len <= 0 && d.Init != nil {
			// An unsized array takes its length from the initialiser.
			// The checker does this sizing for every declaration it
			// walks, but a fragment parsed after Check ran -- the
			// injected entry stub's own __func__ -- arrives with Len
			// still 0, and llirType renders that placeholder as
			// [1 x i8] while the initialiser's bytes are the whole
			// function name: "constant expression type mismatch".
			// Size it here, from the initialiser, so the constant and
			// the type agree however the fragment reached us.
			if n, ok := inferredArrLen(d.Init, gty); ok {
				t2 := *gty
				t2.Len = n
				gty = &t2
			}
		}
		label = e.tr.mintStaticLabel(e.fname, d.Name)
		e.staticSites[d] = label
		// Register the type under the label so expression typing can
		// resolve the name through the same path a file-scope global
		// takes, and so the next mint sees this spelling as taken.
		e.tr.globalTyp[label] = gty
		init, ok := e.c.constInit(d.Init, gty)
		if !ok {
			init = "zeroinitializer"
		}
		e.c.globals = append(e.c.globals, irGlobal{
			name: "G_" + label, ty: e.ty(gty), init: init,
		})
		ty = gty
	}
	// Bind (or re-bind, on a later visit to the same block) the source
	// name to the module global. Re-binding matters: a block's scope is
	// popped when the block ends, so the second iteration of a loop
	// would otherwise find no binding for the name at all.
	//
	// glob holds the symbol exactly as IR spells it ("@G_st_..."), not
	// the bare label: every use site writes it straight into an operand
	// position, and a prefix assembled there would be one more place to
	// get wrong.
	e.tr.declareVar(d.Name, varInfo{ty: ty, glob: "@G_" + label})
}

// inferredArrLen reports the length an unsized array takes from its
// initialiser: a string literal counts its elements plus the
// terminator, a brace counts its top-level elements (the highest
// designated index plus one when designators are used). It mirrors the
// checker's own inference (check.go's checkBraceInit and the string
// case beside it) so a fragment that never reached the checker is sized
// the same way.
func inferredArrLen(init frontend.Expr, t *frontend.Type) (int, bool) {
	switch x := init.(type) {
	case *frontend.StrLit:
		w := 1
		if t.Elem != nil && t.Elem.Width > 1 {
			w = t.Elem.Width
		}
		return len(x.Bytes)/w + 1, true
	case *frontend.BraceInit:
		n := 0
		hasDesig := false
		for _, el := range x.Elems {
			if el.DesigIdx >= 0 {
				hasDesig = true
			}
		}
		if hasDesig {
			for _, el := range x.Elems {
				if el.DesigIdx >= 0 && el.DesigIdx+1 > n {
					n = el.DesigIdx + 1
				}
			}
		} else {
			n = len(x.Elems)
		}
		return n, true
	}
	return 0, false
}

// doReturn emits a return, converting the value to the function's return type.
func (e *irEmitter) doReturn(n *frontend.ReturnStmt) {
	if n.E == nil || e.retTy == "void" {
		e.term("ret void")
		return
	}
	v := e.eval(n.E)
	// The value's own IR type decides, not the static one: a comparison or a
	// short-circuit && / || produces an i1 even though C types it as int, so
	// "static type == return type" can hold while the value is still i1 and
	// `ret i32 %v` is rejected.
	if e.ty(v.ty) != e.retTy {
		// A long double return value reaching an integer return type must take
		// the runtime entry point that matches the return type's signedness:
		// "return (unsigned long long)ld;" saturates at ULLONG_MAX, not
		// LLONG_MAX. convertTo cannot see that signedness (it only has the IR
		// spelling), so route the long double -> int case here with retType.
		if isLongDouble(v.ty) && e.retType != nil {
			v.op = e.tfFromLDTo(v.op, e.retTy, e.retType != nil && !e.retType.Signed)
		} else {
			v.op = e.convertTo(v.op, v.ty, e.retTy)
		}
	}
	e.term("ret %s %s", e.retTy, v.op)
}

// doIf lowers `if (c) A else B`. The condition is materialised into a
// temporary first so the comparison is not repeated in each arm.
func (e *irEmitter) doIf(n *frontend.IfStmt) {
	cond := e.cond(n.Cond)
	thenL := e.newLabel()
	doneL := e.newLabel()
	var elseL string
	if n.Else != nil {
		elseL = e.newLabel()
	}
	e.term("br i1 %s, label %%%s, label %%%s", cond, thenL, pick(elseL, doneL))

	e.blockLabel(thenL)
	e.flushPendingLabels()
	e.stmt(n.Then)
	e.termOpen("br label %%%s", doneL)

	if n.Else != nil {
		e.blockLabel(elseL)
		e.flushPendingLabels()
		e.stmt(n.Else)
		e.termOpen("br label %%%s", doneL)
	}
	e.blockLabel(doneL)
	e.flushPendingLabels()
}

// stackSave emits llvm.stacksave and returns its token. Together with
// stackRestore it is the IR-level twin of goc's per-loop rsp checkpoint: the
// token marks the stack pointer, and a restore rewinds to it, unwinding every
// alloca made since -- including a VLA whose block-exit cleanup a break or
// continue jumps over.
func (e *irEmitter) stackSave() string {
	e.c.noteIntrinsic("llvm.stacksave", "ptr", nil)
	v := e.newTmp()
	e.line("%s = call ptr @llvm.stacksave()", v)
	return v
}

// stackRewind rewinds the stack to a token from stackSave. The spelling is
// stackRestore (llvm's name), the verb here distinguishes it from the label
// binding helpers.
func (e *irEmitter) stackRewind(tok string) {
	if tok == "" {
		return
	}
	e.c.noteIntrinsic("llvm.stackrestore", "void", []string{"ptr"})
	e.line("call void @llvm.stackrestore(ptr %s)", tok)
}

// stmtHasVLA reports whether executing s can allocate a VLA (a declaration
// anywhere inside it). Used for the for-statement's initialiser, whose VLA
// must outlive the loop and is therefore rewound only at the exit.
func stmtHasVLA(s frontend.Stmt) bool {
	found := false
	frontend.WalkStmts(s, func(x frontend.Stmt) {
		if d, ok := x.(*frontend.DeclStmt); ok && d.Typ != nil && d.Typ.HasVLA() {
			found = true
		}
	})
	return found
}

// doWhile lowers `while (c) B`, testing before each iteration.
func (e *irEmitter) doWhile(n *frontend.WhileStmt) {
	testL := e.newLabel()
	bodyL := e.newLabel()
	doneL := e.newLabel()
	// The token is taken before the loop is entered and rewound at the top of
	// every test and at the exit, so each iteration starts from the stack
	// pointer the loop found -- and a break from deep inside the body cannot
	// carry a stale, deeper rsp past the loop.
	tok := ""
	if e.fnHasVLA {
		tok = e.stackSave()
	}
	e.term("br label %%%s", testL)

	e.blockLabel(testL)
	e.flushPendingLabels()
	e.stackRewind(tok)
	cond := e.cond(n.Cond)
	e.term("br i1 %s, label %%%s, label %%%s", cond, bodyL, doneL)

	e.blockLabel(bodyL)
	e.breakTo = append(e.breakTo, doneL)
	e.continueTo = append(e.continueTo, testL)
	e.stmt(n.Body)
	e.breakTo = e.breakTo[:len(e.breakTo)-1]
	e.continueTo = e.continueTo[:len(e.continueTo)-1]
	e.termOpen("br label %%%s", testL)

	e.blockLabel(doneL)
	e.flushPendingLabels()
	e.stackRewind(tok)
}

// doDoWhile lowers `do B while (c)`, running the body before the first test and
// sending continue to the condition rather than to the top of the body.
func (e *irEmitter) doDoWhile(n *frontend.DoWhileStmt) {
	bodyL := e.newLabel()
	testL := e.newLabel()
	doneL := e.newLabel()
	tok := ""
	if e.fnHasVLA {
		tok = e.stackSave()
	}
	e.term("br label %%%s", bodyL)

	e.blockLabel(bodyL)
	e.breakTo = append(e.breakTo, doneL)
	e.continueTo = append(e.continueTo, testL)
	e.stmt(n.Body)
	e.breakTo = e.breakTo[:len(e.breakTo)-1]
	e.continueTo = e.continueTo[:len(e.continueTo)-1]
	e.termOpen("br label %%%s", testL)

	e.blockLabel(testL)
	e.flushPendingLabels()
	// Rewind before the condition: the body's declarations are out of scope
	// there (their block has ended), and continue arrives at this test from
	// mid-body with the stack pointer wherever the body left it.
	e.stackRewind(tok)
	cond := e.cond(n.Cond)
	e.term("br i1 %s, label %%%s, label %%%s", cond, bodyL, doneL)

	e.blockLabel(doneL)
	e.flushPendingLabels()
	e.stackRewind(tok)
}

// doFor lowers the three-clause `for`. The initialiser runs once, the condition
// is tested before each iteration, and the post expression runs at the bottom --
// so `continue` has to reach the post expression, not the top.
func (e *irEmitter) doFor(n *frontend.ForStmt) {
	e.tr.pushScope()
	defer e.tr.popScope()
	// The initialiser's own VLA must survive every iteration (the body may
	// legitimately index it), so it gets a token of its own taken BEFORE it
	// runs and rewound only at the exit -- after the loop token, which sits
	// below it. The loop token itself goes after the initialiser for the same
	// reason: rewinding above a live VLA would let the body's allocations
	// overwrite it.
	initTok := ""
	if n.Init != nil && stmtHasVLA(n.Init) {
		initTok = e.stackSave()
	}
	if n.Init != nil {
		e.stmt(n.Init)
	}
	tok := ""
	if e.fnHasVLA {
		tok = e.stackSave()
	}
	testL := e.newLabel()
	bodyL := e.newLabel()
	postL := e.newLabel()
	doneL := e.newLabel()
	e.term("br label %%%s", testL)

	e.blockLabel(testL)
	e.flushPendingLabels()
	e.stackRewind(tok)
	if n.Cond != nil {
		cond := e.cond(n.Cond)
		e.term("br i1 %s, label %%%s, label %%%s", cond, bodyL, doneL)
	} else {
		e.term("br label %%%s", bodyL)
	}

	e.blockLabel(bodyL)
	e.breakTo = append(e.breakTo, doneL)
	e.continueTo = append(e.continueTo, postL)
	e.stmt(n.Body)
	e.breakTo = e.breakTo[:len(e.breakTo)-1]
	e.continueTo = e.continueTo[:len(e.continueTo)-1]
	e.termOpen("br label %%%s", postL)

	e.blockLabel(postL)
	e.flushPendingLabels()
	if n.Post != nil {
		e.discard(n.Post)
	}
	e.term("br label %%%s", testL)

	e.blockLabel(doneL)
	e.flushPendingLabels()
	e.stackRewind(tok)
	e.stackRewind(initTok)
}

// doSwitch lowers a switch. C allows fall-through, and keeping the case labels
// as ordinary blocks preserves that for free: control simply runs on into the
// next one.
func (e *irEmitter) doSwitch(n *frontend.SwitchStmt) {
	sv := e.rvalue(n.Src)
	sty := e.tr.exprType(n.Src)
	// LLVM's switch takes an i32. A conversion is needed only when the
	// controlling expression is not already one: "trunc i32 %v to i32" is a
	// cast from a type to itself, which LLVM rejects, and a trunc is wrong in
	// the other direction too -- a char (i8) must be zero-extended, not
	// truncated. convert picks the right one from the two widths. The i32 case
	// uses the operand directly; a copy would need a real instruction, and a
	// bare "%t = %t" is not one.
	if e.ty(sty) != "i32" {
		sv = e.convert(sv, sty, frontend.IntType())
	}

	doneL := e.newLabel()
	// Collect the cases first: LLVM's switch needs every target up front, but
	// the blocks are emitted as the body is walked.
	var cases []switchCase
	defIdx := -1
	cur := -1
	e.tr.pushScope()
	for _, st := range n.Body.Stmts {
		switch c := st.(type) {
		case *frontend.CaseStmt:
			// A case label applies to the statements that follow it, up to the
			// next case or default. The block it opens is created here so the
			// switch can name it before the body has been walked.
			cases = append(cases, switchCase{val: c.Val, l: e.newLabel()})
			// An index, not a pointer: append may move the backing array, and a
			// pointer taken before the next append would then name the wrong
			// case -- which showed up as a default arm's statements being
			// emitted under another arm's label.
			cur = len(cases) - 1
		case *frontend.DefaultStmt:
			// A default arm is a branch target like any other, and it keeps
			// its own statements: they belong to the default block, not to
			// whichever case happens to precede it.
			cases = append(cases, switchCase{val: -1, l: e.newLabel()})
			defIdx = len(cases) - 1
			cur = defIdx
		default:
			if cur < 0 {
				continue // statements before any label are unreachable
			}
			if cases[cur].stmt == nil {
				cases[cur].stmt = st
			} else {
				cases[cur].stmt = &frontend.Block{Stmts: []frontend.Stmt{cases[cur].stmt, st}}
			}
		}
	}
	e.tr.popScope()

	var real []switchCase
	for _, c := range cases {
		if c.val >= 0 {
			real = append(real, c)
		}
	}
	def := ""
	if defIdx >= 0 {
		def = cases[defIdx].l
	}

	e.term("switch i32 %s, label %%%s [ %s ]", sv, pick(def, doneL), caseArms(real))

	// `break` inside a switch leaves the switch, not an enclosing loop, so
	// doneL has to be the top of the break stack while the arms are emitted.
	// It never was, which made a break in a switch jump to the enclosing
	// while/for instead -- and to nothing at all when there was no enclosing
	// loop, so the arm merely fell through into the next one.
	//
	// strftime is where it showed: its format loop is a while around a switch,
	// so `%Y` broke out of the loop and every conversion after the first was
	// dropped -- "%Y-%m-%d" formatted as "2025". `%F` looked correct only
	// because it writes the whole date inside one arm. `continue` is left
	// alone on purpose: in C it belongs to the loop, not to the switch, so a
	// switch must not shadow it.
	e.breakTo = append(e.breakTo, doneL)
	for i := range cases {
		c := &cases[i]
		e.blockLabel(c.l)
		if c.stmt != nil {
			e.stmt(c.stmt)
		}
		// An arm that did not end in a jump falls through into the next
		// one, exactly as C says: "case 3: ... case 4: ..." runs both, and
		// a default in the middle runs on into the case that follows it.
		// Branching every arm straight to doneL instead -- which this did
		// -- turned each arm into an implicit break, so switch(3) over
		// "case 3: n += 100; case 4: n += 1000;" answered 100 rather than
		// 1100, and a mid-switch default answered 20 rather than 22.
		//
		// e.closed is true exactly when the statements above ended the
		// block themselves -- a break, return, goto or continue -- and a
		// terminator may only close a block once, so those arms are left
		// alone. The last arm has nothing to fall into and leaves.
		if !e.closed {
			if i+1 < len(cases) {
				e.term("br label %%%s", cases[i+1].l)
			} else {
				e.term("br label %%%s", doneL)
			}
		}
	}
	e.breakTo = e.breakTo[:len(e.breakTo)-1]
	e.blockLabel(doneL)
	e.flushPendingLabels()
}

// switchCase pairs a case value with the block that handles it and the
// statements that follow it in the source.
type switchCase struct {
	val  int
	l    string
	stmt frontend.Stmt
}

func caseArms(cases []switchCase) string {
	if len(cases) == 0 {
		return ""
	}
	var b strings.Builder
	for i, c := range cases {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString("i32 " + itoa(c.val) + ", label %" + c.l)
	}
	return b.String()
}

// doLabel attaches a C label to the block control lands in when it arrives
// there. A label can sit in front of a statement that continues the block
// already open ("x = 1; L: x = 2;") or start a fresh one after a jump
// ("goto L; ... L: x = 1;"), so the binding is made here rather than deferred:
// whichever block the following statement is emitted into is the one the label
// names, and a `goto` that already branched to it can then be resolved.
func (e *irEmitter) doLabel(n *frontend.LabelStmt) {
	// A C label always opens a block of its own. Binding it to the block already
	// being written would make it share that block's predecessors -- and a block
	// reached by `goto` from anywhere else in the function arrives along an edge
	// that no phi in it accounts for, which LLVM's verifier rejects ("PHINode
	// should have one entry for each predecessor"). Branching the open block to
	// the new one keeps the fall-through path.
	if e.closed {
		e.blockLabel(e.newLabel())
	} else {
		nl := e.newLabel()
		e.term("br label %%%s", nl)
		e.blockLabel(nl)
	}
	// A label chain ("A: B: stmt") binds every name to the same block.
	e.flushPendingLabels()
	e.userLabels[n.Name] = e.currentBlock("entry")
	e.stmt(n.Stmt)
}

// flushPendingLabels binds any C labels queued by doLabel to the current block,
// which is where control must arrive.
func (e *irEmitter) flushPendingLabels() {
	if len(e.pendingLabels) == 0 {
		return
	}
	l := e.currentBlock("entry")
	for _, cl := range e.pendingLabels {
		e.userLabels[cl] = l
	}
	e.pendingLabels = nil
}

// currentLabel returns the name of the block being written, which is the last
// label emitted.
func (e *irEmitter) currentLabel() string {
	// The emitter writes labels as it goes; the most recent one is the block
	// currently open. Tracking it explicitly is cheaper than re-parsing.
	return e.lastLabel
}

func pick(alt, def string) string {
	if alt != "" {
		return alt
	}
	return def
}
