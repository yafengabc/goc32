package frontend

import "fmt"

// checker implements the front-end type system: it resolves identifiers
// against a scope stack, computes the type of every expression, and enforces
// the rules a real C front end would (no undeclared names, argument/return
// compatibility, lvalues for & / * / [], pointer element compatibility, ...).
//
// It is deliberately permissive about numeric conversions (any integer-family
// value mixes with any other, and with double) because the code generator uses
// an 8-byte slot for every scalar. The checks that matter for catching real
// mistakes -- scoping, call signatures, pointer element types, lvalues -- are
// strict.

type cScope struct {
	vars map[string]*Type
}

type checker struct {
	funcs   map[string]*FuncDecl // function definitions
	protos  map[string]*FuncDecl // forward declarations from headers
	scopes  []*cScope
	errs    []error
	swDepth int // how many switch statements enclose the statement being checked
	// unit marks a single translation unit being checked on its way to an object
	// file, where the program as a whole has no main yet. See CheckUnit.
	unit bool
}

func (c *checker) push() { c.scopes = append(c.scopes, &cScope{vars: map[string]*Type{}}) }
func (c *checker) pop()  { c.scopes = c.scopes[:len(c.scopes)-1] }

func (c *checker) put(name string, t *Type, line int) {
	top := c.scopes[len(c.scopes)-1]
	prev, dup := top.vars[name]
	if dup {
		// At file (global) scope a declaration may legitimately be followed
		// by a definition or another compatible declaration of the same
		// object: C 6.9.2 tentative-definition rules allow an
		//   extern T x;            // declaration in a header
		//   T x = ...;             // definition in the .c file
		// pairing, which refers to one object. Inside a function such a
		// duplicate is always a genuine redefinition.
		if len(c.scopes) > 1 || !typesEqual(prev, t) {
			c.errf(line, "redefinition of %q in the same scope", name)
		}
		return
	}
	top.vars[name] = t
}

func (c *checker) lookup(name string) *Type {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if t, ok := c.scopes[i].vars[name]; ok {
			return t
		}
	}
	return nil
}

func (c *checker) errf(line int, format string, a ...any) {
	if line > 0 {
		c.errs = append(c.errs, fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, a...)))
	} else {
		c.errs = append(c.errs, fmt.Errorf(format, a...))
	}
}

// Check walks the whole program and returns every diagnostic found. A normal
// (successful) run returns a nil or empty slice.
func Check(prog *Program) []error { return check(prog, false) }

// CheckUnit is Check for a single translation unit on its way to becoming an
// object file.
//
// It differs in exactly one rule: a missing main is not an error. A whole
// program must define one, but a translation unit is one piece of several, and
// the piece that defines main is rarely the piece that defines the functions
// this one calls. Rejecting a.c for having no main would make it impossible to
// compile a program whose main.c is a separate file -- which is the ordinary
// shape of a C program, not an unusual one.
//
// The entry point is the linker's to settle, and it can only settle it after
// every unit has been seen. Reporting it here would report it too early.
func CheckUnit(prog *Program) []error { return check(prog, true) }

func check(prog *Program, unit bool) []error {
	c := &checker{
		funcs:  map[string]*FuncDecl{},
		protos: map[string]*FuncDecl{},
		unit:   unit,
	}
	for _, f := range prog.Funcs {
		c.funcs[f.Name] = f
	}
	for _, f := range prog.Prototypes {
		// A later prototype overrides an earlier one; a definition (in
		// c.funcs) always wins because checkCall looks there first.
		if _, ok := c.funcs[f.Name]; !ok {
			c.protos[f.Name] = f
		}
	}
	// A persistent global scope holds the top-level variables, visible to every
	// function. It is never popped.
	c.scopes = append(c.scopes, &cScope{vars: map[string]*Type{}})
	for _, g := range prog.Globals {
		c.checkStmt(g, nil)
	}
	for _, f := range prog.Funcs {
		if f.Ret.IsArray() || f.Ret.IsFunc() {
			c.errf(0, "function %q cannot return %s", f.Name, f.Ret)
		}
		for i, pt := range f.ParamTypes {
			if pt.IsArray() || pt.IsFunc() {
				c.errf(0, "parameter %d of %q has invalid type %s", i+1, f.Name, pt)
			}
		}
		c.push()
		for i, p := range f.Params {
			c.put(p, f.ParamTypes[i], 0)
		}
		c.checkBlock(f.Body, f)
		c.checkLabels(f.Body)
		c.pop()
	}
	// The entry point: main, or one of the two MSVC GUI entries (a
	// /SUBSYSTEM:WINDOWS program has no main at all). Check does not know the
	// target, so it accepts all three and codegen rejects a GUI entry when
	// targeting Linux, where the entry really must be main.
	//
	// A translation unit on its way to an object file is exempt: main belongs to
	// some unit in the program, not necessarily this one, and the linker is the
	// stage that can tell whether the program has one at all.
	if _, ok := c.funcs["main"]; !ok {
		_, wide := c.funcs["wWinMain"]
		_, ansi := c.funcs["WinMain"]
		if !wide && !ansi && !c.unit {
			c.errf(0, "program has no main()")
		}
	}
	return c.errs
}

func (c *checker) checkBlock(b *Block, fn *FuncDecl) {
	c.push()
	for _, st := range b.Stmts {
		c.checkStmt(st, fn)
	}
	c.pop()
}

