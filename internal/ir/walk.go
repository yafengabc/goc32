package gocl

// walk.go carries the expression walker the IR translator uses for
// reachability and TLS analysis. It was part of the LLVM backend seam in the
// original gocl; here it is independent of the (removed) DLL loader.

import "goc/frontend"

func walkExprsIn(s frontend.Stmt, fn func(frontend.Expr)) {
	frontend.WalkStmts(s, func(x frontend.Stmt) {
		for _, e := range stmtExprs(x) {
			walkExpr(e, fn)
		}
	})
}

func stmtExprs(s frontend.Stmt) []frontend.Expr {
	switch n := s.(type) {
	case *frontend.DeclStmt:
		return []frontend.Expr{n.Init}
	case *frontend.AssignStmt:
		return []frontend.Expr{n.Lhs, n.Rhs}
	case *frontend.ExprStmt:
		return []frontend.Expr{n.E}
	case *frontend.ReturnStmt:
		return []frontend.Expr{n.E}
	case *frontend.IfStmt:
		return []frontend.Expr{n.Cond}
	case *frontend.WhileStmt:
		return []frontend.Expr{n.Cond}
	case *frontend.DoWhileStmt:
		return []frontend.Expr{n.Cond}
	case *frontend.SwitchStmt:
		return []frontend.Expr{n.Src}
	case *frontend.ForStmt:
		out := []frontend.Expr{n.Cond, n.Post}
		if e := firstExprOf(n.Init); e != nil {
			out = append(out, e)
		}
		return out
	}
	return nil
}

func firstExprOf(s frontend.Stmt) frontend.Expr {
	if s == nil {
		return nil
	}
	if es := stmtExprs(s); len(es) > 0 {
		return es[0]
	}
	return nil
}

func walkExpr(e frontend.Expr, fn func(frontend.Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch n := e.(type) {
	case *frontend.Call:
		for _, a := range n.Args {
			walkExpr(a, fn)
		}
	case *frontend.IndirectCall:
		walkExpr(n.Fn, fn)
		for _, a := range n.Args {
			walkExpr(a, fn)
		}
	case *frontend.Unary:
		walkExpr(n.E, fn)
	case *frontend.Binary:
		walkExpr(n.L, fn)
		walkExpr(n.R, fn)
	case *frontend.AssignExpr:
		walkExpr(n.Lhs, fn)
		walkExpr(n.Rhs, fn)
	case *frontend.CommaExpr:
		walkExpr(n.Left, fn)
		walkExpr(n.Right, fn)
	case *frontend.Index:
		walkExpr(n.Base, fn)
		walkExpr(n.Idx, fn)
	case *frontend.CondExpr:
		walkExpr(n.Cond, fn)
		walkExpr(n.Then, fn)
		walkExpr(n.Else, fn)
	case *frontend.CastExpr:
		walkExpr(n.E, fn)
	case *frontend.IncDecExpr:
		walkExpr(n.E, fn)
	case *frontend.MemberExpr:
		walkExpr(n.Base, fn)
	case *frontend.VaArgExpr:
		walkExpr(n.Ap, fn)
	case *frontend.GenericExpr:
		walkExpr(n.Chosen, fn)
	case *frontend.CompoundLit:
		if n.Init != nil {
			for _, el := range n.Init.Elems {
				walkExpr(el.E, fn)
			}
		}
	case *frontend.SizeofExpr:
		walkExpr(n.E, fn)
	}
}
