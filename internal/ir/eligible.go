package gocl

// eligible.go keeps the eligibility analysis the IR translator uses to decide
// which functions it can model. The original file also drove libLLVM; that
// half is gone, so only the pure predicates remain.

import "goc/frontend"

func llvmEligible(f *frontend.FuncDecl) bool {
	if f == nil || f.Body == nil {
		return false
	}
	if typUnsupported(f.Ret) {
		return false
	}
	for _, pt := range f.ParamTypes {
		if typUnsupported(pt) {
			return false
		}
	}
	return !usesUnsupportedLLVM(f.Body)
}

func usesUnsupportedLLVM(s frontend.Stmt) bool {
	found := false
	frontend.WalkStmts(s, func(x frontend.Stmt) {
		switch n := x.(type) {
		case *frontend.AsmStmt:
			found = true
		case *frontend.DeclStmt:
			if n.Typ != nil && typUnsupported(n.Typ) {
				found = true
			}
		}
	})
	if found {
		return true
	}
	ok := true
	frontend.WalkStmts(s, func(x frontend.Stmt) {
		walkExprsIn(x, func(e frontend.Expr) {
			switch n := e.(type) {
			case *frontend.MemberExpr:
				if memberIsBitField(n) {
					ok = false
				}
			case *frontend.DeclStmt:
				if n.Typ != nil && typUnsupported(n.Typ) {
					ok = false
				}
			case *frontend.CastExpr:
				if n.Typ != nil && typUnsupported(n.Typ) {
					ok = false
				}
			case *frontend.SizeofExpr:
				if n.Typ != nil && typUnsupported(n.Typ) {
					ok = false
				}
			}
		})
	})
	return !ok
}

func memberIsBitField(n *frontend.MemberExpr) bool {
	for _, t := range frontend.Structs() {
		for _, m := range t.Members {
			if m.Name == n.Name && m.BitWidth > 0 {
				return true
			}
		}
	}
	return false
}

func typUnsupported(t *frontend.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind == frontend.KBitInt {
		return true
	}
	if t.Kind == frontend.KArr {
		return typUnsupported(t.Elem)
	}
	if t.Kind == frontend.KStruct || t.Kind == frontend.KUnion {
		for _, mem := range t.Members {
			if typUnsupported(mem.Type) {
				return true
			}
			if mem.BitWidth > 0 {
				return true
			}
		}
	}
	return false
}
