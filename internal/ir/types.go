package gocl

import (
	"fmt"
	"goc/common"
	"goc/frontend"
)

// typeResolver answers "what type is this expression" and "where does this
// name live", and it belongs to neither back end.
//
// The assembly generator and the LLVM IR translator both drive it, so the two
// never disagree about a program's meaning -- and, crucially, the IR translator
// needs no other handle on CG. When the resolver is serving the assembly path
// it reads tables built from the typed AST and the C runtime. One set of rules,
// no second opinion about the same program.
// varInfo is what the IR path needs to know about a local variable: which
// storage slot it lives in and what type it has.
//
// The native generator carries more in the same struct -- whether the address
// was taken, whether ptrCapable proved it holds a pointer, which register it
// prefers -- because it has to encode a load for each of those situations. The
// IR has no such distinction: LLVM decides register allocation, and an escaping
// address is a fact about the IR (an alloca that is loaded from), not
// something the front end has to track. Carrying the native fields here would
// mean maintaining answers to questions this back end never asks.
type varInfo struct {
	op int // the uid of the alloca holding the value
	ty *frontend.Type
	// glob is the module-global symbol a static local lives under, empty
	// for an ordinary automatic. A static local has static storage
	// duration -- one object for the whole program, initialised once --
	// so it is emitted as a module global and the name resolves to that
	// symbol instead of to an alloca. The label is minted per
	// declaration site; see irEmitter.staticLocalDecl.
	glob string
}

type typeResolver struct {
	funcDefs   map[string]*frontend.FuncDecl
	globalTyp  map[string]*frontend.Type
	staticVars map[string]string
	lib        *common.Program

	// userDefs holds only the functions the *program* defines, keeping them
	// apart from funcDefs, which also holds the C runtime's. The two have to be
	// distinguishable: a rewrite that assumes the library symbol is there --
	// "call fwrite directly, it is certainly ours" -- must first be sure the
	// program has not defined its own fwrite, and funcDefs cannot answer that
	// because it contains the runtime's copy too.
	userDefs map[string]bool

	// Per-function emission scope state, used only when cg == nil.
	scopes  []map[string]int
	varEnts map[int]varInfo
	varUID  int
	declUID map[*frontend.DeclStmt]int
}

// --- dispatch helpers -------------------------------------------------------
func (tr *typeResolver) funcDef(name string) (*frontend.FuncDecl, bool) {
	f, ok := tr.funcDefs[name]
	return f, ok
}

func (tr *typeResolver) globalType(name string) (*frontend.Type, bool) {

	t, ok := tr.globalTyp[name]
	return t, ok
}

// funcDefFor resolves a function name to the declaration that names it: this
// module's own definition, or the C runtime's declaration of it when the
// module defines no such function.
//
// The two tables have to be consulted separately because a library function
// has no body. Taking the address of one is ordinary C -- "vfn pf = printf"
// and "unsigned long (*slen)(const char *) = strlen" both do it -- so asking
// only about defined functions answered "not a function" for exactly the
// names a program is most likely to put in a pointer.
func (tr *typeResolver) funcDefFor(name string) (*frontend.FuncDecl, bool) {
	if f, ok := tr.funcDef(name); ok && f != nil && f.Body != nil {
		return f, true
	}
	return tr.libFunc(name)
}

// isFuncName reports whether name designates a function -- the user's own or the
// C runtime's, which funcDefFor covers for both paths.
//
// It exists for the function-designator conversion: a bare function name in a
// value context (assigned to a pointer, passed as an argument) *is* the
// function's address. It cannot be inferred from exprType, which reports a
// call's result type for a name that is also callable -- asking whether the name
// has a type answers "int" for `twice` and misses the conversion entirely.
func (tr *typeResolver) isFuncName(name string) bool {
	f, ok := tr.funcDefFor(name)
	return ok && f != nil
}

// fnPtrTy is the type a function designator decays to: a pointer to the
// function's own type. The parameter list is carried over so the pointer type
// matches the declaration, which is what lets the indirect call type-check.
//
// funcDefFor, so a runtime function's address carries its real signature too:
// "vfn pf = printf" has to type pf as a pointer to "int (const char *, ...)"
// or the variadic call through it would pass a pointer where an int belongs.
func (e *irEmitter) fnPtrTy(name string) *frontend.Type {
	f, ok := e.tr.funcDefFor(name)
	if !ok || f == nil {
		return frontend.PtrType(frontend.IntType())
	}
	return frontend.PtrType(frontend.FuncType(f.Ret, f.ParamTypes))
}