func (c *checker) checkStmt(st Stmt, fn *FuncDecl) {
	switch n := st.(type) {
	case *DeclStmt:
		// C11/C23 6.7.1 constraint: _Thread_local at block scope must also carry
		// static or extern. A plain "_Thread_local int x;" inside a function is
		// ill-formed (gcc rejects it: 'function-scope ... implicitly auto'). Without
		// this check the variable is laid out neither on the frame nor in the TLS
		// section, so codegen later nil-derefs vi.typ (IsArray panic at codegen.go
		// ~4785). Reject here with a clean diagnostic instead of crashing.
		if n.IsTLS && fn != nil && n.Storage != "static" && n.Storage != "extern" {
			c.errf(n.Line, "_Thread_local variable %q at block scope must be static or extern", n.Name)
		}
		// C99 variable-length array constraints, checked once here so both
		// back ends report them before any code is generated. Without these,
		// gocl silently dropped the initialiser of "int a[n] = {...}", emitted
		// a crashing binary for the second-dimension form "int a[3][n]"
		// (a constant stride against a run-time element size), and accepted a
		// file-scope "int a[n];" whose length expression has no run-time to
		// evaluate. gcc rejects all three.
		if n.Typ != nil && n.Typ.HasVLA() {
			if fn == nil {
				// File scope: there is no run-time point at which the length
				// expression could be evaluated (gcc: "variably modified ...").
				c.errf(n.Line, "variably modified %q at file scope", n.Name)
				c.put(n.Name, n.Typ, n.Line)
				return
			}
			if !n.Typ.IsVLA() || n.Typ.Elem.HasVLA() {
				// "int a[3][n]" / "int a[n][m]": the element stride is a
				// run-time value, which no back end supports.
				c.errf(n.Line, "only a single variable-length dimension is supported (%s)", n.Typ)
				c.put(n.Name, n.Typ, n.Line)
				return
			}
			if n.Init != nil {
				// C11 6.7.9p3: no initialiser for a variable-length array.
				c.errf(n.Line, "a variable-length array cannot have an initialiser")
				c.put(n.Name, n.Typ, n.Line)
				return
			}
		}
		// ("auto x = expr;"). The declared type is the type of the initialiser
		// after lvalue/array-to-pointer/function-to-pointer decay -- exactly
		// what checkExpr returns -- or the type of the single element of the
		// braced form "auto x = { expr };" (C23 6.7.9). Deduction checks the
		// initialiser, so nothing further is validated here.
		if n.Typ != nil && n.Typ.AutoDeduce {
			ph := n.Typ
			if n.Init == nil {
				c.errf(n.Line, "auto declaration of %q requires an initialiser", n.Name)
				n.Typ = IntType()
			} else {
				n.Typ = c.deduceAutoType(n.Init, fn, n.Line)
			}
			// Qualifiers on the placeholder ("const auto x = 5;") carry over
			// to the deduced type (C23 6.7.9). Stamp a clone: the deduced type
			// may be a pointer to another variable's stored type, which must
			// not be mutated in place.
			if ph.Const && !n.Typ.Const {
				t2 := *n.Typ
				t2.Const = true
				n.Typ = &t2
			}
			c.put(n.Name, n.Typ, n.Line)
			return
		}
		// The name is in scope inside its own initialiser: C 6.2.1p7 puts the
		// scope of an identifier at the end of its declarator, so the
		// "self-referential sizeof" idiom -- T x = { sizeof(x), ... } -- is
		// legal and is how every Win32 struct with a dwSize field is filled in
		// (INITCOMMONCONTROLSEX, BITMAPINFOHEADER, ...). put is a pointer store
		// into the scope, so the array-length inference below, which mutates
		// n.Typ in place, is still visible through it.
		c.put(n.Name, n.Typ, n.Line)
		// Initialiser forms, in order of specificity: a string literal for a
		// char array (the one array initialiser C allows outside braces), a
		// braced initialiser for any aggregate (or scalar), and everything
		// else through the normal expression/assignable path.
		strInit, braceInit := false, false
		if n.Typ.IsArray() && n.Init != nil {
			if sl, ok := n.Init.(*StrLit); ok {
				if sl.Wide && n.Typ.Elem.Width == 2 {
					// L"..." initialises a wchar_t[] array (UTF-16 elements).
					strInit = true
					need := len(sl.Bytes)/2 + 1
					if n.Typ.Len == 0 {
						n.Typ.Len = need
					} else if n.Typ.Len < need {
						c.errf(n.Line, "initialiser wide string of %d elements does not fit in wchar_t array %q of %d elements",
							len(sl.Bytes)/2, n.Name, n.Typ.Len)
					}
				} else if !sl.Wide && n.Typ.Elem.IsChar() {
					strInit = true
					need := len(sl.Bytes) + 1
					if n.Typ.Len == 0 {
						n.Typ.Len = need
					} else if n.Typ.Len < need {
						c.errf(n.Line, "initialiser string of length %d does not fit in char array %q of %d bytes",
							len(sl.Bytes), n.Name, n.Typ.Len)
					}
				}
			} else if _, ok := n.Init.(*BraceInit); ok {
				braceInit = true
				c.checkBraceInit(n.Typ, n.Init.(*BraceInit), fn, n.Line)
			} else {
				c.errf(n.Line, "array %q cannot be initialised here (use memset / a loop)", n.Name)
			}
		} else if bi, ok := n.Init.(*BraceInit); ok {
			braceInit = true
			c.checkBraceInit(n.Typ, bi, fn, n.Line)
		}
		if n.Init != nil && !strInit && !braceInit {
			t := c.checkExpr(n.Init, fn)
			// A string literal "decays" to char*, which is not assignable to
			// the char-array type proper -- but it is the sanctioned form of
			// array initialisation, so the mismatch is not an error here.
			if !assignable(n.Typ, t) && !strInit {
				c.errf(n.Line, "initialiser of type %s is not assignable to %s", t, n.Typ)
			}
		}
	case *DeclList:
		for _, d := range n.Decls {
			c.checkStmt(d, fn)
		}
	case *AssignStmt:
		lt, ok := c.checkLValue(n.Lhs, fn)
		var line int
		if id, ok2 := n.Lhs.(*Ident); ok2 {
			line = id.Line
		}
		if ok {
			rt := c.checkExpr(n.Rhs, fn)
			if !assignable(lt, rt) {
				c.errf(line, "cannot assign %s to %s", rt, lt)
			}
		}
	case *ExprStmt:
		c.checkExpr(n.E, fn)
	case *ReturnStmt:
		if n.E != nil {
			t := c.checkExpr(n.E, fn)
			if fn.Ret.IsVoid() {
				c.errf(0, "returning a value from void function %q", fn.Name)
			} else if !assignable(fn.Ret, t) {
				c.errf(0, "returning %s from %q which returns %s", t, fn.Name, fn.Ret)
			}
		} else if !fn.Ret.IsVoid() {
			c.errf(0, "function %q returning %s must return a value", fn.Name, fn.Ret)
		}
	case *IfStmt:
		c.checkExpr(n.Cond, fn)
		c.checkStmt(n.Then, fn)
		if n.Else != nil {
			c.checkStmt(n.Else, fn)
		}
	case *WhileStmt:
		c.checkExpr(n.Cond, fn)
		c.checkStmt(n.Body, fn)
	case *ForStmt:
		c.push()
		if n.Init != nil {
			c.checkStmt(n.Init, fn)
		}
		if n.Cond != nil {
			c.checkExpr(n.Cond, fn)
		}
		if n.Body != nil {
			c.checkStmt(n.Body, fn)
		}
		if n.Post != nil {
			c.checkExpr(n.Post, fn)
		}
		c.pop()
	case *DoWhileStmt:
		c.checkExpr(n.Cond, fn)
		if n.Body != nil {
			c.checkStmt(n.Body, fn)
		}
	case *SwitchStmt:
		st := c.checkExpr(n.Src, fn)
		if st != nil && st.Kind != KInt && !st.IsPtr() {
			c.errf(n.Line, "switch quantity must be an integer, not %s", st)
		}
		c.swDepth++
		c.checkSwitchBody(n, fn)
		c.swDepth--
	case *CaseStmt:
		if c.swDepth == 0 {
			c.errf(n.Line, "case label outside a switch")
		}
	case *DefaultStmt:
		if c.swDepth == 0 {
			c.errf(n.Line, "default label outside a switch")
		}
	case *GotoStmt:
		// The target is validated once the whole function body is known
		// (see checkLabels), because goto may jump forward.
	case *AsmStmt:
		// Inline assembly: the body is raw assembler text, not C, so there
		// is nothing to type-check here. Variable names it refers to are
		// bound to frame slots by the code generator (unknown identifiers
		// are left alone and reported by goa as undefined symbols). The
		// block cannot jump out of the function on its own, and any
		// control-flow labels it defines are assembler-local, so no scope
		// bookkeeping is needed either.
	case *LabelStmt:
		if n.Stmt != nil {
			c.checkStmt(n.Stmt, fn)
		}
	case *BreakStmt:
		// no type effect
	case *ContinueStmt:
		// no type effect
	case *Block:
		c.checkBlock(n, fn)
	}
}

// checkSwitchBody checks a switch body as a block, then enforces the rules C
// puts on its labels: case values must be unique and there may be at most one
// default. Statements before the first label are unreachable (C ignores them);
// they are still checked, so an undeclared name there is reported.
func (c *checker) checkSwitchBody(n *SwitchStmt, fn *FuncDecl) {
	seen := map[int]bool{}
	defaults := 0
	for _, st := range n.Body.Stmts {
		switch cs := st.(type) {
		case *CaseStmt:
			if seen[cs.Val] {
				c.errf(cs.Line, "duplicate case value %d in switch", cs.Val)
			}
			seen[cs.Val] = true
			continue
		case *DefaultStmt:
			defaults++
			if defaults > 1 {
				c.errf(cs.Line, "more than one default label in switch")
			}
			continue
		}
		c.checkStmt(st, fn)
	}
}

// checkLabels verifies that every goto in a function targets a label defined in
// that same function. C labels are function-scoped, so the whole body must be
// collected before any goto can be validated.
func (c *checker) checkLabels(b *Block) {
	if b == nil {
		return
	}
	defined := map[string]bool{}
	walkStmts(b, func(s Stmt) {
		if lab, ok := s.(*LabelStmt); ok {
			defined[lab.Name] = true
		}
	})
	walkStmts(b, func(s Stmt) {
		g, ok := s.(*GotoStmt)
		if !ok {
			return
		}
		if !defined[g.Label] {
			c.errf(g.Line, "goto %q: no such label in this function", g.Label)
		}
	})
}

// walkStmts visits every statement in a tree, including those nested inside
// if/loop/switch bodies and labelled statements. Expression trees are not
// visited: they contain no statements in goc's C subset.
func walkStmts(s Stmt, visit func(Stmt)) {
	if s == nil {
		return
	}
	visit(s)
	switch n := s.(type) {
	case *Block:
		for _, st := range n.Stmts {
			walkStmts(st, visit)
		}
	case *DeclList:
		for _, d := range n.Decls {
			walkStmts(d, visit)
		}
	case *IfStmt:
		walkStmts(n.Then, visit)
		walkStmts(n.Else, visit)
	case *WhileStmt:
		walkStmts(n.Body, visit)
	case *ForStmt:
		walkStmts(n.Init, visit)
		walkStmts(n.Body, visit)
	case *DoWhileStmt:
		walkStmts(n.Body, visit)
	case *SwitchStmt:
		walkStmts(n.Body, visit)
	case *LabelStmt:
		walkStmts(n.Stmt, visit)
	}
}

