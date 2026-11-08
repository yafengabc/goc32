package frontend

// Marker builtins: compiler intrinsics that carry no runtime work.
//
// gcc and clang expose a handful of functions that exist only to give the
// optimiser information or to mark a path as impossible. They are real calls
// in the AST, so a front end that does not know them reports them as undefined
// symbols, but there is nothing to lower: the answer is always "do nothing".
//
// Real code calls these. Nim's generated C carries __builtin_unreachable()
// after every noreturn call (its own noreturn procs are marked
// __attribute__((noreturn))), and __builtin_expect / __builtin_assume let a
// program state a branch hint. Accepting them costs one table lookup and makes
// a large body of real-world C compile unchanged.
var markerBuiltins = map[string]MarkerSig{
	// __builtin_unreachable(): control flow never gets here. Emitting nothing
	// is correct because the path is dead; the value the caller assigned before
	// the call is what remains live, and the call itself produces no value.
	"__builtin_unreachable": {args: 0, ret: VoidType()},
	// __builtin_expect(exp, c): exp, with c telling the optimiser which way the
	// branch is likely to go. The value is exp; the hint has no runtime effect
	// in this model.
	"__builtin_expect":                  {args: 2, ret: nil}, // ret nil = first argument's type
	"__builtin_expect_with_probability": {args: 3, ret: nil},
	// __builtin_assume(cond): the optimiser may assume cond holds. No code.
	"__builtin_assume": {args: 1, ret: VoidType()},
	// __builtin_trap(): abort. goclib has no raise(), and the only caller in
	// real code is a __builtin_unreachable sibling on a path that is already
	// dead, so this lowers to the same abort the C library would.
	"__builtin_trap": {args: 0, ret: VoidType()},
}

// MarkerSig describes one marker builtin's shape. A nil ret means "the type of
// the first argument", which is how the branch-hint forms report their value.
type MarkerSig struct {
	args int
	ret  *Type
}

// LookupMarkerBuiltin reports whether name is a marker builtin. Both back ends
// share this table so the recognised set cannot drift apart.
func LookupMarkerBuiltin(name string) (MarkerSig, bool) {
	ms, ok := markerBuiltins[name]
	return ms, ok
}

// checkMarkerBuiltin type-checks a marker builtin: every argument is an
// ordinary expression, and the result is either void or the first argument's
// own type.
func (c *checker) checkMarkerBuiltin(n *Call, fn *FuncDecl, ms MarkerSig) *Type {
	if len(n.Args) != ms.args {
		c.errf(0, "call to %q: expected %d arguments, got %d", n.Name, ms.args, len(n.Args))
		for _, a := range n.Args {
			c.checkExpr(a, fn)
		}
		return IntType()
	}
	var first *Type
	for i, a := range n.Args {
		t := c.checkExpr(a, fn)
		if i == 0 {
			first = t
		}
	}
	if ms.ret != nil {
		return ms.ret
	}
	if first == nil {
		return IntType()
	}
	return first
}