func (tr *typeResolver) staticLabel(name string) (string, bool) {

	l, ok := tr.staticVars[name]
	return l, ok
}

// mintStaticLabel returns an unused module-global spelling for a
// function-local static. The prefix keeps it clear of both the user's
// file-scope globals (registered under their own bare names) and the C
// runtime's, and the trailing counter guarantees that two declaration
// sites -- two functions with a same-named static, or two sibling blocks
// in one function -- never share storage. The check is against
// globalTyp because that is the table every other global's spelling
// already went through.
func (tr *typeResolver) mintStaticLabel(fn, name string) string {
	for n := 0; ; n++ {
		cand := fmt.Sprintf("st_%s_%s_%d", fn, name, n)
		if _, taken := tr.globalTyp[cand]; !taken {
			return cand
		}
	}
}

func (tr *typeResolver) libFunc(name string) (*frontend.FuncDecl, bool) {
	if tr.lib != nil {
		if f, ok := tr.lib.Funcs[name]; ok {
			return f, true
		}
		// A Win32 entry point is declared by a header and defined by the DLL, so
		// it appears among the library's prototypes and never among its
		// functions. Missing those left every such call with no parameter types,
		// and calleeSig's fallback then typed them all as int -- which truncated
		// the HANDLE GetStdHandle returns to 32 bits and left WriteFile holding
		// a handle with no high half, so any program that printed anything died
		// on a write it should never have attempted.
		for _, p := range tr.lib.Protos {
			if p.Name == name {
				return p, true
			}
		}
	}
	return nil, false
}

// --- scope state ------------------------------------------------------------

func (tr *typeResolver) pushScope() {
	tr.scopes = append(tr.scopes, map[string]int{})
}

func (tr *typeResolver) popScope() {
	if len(tr.scopes) > 0 {
		tr.scopes = tr.scopes[:len(tr.scopes)-1]
	}
}

func (tr *typeResolver) declareVar(name string, info varInfo) int {
	uid := tr.varUID
	tr.varUID++
	// op is the uid this declaration was given, so a later lookupVar can hand
	// the same number back to slotFor. Leaving it at the zero value silently
	// resolved every local to whatever slot uid 0 happened to own -- which is
	// nothing at all once a function-local static takes uid 0 (it has no
	// alloca), so every use of every local then minted a fresh alloca of its
	// own and read and wrote that instead: two locals became one object and
	// the program computed garbage.
	info.op = uid
	tr.varEnts[uid] = info
	tr.scopes[len(tr.scopes)-1][name] = uid
	return uid
}

func (tr *typeResolver) lookupVar(name string) (varInfo, bool) {
	for i := len(tr.scopes) - 1; i >= 0; i-- {
		if uid, ok := tr.scopes[i][name]; ok {
			return tr.varEnts[uid], true
		}
	}
	return varInfo{}, false
}

func (tr *typeResolver) lookupUID(name string) (int, bool) {
	for i := len(tr.scopes) - 1; i >= 0; i-- {
		if uid, ok := tr.scopes[i][name]; ok {
			return uid, true
		}
	}
	return 0, false
}

// resetScope clears the per-function emission scope state. The assembly path
// resets its own scope when a function body starts, so each function gets a
// fresh variable numbering.
func (tr *typeResolver) resetScope() {
	tr.varEnts = map[int]varInfo{}
	tr.scopes = nil
	tr.varUID = 0
	tr.declUID = map[*frontend.DeclStmt]int{}
}

// --- type analysis ----------------------------------------------------------