// checkGeneric resolves a _Generic selection at compile time (C11 6.5.1.1 /
// C23 6.5.1.1). The controlling expression is type-checked but never
// evaluated; its type after lvalue conversion -- checkExpr already applies
// array-to-pointer and function-to-pointer decay, and top-level qualifiers
// are dropped here -- selects the association with an exactly matching type,
// falling back to the (single) default. Only the selected branch is
// type-checked: unselected branches are syntax, not semantics.
func (c *checker) checkGeneric(n *GenericExpr, fn *FuncDecl) *Type {
	ct := c.checkExpr(n.Control, fn)
	if ct != nil && ct.Const {
		t2 := *ct
		t2.Const = false
		ct = &t2
	}
	defaultIdx := -1
	chosen := -1
	for i := range n.Assocs {
		a := &n.Assocs[i]
		if a.IsDefault {
			if defaultIdx >= 0 {
				c.errf(n.Line, "more than one default association in _Generic")
			}
			defaultIdx = i
			continue
		}
		for j := 0; j < i; j++ {
			if !n.Assocs[j].IsDefault && genericTypeMatch(n.Assocs[j].Typ, a.Typ) {
				c.errf(n.Line, "type %s appears twice in the _Generic association list", a.Typ)
			}
		}
		if chosen < 0 && genericTypeMatch(a.Typ, ct) {
			chosen = i
		}
	}
	if chosen < 0 {
		if defaultIdx >= 0 {
			chosen = defaultIdx
		} else {
			c.errf(n.Line, "controlling expression of type %s matches no _Generic association and there is no default", ct)
			return IntType()
		}
	}
	n.ChosenIdx = chosen
	n.Chosen = n.Assocs[chosen].E
	return c.checkExpr(n.Chosen, fn)
}

// genericTypeMatch reports whether a controlling expression of type ctl
// selects an association of type assoc under _Generic's exact-match rule.
// Unlike typesEqual (which treats every integer kind as interchangeable --
// fine for assignments, where the 8-byte slot absorbs the width), _Generic
// distinguishes int from long long from unsigned, and int* from char*: the
// whole point of the feature is type-dependent selection, so the comparison
// recurses with exact integer width/signedness.
func genericTypeMatch(assoc, ctl *Type) bool {
	if assoc == nil || ctl == nil {
		return false
	}
	// Qualification is part of type compatibility (C11 6.7.3p11), so it is
	// compared at every level. checkGeneric has already dropped the
	// controlling expression's *top-level* qualifier (lvalue conversion), so
	// a "const int" association never swallows a plain int controlling
	// expression -- but "const char*" still differs from "char*", because
	// there the const belongs to the pointee, not to the expression.
	if assoc.Const != ctl.Const {
		return false
	}
	if assoc.Kind != ctl.Kind {
		return false
	}
	switch assoc.Kind {
	case KInt:
		// char, signed char and unsigned char are three distinct types
		// (C11 6.2.5p15), so a bare "char" association must not swallow a
		// "signed char" controlling expression even on an implementation
		// where char is signed. gcc agrees; the fall-through to default is
		// what makes the usual "#if defined(__CHAR_IS_SIGNED__)" idiom work.
		return assoc.Width == ctl.Width && assoc.Signed == ctl.Signed &&
			assoc.PlainChar == ctl.PlainChar && assoc.LongLong == ctl.LongLong
		// double / float / long double need no case here: they are three
		// distinct Kinds, so the Kind comparison above already separates
		// them (C11 6.2.5p10).
	case KPtr:
		return genericTypeMatch(assoc.Elem, ctl.Elem)
	case KArr:
		return assoc.Len == ctl.Len && genericTypeMatch(assoc.Elem, ctl.Elem)
	case KFunc:
		if !genericTypeMatch(assoc.Ret, ctl.Ret) || len(assoc.Params) != len(ctl.Params) {
			return false
		}
		for i := range assoc.Params {
			if !genericTypeMatch(assoc.Params[i], ctl.Params[i]) {
				return false
			}
		}
		return true
	}
	// double/float/bool are distinct kinds already; struct/union compare by
	// tag (and anonymous struct members rarely appear in associations).
	return typesEqual(assoc, ctl)
}

// deduceAutoType implements C23 6.7.9 type inference for "auto": the declared
// type is the type of the initialiser expression (checkExpr already applies
// array-to-pointer / function-to-pointer decay), or of the single element of
// the braced form. Integer literals keep their suffix width and signedness
// ("auto x = 1LL;" really is long long), mirroring typeof(constant).
func (c *checker) deduceAutoType(init Expr, fn *FuncDecl, line int) *Type {
	if bi, ok := init.(*BraceInit); ok {
		if len(bi.Elems) != 1 {
			c.errf(line, "auto deduction from a braced initialiser requires exactly one element")
			return IntType()
		}
		el := bi.Elems[0]
		if el.Desig != "" || el.DesigIdx >= 0 {
			c.errf(line, "designators are not allowed in an auto initialiser")
			return IntType()
		}
		if _, nested := el.E.(*BraceInit); nested {
			c.errf(line, "auto deduction does not support nested brace initialisers")
			return IntType()
		}
		return c.deduceAutoType(el.E, fn, line)
	}
	if nl, ok := init.(*NumLit); ok && nl.Kind != TDouble {
		w := 4
		if nl.Long {
			w = 8
		}
		return &Type{Kind: KInt, Width: w, Signed: !nl.Unsig}
	}
	return c.checkExpr(init, fn)
}

// checkLValue returns the type of e and whether e is a modifiable lvalue.
func (c *checker) checkLValue(e Expr, fn *FuncDecl) (*Type, bool) {
	return c.checkLValueAddr(e, fn, false)
}

// checkAddrOperand validates the operand of unary "&" and returns its type.
//
// C11 6.5.3.2p1 asks for an lvalue, but deliberately NOT a *modifiable* one:
// "&const_object" is well-formed and yields a pointer-to-const, and arrays and
// function designators are addressable too. Reusing checkLValue here used to
// reject all three, so `return &static_const_struct;` -- the ordinary way to
// hand out a table of constant data -- was reported as "cannot assign to
// const-typed". Only bit-fields and register objects stay forbidden, and the
// bit-field case is filtered out by the caller.
func (c *checker) checkAddrOperand(e Expr, fn *FuncDecl) (*Type, bool) {
	return c.checkLValueAddr(e, fn, true)
}

