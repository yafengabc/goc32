package gocl

import "goc/frontend"

// collectVaListNames records, in out, the name of every identifier the body
// uses as a va_list: the operand of va_start / va_arg / va_end / va_copy.
//
// It exists because the front end types va_list as `char *`
// (frontend/parser.go: typedefs["va_list"] = PtrType(CharType())), which is
// indistinguishable from an ordinary pointer, so a call that forwards a
// va_list cannot be recognised from the types alone. On Windows x64 that
// ambiguity is harmless -- a va_list's value IS the cursor pointer, exactly
// what a `char *` read yields. On x86-64 SysV it is not: a va_list is the
// 24-byte __va_list_tag, and reading it as a `char *` hands the callee the
// first eight bytes, which are two packed cursor integers rather than an
// address. So the emitter has to know which names are va_lists, and it has to
// know before it lowers any call: a va_list may be forwarded before the
// va_start that would otherwise have revealed it, as in
//
//	void log_it(const char *fmt, va_list ap);
//	void outer(const char *fmt, ...) { va_list ap; va_start(ap, fmt); log_it(fmt, ap); }
//
// where the forward at the top is the first mention of `ap` in evaluation
// order only because the call is what the sweep has already seen.
//
// The sweep is a plain structural walk. A miss is not a crash: an unlisted
// name simply falls back to the `char *` reading, which is the Windows
// behaviour -- so the failure mode is the old, already-diagnosed one rather
// than a new one.
func collectVaListNames(n interface{}, out map[string]bool) {
	switch v := n.(type) {
	case nil:
		return

	// --- statements -------------------------------------------------------
	case *frontend.Block:
		for _, s := range v.Stmts {
			collectVaListNames(s, out)
		}
	case *frontend.DeclList:
		for _, d := range v.Decls {
			collectVaListNames(d, out)
		}
	case *frontend.DeclStmt:
		collectVaListNames(v.Init, out)
	case *frontend.ExprStmt:
		collectVaListNames(v.E, out)
	case *frontend.AssignStmt:
		collectVaListNames(v.Lhs, out)
		collectVaListNames(v.Rhs, out)
	case *frontend.ReturnStmt:
		collectVaListNames(v.E, out)
	case *frontend.IfStmt:
		collectVaListNames(v.Cond, out)
		collectVaListNames(v.Then, out)
		collectVaListNames(v.Else, out)
	case *frontend.WhileStmt:
		collectVaListNames(v.Cond, out)
		collectVaListNames(v.Body, out)
	case *frontend.DoWhileStmt:
		collectVaListNames(v.Body, out)
		collectVaListNames(v.Cond, out)
	case *frontend.ForStmt:
		collectVaListNames(v.Init, out)
		collectVaListNames(v.Cond, out)
		collectVaListNames(v.Post, out)
		collectVaListNames(v.Body, out)
	case *frontend.SwitchStmt:
		collectVaListNames(v.Src, out)
		collectVaListNames(v.Body, out)
	case *frontend.CaseStmt, *frontend.DefaultStmt:
		// Labels only: a case value is a folded integer constant, and the
		// statements that follow live in the enclosing Block.
	case *frontend.LabelStmt:
		collectVaListNames(v.Stmt, out)
	case *frontend.GotoStmt, *frontend.BreakStmt, *frontend.ContinueStmt,
		*frontend.NumLit, *frontend.StrLit, *frontend.Ident,
		*frontend.SizeofExpr, *frontend.AsmStmt:
		// No sub-expressions to reach a va_list through.

	// --- expressions ------------------------------------------------------
	case *frontend.Call:
		if isVaBuiltin(v.Name) {
			markVaListIdent(v.Args, out)
		}
		for _, a := range v.Args {
			collectVaListNames(a, out)
		}
	case *frontend.IndirectCall:
		collectVaListNames(v.Fn, out)
		for _, a := range v.Args {
			collectVaListNames(a, out)
		}
	case *frontend.VaArgExpr:
		markVaListIdent([]frontend.Expr{v.Ap}, out)
	case *frontend.Unary:
		collectVaListNames(v.E, out)
	case *frontend.Binary:
		collectVaListNames(v.L, out)
		collectVaListNames(v.R, out)
	case *frontend.AssignExpr:
		collectVaListNames(v.Lhs, out)
		collectVaListNames(v.Rhs, out)
	case *frontend.TmpLoad:
		// A saved temporary: a slot number, not a sub-expression.
	case *frontend.CommaExpr:
		collectVaListNames(v.Left, out)
		collectVaListNames(v.Right, out)
	case *frontend.Index:
		collectVaListNames(v.Base, out)
		collectVaListNames(v.Idx, out)
	case *frontend.MemberExpr:
		collectVaListNames(v.Base, out)
	case *frontend.CondExpr:
		collectVaListNames(v.Cond, out)
		collectVaListNames(v.Then, out)
		collectVaListNames(v.Else, out)
	case *frontend.CastExpr:
		collectVaListNames(v.E, out)
	case *frontend.IncDecExpr:
		collectVaListNames(v.E, out)
	case *frontend.BraceInit:
		for _, el := range v.Elems {
			collectVaListNames(el, out)
		}
	case *frontend.CompoundLit:
		collectVaListNames(v.Init, out)
	}
}

// isVaBuiltin reports whether name is one of the variadic built-ins whose first
// operand is a va_list.
func isVaBuiltin(name string) bool {
	switch name {
	case "va_start", "va_end", "va_copy":
		return true
	}
	return false
}

// markVaListIdent records Args[0] when it is a plain identifier. A computed
// va_list operand (`va_arg(*p, int)`, `va_start(va, fmt)`) has no name to
// record; the emitter handles that case where it arises.
func markVaListIdent(args []frontend.Expr, out map[string]bool) {
	if len(args) == 0 {
		return
	}
	if id, ok := args[0].(*frontend.Ident); ok {
		out[id.Name] = true
	}
}