func (tr *typeResolver) exprType(e frontend.Expr) *frontend.Type {
	switch n := e.(type) {
	case *frontend.NumLit:
		// A literal's own type, which decides whether the arithmetic around it
		// is floating or integral. Kind == frontend.TDouble marks it floating and IsFloat
		// picks the width; without this an expression like "1.0 / 0.0" -- the
		// INFINITY macro -- reported no type at all, so the division was
		// emitted as an integer one.
		if n.BigWords != nil {
			return &frontend.Type{Kind: frontend.KBitInt, Bits: n.BigBits, Signed: n.BigSigned}
		}
		if n.Kind == frontend.TF128 {
			// A long double literal is binary128; an exprType of int would
			// send "1.0L / 3.0L" down the integer sdiv path, where the
			// operands are the literal's raw bit patterns read as decimals.
			return frontend.LongDoubleType()
		}
		if n.Kind == frontend.TDouble {
			if n.IsFloat {
				return frontend.FloatType()
			}
			return frontend.DoubleType()
		}
		if n.Wide {
			return frontend.WCharType()
		}
		return numLitType(n)
	case *frontend.StrLit:
		// A string literal is an array of char, which decays to a pointer when
		// it is used as a value.
		return &frontend.Type{Kind: frontend.KArr, Elem: &frontend.Type{Kind: frontend.KInt, Width: 1, Signed: true}}
	case *frontend.CompoundLit:
		// The unnamed object's own declared type (arrays included: callers
		// decide between value-address and decay handling).
		return n.Typ
	case *frontend.GenericExpr:
		// The selection's type is the type of the chosen branch (the
		// controlling expression's own type is irrelevant after the pick).
		if n.Chosen != nil {
			return tr.exprType(n.Chosen)
		}
		return nil
	case *frontend.Ident:
		if vi, ok := tr.lookupVar(n.Name); ok {
			return vi.ty
		}
		// A static local's type is registered under its .data label, not its
		// source name (which may even collide with a file-scope global).
		if lab, ok := tr.staticLabel(n.Name); ok {
			if gt, ok := tr.globalType(lab); ok {
				return gt
			}
		}
		// A function designator decays to a pointer to that function.
		if fd, ok := tr.funcDef(n.Name); ok {
			ft := frontend.FuncType(fd.Ret, fd.ParamTypes)
			ft.Variadic = fd.Variadic
			return frontend.PtrType(ft)
		}
		if gt, ok := tr.globalType(n.Name); ok {
			return gt
		}
		// An enumeration constant has type int in C (6.4.4.3), and it has
		// to say so here. A nil left the value untyped, so coercing it to a
		// narrower destination short-circuited on "v.ty == nil" and stored
		// the i32 straight in: "enum Small s = C" wrote four bytes into the
		// enum's one-byte slot, and reading it back gave zero.
		if _, ok := tr.constValue(n.Name); ok {
			return frontend.IntType()
		}
		return nil
	case *frontend.Unary:
		// frontend.Unary '-' and '~' keep the operand's (promoted) type; this matters
		// for width-sensitive nesting such as "~x | y" or "z = ~mask", where a
		// nil here would make an enclosing operator pick the wrong width.
		if n.Op == "-" || n.Op == "~" {
			return tr.exprType(n.E)
		}
		if n.Op == "*" {
			if t := tr.exprType(n.E); t != nil && t.IsPtr() {
				return t.Elem
			}
		}
		if n.Op == "&" {
			// "&x" is a pointer to x's type.
			if t := tr.exprType(n.E); t != nil {
				if t.IsFunc() {
					return frontend.PtrType(t)
				}
				if t.IsPtr() && t.Elem != nil && t.Elem.IsFunc() {
					return t
				}
				return frontend.PtrType(t)
			}
		}
	case *frontend.Index:
		if t := tr.exprType(n.Base); t != nil {
			if t.IsPtr() || t.IsArray() {
				return t.Elem
			}
		}
	case *frontend.Binary:
		// Pointer arithmetic keeps a pointer type, so "(p + 1) - base" is
		// still pointer-minus-pointer.
		return tr.binaryType(n)
	case *frontend.AssignExpr:
		// An assignment yields the left operand's value, and its type is the
		// left operand's type -- "(*d++ = *src++) != 0" compares a char against
		// zero, and reading the type from anywhere else made the comparison
		// i32 against a one-byte load.
		return tr.exprType(n.Lhs)
	case *frontend.AssignStmt:
		return tr.exprType(n.Lhs)
	case *frontend.CondExpr:
		// A ternary yields the common type of its two arms, after the usual
		// conversions. Without this the arm types were invisible, so an
		// enclosing operator saw no type and fell back to the default width:
		// "(y >= 0 ? y : y - 399) / 400" divided an i64 phi by an i32.
		return arithCommon(tr.exprType(n.Then), tr.exprType(n.Else))
	case *frontend.IncDecExpr:
		return tr.exprType(n.E)
	case *frontend.TmpLoad:
		// Internal node produced only by genCompoundAssign: the type is the
		// parked lvalue's static type.
		return n.Typ
	case *frontend.MemberExpr:
		return tr.memberType(n.Base, n.Name)
	case *frontend.CastExpr:
		return n.Typ
	case *frontend.Call:
		// A call's type is the callee's declared return type. A function
		// defined in this module or supplied by the C runtime (goclib /
		// extern) both count -- the latter matters because some library
		// functions return long (i64 on this target), and treating their
		// result as int made a comparison against them emit "icmp i32" while
		// the call itself was "i64", which LLVM rejects. Only a call with no
		// known declaration falls back to int, which is the common case.
		if fd, ok := tr.funcDef(n.Name); ok {
			return fd.Ret
		}
		// The name may designate a function-pointer VARIABLE rather than a
		// function: its result type comes from the pointer's static type.
		if _, ft, ok := tr.fnPtrVar(n.Name); ok {
			return ft.Ret
		}
		if fd, ok := tr.libFunc(n.Name); ok {
			return fd.Ret
		}
		return nil
	case *frontend.IndirectCall:
		// A UFCS method call types like the direct call it was rewritten to.
		if n.UFCS != nil {
			if fd, ok := tr.funcDef(n.UFCS.Name); ok {
				return fd.Ret
			}
			return nil
		}
		if ft := frontend.FuncTypeOf(tr.exprType(n.Fn)); ft != nil {
			return ft.Ret
		}
		return nil
	}
	return nil
}