// checkLValueAddr is the shared implementation. addrOf relaxes the
// "modifiable" requirement to plain "addressable" (see checkAddrOperand).
func (c *checker) checkLValueAddr(e Expr, fn *FuncDecl, addrOf bool) (*Type, bool) {
	switch n := e.(type) {
	case *Ident:
		t := c.lookup(n.Name)
		if t == nil {
			if _, ok := enumConsts[n.Name]; ok {
				// An enumerator is an integer constant, not an lvalue.
				c.errf(n.Line, "enum constant %q is not an lvalue", n.Name)
				return IntType(), false
			}
			c.errf(n.Line, "undeclared identifier %q", n.Name)
			return IntType(), false
		}
		if t.IsArray() {
			// "&arr" is well-formed and yields a pointer to the array; only
			// assigning to the array itself is not.
			if addrOf {
				return t, true
			}
			c.errf(n.Line, "array %q is not a modifiable lvalue", n.Name)
			return t, false
		}
		if t.IsFunc() {
			// A function designator is addressable: "&f" and "f" are
			// equivalent for a pointer-to-function.
			if addrOf {
				return t, true
			}
			c.errf(n.Line, "function %q is not a modifiable lvalue", n.Name)
			return t, false
		}
		if t.Const {
			// "&const_obj" is well-formed (pointer-to-const); only writing
			// through the object is not.
			if addrOf {
				return t, true
			}
			c.errf(n.Line, "cannot assign to const-typed %q", n.Name)
			return t, false
		}
		return t, true
	case *Unary:
		if n.Op != "*" {
			c.errf(0, "expression is not an lvalue")
			return IntType(), false
		}
		t := c.checkExpr(n.E, fn)
		if !t.IsPtr() {
			c.errf(0, "cannot dereference non-pointer type %s", t)
			return IntType(), false
		}
		if t.Elem == nil {
			return VoidType(), true
		}
		return t.Elem, true
	case *Index:
		base := c.checkExpr(n.Base, fn)
		c.checkExpr(n.Idx, fn)
		if base.IsArray() {
			if base.Elem == nil {
				c.errf(0, "cannot index incomplete array type")
				return IntType(), false
			}
			return base.Elem, true
		}
		if base.IsPtr() {
			if base.Elem == nil {
				return VoidType(), true
			}
			return base.Elem, true
		}
		c.errf(0, "cannot index non-array/non-pointer type %s", base)
		return IntType(), false
	case *MemberExpr:
		return c.checkMemberLValue(n, fn)
	case *CompoundLit:
		// A compound literal is a modifiable lvalue: "(T){...} = ..." is
		// nonsense but "&(T){...}" and "(T){...}.member" are ordinary C.
		// A const-qualified compound literal is still addressable, so
		// "&(const T){...}" is fine; only writing to it is not.
		c.checkExpr(n, fn)
		if n.Typ.Const {
			if addrOf {
				return n.Typ, true
			}
			c.errf(n.Line, "compound literal is const-qualified")
			return n.Typ, false
		}
		return n.Typ, true
	}
	c.errf(0, "expression is not an lvalue")
	return IntType(), false
}

// checkMemberLValue resolves the lvalue type of a struct/union member access
// (base.name or base->name) and reports an error if the base is not a
// struct/union (or pointer to one) or the member does not exist.
func (c *checker) checkMemberLValue(n *MemberExpr, fn *FuncDecl) (*Type, bool) {
	bt := c.checkExpr(n.Base, fn)
	var st *Type
	if n.Arrow {
		if !bt.IsPtr() {
			c.errf(n.Line, "left of '->' must be a pointer to struct/union, got %s", bt)
			return IntType(), false
		}
		st = bt.Elem
	} else {
		st = bt
	}
	if st == nil || (st.Kind != KStruct && st.Kind != KUnion) {
		c.errf(n.Line, "member access on non-struct/union type %s%s", st, c.arrayMethodHint(n))
		return IntType(), false
	}
	for _, m := range st.Members {
		if m.Name == n.Name {
			if m.Type.Const {
				c.errf(n.Line, "cannot assign to const member %q", n.Name)
				return m.Type, false
			}
			return m.Type, true
		}
	}
	c.errf(n.Line, "no member %q in %s", n.Name, st.String())
	return IntType(), false
}

// findStructMember resolves the *Member of a MemberExpr without reporting
// errors (used by the address-of check to reject bit-field members, and by
// brace-initialisation to reject initialising bit-fields).
func (c *checker) findStructMember(n *MemberExpr, fn *FuncDecl) *Member {
	bt := c.checkExpr(n.Base, fn)
	var st *Type
	if n.Arrow {
		if !bt.IsPtr() || bt.Elem == nil {
			return nil
		}
		st = bt.Elem
	} else {
		st = bt
	}
	if st == nil || (st.Kind != KStruct && st.Kind != KUnion) {
		return nil
	}
	for _, m := range st.Members {
		if m.Name == n.Name {
			return m
		}
	}
	return nil
}

