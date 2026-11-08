package frontend

// GCC's overflow-checked arithmetic builtins.
//
//   int __builtin_saddll_overflow(long long a, long long b, long long *res)
//
// stores a+b in *res and returns 1 when the exact result did not fit the type
// of *res. C cannot express that: "res = a + b" wraps silently, and every
// range test written in C has to redo the arithmetic in a wider type -- which
// is what makes these builtins, not library functions. They are the mechanism
// behind every checked-arithmetic language that targets C, Nim's
// --overflowChecks included, so a front end that does not know them cannot
// compile that output at all.
//
// The operand width is part of the name: sadd/smul is int (32-bit), saddl is
// long (64-bit here, as everywhere on LP64) and saddll is long long. The
// generic __builtin_add_overflow has no width in its name and takes it from
// the result pointer instead, which is why Width 0 means "derive it".

// An OverflowBuiltin describes one of the builtins: which operation it
// performs, and the width and signedness the result has to fit.
type OverflowBuiltin struct {
	// Op is "+", "-" or "*".
	Op string
	// Signed is whether the comparison is signed, from the s/u prefix of the
	// name. It is ignored for the generic form, which reads it from the
	// result type.
	Signed bool
	// Width is the result width in bytes: 4 for the int forms, 8 for the
	// long/long long ones. Zero marks the generic form, which takes the width
	// -- and the signedness -- from the result pointer.
	Width int
}

var overflowBuiltins = map[string]OverflowBuiltin{
	// The generic forms. GCC infers the checked type from the result operand,
	// so these carry no width of their own.
	"__builtin_add_overflow": {Op: "+"},
	"__builtin_sub_overflow": {Op: "-"},
	"__builtin_mul_overflow": {Op: "*"},

	// int (32-bit).
	"__builtin_sadd_overflow": {Op: "+", Signed: true, Width: 4},
	"__builtin_ssub_overflow": {Op: "-", Signed: true, Width: 4},
	"__builtin_smul_overflow": {Op: "*", Signed: true, Width: 4},
	"__builtin_uadd_overflow": {Op: "+", Width: 4},
	"__builtin_usub_overflow": {Op: "-", Width: 4},
	"__builtin_umul_overflow": {Op: "*", Width: 4},

	// long. goc is LP64, so long and long long are the same 64-bit type and
	// the two spellings share a lowering.
	"__builtin_saddl_overflow":  {Op: "+", Signed: true, Width: 8},
	"__builtin_ssubl_overflow":  {Op: "-", Signed: true, Width: 8},
	"__builtin_smull_overflow":  {Op: "*", Signed: true, Width: 8},
	"__builtin_uaddl_overflow":  {Op: "+", Width: 8},
	"__builtin_usubl_overflow":  {Op: "-", Width: 8},
	"__builtin_umull_overflow":  {Op: "*", Width: 8},
	"__builtin_saddll_overflow": {Op: "+", Signed: true, Width: 8},
	"__builtin_ssubll_overflow": {Op: "-", Signed: true, Width: 8},
	"__builtin_smulll_overflow": {Op: "*", Signed: true, Width: 8},
	"__builtin_uaddll_overflow": {Op: "+", Width: 8},
	"__builtin_usubll_overflow": {Op: "-", Width: 8},
	"__builtin_umulll_overflow": {Op: "*", Width: 8},
}

// LookupOverflowBuiltin reports whether name is one of the builtins above. Both
// back ends share this table so the recognised set cannot drift apart.
func LookupOverflowBuiltin(name string) (OverflowBuiltin, bool) {
	ob, ok := overflowBuiltins[name]
	return ob, ok
}

// checkOverflowBuiltin type-checks one of the builtins. All three arguments are
// ordinary expressions -- two operands and the address of the result -- and the
// builtin yields the 0/1 overflow flag as an int.
func (c *checker) checkOverflowBuiltin(n *Call, fn *FuncDecl, ob OverflowBuiltin) *Type {
	if len(n.Args) != 3 {
		c.errf(0, "call to %q: expected 3 arguments, got %d", n.Name, len(n.Args))
		for _, a := range n.Args {
			c.checkExpr(a, fn)
		}
		return IntType()
	}
	var res *Type
	for i, a := range n.Args {
		t := c.checkExpr(a, fn)
		if i == 2 {
			res = t
		}
	}
	// The generic form has to see a pointer, because that is the only place
	// its checked type is written down.
	if ob.Width == 0 && (res == nil || !res.IsPtr() || res.Elem == nil) {
		c.errf(0, "%s: third argument must be a pointer to the result", n.Name)
	}
	return IntType()
}