// memberType resolves the type of a struct/union member access.
func (tr *typeResolver) memberType(base frontend.Expr, name string) *frontend.Type {
	t := tr.exprType(base)
	// "->" on an array-typed base is legal C too: it decays to a pointer, so
	// "buffer->field" addresses element 0. Both spellings unwrap to the elem.
	if t != nil && (t.Kind == frontend.KPtr || t.Kind == frontend.KArr) && t.Elem != nil {
		t = t.Elem
	}
	if t == nil || (t.Kind != frontend.KStruct && t.Kind != frontend.KUnion) {
		return nil
	}
	for _, m := range t.Members {
		if m.Name == name {
			return m.Type
		}
	}
	return nil
}

// binaryType mirrors the checker's rules (checkBinary) so the translator can
// classify an expression that is itself arithmetic.
//
// The runtime lowering already widens operands through arithCommon; what this
// must do is report, for a node that is itself an operand of a larger
// expression, the type that node will have. A pure arithmetic node (a+b, x/64,
// a&b) therefore resolves to the usual arithmetic common type, not nil -- a nil
// here makes an enclosing operator pick the wrong width and emit, say, an i32
// sdiv against an i64 operand.
func (tr *typeResolver) binaryType(n *frontend.Binary) *frontend.Type {
	lt := tr.exprType(n.L)
	rt := tr.exprType(n.R)
	if frontend.IsBig(lt) || frontend.IsBig(rt) {
		switch n.Op {
		case "+", "-", "*", "/", "%", "<<", ">>", "&", "|", "^":
			if (lt == nil || lt.IsIntClass() || frontend.IsBig(lt)) && (rt == nil || rt.IsIntClass() || frontend.IsBig(rt)) {
				lt2 := lt
				if lt2 == nil {
					lt2 = frontend.IntType()
				}
				rt2 := rt
				if rt2 == nil {
					rt2 = frontend.IntType()
				}
				return frontend.BigArithResult(n.Op, lt2, rt2)
			}
		}
		return nil
	}
	switch n.Op {
	case "&&", "||":
		// Logical operators yield int in C; the IR path short-circuits them,
		// but a surrounding expression still needs a width to resolve against.
		return frontend.IntType()
	case "==", "!=", "<", ">", "<=", ">=":
		// A comparison's result type is int in C, whatever the operands are.
		// Returning the common operand type here instead (which this did, to
		// give a surrounding expression "a width to resolve against") made
		// "(a != b)" look like a double, and then "long v; v += (a != b)"
		// resolved its common type as double and emitted
		//   sitofp i64 -> double, fadd double, fptosi double -> i64
		// for what is an integer add. The emitter widens each operand to the
		// common type on its own, so nothing here needs the operand type --
		// a width-sensitive surrounding expression gets int, which is what
		// C says the comparison yields, and integer promotion of int is a
		// no-op anyway.
		return frontend.IntType()
	case "+", "-":
		if lt != nil && lt.IsPtr() && rt != nil && rt.IsPtr() {
			if n.Op == "-" {
				// ptrdiff_t: 64 bits on this target, like the value the
				// emitter produces for the difference.
				return &frontend.Type{Kind: frontend.KInt, Width: 8, Signed: true}
			}
			return nil
		}
		if lt != nil && (lt.IsPtr() || lt.IsArray()) && (rt == nil || (!rt.IsPtr() && !rt.IsArray())) {
			if lt.IsArray() && lt.Elem != nil {
				return frontend.PtrType(lt.Elem)
			}
			return lt
		}
		if rt != nil && (rt.IsPtr() || rt.IsArray()) && (lt == nil || (!lt.IsPtr() && !lt.IsArray())) {
			if rt.IsArray() && rt.Elem != nil {
				return frontend.PtrType(rt.Elem)
			}
			return rt
		}
		return arithCommon(lt, rt)
	case "*", "/", "%", "&", "|", "^":
		return arithCommon(lt, rt)
	case "<<", ">>":
		// A shift keeps the left operand's type; the right operand only sizes it.
		if lt != nil {
			return lt
		}
		return arithCommon(lt, rt)
	}
	return nil
}