func (c *checker) checkExpr(e Expr, fn *FuncDecl) *Type {
	switch n := e.(type) {
	case *GenericExpr:
		return c.checkGeneric(n, fn)
	case *CompoundLit:
		// C99 compound literal. The unnamed object has block scope and
		// automatic storage; file-scope literals (static storage) are not
		// supported -- the fn==nil guard is exactly the file-scope signal,
		// since Check() walks globals with fn == nil.
		if fn == nil {
			c.errf(n.Line, "compound literal requires block scope (file-scope static literals are not supported)")
			return IntType()
		}
		// Validates the brace list against T and fills an incomplete array's
		// length ("(int[]){1,2,3}").
		c.checkBraceInit(n.Typ, n.Init, fn, n.Line)
		if n.Typ.IsArray() {
			// An array literal used as a value decays to a pointer to its
			// first element, exactly like a named array.
			return PtrType(n.Typ.Elem)
		}
		return n.Typ
	case *NumLit:
		if n.BigWords != nil {
			w := (n.BigBits + 63) / 64
			return &Type{Kind: KBitInt, Bits: n.BigBits, Size: w * 8, Signed: n.BigSigned}
		}
		// A long double constant is its own class (16-byte binary128), not
		// the TDouble slot, so it has to be recognised before the double test
		// -- otherwise typeof(1.0L) and _Generic(1.0L, long double: ...) both
		// fall through to the integer/default branch. (EnableLongDouble is the
		// #47/#48 scaffolding described in fp128.go.)
		if n.IsLongDouble && EnableLongDouble {
			return LongDoubleType()
		}
		if n.Kind == TDouble {
			if n.IsFloat {
				return FloatType()
			}
			return DoubleType()
		}
		if n.Wide {
			return WCharType() // L'x' is a wchar_t constant
		}
		// Integer literals carry their real type from the suffix: a u/U suffix
		// makes the constant unsigned, an l/L suffix makes it at least 64 bits
		// wide. Without this, `0u` would be typed as a signed int and a
		// _Generic(0u, unsigned: ...) selection (which <stdbit.h> relies on)
		// would silently match the int branch instead. The width/signedness
		// derivation mirrors typeof(0U) in parseTypeof.
		w := 4
		if n.Long {
			w = 8
		}
		return &Type{Kind: KInt, Width: w, Signed: !n.Unsig, LongLong: n.LongLong}
	case *StrLit:
		if n.Wide {
			return PtrType(WCharType()) // L"..." decays to wchar_t*
		}
		return PtrType(CharType()) // string literal decays to char*
	case *Ident:
		t := c.lookup(n.Name)
		if t == nil {
			// Functions live in their own namespace, not in the variable
			// scopes: a bare function name used as an expression is valid even
			// though no variable declaration provides a type for it.
			if ft := c.funcTypeByName(n.Name); ft != nil {
				t = ft
			} else if _, ok := enumConsts[n.Name]; ok {
				// Enumerators are integer constants: usable as rvalues.
				return IntType()
			} else {
				c.errf(n.Line, "undeclared identifier %q", n.Name)
				return IntType()
			}
		}
		if t.IsArray() {
			return PtrType(t.Elem) // array decays to pointer
		}
		if t.IsFunc() {
			// A function designator used as a value decays to a pointer to
			// that function, exactly like an array: "fp = add;".
			return PtrType(t)
		}
		return t
	case *Unary:
		switch n.Op {
		case "-":
			t := c.checkExpr(n.E, fn)
			if !t.IsArith() && t.Kind != KBitInt {
				c.errf(0, "operand of '-' must be arithmetic, got %s", t)
			}
			if t.Kind == KBitInt {
				return t
			}
			// Unary minus does not narrow its operand either: "-1u" is an
			// unsigned int, not a signed one. Floating types keep theirs
			// (the promotion only applies to the integer types).
			//
			// _Bool is an integer type and promotes to int like the other
			// narrow ones, so `-b` is int (C11 6.3.1.1p2).
			if t.Kind == KInt || t.Kind == KBool {
				return promotedInt(t)
			}
			return t
		case "+":
			// Unary plus: promote the operand and return it unchanged. The
			// promotions are the whole point -- `+c` for a char c is an int.
			t := c.checkExpr(n.E, fn)
			if !t.IsArith() && t.Kind != KBitInt {
				c.errf(0, "operand of '+' must be arithmetic, got %s", t)
			}
			if t.Kind == KBitInt {
				return t
			}
			if t.Kind == KInt || t.Kind == KBool {
				return promotedInt(t)
			}
			return t
		case "!":
			t := c.checkExpr(n.E, fn)
			if !t.IsScalar() && t.Kind != KBitInt {
				c.errf(0, "operand of '!' must be scalar, got %s", t)
			}
			return IntType()
		case "~":
			// Bitwise complement: integer operands only, and the operand
			// promotes to int exactly like the binary bitwise operators do.
			// A _BitInt operand keeps its own width and signedness.
			t := c.checkExpr(n.E, fn)
			if !t.IsIntClass() && t.Kind != KBitInt {
				c.errf(0, "operand of '~' must be an integer, got %s", t)
			}
			if t.Kind == KBitInt {
				return t
			}
			// The usual integer promotions, but *after* them: "~0u" is an
			// unsigned int, and _Generic sees that difference.
			return promotedInt(t)
		case "&":
			// Taking the address of a function designator is how a function
			// pointer is initialised ("fp = &add"). The designator is not an
			// lvalue, so it must be resolved before the lvalue check.
			if id, ok := n.E.(*Ident); ok {
				// A local variable hides a same-named function: C block
				// scoping hides file-scope names, and function names are
				// file-scope. Without this guard `&exp` on a local `int exp`
				// resolved to math.h's exp() and came out as double(double)*
				// -- the variable was readable and writable, and only taking
				// its address went to the function.
				if c.lookup(id.Name) == nil {
					if ft := c.funcTypeByName(id.Name); ft != nil {
						return PtrType(ft)
					}
				}
				if t := c.lookup(id.Name); t != nil && t.IsArray() {
					// &array yields a pointer to the whole array
					// (int (*)[N]), NOT a decayed pointer to element 0. Arrays
					// are lvalues for the purpose of taking their address even
					// though they are not modifiable lvalues.
					return PtrType(t)
				}
			}
			// A bit-field member has no address of its own: "&s.bf" is an
			// error in C.
			if me, ok := n.E.(*MemberExpr); ok {
				if m := c.findStructMember(me, fn); m != nil && m.BitWidth > 0 {
					c.errf(me.Line, "cannot take address of bit-field member %q", me.Name)
					return IntType()
				}
			}
			lt, ok := c.checkAddrOperand(n.E, fn)
			if !ok {
				return IntType()
			}
			return PtrType(lt)
		case "*":
			t := c.checkExpr(n.E, fn)
			if !t.IsPtr() {
				c.errf(0, "cannot dereference non-pointer type %s", t)
				return IntType()
			}
			if t.Elem == nil {
				return VoidType()
			}
			// *fp on a pointer to function names the function itself, which
			// decays straight back to the pointer to it.
			if t.Elem.IsFunc() {
				return PtrType(t.Elem)
			}
			return t.Elem
		}
	case *Binary:
		return c.checkBinary(n, fn)
	case *Call:
		return c.checkCall(n, fn)
	case *IndirectCall:
		return c.checkIndirectCall(n, fn)
	case *Index:
		base := c.checkExpr(n.Base, fn)
		idx := c.checkExpr(n.Idx, fn)
		if !idx.IsArith() {
			c.errf(0, "array index must be an integer, got %s", idx)
		}
		if base.IsArray() {
			if base.Elem == nil {
				c.errf(0, "cannot index incomplete array type")
				return VoidType()
			}
			return base.Elem
		}
		if base.IsPtr() {
			if base.Elem == nil {
				return VoidType()
			}
			return base.Elem
		}
		c.errf(0, "cannot index non-array/non-pointer type %s", base)
		return IntType()
	case *CondExpr:
		c.checkExpr(n.Cond, fn)
		tt := c.checkExpr(n.Then, fn)
		et := c.checkExpr(n.Else, fn)
		return c.condResultType(tt, et)
	case *CommaExpr:
		// Evaluate the left operand (for its side effects / errors) and keep
		// the type of the right one as the result, exactly like C.
		c.checkExpr(n.Left, fn)
		return c.checkExpr(n.Right, fn)
	case *CastExpr:
		c.checkExpr(n.E, fn)
		return n.Typ
	case *IncDecExpr:
		// ++/-- operate in place, so the operand must be a modifiable lvalue
		// (this also rejects const-typed and bit-field operands through
		// checkLValue).
		lt, ok := c.checkLValue(n.E, fn)
		if !ok {
			return IntType()
		}
		if !lt.IsArith() && !lt.IsPtr() && !lt.IsBool() && lt.Kind != KBitInt {
			c.errf(0, "operand of %s must be arithmetic or pointer, got %s", n.Op, lt)
		}
		return lt
	case *MemberExpr:
		_, ok := c.checkMemberLValue(n, fn)
		if !ok {
			return IntType()
		}
		// Re-resolve the member type (checkMemberLValue already validated it).
		bt := c.checkExpr(n.Base, fn)
		st := bt
		if n.Arrow {
			st = bt.Elem
		}
		for _, m := range st.Members {
			if m.Name == n.Name {
				// An array member used as a value decays to a pointer to its
				// element 0, exactly like a bare array identifier ("char *q =
				// s.name" must type-check the same way "char *q = s" does).
				if m.Type.IsArray() && m.Type.Elem != nil {
					return PtrType(m.Type.Elem)
				}
				return m.Type
			}
		}
		return IntType()
	case *SizeofExpr:
		// sizeof(expr) must still type-check its operand: an incomplete-array
		// compound literal inside ("sizeof((int[]){1,2,3})") borrows its length
		// from the brace list during checking. Without this visit the Len stayed
		// 0, so the fold produced 0 instead of 12 (P0.5).
		if n.E != nil {
			c.checkExpr(n.E, fn)
		}
		return IntType()
	case *AssignExpr:
		// Assignment "a = b" (statement-level or in expression position) must
		// have a modifiable lvalue on the left. Statement-level assignments are
		// parsed as ExprStmt{AssignExpr}, so the lvalue check must live here,
		// not in checkStmt (whose *AssignStmt branch is never produced by the
		// parser).
		lt, ok := c.checkLValue(n.Lhs, fn)
		if !ok {
			return IntType()
		}
		if n.Op != "" {
			// Compound assignment "E1 op= E2" (C11 6.5.16.2) types as "E1 =
			// E1 op E2" but evaluates E1 exactly once. The result type is the
			// arithmetic result, which must be assignable back to the lvalue.
			rt := c.checkExpr(n.Rhs, fn)
			res := c.binaryResultType(n.Op, lt, rt)
			if !lt.IsVoid() && !assignable(lt, res) {
				c.errf(0, "cannot assign %s to %s", res, lt)
			}
			// C11 6.5.16p3: an assignment expression has the type of its left
			// operand after lvalue conversion -- NOT the type of the
			// arithmetic result. The value is converted back to that type on
			// assignment, so with `signed char c = 100` the expression
			// `(c += 100)` is a signed char holding -56 and compares negative.
			return lt
		}
		rt := c.checkExpr(n.Rhs, fn)
		// Writing through a void* yields a void location; any value may be
		// stored there (the checker models it as "we do not know the element
		// type"), so skip the assignability check for that case.
		if !lt.IsVoid() && !assignable(lt, rt) {
			c.errf(0, "cannot assign %s to %s", rt, lt)
		}
		return rt
	case *VaArgExpr:
		ap := c.checkExpr(n.Ap, fn)
		if !ap.IsPtr() {
			c.errf(0, "va_arg first argument must be a va_list, got %s", ap)
		}
		return n.Typ
	}
	return IntType()
}
func (c *checker) checkBinary(n *Binary, fn *FuncDecl) *Type {
	lt := c.checkExpr(n.L, fn)
	rt := c.checkExpr(n.R, fn)
	return c.binaryResultType(n.Op, lt, rt)
}

