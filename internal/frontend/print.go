package frontend

// print: a Python-style print builtin, lowered to printf at check time.
//
// C values carry no runtime type tags, but the checker sees every
// argument's static type -- so print(expr, ...) is a compiler builtin that
// builds one format string from those types and renames the call to printf
// with the literal prepended. The generated call rides the existing printf
// machinery (variadic GP-slot marshalling, the LC string pool) untouched.
//
// Single-argument calls on common shapes are dispatched to thin goclib
// primitives instead, so the weight of the vfmt interpreter is not linked
// in when only print("str") / print(int) / print(long) / print() are used:
//   - no arguments            -> str_print("")   (just the newline)
//   - char*                   -> str_print(s)    (char arrays decay here)
//   - int / char / _Bool      -> int_print(v)
//   - long                    -> long_print(v)
//   - short[N]/int[N]/long[N] ident -> the matching *_array_print printer
//                               (bool/float/double have their own printers
//                               too); Python-style "[1, 2, 3]". The checker
//                               knows KArr.Len so the count is a compile-time
//                               constant -- a C array carries no length at
//                               runtime. char arrays are NOT included: string
//                               semantics win, they lower via the char* decay
//                               above. A struct/union array with a tag T
//                               dispatches to a user-defined T_array_print --
//                               the same T_f convention as UFCS methods --
//                               so any element type the user can name has a
//                               printable array form. Non-identifier
//                               expressions fall to the %p path.
//   - anything else (floats, other pointers, structs) -> printf, below
//
// Argument conversion, deliberately honest about C semantics:
//   - integers (and _Bool)  -> %d   (a char prints as its numeric value)
//   - float / double        -> %g   (fixed point, trailing zeros stripped)
//   - char*                 -> %s   (string literals and char arrays decay)
//   - any other pointer     -> %p   (an int-array name decays to elem* first)
//   - struct / union        -> rejected: define a T_print method and call
//     it as x.print() (UFCS) instead
//
// Arguments are separated by one space and the line ends with a newline;
// print() with no arguments is just the newline. The result type is
// printf's (int): the number of characters the line would have written.

import "fmt"

// rewritePrint rewrites a print(...) Call in place into the equivalent
// printf call and type-checks it against printf's goclib prototype. It
// keeps going after a bad argument (with %d substituted) so one erroneous
// argument reports exactly once.
func (c *checker) rewritePrint(n *Call, fn *FuncDecl) *Type {
	// Static dispatch: a single argument whose static type is a printable
	// scalar or string lowers to the thin str_print / int_print / long_print
	// primitives -- no format interpreter linked. Anything
	// else (multiple arguments, floats, non-string pointers, structs, an
	// already-errored argument) falls through to the printf lowering.
	typed := make([]*Type, len(n.Args))
	// A bare array identifier is recognised by its declared (KArr) type --
	// checkExpr would already have decayed it to a pointer. char arrays are
	// excluded here so they keep the string path above. A struct/union array
	// without a user printer leaves a hint behind (arrHint) so the caller can
	// point at the T_array_print function to define instead of silently
	// printing the array's address.
	var arrName string
	var arrArgs []Expr
	var arrHint string
	if len(n.Args) == 1 {
		var arrOK bool
		arrName, arrArgs, arrOK, arrHint = c.arrayPrintDispatch(n.Args[0])
		if !arrOK {
			arrName = ""
		}
	}
	if len(n.Args) <= 1 {
		var t *Type
		if len(n.Args) == 1 {
			t = c.checkExpr(n.Args[0], fn)
			typed[0] = t
		}
		switch {
		case len(n.Args) == 0:
			// print() is just the newline: str_print("") writes "\n".
			return c.rewritePrintThin(n, "str_print", []Expr{&StrLit{Bytes: []byte{}}}, fn)
		case t == nil:
			// errored argument: keep going through the printf path so the
			// %d stand-in reports exactly once.
		case t.Kind == KInt && litNeedsLong(n.Args[0]):
			// T1.6 (C3): a plain (possibly negated) integer literal whose
			// magnitude exceeds 32 bits is typed long by C's literal rules
			// (int -> long -> long long), but checkExpr types every NumLit as
			// int regardless of value. The thin print dispatch must widen by
			// value the same way the code generator does, or
			// print(-1234567890123456789) lowers to int_print and the callee
			// reads only the truncated low 32 bits. This sat long latent
			// because the (long) cast inside int_print used to pass the full
			// value through unextended; C3's cast widening exposed it.
			return c.rewritePrintThin(n, "long_print", n.Args, fn)
		case t.Kind == KBool || (t.Kind == KInt && t.Width <= 4):
			return c.rewritePrintThin(n, "int_print", n.Args, fn)
		case t.Kind == KInt && t.Width == 8:
			return c.rewritePrintThin(n, "long_print", n.Args, fn)
		case t.Kind == KPtr && t.Elem != nil && t.Elem.Kind == KInt && t.Elem.Width == 1:
			return c.rewritePrintThin(n, "str_print", n.Args, fn)
		case arrName != "":
			return c.rewritePrintThin(n, arrName, arrArgs, fn)
		}
	}
	// A struct/union array whose T_array_print the user never defined: report
	// the printer to write, then fall through -- the array decays to a
	// pointer and the %p stand-in keeps the program compilable.
	if arrHint != "" {
		c.errf(0, "%s", arrHint)
	}
	fs := make([]byte, 0, 8*len(n.Args)+2)
	for i, a := range n.Args {
		t := typed[i]
		if t == nil {
			t = c.checkExpr(a, fn)
		}
		spec, ok := printSpec(t)
		if !ok {
			hint := ""
			if t != nil && (t.Kind == KStruct || t.Kind == KUnion) && t.Tag != "" {
				hint = fmt.Sprintf(" (define a %s_print method and call it as x.print())", t.Tag)
			}
			c.errf(0, "print: cannot print a value of type %s%s", t, hint)
			spec = "%d"
		}
		if i > 0 {
			fs = append(fs, ' ')
		}
		fs = append(fs, spec...)
	}
	fs = append(fs, '\n')
	args := make([]Expr, 0, len(n.Args)+1)
	args = append(args, &StrLit{Bytes: fs})
	args = append(args, n.Args...)
	n.Name = "printf"
	n.Args = args
	if pd, ok := c.protos["printf"]; ok {
		return c.checkArgs("print", pd.ParamTypes, pd.Variadic, args, fn, pd.Ret)
	}
	return IntType() // unreachable: goclib.h always feeds the prototype table
}