// memberOffset resolves the byte offset of a struct member within its struct,
// walking the same path the checker uses.
//
// mem.Offset is already absolute for a promoted (anonymous-member) field:
// computeLayout's final pass rewrote it to the shell's position plus the
// offset the field had inside the anonymous type (frontend/types.go). Adding
// AnonBase.Offset here as well counted the shell a second time -- s.x came
// out at 8 instead of 4, s.a at 24 instead of 12 -- while sizeof stayed
// right, because the shell itself is laid out as an ordinary member and only
// the promoted fields' offsets were doubled.
func (tr *typeResolver) memberOffset(n *frontend.MemberExpr, sty *frontend.Type) (int, bool) {
	st := sty
	if n.Arrow {
		if st == nil || st.Kind != frontend.KPtr || st.Elem == nil {
			return 0, false
		}
		st = st.Elem
	}
	if st == nil || (st.Kind != frontend.KStruct && st.Kind != frontend.KUnion) {
		return 0, false
	}
	for _, mem := range st.Members {
		if mem.Name == n.Name {
			return mem.Offset, true
		}
	}
	return 0, false
}

// calleeParams returns the declared parameter types of a function, whether it
// is defined in this program or supplied by the C runtime.
func (tr *typeResolver) calleeParams(name string) ([]*frontend.Type, bool) {
	if fd, ok := tr.funcDef(name); ok {
		return fd.ParamTypes, true
	}
	if fd, ok := tr.libFunc(name); ok {
		return fd.ParamTypes, true
	}
	return nil, false
}

// calleeRet returns a function's declared return type.
func (tr *typeResolver) calleeRet(name string) *frontend.Type {
	if fd, ok := tr.funcDef(name); ok {
		return fd.Ret
	}
	if fd, ok := tr.libFunc(name); ok {
		return fd.Ret
	}
	return frontend.IntType()
}

// constValue resolves a name with no storage to an integer constant, which is
// what an enum member looks like by the time code generation runs.
func (tr *typeResolver) constValue(name string) (int64, bool) {
	if v, ok := frontend.EnumConsts[name]; ok {
		return v, true
	}
	return 0, false
}

// fnPtrVar reports whether name denotes a function-pointer variable (or
// global), returning its function type.
func (tr *typeResolver) fnPtrVar(name string) (frontend.Expr, *frontend.Type, bool) {
	if vi, ok := tr.lookupVar(name); ok {
		if ft := frontend.FuncTypeOf(vi.ty); ft != nil {
			return &frontend.Ident{Name: name}, ft, true
		}
	}
	if gt, ok := tr.globalType(name); ok {
		if ft := frontend.FuncTypeOf(gt); ft != nil {
			return &frontend.Ident{Name: name}, ft, true
		}
	}
	return nil, nil, false
}