// binaryResultType computes the result type of "lt op rt" (C11 6.5.5-6.5.12)
// and reports errors for invalid operand combinations, without re-checking
// the operands. checkBinary evaluates both operands and delegates here;
// compound assignment ("E1 op= E2") uses it directly so the lvalue is checked
// exactly once and the arithmetic validity rules are shared verbatim.
func (c *checker) binaryResultType(op string, lt, rt *Type) *Type {
	switch op {
	case "+", "-", "*", "/", "%":
		if op == "%" && !(lt.IsIntClass() && rt.IsIntClass()) && !isBig(lt) && !isBig(rt) {
			c.errf(0, "operator '%%' requires integer operands, got %s and %s", lt, rt)
		}
		// Pointer arithmetic: ptr +/- int, ptr - ptr.
		if lt.IsPtr() || rt.IsPtr() {
			switch {
			case op == "+" && lt.IsPtr() && rt.IsIntClass():
				return lt
			case op == "+" && lt.IsIntClass() && rt.IsPtr():
				return rt
			case op == "-" && lt.IsPtr() && rt.IsIntClass():
				return lt
			case op == "-" && lt.IsPtr() && rt.IsPtr():
				return IntType() // pointer difference
			}
			c.errf(0, "invalid pointer arithmetic on %s and %s", lt, rt)
			return lt
		}
		// Usual arithmetic conversions: the wider floating type wins, so
		// float+double is double while float+float stays float. Either way the
		// value is carried in an XMM register; only its 4/8-byte storage
		// footprint differs.
		if lt.IsFloating() || rt.IsFloating() {
			if isBig(lt) || isBig(rt) {
				c.errf(0, "floating operands with _BitInt are not supported (%s and %s)", lt, rt)
				return lt
			}
			if op == "%" {
				c.errf(0, "operator '%%' requires integer operands, got %s and %s", lt, rt)
			}
			// The usual arithmetic conversions rank the floating types:
			// long double > double > float. (Only the storage footprint and
			// the soft-float helpers differ -- every one of them is a real
			// floating type, so the ordering is the only thing to get right
			// here.)
			if lt.Kind == KLongDouble || rt.Kind == KLongDouble {
				return LongDoubleType()
			}
			if lt.Kind == KDouble || rt.Kind == KDouble {
				return DoubleType()
			}
			return FloatType()
		}
		// C23 bit-precise arithmetic: the wider _BitInt wins (equal widths
		// resolve to unsigned); an int-class operand converts to the bitint
		// side. Pointer mixes were handled above, so this is safe.
		if isBig(lt) || isBig(rt) {
			if !(lt.IsIntClass() || isBig(lt)) || !(rt.IsIntClass() || isBig(rt)) {
				c.errf(0, "invalid operands with _BitInt: %s and %s", lt, rt)
				return IntType()
			}
			return bigArithResult(op, lt, rt)
		}
		return usualArithInt(lt, rt)
	case "<", ">", "<=", ">=", "==", "!=":
		if !(lt.IsScalar() && rt.IsScalar()) && !isBig(lt) && !isBig(rt) {
			c.errf(0, "relational operator requires scalar operands, got %s and %s", lt, rt)
		}
		return IntType()
	case "&&", "||":
		if !(lt.IsScalar() && rt.IsScalar()) && !isBig(lt) && !isBig(rt) {
			c.errf(0, "logical operator requires scalar operands, got %s and %s", lt, rt)
		}
		return IntType()
	case "<<", ">>":
		if !(lt.IsIntClass() && rt.IsIntClass()) && !isBig(lt) && !isBig(rt) {
			c.errf(0, "operator %q requires integer operands, got %s and %s", op, lt, rt)
		}
		if isBig(lt) || isBig(rt) {
			if !(lt.IsIntClass() || isBig(lt)) || !(rt.IsIntClass() || isBig(rt)) {
				c.errf(0, "invalid operands with _BitInt: %s and %s", lt, rt)
				return IntType()
			}
			return bigArithResult(op, lt, rt)
		}
		// A shift does not convert its operands: the result is the promoted
		// left operand (C11 6.5.7p3), so "1u << 15" stays unsigned int. That
		// is observable through _Generic, which is exactly how <stdbit.h>'s
		// type-generic macros dispatch.
		return promotedInt(lt)
	case "&", "|", "^":
		if !(lt.IsIntClass() && rt.IsIntClass()) && !isBig(lt) && !isBig(rt) {
			c.errf(0, "operator %q requires integer operands, got %s and %s", op, lt, rt)
		}
		if isBig(lt) || isBig(rt) {
			if !(lt.IsIntClass() || isBig(lt)) || !(rt.IsIntClass() || isBig(rt)) {
				c.errf(0, "invalid operands with _BitInt: %s and %s", lt, rt)
				return IntType()
			}
			return bigArithResult(op, lt, rt)
		}
		return usualArithInt(lt, rt)
	}
	return IntType()
}

// promotedInt applies the integer promotions (C11 6.3.1.1): a type narrower
// than int becomes int, everything else keeps its width and signedness.
func promotedInt(t *Type) *Type {
	if t == nil || t.Kind != KInt {
		return IntType()
	}
	if t.Width < 4 {
		return IntType()
	}
	return &Type{Kind: KInt, Width: t.Width, Signed: t.Signed}
}

// condResultType is the type of a conditional expression (C11 6.5.15p5-p6).
//
// Returning the then-branch's type unchanged was wrong whenever the two arms
// differed: `i ? i : u` is unsigned int (the usual arithmetic conversions
// apply to any pair of arithmetic operands), `c ? c : s` is int (both promote),
// and a void* paired with an object pointer yields void*.
func (c *checker) condResultType(tt, et *Type) *Type {
	switch {
	case tt == nil && et == nil:
		return IntType()
	case tt == nil:
		return et
	case et == nil:
		return tt
	}
	if tt.IsArith() && et.IsArith() {
		// Any binary arithmetic operator applies the same conversions.
		return c.binaryResultType("+", tt, et)
	}
	if tt.IsVoid() && et.IsVoid() {
		return VoidType()
	}
	if tt.IsPtr() && et.IsPtr() {
		// void* absorbs any object pointer type; otherwise keep the first
		// branch's type (mismatched object pointers are not our problem to
		// diagnose here -- the assignment/use site reports it).
		if tt.Elem != nil && tt.Elem.IsVoid() {
			return tt
		}
		if et.Elem != nil && et.Elem.IsVoid() {
			return et
		}
		return tt
	}
	// One arm is a null pointer constant and the other a pointer, or the two
	// are the same aggregate: the non-null / shared type wins.
	if tt.IsPtr() {
		return tt
	}
	if et.IsPtr() {
		return et
	}
	return tt
}

// usualArithInt applies the usual arithmetic conversions (C11 6.3.1.8) to two
// integer types and returns their common type: both operands are promoted, the
// wider one wins, and the result is unsigned when the unsigned operand's width
// is at least the signed one's (a strictly wider signed type can represent
// every value of the narrower unsigned one, so it wins in that case).
func usualArithInt(lt, rt *Type) *Type {
	if lt == nil || rt == nil {
		return IntType()
	}
	// _Bool is an integer type and takes part in the usual arithmetic
	// conversions like any other: it promotes to int first (C11 6.3.1.1p2),
	// so `b + 1L` is long rather than int.
	if lt.Kind == KBool {
		lt = IntType()
	}
	if rt.Kind == KBool {
		rt = IntType()
	}
	if lt.Kind != KInt || rt.Kind != KInt {
		return IntType()
	}
	lp, rp := promotedInt(lt), promotedInt(rt)
	w := lp.Width
	if rp.Width > w {
		w = rp.Width
	}
	signed := lp.Signed && rp.Signed
	if lp.Signed != rp.Signed {
		// Rank is the promoted width here: an unsigned operand of equal or
		// greater width forces an unsigned result, otherwise the (strictly
		// wider) signed type absorbs it.
		uw := rp.Width
		if !lp.Signed {
			uw = lp.Width
		}
		signed = uw < w
	}
	return &Type{Kind: KInt, Width: w, Signed: signed}
}

