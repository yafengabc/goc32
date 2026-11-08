package frontend

// UFCS: uniform function call syntax for struct methods, Go/Nim style.
//
// A member call "x.f(args)" (or "p->f(args)") whose member f does not exist
// may still name a method: a plain function spelled T_f (T = x's struct tag)
// whose first parameter accepts x. The checker rewrites such a call into the
// direct call T_f(recv, args) with the receiver prepended -- its address for
// a struct-pointer receiver, the value itself for a value receiver. This is
// pure compile-time rewriting: no vtables, no metadata, zero runtime cost.
//
// Member lookups always win, so an existing function-pointer member keeps
// its C meaning ("s.cb(x)" is untouched) -- same precedence rule as Go.

import "fmt"

// tryUFCS attempts to resolve n as a method call. On success it fills
// n.UFCS with the rewritten direct call and returns the method's return
// type. Any mismatch -- non-member callee, non-struct or anonymous base,
// existing member, missing or wrongly-shaped T_f -- returns ok = false so
// the ordinary indirect-call error paths run unchanged.
func (c *checker) tryUFCS(n *IndirectCall, fn *FuncDecl) (*Type, bool) {
	me, ok := n.Fn.(*MemberExpr)
	if !ok {
		return nil, false
	}
	// An array base carries methods spelled T_array_f: arr.f(args) is the
	// direct call T_array_f(arr, len, args), the count a compile-time
	// constant because a C array has no runtime length. Only a bare array
	// identifier can be a receiver -- an expression like a+1 or a function
	// returning a pointer carries no length the checker can see. Dot form
	// only: arr->f would mean "member of *arr", never an array method.
	if !me.Arrow {
		if id, ok := me.Base.(*Ident); ok {
			if at := c.lookup(id.Name); at != nil && at.Kind == KArr && at.Len > 0 && at.Elem != nil {
				return c.tryArrayUFCS(n, me, id, at, fn)
			}
		}
	}
	bt := c.checkExpr(me.Base, fn)
	st := bt
	if me.Arrow {
		if !bt.IsPtr() {
			return nil, false
		}
		st = bt.Elem
	}
	// Methods need a nameable base: a struct with a tag (prefix from the
	// tag), or a scalar arithmetic type with a fixed spelling (int_print,
	// char_print, ...). Anonymous structs, pointers and arrays don't carry
	// methods.
	scalar := ""
	if st == nil {
		return nil, false
	}
	if st.Kind == KStruct {
		if st.Tag == "" {
			return nil, false // anonymous structs cannot carry methods
		}
	} else if !me.Arrow {
		// Scalar methods have no arrow form: p->f() means "member of what
		// p points at", and a scalar has no members. Pointer/array bases
		// don't participate either (their tag spelling is ambiguous).
		scalar = scalarTag(st)
	}
	if st.Kind != KStruct && scalar == "" {
		return nil, false
	}
	var meth string
	if scalar != "" {
		meth = scalar + "_" + me.Name
	} else {
		for _, m := range st.Members {
			if m.Name == me.Name {
				return nil, false // a real member (e.g. a function pointer) wins
			}
		}
		meth = st.Tag + "_" + me.Name
	}
	fd, ok := c.funcs[meth]
	if !ok {
		fd = c.protos[meth]
	}
	if fd == nil || len(fd.ParamTypes) == 0 {
		return nil, false // no such method: report "no member" as before
	}
	// The receiver must fit the method's first parameter. Scalars take a
	// value receiver with exact spelling (int_print takes an int); a
	// pointer receiver is rejected -- an rvalue like (x+y).print() has no
	// address to pass. Struct T* takes the address (or the pointer itself
	// through ->), struct T the value (dereferenced through ->).
	p0 := fd.ParamTypes[0]
	var recv Expr
	if scalar != "" {
		if !sameScalar(p0, st) {
			return nil, false // wrong receiver spelling: report the normal error
		}
		recv = me.Base
	} else {
		sameStruct := func(t *Type) bool {
			return t != nil && t.Kind == KStruct && t.Tag == st.Tag
		}
		switch {
		case p0.IsPtr() && sameStruct(p0.Elem):
			if me.Arrow {
				recv = me.Base // p is already a struct T*
			} else {
				recv = &Unary{Op: "&", E: me.Base} // &x
			}
		case sameStruct(p0):
			if me.Arrow {
				recv = &Unary{Op: "*", E: me.Base} // *p, passed by value
			} else {
				recv = me.Base // x, passed by value
			}
		default:
			return nil, false // wrong receiver shape: report the normal error
		}
	}
	args := make([]Expr, 0, len(n.Args)+1)
	args = append(args, recv)
	args = append(args, n.Args...)
	n.UFCS = &Call{Name: meth, Args: args}
	return c.checkArgs(meth, fd.ParamTypes, fd.Variadic, args, fn, fd.Ret), true
}

