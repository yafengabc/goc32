package frontend

// C11 7.17 <stdatomic.h> builtins.
//
// atomic_fetch_add and its siblings cannot be written in C. Each one is a
// single locked read-modify-write that hands back the value the object held
// BEFORE the update, and no C expression can produce that: "old = *p; *p += v"
// is two operations, and a concurrent update between them is lost. goclib is
// deliberately pure C -- the LLVM back end cannot translate inline asm -- so
// <stdatomic.h> maps the standard names onto these builtins and each back end
// lowers them to its own atomic primitive.
//
// Every one of them takes the address of the atomic object first. The width
// and signedness of the locked access come from the pointee, never from the
// operand: atomic_fetch_add on an _Atomic char must not touch the seven bytes
// that follow it.

// An AtomicBuiltin describes what one of the builtins does.
type AtomicBuiltin struct {
	// Op is the binary operator applied to (old value, operand). It is empty
	// for atomic_exchange, which stores the operand unchanged.
	Op string
	// CAS marks the compare-exchange form, whose signature and result differ:
	// (object, expected, desired) -> bool, with *expected updated on failure.
	CAS bool
	// GCCOrder marks the GCC __atomic_* spelling of the builtin. Those take
	// trailing arguments the model has no use for -- a memory_order for every
	// form, plus a "weak" flag in front of the two orders for the
	// compare-exchange form:
	//
	//	__atomic_load_n(p, order)
	//	__atomic_store_n(p, v, order)
	//	__atomic_exchange_n(p, v, order)
	//	__atomic_fetch_add(p, v, order)
	//	__atomic_compare_exchange_n(p, e, d, weak, succ, fail)
	//
	// GCCOrderArgs is how many arguments that is. They are accepted and
	// ignored, exactly as stdatomic.h's macros do with their _explicit forms.
	GCCOrderArgs int
	// NoResult marks a GCC builtin whose C result is discarded (__atomic_store_n
	// returns void, while __goc_atomic_exchange returns the old value). Only the
	// GCC spellings that way round the value are marked.
	NoResult bool
	// LoadOnly marks __atomic_load_n, whose only argument is the object
	// pointer: it has no value operand to apply Op to.
	LoadOnly bool
}

var atomicBuiltins = map[string]AtomicBuiltin{
	"__goc_atomic_fetch_add":        {Op: "+"},
	"__goc_atomic_fetch_sub":        {Op: "-"},
	"__goc_atomic_fetch_and":        {Op: "&"},
	"__goc_atomic_fetch_or":         {Op: "|"},
	"__goc_atomic_fetch_xor":        {Op: "^"},
	"__goc_atomic_exchange":         {},
	"__goc_atomic_compare_exchange": {CAS: true},

	// The GCC __atomic_* family. Real code calls these directly: Nim's
	// nimbase.h falls back to them when the host has no C11 <stdatomic.h>, and
	// they are the spelling every GCC/Clang program uses. They differ from the
	// __goc_ forms only in the trailing arguments and in __atomic_store_n
	// returning void, so they share one lowering.
	"__atomic_load_n":             {GCCOrderArgs: 1, LoadOnly: true},
	"__atomic_store_n":            {GCCOrderArgs: 1, NoResult: true},
	"__atomic_exchange_n":         {Op: "", GCCOrderArgs: 1},
	"__atomic_compare_exchange_n": {CAS: true, GCCOrderArgs: 3},
	"__atomic_fetch_add":          {Op: "+", GCCOrderArgs: 1},
	"__atomic_fetch_sub":          {Op: "-", GCCOrderArgs: 1},
	"__atomic_fetch_and":          {Op: "&", GCCOrderArgs: 1},
	"__atomic_fetch_or":           {Op: "|", GCCOrderArgs: 1},
	"__atomic_fetch_xor":          {Op: "^", GCCOrderArgs: 1},
}

// LookupAtomicBuiltin reports whether name is one of the builtins above. Both
// back ends share this table so the recognised set cannot drift apart.
func LookupAtomicBuiltin(name string) (AtomicBuiltin, bool) {
	ab, ok := atomicBuiltins[name]
	return ab, ok
}

// atomicValueType returns the value type of the atomic object a builtin's
// first argument points at: the pointee with the Atomic flag dropped, since
// the fetch family returns a plain value, not an atomic lvalue. It reports a
// diagnostic and returns nil when the argument is not a pointer to an integer
// or _Bool.
func (c *checker) atomicValueType(n *Call, fn *FuncDecl) *Type {
	pt := c.checkExpr(n.Args[0], fn)
	if pt == nil || !pt.IsPtr() || pt.Elem == nil {
		got := "an untyped expression"
		if pt != nil {
			got = pt.String()
		}
		c.errf(0, "%s: first argument must be a pointer to an _Atomic integer, got %s", n.Name, got)
		return nil
	}
	et := pt.Elem
	// C11 7.17.7.2 and the GCC __atomic_* builtins both cover every
	// trivially-copyable scalar, which includes pointers; a lock-free
	// allocator (Nim's own, for one) keeps its free lists in atomic pointer
	// slots and needs exactly that. The locked access is one instruction wide
	// either way, so the model widens rather than restricting.
	if et.Kind != KInt && et.Kind != KBool && et.Kind != KPtr {
		c.errf(0, "%s: atomic operations are only defined on integer and pointer types, got %s", n.Name, et)
		return nil
	}
	res := *et
	res.Atomic = false
	return &res
}

// checkAtomicBuiltin type-checks one of the builtins. The result of the fetch
// family and of atomic_exchange is the object's value type; the result of the
// compare-exchange form is _Bool.
func (c *checker) checkAtomicBuiltin(n *Call, fn *FuncDecl, ab AtomicBuiltin) *Type {
	// The GCC spelling appends arguments the model has no use for (see
	// AtomicBuiltin.GCCOrderArgs); __atomic_load_n has no value operand at
	// all, so its counted arity is one below the others'.
	want := 2
	if ab.CAS {
		want = 3
	}
	if ab.LoadOnly {
		want = 1
	}
	total := want + ab.GCCOrderArgs
	if len(n.Args) != total {
		c.errf(0, "call to %q: expected %d arguments, got %d", n.Name, total, len(n.Args))
		for _, a := range n.Args {
			c.checkExpr(a, fn)
		}
		return IntType()
	}
	// The trailing arguments are compile-time constants by definition, but they
	// still have to type-check as expressions.
	for _, a := range n.Args[want:] {
		c.checkExpr(a, fn)
	}
	// A load takes only the object pointer, so it yields that pointer's value
	// without consulting an operand.
	if ab.LoadOnly {
		vt := c.atomicValueType(n, fn)
		if vt == nil {
			return IntType()
		}
		return vt
	}
	vt := c.atomicValueType(n, fn)
	// The remaining arguments are ordinary expressions; expected is a pointer
	// to the value type and desired is a value.
	for _, a := range n.Args[1:want] {
		c.checkExpr(a, fn)
	}
	if ab.CAS {
		return &Type{Kind: KBool, Width: 1, Signed: true}
	}
	if ab.NoResult {
		return VoidType()
	}
	if vt == nil {
		return IntType()
	}
	return vt
}