func (c *checker) checkCall(n *Call, fn *FuncDecl) *Type {
	// print(...) is a compiler builtin: the checker sees every argument's
	// static type, so it builds the format string and lowers the call to
	// printf. A user-declared print function keeps precedence.
	if n.Name == "print" {
		if _, user := c.funcs["print"]; !user {
			return c.rewritePrint(n, fn)
		}
	}
	// The <stdatomic.h> fetch family: see src/frontend/atomic.go. Each of them
	// is one locked instruction, so it is a builtin rather than a goclib call.
	if _, user := c.funcs[n.Name]; !user {
		if ab, ok := LookupAtomicBuiltin(n.Name); ok {
			return c.checkAtomicBuiltin(n, fn, ab)
		}
		// The marker builtins (__builtin_unreachable, __builtin_expect, ...)
		// exist only to carry optimiser information: see src/frontend/marker.go.
		if ms, ok := LookupMarkerBuiltin(n.Name); ok {
			return c.checkMarkerBuiltin(n, fn, ms)
		}
		// GCC's overflow-checked arithmetic (__builtin_saddll_overflow and
		// friends): see src/frontend/overflow.go.
		if ob, ok := LookupOverflowBuiltin(n.Name); ok {
			return c.checkOverflowBuiltin(n, fn, ob)
		}
	}
	// A local variable holding a function pointer shadows a same-named
	// function (C block scoping hides file-scope names, function names
	// included): "fp(x)" through such a variable is an indirect call.
	if ft := funcTypeOf(c.lookup(n.Name)); ft != nil {
		return c.checkArgs(n.Name, ft.Params, ft.Variadic, n.Args, fn, ft.Ret)
	}
	if fd, ok := c.funcs[n.Name]; ok {
		return c.checkArgs(n.Name, fd.ParamTypes, fd.Variadic, n.Args, fn, fd.Ret)
	}
	if pd, ok := c.protos[n.Name]; ok {
		return c.checkArgs(n.Name, pd.ParamTypes, pd.Variadic, n.Args, fn, pd.Ret)
	}
	// External / goclib call whose signature we do not model: accept it and
	// assume an int result (true for every goclib function goc exposes).
	return IntType()
}

// funcTypeOf returns the function type behind t: KFunc as it stands, or the
// pointee of a pointer to function -- the only shape a function type can take
// once it is stored in a variable, parameter, struct member or return value.
func funcTypeOf(t *Type) *Type {
	if t == nil {
		return nil
	}
	if t.IsFunc() {
		return t
	}
	if t.IsPtr() && t.Elem != nil && t.Elem.IsFunc() {
		return t.Elem
	}
	return nil
}

// checkArgs validates a resolved argument list against a parameter list. It
// reports arity and argument-type mismatches and returns the call result type.
// A variadic signature requires at least its named parameters.
func (c *checker) checkArgs(what string, params []*Type, variadic bool, args []Expr, fn *FuncDecl, ret *Type) *Type {
	if variadic {
		if len(args) < len(params) {
			c.errf(0, "call to %q: expected at least %d arguments, got %d", what, len(params), len(args))
		}
		for i, a := range args {
			if i >= len(params) {
				// Trailing variadic arguments have no parameter to check
				// assignability against, but they are still walked: every
				// argument must be a valid expression, and expression nodes
				// that resolve at check time (e.g. _Generic selections)
				// must be resolved even when the code generator will read
				// them out of a variadic position.
				c.checkExpr(a, fn)
				continue
			}
			at := c.checkExpr(a, fn)
			if !assignable(params[i], at) {
				c.errf(0, "call to %q: argument %d has type %s, expected %s", what, i+1, at, params[i])
			}
		}
		return ret
	}
	if len(args) != len(params) {
		c.errf(0, "call to %q: expected %d arguments, got %d", what, len(params), len(args))
		return ret
	}
	for i, a := range args {
		at := c.checkExpr(a, fn)
		if !assignable(params[i], at) {
			c.errf(0, "call to %q: argument %d has type %s, expected %s",
				what, i+1, at, params[i])
		}
	}
	return ret
}

// funcTypeByName returns the type of a declared function (a definition or a
// prototype), or nil when the name is not one. Functions live in their own
// namespace, separate from variables.
func (c *checker) funcTypeByName(name string) *Type {
	var fd *FuncDecl
	if f, ok := c.funcs[name]; ok {
		fd = f
	} else if p, ok := c.protos[name]; ok {
		fd = p
	}
	if fd == nil {
		return nil
	}
	ft := FuncType(fd.Ret, fd.ParamTypes)
	ft.Variadic = fd.Variadic
	return ft
}

// checkIndirectCall validates a call through a computed function address.
func (c *checker) checkIndirectCall(n *IndirectCall, fn *FuncDecl) *Type {
	// A member call x.f(args) may be a method on x's struct type (UFCS).
	// Resolved calls carry the rewritten direct form in n.UFCS; everything
	// else falls through to the ordinary indirect-call rules.
	if t, ok := c.tryUFCS(n, fn); ok {
		return t
	}
	ft := funcTypeOf(c.checkExpr(n.Fn, fn))
	if ft == nil {
		c.errf(0, "called expression (%T) is not a function pointer", n.Fn)
		for _, a := range n.Args {
			c.checkExpr(a, fn) // still walk the arguments for their own errors
		}
		return IntType()
	}
	return c.checkArgs(ft.String(), ft.Params, ft.Variadic, n.Args, fn, ft.Ret)
}

// assignable reports whether a value of src may be stored into a location of
// dst under C's implicit conversion rules (relaxed for the toy model).
func isBig(t *Type) bool { return t != nil && t.Kind == KBitInt }

func assignable(dst, src *Type) bool {
	if dst == nil || src == nil {
		return true // an error was already reported upstream
	}
	if dst.IsVoid() || src.IsVoid() {
		return false
	}
	switch {
	case dst.IsArith() && src.IsArith():
		return true
	case dst.IsBool() && src.IsIntClass():
		return true // int -> bool conversion (0/1)
	case dst.IsIntClass() && src.IsBool():
		return true // bool -> int conversion
	case dst.IsPtr() && src.IsPtr() && src.Elem != nil && src.Elem.Kind == KBitInt &&
		dst.Elem != nil && (dst.Elem.IsIntClass() || dst.Elem.IsVoid()):
		return true // a _BitInt's word array is addressable as unsigned long long*
	case dst.IsPtr() && src.IsPtr():
		de, se := dst.Elem, src.Elem
		if de == nil || se == nil || de.IsVoid() || se.IsVoid() {
			return true // any pointer compares with void* / unknown
		}
		return typesEqual(de, se)
	case dst.IsPtr() && src.IsIntClass():
		return true // int -> pointer (e.g. 0 / NULL)
	case dst.IsIntClass() && src.IsPtr():
		return true // pointer -> int
	case dst.IsStruct() && src.IsStruct():
		return typesEqual(dst, src) // whole-struct assignment
	case dst.IsUnion() && src.IsUnion():
		return typesEqual(dst, src)
	case dst.Kind == KBitInt && src.Kind == KBitInt:
		return true // different widths convert by truncation/sign-extension
	case dst.Kind == KBitInt && src.IsIntClass():
		return true
	case dst.IsIntClass() && src.Kind == KBitInt:
		return true // truncating conversion to the integer type
	case dst.IsBool() && src.Kind == KBitInt:
		return true
	case dst.IsPtr() && src.Kind == KBitInt:
		return false // bitint is not a pointer source
	}
	return false
}

// checkBraceInit validates a braced initialiser against type t. An incomplete
// array ("int a[] = {...}") borrows its length from the number of top-level
// elements; every byte not explicitly initialised is zero (C semantics).
func (c *checker) checkBraceInit(t *Type, bi *BraceInit, fn *FuncDecl, line int) {
	switch {
	case t.IsArray():
		hasDesig := false
		for _, el := range bi.Elems {
			if el.Desig != "" {
				c.errf(line, "member designator %q is only valid in a struct/union initialiser", el.Desig)
			}
			if el.DesigIdx >= 0 {
				hasDesig = true
			}
		}
		if t.Len == 0 {
			n := 0
			if hasDesig {
				for _, el := range bi.Elems {
					if el.DesigIdx >= 0 && el.DesigIdx+1 > n {
						n = el.DesigIdx + 1
					}
				}
			} else {
				n = len(bi.Elems)
			}
			if n == 0 {
				// empty {} on an incomplete array: a zero-length array, which
				// zero-initialises (no elements).
				t.Len = 0
				return
			}
			t.Len = n
		}
		if hasDesig {
			for _, el := range bi.Elems {
				if el.DesigIdx < 0 {
					c.errf(line, "cannot mix positional and designated (\"[i] =\") initialisers")
					continue
				}
				if el.DesigIdx >= t.Len {
					c.errf(line, "designator index %d out of range for array of %d element(s)", el.DesigIdx, t.Len)
					continue
				}
				c.checkBraceElem(t.Elem, el.E, fn, line)
			}
		} else {
			for i, el := range bi.Elems {
				if el.Desig != "" {
					continue
				}
				if i >= t.Len {
					break
				}
				c.checkBraceElem(t.Elem, el.E, fn, line)
			}
		}
	case t.IsStruct():
		c.checkStructBrace(t, bi, fn, line)
	case t.IsUnion():
		if len(bi.Elems) > 1 {
			c.errf(line, "too many initialisers for union (at most one member)")
		}
		if len(bi.Elems) == 0 {
			return
		}
		el := bi.Elems[0]
		var m *Member
		if el.DesigIdx >= 0 {
			c.errf(line, "array designator \"[%d] =\" is only valid in an array initialiser", el.DesigIdx)
			return
		}
		if el.Desig != "" {
			mi := memberIndex(t, el.Desig)
			if mi < 0 {
				c.errf(line, "union has no member %q", el.Desig)
				return
			}
			m = t.Members[mi]
		} else if vis := posMembers(t); len(vis) > 0 {
			m = vis[0]
		}
		if m != nil {
			if m.BitWidth > 0 {
				c.errf(line, "cannot brace-initialise bit-field member %q", m.Name)
				return
			}
			c.checkBraceElem(m.Type, el.E, fn, line)
		}
	default:
		// C23 empty brace initialiser "T x = {};" zero-initialises the object.
		if len(bi.Elems) == 0 {
			return
		}
		// C allows a scalar to be initialised from a single braced value.
		if len(bi.Elems) != 1 || bi.Elems[0].Desig != "" {
			c.errf(line, "invalid initialiser for scalar type %s", t)
			return
		}
		if bi.Elems[0].DesigIdx >= 0 {
			c.errf(line, "array designator \"[%d] =\" is only valid in an array initialiser", bi.Elems[0].DesigIdx)
			return
		}
		c.checkBraceElem(t, bi.Elems[0].E, fn, line)
	}
}