// tryArrayUFCS resolves a method call on an array base. arr.f(args) for an
// array of T rewrites into the direct call T_array_f(arr, len, args) -- the
// same T_array_ naming convention the array printers use (T_array_print),
// for any T the user can name. The length is passed as a compile-time
// constant, which is exactly why only a bare array identifier can be a
// receiver. The method's first parameter must be a pointer to the element
// type, as T_array_print(T *a, long n) demands; checkArgs then validates
// the count against the second parameter and the user's arguments against
// the rest. On any mismatch -- no such method, or a differently-shaped
// T_array_f -- ok = false sends the call down the ordinary member-lookup
// error path, exactly like a struct method that does not fit.
func (c *checker) tryArrayUFCS(n *IndirectCall, me *MemberExpr, id *Ident, at *Type, fn *FuncDecl) (*Type, bool) {
	tag := arrayTag(at.Elem)
	if tag == "" {
		return nil, false
	}
	meth := tag + "_array_" + me.Name
	fd, ok := c.funcs[meth]
	if !ok {
		fd = c.protos[meth]
	}
	if fd == nil || len(fd.ParamTypes) == 0 {
		return nil, false // no such array method: report the member error
	}
	p0 := fd.ParamTypes[0]
	if !p0.IsPtr() || !sameElem(p0.Elem, at.Elem) {
		return nil, false // wrong receiver shape: report the member error
	}
	length := &NumLit{Val: int64(at.Len), Kind: TInt}
	args := make([]Expr, 0, len(n.Args)+2)
	args = append(args, id, length)
	args = append(args, n.Args...)
	n.UFCS = &Call{Name: meth, Args: args}
	return c.checkArgs(meth, fd.ParamTypes, fd.Variadic, args, fn, fd.Ret), true
}

// arrayMethodHint returns a "define a T_array_f method" hint when the
// member's base is a bare array identifier, or "" for every other base.
// It is the array twin of the struct method escape hatch: arr.f() with no
// T_array_f defined reports the plain member error but points at the
// function to write instead of leaving the user to guess.
func (c *checker) arrayMethodHint(n *MemberExpr) string {
	if n.Arrow {
		return ""
	}
	id, ok := n.Base.(*Ident)
	if !ok {
		return ""
	}
	at := c.lookup(id.Name)
	if at == nil || at.Kind != KArr || at.Len <= 0 || at.Elem == nil {
		return ""
	}
	tag := arrayTag(at.Elem)
	if tag == "" {
		return ""
	}
	return fmt.Sprintf(" (a method on this array can be defined as a function %s_array_%s and called as %s.%s())",
		tag, n.Name, id.Name, n.Name)
}

// arrayTag returns the method-name prefix of an array element type: the
// scalarTag spelling for arithmetic elements (int_array_add, uint_array_add,
// double_array_avg, ...), the struct/union tag for named aggregates
// (Point_array_sum). "" for element types with no nameable spelling --
// pointers, nested arrays, anonymous structs. Note unsigned elements keep
// their u spelling here: an array method reads and writes its elements, so
// uint_array_add is genuinely different from int_array_add (print's shared
// same-width printer has no such requirement).
func arrayTag(elem *Type) string {
	if elem == nil {
		return ""
	}
	if elem.Kind == KStruct || elem.Kind == KUnion {
		if elem.Tag == "" {
			return ""
		}
		return elem.Tag
	}
	return scalarTag(elem)
}

// sameElem reports whether t spells exactly like the array's element type:
// the same kind, and for ints the same width and signedness, for structs
// and unions the same tag. Array methods are found by exact element
// spelling -- int_array_add takes an int*, never a long* -- so a
// differently-shaped T_array_f is not a method and the call reports the
// ordinary member error.
func sameElem(t, elem *Type) bool {
	if t == nil || elem == nil || t.Kind != elem.Kind {
		return false
	}
	switch t.Kind {
	case KInt:
		return t.Width == elem.Width && t.Signed == elem.Signed
	case KStruct, KUnion:
		return t.Tag != "" && t.Tag == elem.Tag
	}
	return true
}

// ufcsHint builds the "define a T_f method" hint used in error messages when
// a member lookup on a named struct fails. Empty for anonymous structs.
func ufcsHint(st *Type, name string) string {
	if st == nil || st.Kind != KStruct || st.Tag == "" {
		return ""
	}
	return fmt.Sprintf(" (a method can be defined as a function %s_%s and called as x.%s())",
		st.Tag, name, name)
}

// scalarTag returns the fixed tag spelling of a scalar arithmetic type for
// scalar methods ("int_print", "char_print", ...), or "" for types that
// don't carry methods. The spelling is compact and identifier-safe: unsigned
// forms use a u prefix (uchar, ushort, uint, ulong) so no tag ever contains
// a space. Pointer, array and function types return "" -- their method
// spelling would be ambiguous.
func scalarTag(t *Type) string {
	if t == nil {
		return ""
	}
	switch t.Kind {
	case KInt:
		u := ""
		if !t.Signed {
			u = "u"
		}
		switch t.Width {
		case 1:
			return u + "char"
		case 2:
			return u + "short"
		case 4:
			return u + "int"
		case 8:
			return u + "long"
		}
	case KBool:
		return "bool"
	case KFloat:
		return "float"
	case KDouble:
		return "double"
	}
	return ""
}

// sameScalar reports whether t spells exactly like the caller's scalar base:
// same kind, and for ints the same width and signedness. Method lookup is
// deliberately strict -- char_print takes a char, not an int -- so a
// mismatched receiver falls through to the normal error.
func sameScalar(t, base *Type) bool {
	if t == nil || base == nil || t.Kind != base.Kind {
		return false
	}
	if t.Kind == KInt {
		return t.Width == base.Width && t.Signed == base.Signed
	}
	return true
}