// litNeedsLong reports whether e is an integer literal (possibly wrapped in
// unary minus) whose magnitude exceeds 32 bits. checkExpr types every NumLit
// as int, but C types such a literal long (int -> long -> long long), and the
// code generator widens by value at emission time; the thin print dispatch
// must follow the value so print(-1234567890123456789) lowers to long_print.
func litNeedsLong(e Expr) bool {
	for {
		if u, ok := e.(*Unary); ok && u.Op == "-" {
			e = u.E
			continue
		}
		break
	}
	lit, ok := e.(*NumLit)
	if !ok || lit.IsFloat || lit.BigWords != nil {
		return false
	}
	return lit.Val > 2147483647 || lit.Val < -2147483648
}

// rewritePrintThin renames a print call in place to one of the thin
// single-argument primitives and type-checks it against the goclib
// prototype (or, for a user-defined struct-array printer, the user's own
// function).
func (c *checker) rewritePrintThin(n *Call, name string, args []Expr, fn *FuncDecl) *Type {
	n.Name = name
	n.Args = args
	if fd, ok := c.funcs[name]; ok {
		return c.checkArgs(name, fd.ParamTypes, fd.Variadic, args, fn, fd.Ret)
	}
	if pd, ok := c.protos[name]; ok {
		return c.checkArgs(name, pd.ParamTypes, pd.Variadic, args, fn, pd.Ret)
	}
	return IntType() // unreachable: goclib.h feeds the prototype table, and
	// user printers were resolved by arrayPrintDispatch against c.funcs/c.protos
}

// arrayPrintDispatch recognises a bare array identifier passed to print()
// (e.g. print(a) for "int a[5]") and returns the thin goclib array printer
// plus the rewritten argument list: the identifier itself and its length as
// a compile-time constant. A C array carries no runtime length, but the
// checker sees the declared KArr type, whose Len is exactly what the printer
// needs. The element type picks the printer -- short/int/long dispatch on
// width (KInt), bool/float/double on their own kinds; the unsigned variants
// reuse the same-width signed printer, which reads at the true element width
// (values above the signed max print negative, the printf %d caveat). char
// arrays are deliberately excluded (they are strings and lower via the char*
// case above). A struct/union array with a tag T dispatches to a
// user-defined T_array_print -- the same T_f convention as UFCS methods --
// when one exists; without one, ok=false comes back with a hint (4th slot)
// naming the function to write, so the caller points at it instead of
// silently printing the address. Other element types and non-identifier
// expressions (a+1, &a, a function that returns a pointer, ...) cannot carry
// a length and stay on the %p / printf path.
func (c *checker) arrayPrintDispatch(a Expr) (string, []Expr, bool, string) {
	id, ok := a.(*Ident)
	if !ok {
		return "", nil, false, ""
	}
	at := c.lookup(id.Name)
	if at == nil || at.Kind != KArr || at.Len <= 0 || at.Elem == nil {
		return "", nil, false, ""
	}
	name := ""
	switch at.Elem.Kind {
	case KInt:
		switch at.Elem.Width { // char(1) stays a string, never an element list
		case 2:
			name = "short_array_print"
		case 4:
			name = "int_array_print"
		case 8:
			name = "long_array_print"
		default:
			return "", nil, false, ""
		}
	case KBool:
		name = "bool_array_print"
	case KFloat:
		name = "float_array_print"
	case KDouble:
		name = "double_array_print"
	case KStruct, KUnion:
		if at.Elem.Tag == "" {
			return "", nil, false, "" // anonymous: nothing to name
		}
		name = at.Elem.Tag + "_array_print"
		if c.funcs[name] == nil && c.protos[name] == nil {
			return "", nil, false, fmt.Sprintf(
				"print: no printer for a %s array (define int %s(%s *a, long n) to print its contents)",
				at.Elem, name, at.Elem)
		}
	default:
		return "", nil, false, ""
	}
	length := &NumLit{Val: int64(at.Len), Kind: TInt}
	return name, []Expr{a, length}, true, ""
}

// printSpec picks the printf conversion for a print() argument of static
// type t. char is modelled as KInt with width 1, so "char*" is the KPtr
// whose element is a width-1 int.
func printSpec(t *Type) (string, bool) {
	if t == nil {
		return "", false
	}
	switch t.Kind {
	case KInt, KBool:
		return "%d", true
	case KFloat, KDouble:
		return "%g", true
	case KPtr:
		if t.Elem != nil && t.Elem.Kind == KInt && t.Elem.Width == 1 {
			return "%s", true
		}
		return "%p", true
	}
	return "", false // structs, unions, anything else
}