// checkStructBrace validates a struct initialiser, either all-positional or
// all-designated (".x = ..."); mixing the two styles is rejected.
func (c *checker) checkStructBrace(t *Type, bi *BraceInit, fn *FuncDecl, line int) {
	for _, el := range bi.Elems {
		if el.DesigIdx >= 0 {
			c.errf(line, "array designator \"[%d] =\" is only valid in an array initialiser", el.DesigIdx)
			return
		}
	}
	allDesig := true
	for _, el := range bi.Elems {
		if el.Desig == "" {
			allDesig = false
			break
		}
	}
	if allDesig {
		for _, el := range bi.Elems {
			mi := memberIndex(t, el.Desig)
			if mi < 0 {
				c.errf(line, "struct has no member %q", el.Desig)
				continue
			}
			if t.Members[mi].BitWidth > 0 {
				c.errf(line, "cannot brace-initialise bit-field member %q", el.Desig)
				continue
			}
			c.checkBraceElem(t.Members[mi].Type, el.E, fn, line)
		}
		return
	}
	vis := posMembers(t)
	for i, el := range bi.Elems {
		if i >= len(vis) {
			c.errf(line, "too many initialisers for struct %s", t)
			break
		}
		if el.Desig != "" {
			// C99 allows continuing after a designator positionally, but
			// implementing that partial-order rule on top of the positional
			// walk would silently overwrite; reject the mix instead.
			c.errf(line, "cannot mix positional and designated (\".%s =\") initialisers", el.Desig)
			return
		}
		if vis[i].BitWidth > 0 {
			c.errf(line, "cannot brace-initialise bit-field member %q", vis[i].Name)
			continue
		}
		c.checkBraceElem(vis[i].Type, el.E, fn, line)
	}
}

// checkBraceElem validates one element of a braced initialiser: either a
// nested brace for an aggregate target, a string for a char array (or char*),
// or an ordinary expression checked through the normal expression path.
func (c *checker) checkBraceElem(t *Type, e Expr, fn *FuncDecl, line int) {
	if nbi, ok := e.(*BraceInit); ok {
		if t.IsArray() || t.IsStruct() || t.IsUnion() {
			c.checkBraceInit(t, nbi, fn, line)
			return
		}
		c.errf(line, "braced initialiser for scalar type %s", t)
		return
	}
	if sl, ok := e.(*StrLit); ok {
		if t.IsArray() {
			if sl.Wide {
				// An L"..." literal initialises a wchar_t[] array (UTF-16).
				if t.Elem.Width == 2 {
					n := len(sl.Bytes) / 2
					if t.Len != 0 && t.Len < n+1 {
						c.errf(line, "wide string of %d elements does not fit in wchar_t array of %d elements", n, t.Len)
					}
					return
				}
			} else if t.Elem.IsChar() {
				if t.Len != 0 && t.Len < len(sl.Bytes)+1 {
					c.errf(line, "string of length %d does not fit in char array of %d bytes", len(sl.Bytes), t.Len)
				}
				return
			}
		}
		if t.IsPtr() {
			if (sl.Wide && t.Elem.Width == 2) || (!sl.Wide && t.Elem.IsChar()) {
				return // a (wchar_t*|char*) member/element may hold a string literal
			}
		}
		c.errf(line, "string literal cannot initialise %s", t)
		return
	}
	tt := c.checkExpr(e, fn)
	if !assignable(t, tt) {
		c.errf(line, "initialiser element of type %s is not assignable to %s", tt, t)
	}
}

// memberIndex returns the index of the named member, or -1.
func memberIndex(t *Type, name string) int {
	for i, m := range t.Members {
		if m.Name == name {
			return i
		}
	}
	return -1
}

func typesEqual(a, b *Type) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KPtr, KArr:
		return typesEqual(a.Elem, b.Elem)
	case KStruct, KUnion:
		// Named structs/unions compare by tag; anonymous ones compare
		// member-for-member.
		if a.Tag != "" || b.Tag != "" {
			return a.Tag == b.Tag
		}
		if len(a.Members) != len(b.Members) {
			return false
		}
		for i := range a.Members {
			if a.Members[i].Name != b.Members[i].Name {
				return false
			}
			if !typesEqual(a.Members[i].Type, b.Members[i].Type) {
				return false
			}
		}
		return true
	case KFunc:
		if !typesEqual(a.Ret, b.Ret) {
			return false
		}
		if len(a.Params) != len(b.Params) {
			return false
		}
		for i := range a.Params {
			if !typesEqual(a.Params[i], b.Params[i]) {
				return false
			}
		}
		return true
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
//
// Exported views of internal helpers.
//
// The code generator uses these, and it lives in another package now that the
// front end is a library. Rather than rename the internals -- which would touch
// hundreds of call sites inside this package for no benefit -- each one gets an
// exported alias. A wrapper is preferable to a rename here because these names
// are used heavily in the type rules below, where "Sizeof" reading better than
// "sizeOf" is not worth the churn.

// Sizeof is the size in bytes of a type, or -1 when it is incomplete.
func Sizeof(t *Type) int { return sizeOf(t) }

// BigArithResult is the result type of a _BitInt arithmetic operation.
func BigArithResult(op string, lt0, rt0 *Type) *Type { return bigArithResult(op, lt0, rt0) }

// PosMembers is a struct's members that participate in layout.
func PosMembers(t *Type) []*Member { return posMembers(t) }

// IsBig reports whether t is a _BitInt type, the only kind that needs
// multi-word codegen.
func IsBig(t *Type) bool { return isBig(t) }

// FuncTypeOf extracts the function type from a function type or a pointer to
// one, or nil when t is neither.
func FuncTypeOf(t *Type) *Type { return funcTypeOf(t) }

// MemberIndex is the offset of the named member within its struct, or -1.
func MemberIndex(t *Type, name string) int { return memberIndex(t, name) }

// WalkStmts visits every statement in a body, including nested blocks.
func WalkStmts(s Stmt, visit func(Stmt)) { walkStmts(s, visit) }

// Structs is the file-scope struct and union table, keyed by tag. The code
// generator needs it to resolve a forward reference it sees after the front end
// has already recorded the layout.
func Structs() map[string]*Type { return structs }

// Typedefs is the file-scope typedef table.
func Typedefs() map[string]*Type { return typedefs }

// EnumConsts is the enumeration-constant table produced by the checker, keyed
// by name. The LLVM backend folds these into its IR, and it indexes and ranges
// over it directly, so this is the map itself rather than an accessor.
var EnumConsts = enumConsts

// Keywords is the C keyword set. The generator consults it when it parses a
// gcc-style command line and has to decide whether an identifier is a type name
// it may redeclare.
func Keywords() map[string]bool { return keywords }
