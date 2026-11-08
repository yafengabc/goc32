package link

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"goc/frontend"
)

// This file is the data half of the back end's job: given a declaration and a
// label, decide the bytes that represent it in the image, and decide which
// section it belongs in. None of it is C -- it is the layout of a program's
// variables, which both back ends must agree on byte for byte or the same
// source would produce two different programs.
//
// The two questions kept apart here are "is this all zeros?" (which decides
// .data against .bss) and "what are the bytes?" (.data against .bss aside).
// Answering the first needs the initialiser folded, which is why TypeWidth and
// the folding helpers below are here rather than in a back end.

// typeWidth returns the byte width used to lay out / step over a value of type
// t: char=1, short=2, int/long=4/8, double/pointer=8, array=element*len,
// struct/union=its computed Size.
func typeWidth(t *frontend.Type) int {
	if t == nil {
		return 8
	}
	switch t.Kind {
	case frontend.KPtr, frontend.KFunc:
		return 8
	case frontend.KFloat:
		return 4
	case frontend.KDouble:
		return 8
	case frontend.KLongDouble:
		// goc's long double is IEEE binary128: 16 bytes, whichever back end
		// lays the value out (the native side addresses it by copy, gocl
		// carries it as an i128).
		return 16
	case frontend.KBitInt:
		return frontend.Sizeof(t)
	case frontend.KArr:
		if t.Elem != nil {
			return typeWidth(t.Elem) * t.Len
		}
		return 8
	case frontend.KInt:
		if t.Width < 1 {
			return 1
		}
		return t.Width
	case frontend.KBool:
		return 1
	case frontend.KStruct, frontend.KUnion:
		if t.Size != 0 {
			return t.Size
		}
		return 8
	}
	return 8
}

// TypeWidth is the one place a type's byte width is decided, exported because
// both back ends lay out the same values and must not disagree about it.
func TypeWidth(t *frontend.Type) int { return typeWidth(t) }

// IsAgg reports whether t is a struct/union aggregate (or a C23 _BitInt /
// long double, which ride the same by-address value model), i.e. a value that
// is never loaded into a register but always handled by address + copyBytes.
// long double is 16 bytes -- wider than any register class the native back end
// moves scalars in -- so it travels by address like an aggregate.
func IsAgg(t *frontend.Type) bool {
	return t != nil && (t.IsStruct() || t.IsUnion() || t.Kind == frontend.KBitInt ||
		t.Kind == frontend.KLongDouble)
}

// tlsAlignedSize returns the exact number of bytes emitGlobalVar will write for
// t, so the per-variable offset bookkeeping stays in lockstep with the emitted
// .tls image. Scalars are emitted as an 8-byte dq (a float single is 4 bytes);
// arrays/aggregates are rounded up to 8. Matching this exactly is what keeps
// each variable's label at the byte emitGlobalVar actually wrote.
func tlsAlignedSize(t *frontend.Type) int {
	if t != nil && t.Kind == frontend.KFloat {
		return 4
	}
	sz := typeWidth(t)
	if sz < 1 {
		sz = 1
	}
	n := (sz + 7) &^ 7
	if n < 8 {
		n = 8
	}
	return n
}

// TLSAlignedSize is the .tls slot size of one variable, exported because the
// back end that lays the section out and the back end that generates the
// access code must agree on it or a variable's label lands off its own bytes.
func TLSAlignedSize(t *frontend.Type) int { return tlsAlignedSize(t) }

// BigWordsOf is the number of 64-bit words a _BitInt of type t occupies.
func BigWordsOf(t *frontend.Type) int {
	w := (t.Bits + 63) / 64
	if w < 1 {
		w = 1
	}
	return w
}

// bigInitWords folds a static (global / static-local / TLS) _BitInt
// initialiser into its little-endian word image. Supported forms are: no
// initialiser, "{}", "{v}", a wb/uwb literal (already split into words by the
// lexer) and a plain integer constant (sign- or zero-extended to the declared
// width). Anything else folds to zero -- the front end rejects non-constant
// static initialisers, so this is only a defensive fallback.
func bigInitWords(t *frontend.Type, init frontend.Expr) []uint64 {
	w := bigWordsOf(t)
	if w < 1 {
		w = 1
	}
	words := make([]uint64, w)
	if init == nil {
		return words
	}
	if bi, ok := init.(*frontend.BraceInit); ok {
		if len(bi.Elems) == 0 {
			return words
		}
		init = bi.Elems[0].E
	}
	// Peel casts and a leading unary minus: "(signed _BitInt(8))200" arrives
	// as a frontend.CastExpr over a frontend.NumLit, and "(signed _BitInt(64))-1" parses as
	// frontend.CastExpr(frontend.Unary("-", frontend.NumLit(1))). The C23 conversion to the target width
	// happens below, so the emitted .data image wraps exactly like the
	// runtime path.
	neg := false
	for {
		ce, ok := init.(*frontend.CastExpr)
		if !ok {
			break
		}
		init = ce.E
	}
	if u, ok := init.(*frontend.Unary); ok && u.Op == "-" {
		neg = true
		init = u.E
	}
	nl, ok := init.(*frontend.NumLit)
	if !ok {
		return words
	}
	for i := 0; i < w && i < len(nl.BigWords); i++ {
		words[i] = nl.BigWords[i]
	}
	if neg {
		// two's complement negation across the whole word image
		carry := uint64(1)
		for i := 0; i < w; i++ {
			words[i] = ^words[i] + carry
			if words[i] != 0 || carry == 0 {
				carry = 0
			}
		}
	}
	if nl.BigWords == nil {
		if neg {
			words[0] = uint64(-nl.Val)
		} else {
			words[0] = uint64(nl.Val)
		}
		if nl.Val < 0 || neg {
			for i := 1; i < w; i++ {
				words[i] = ^uint64(0)
			}
		}
	}
	// C23 conversion to a narrower _BitInt: reduce modulo 2^Bits, then
	// sign-extend when the target is signed (6.3.1.3).
	if t != nil && t.Bits < 64 {
		mask := (uint64(1) << t.Bits) - 1
		words[0] &= mask
		if t.Signed && (words[0]>>(t.Bits-1))&1 != 0 {
			words[0] |= ^mask
			for i := 1; i < w; i++ {
				words[i] = ^uint64(0)
			}
		}
	}
	return words
}

// bigWordsOf is the internal spelling of BigWordsOf.
func bigWordsOf(t *frontend.Type) int { return BigWordsOf(t) }

// foldConstInit folds the constant initialiser of a global variable down to an
// integer. The parser represents "= 42" as a frontend.NumLit, "= -1" as frontend.Unary{'-'}, and
// "= GREEN" as an frontend.Ident naming an enumerator. Anything else (a double literal,
// an expression, a struct initialiser) is not foldable at emission time and
// yields ok=false, so the global falls back to zero.
func foldConstInit(e frontend.Expr) (int64, bool) {
	switch n := e.(type) {
	case nil:
		return 0, true
	case *frontend.NumLit:
		if n.Kind == frontend.TDouble {
			return 0, false
		}
		return n.Val, true
	case *frontend.Ident:
		if v, ok := frontend.EnumConsts[n.Name]; ok {
			return v, true
		}
		return 0, false
	case *frontend.Unary:
		v, ok := foldConstInit(n.E)
		if !ok {
			return 0, false
		}
		switch n.Op {
		case "-":
			return -v, true
		case "+":
			return v, true
		case "~":
			return ^v, true
		case "!":
			return boolVal(v == 0), true
		}
	case *frontend.CondExpr:
		// a ? b : c -- the condition is itself a constant integer fold.
		c, ok := foldConstInit(n.Cond)
		if !ok {
			return 0, false
		}
		if c != 0 {
			return foldConstInit(n.Then)
		}
		return foldConstInit(n.Else)
	case *frontend.CastExpr:
		// A cast to an integer (or _Bool) type truncates/sign-extends the
		// folded operand exactly as the C integer-conversion rules require;
		// a cast to any other type leaves the bit pattern unchanged (a
		// (void*)0 null pointer, for instance, keeps the value 0). Without
		// this case, "((NU)(1) << 62)" -- the definition of Nim's
		// NIM_STRLIT_FLAG -- failed to fold and its static initialiser was
		// emitted as 0, which broke every Nim float->string conversion.
		v, ok := foldConstInit(n.E)
		if !ok {
			return 0, false
		}
		return foldCastInt(v, n.Typ)
	case *frontend.Binary:
		// Every integer constant operator C allows in a static initialiser.
		// Shifts refuse counts outside [0,63] (x86 masks the count, so the
		// folded value would disagree with the generated instruction), and
		// division/modulo refuse a zero divisor instead of trapping.
		l, ok1 := foldConstInit(n.L)
		r, ok2 := foldConstInit(n.R)
		if !ok1 || !ok2 {
			return 0, false
		}
		switch n.Op {
		case "+":
			return l + r, true
		case "-":
			return l - r, true
		case "*":
			return l * r, true
		case "/":
			if r == 0 {
				return 0, false
			}
			return l / r, true
		case "%":
			if r == 0 {
				return 0, false
			}
			return l % r, true
		case "<<":
			if r < 0 || r > 63 {
				return 0, false
			}
			return l << uint(r), true
		case ">>":
			if r < 0 || r > 63 {
				return 0, false
			}
			return l >> uint(r), true
		case "&":
			return l & r, true
		case "|":
			return l | r, true
		case "^":
			return l ^ r, true
		case "<":
			return boolVal(l < r), true
		case ">":
			return boolVal(l > r), true
		case "<=":
			return boolVal(l <= r), true
		case ">=":
			return boolVal(l >= r), true
		case "==":
			return boolVal(l == r), true
		case "!=":
			return boolVal(l != r), true
		case "&&":
			return boolVal(l != 0 && r != 0), true
		case "||":
			return boolVal(l != 0 || r != 0), true
		}
		return 0, false
	}
	return 0, false
}

// FoldConstInit is the integer constant folder, exported because a back end
// folds a static initialiser when it generates code for the same expression.
func FoldConstInit(e frontend.Expr) (int64, bool) { return foldConstInit(e) }

// boolVal turns a comparison result into the 0/1 integer C expects.
func boolVal(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// foldCastInt applies an integer (or _Bool) cast to an already-folded value.
// Narrower widths truncate and sign-extend back to int64; a 64-bit or unknown
// width keeps the full value. Pointer/other targets keep the raw value (so a
// (void*)0 null pointer stays 0). Returns (0, false) only when the target type
// is missing, which the folder cannot reason about.
func foldCastInt(v int64, t *frontend.Type) (int64, bool) {
	if t == nil {
		return 0, false
	}
	switch t.Kind {
	case frontend.KInt:
		return truncInt(v, t.Width, t.Signed), true
	case frontend.KBitInt:
		if t.Bits <= 0 || t.Bits > 64 {
			return 0, false
		}
		if t.Bits == 64 {
			return v, true
		}
		bits := uint(t.Bits)
		mask := (int64(1) << bits) - 1
		u := v & mask
		if t.Signed && u&(int64(1)<<(bits-1)) != 0 {
			u |= ^mask
		}
		return u, true
	case frontend.KBool:
		return boolVal(v != 0), true
	default:
		// Keep the bit pattern for pointers and other non-integer targets.
		return v, true
	}
}

// truncInt narrows v to width bytes, sign-extending when signed. width >= 8 (or
// <= 0, meaning "unknown") passes the value through unchanged.
func truncInt(v int64, width int, signed bool) int64 {
	if width <= 0 || width >= 8 {
		return v
	}
	bits := uint(width * 8)
	mask := (int64(1) << bits) - 1
	u := v & mask
	if signed && u&(int64(1)<<(bits-1)) != 0 {
		u |= ^mask
	}
	return u
}

// foldFloatInit folds the constant initialiser of a global float/double down
// to its value. Like foldConstInit it understands a bare literal, a negated
// literal and an enumerator name; anything else yields ok=false and the global
// falls back to 0.0.
func foldFloatInit(e frontend.Expr) (float64, bool) {
	switch n := e.(type) {
	case nil:
		return 0, true
	case *frontend.NumLit:
		if n.Kind == frontend.TDouble {
			return n.Fval, true
		}
		return float64(n.Val), true
	case *frontend.Ident:
		if v, ok := frontend.EnumConsts[n.Name]; ok {
			return float64(v), true
		}
		return 0, false
	case *frontend.Unary:
		v, ok := foldFloatInit(n.E)
		if !ok {
			return 0, false
		}
		switch n.Op {
		case "-":
			return -v, true
		case "+":
			return v, true
		}
	case *frontend.Binary:
		l, ok1 := foldFloatInit(n.L)
		r, ok2 := foldFloatInit(n.R)
		if !ok1 || !ok2 {
			return 0, false
		}
		switch n.Op {
		case "+":
			return l + r, true
		case "-":
			return l - r, true
		case "*":
			return l * r, true
		case "/":
			if r == 0 {
				return 0, false
			}
			return l / r, true
		}
		return 0, false
	case *frontend.CastExpr:
		if v, ok := foldFloatInit(n.E); ok {
			return v, true
		}
		if v, ok := foldConstInit(n.E); ok {
			return float64(v), true
		}
		return 0, false
	}
	return 0, false
}

// FoldFloatInit is the floating-point constant folder, exported for the same
// reason as FoldConstInit.
func FoldFloatInit(e frontend.Expr) (float64, bool) { return foldFloatInit(e) }

// foldTFInit folds the constant initialiser of a global long double to its
// binary128 bit pattern (low word first, matching the little-endian layout).
// The checker has already parsed the literal into the exact 128-bit encoding,
// so this is a copy, not a rounding; a negation flips the sign bit in place
// and a cast passes through to its operand. Anything else is not foldable and
// the global falls back to +0.0, exactly as the float/double folder does.
func foldTFInit(e frontend.Expr) (uint64, uint64, bool) {
	switch n := e.(type) {
	case nil:
		return 0, 0, true
	case *frontend.NumLit:
		if n.IsLongDouble {
			return n.F128.Lo, n.F128.Hi, true
		}
		return 0, 0, false
	case *frontend.Unary:
		lo, hi, ok := foldTFInit(n.E)
		if !ok {
			return 0, 0, false
		}
		switch n.Op {
		case "-":
			return lo, hi ^ 1<<63, true
		case "+":
			return lo, hi, true
		}
		return 0, 0, false
	case *frontend.CastExpr:
		return foldTFInit(n.E)
	}
	return 0, 0, false
}

// encodeStr renders decoded string bytes as a double-quoted literal with
// escapes, for goa's db directive.
func encodeStr(b []byte) string {
	var sb strings.Builder
	for _, ch := range b {
		switch ch {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\t':
			sb.WriteString("\\t")
		case '\r':
			sb.WriteString("\\r")
		default:
			if ch >= 32 && ch < 127 {
				sb.WriteByte(ch)
			} else {
				fmt.Fprintf(&sb, "\\%03o", ch)
			}
		}
	}
	return sb.String()
}

// formatDouble renders a float64 as a Go-syntax literal that goa's `dq`
// directive can parse back into IEEE-754 bits ("1.5", "0x1.2p3", ...).
//
// 'g' formatting would print integral doubles as bare integers ("10"), and goa
// parses those via ParseInt -> the integer bit pattern (0xa) instead of the
// double (0x4024000000000000). Force a trailing ".0" so goa's ParseFloat path
// is taken.
func formatDouble(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// globalInitFuncName reports the function named by a global initialiser
// that is a bare function designator ("malloc", "&free"). It answers only
// whether the initialiser *names* something; the caller still has to confirm
// through funcAddrSym that it really names a function whose code is here.
func globalInitFuncName(e frontend.Expr) (string, bool) {
	e = stripInitCasts(e)
	switch n := e.(type) {
	case *frontend.Ident:
		return n.Name, true
	case *frontend.Unary:
		if n.Op == "&" {
			if id, ok := n.E.(*frontend.Ident); ok {
				return id.Name, true
			}
		}
	}
	return "", false
}

// stripInitCasts peels the casts off a global/static initialiser. C writes the
// address of an object behind a cast more often than not -- Nim's generated C
// spells it "(NimStrPayload*)&literal" -- and a cast to a pointer type changes
// nothing about *which* object is named. Every "does this initialiser name
// something" scan therefore has to look through them first, or the slot is
// taken for a plain 0 and the address is never bound.
func stripInitCasts(e frontend.Expr) frontend.Expr {
	for {
		c, ok := e.(*frontend.CastExpr)
		if !ok {
			return e
		}
		e = c.E
	}
}

// walkGlobalInit scans a global/static-local initialiser for every pointer
// slot initialised by a string literal and records the (label, byte offset,
// string label) triple in d.StringSlots so the entry stub can bind it at
// startup. It mirrors the layout walk of fillBraceImage: arrays by element
// stride, structs by member offset, unions by first member. A string filling a
// char array needs no binding (its bytes live directly in .data).
func walkGlobalInit(d *Data, t *frontend.Type, init frontend.Expr, glab string, off int) {
	if t == nil {
		return
	}
	if frontend.IsBig(t) {
		// Global _BitInt: the .bss/.data image is already zero, which is the
		// only supported global initialisation ("{}" or none). Non-zero
		// initialisers are rejected at emission (emitGlobalVar).
		return
	}
	bi, ok := init.(*frontend.BraceInit)
	if !ok {
		// "(NimStrPayload*)&literal" and "void *p = (void*)g" name an object
		// exactly as the uncast forms do; the cast has to come off before any
		// of the scans below can recognise it.
		init = stripInitCasts(init)
		// A bare function designator naming a function ("void *(*fp)(long) =
		// malloc;", or a function-pointer member reached through a brace
		// walk) needs the same startup binding a string literal does.
		if fname, ok := globalInitFuncName(init); ok && t != nil && t.Kind == frontend.KPtr {
			if sym, ok := d.FuncAddr(fname); ok {
				d.FuncSlots = append(d.FuncSlots, FuncSlot{Global: glab, Func: sym, Offset: off})
				return
			}
		}
		if sl, ok := init.(*frontend.StrLit); ok && t.Kind == frontend.KPtr {
			lab, ok := d.StrLabs[sl]
			if !ok {
				lab = fmt.Sprintf("%sLC%d", d.UnitPrefix, len(d.Strings))
				d.Strings = append(d.Strings, StringConst{Label: lab, Text: string(sl.Bytes), Wide: sl.Wide})
				d.StrLabs[sl] = lab
			}
			d.StringSlots = append(d.StringSlots, StringSlot{Global: glab, String: lab, Offset: off})
		}
		// A pointer slot filled by the address of a global/static object --
		// either a bare identifier (an array decays to a pointer in C, e.g.
		// "const char *const *extra_words = json_words") or an explicit
		// "&g" (e.g. "int *p = &g"). goa cannot relocate either address
		// into .data, so record it for the entry stub to bind at startup,
		// exactly like a function-pointer slot.
		var varName string
		if id, ok := init.(*frontend.Ident); ok {
			varName = id.Name
		} else if u, ok := init.(*frontend.Unary); ok && u.Op == "&" {
			if id, ok := u.E.(*frontend.Ident); ok {
				varName = id.Name
			}
		}
		if varName != "" && t.Kind == frontend.KPtr {
			if lab, ok := d.Globals[varName]; ok {
				d.ArraySlots = append(d.ArraySlots, ArraySlot{Global: glab, Array: lab, Offset: off})
				return
			}
			if lab, ok := d.StaticVars[varName]; ok {
				d.ArraySlots = append(d.ArraySlots, ArraySlot{Global: glab, Array: lab, Offset: off})
				return
			}
		}
		return
	}
	if t.IsArray() {
		ew := typeWidth(t.Elem)
		for i, el := range bi.Elems {
			if i >= t.Len {
				break
			}
			walkGlobalInit(d, t.Elem, el.E, glab, off+i*ew)
		}
		return
	}
	if t.IsStruct() {
		allDesig := len(bi.Elems) > 0
		for _, el := range bi.Elems {
			if el.Desig == "" {
				allDesig = false
				break
			}
		}
		if allDesig {
			for _, el := range bi.Elems {
				if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 {
					m := t.Members[mi]
					walkGlobalInit(d, m.Type, el.E, glab, off+m.Offset)
				}
			}
			return
		}
		vis := frontend.PosMembers(t)
		for i, el := range bi.Elems {
			if i >= len(vis) {
				break
			}
			m := vis[i]
			walkGlobalInit(d, m.Type, el.E, glab, off+m.Offset)
		}
		return
	}
	if t.IsUnion() {
		if len(bi.Elems) == 0 {
			return
		}
		el := bi.Elems[0]
		var m *frontend.Member
		if el.Desig != "" {
			if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 {
				m = t.Members[mi]
			}
		} else if vis := frontend.PosMembers(t); len(vis) > 0 {
			m = vis[0]
		}
		if m != nil {
			walkGlobalInit(d, m.Type, el.E, glab, off+m.Offset)
		}
		return
	}
	if len(bi.Elems) != 1 {
		return
	}
	walkGlobalInit(d, t, bi.Elems[0].E, glab, off)
}

// emitGlobalVar lays out one program-level variable (true global or static
// local) in .data under the given label. The emission mirrors the rules the
// .data loop used for globals: a braced initialiser is rendered as a byte
// image; a char array from a string literal keeps its bytes (NUL-padded); an
// aggregate/array with no brace is zero-filled for its full byte size; a float
// is 4 bytes of IEEE single or a double quad; everything else is an integer
// dq (zero when the initialiser does not fold).
func emitGlobalVar(out *strings.Builder, g *frontend.DeclStmt, lab string) error {
	// Global _BitInt: zero image only (the storage is a word array; the
	// isZeroInit classification sends the zero case to .bss).
	if frontend.IsBig(g.Typ) {
		words := bigInitWords(g.Typ, g.Init)
		out.WriteString(lab + " dq ")
		for i, wv := range words {
			if i > 0 {
				out.WriteString(", ")
			}
			// signed decimal keeps the bit pattern while staying within goa's
			// dq ParseInt range ("0xffffffffffffffc8" overflows int64 there).
			out.WriteString(fmt.Sprintf("%d", int64(wv)))
		}
		out.WriteString("\n")
		return nil
	}
	if bi, ok := g.Init.(*frontend.BraceInit); ok {
		return emitGlobalBrace(out, g.Typ, bi, lab)
	}
	// A char array initialised by a string literal holds the bytes (plus NUL)
	// directly in .data, zero-padded to the full array size ("char g[8] =
	// \"hi\"" keeps five zero tail bytes). A global char* initialised by a
	// string literal is NOT supported: goa's dq takes no symbol operands, so
	// the pointer could not be relocated to the constant.
	if g.Typ != nil && g.Typ.IsArray() {
		if sl, ok := g.Init.(*frontend.StrLit); ok &&
			((!sl.Wide && g.Typ.Elem.IsChar()) || (sl.Wide && g.Typ.Elem.Width == 2)) {
			size := typeWidth(g.Typ)
			if sl.Wide {
				// wchar_t array: UTF-16 code units plus a 2-byte NUL terminator.
				out.WriteString(fmt.Sprintf("%s db \"%s\", 0, 0", lab, encodeStr(sl.Bytes)))
				for i := len(sl.Bytes) + 2; i < size; i++ {
					out.WriteString(", 0")
				}
			} else {
				out.WriteString(fmt.Sprintf("%s db \"%s\", 0", lab, encodeStr(sl.Bytes)))
				for i := len(sl.Bytes) + 1; i < size; i++ {
					out.WriteString(", 0")
				}
			}
			out.WriteString("\n")
			return nil
		}
	}
	if g.Typ != nil && g.Typ.Kind == frontend.KLongDouble {
		// binary128 initialiser: two quads, low word at offset 0. The signed
		// decimal form keeps the bit pattern within goa's dq range.
		lo, hi, ok := foldTFInit(g.Init)
		if !ok {
			lo, hi = 0, 0
		}
		out.WriteString(fmt.Sprintf("%s dq %d, %d\n", lab, int64(lo), int64(hi)))
		return nil
	}
	if g.Typ != nil && (g.Typ.IsArray() || IsAgg(g.Typ)) {
		// typeWidth already returns the full byte size (elem width * len for
		// arrays, the computed Size for structs/unions), so that IS the size
		// to zero-fill.
		size := typeWidth(g.Typ)
		if size < 1 {
			size = 1
		}
		n := (size + 7) / 8
		out.WriteString(lab + " dq 0")
		for i := 1; i < n; i++ {
			out.WriteString(", 0")
		}
		out.WriteString("\n")
		return nil
	}
	if g.Typ != nil && g.Typ.IsFloating() {
		// A float global is 4 bytes of IEEE single; a double is a quad. Only a
		// literal initialiser (optionally negated) is foldable here; anything
		// else falls back to zero.
		f := 0.0
		if v, ok := foldFloatInit(g.Init); ok {
			f = v
		}
		if g.Typ.Kind == frontend.KFloat {
			bits := math.Float32bits(float32(f))
			out.WriteString(fmt.Sprintf("%s db %d, %d, %d, %d\n", lab,
				bits&0xff, (bits>>8)&0xff, (bits>>16)&0xff, (bits>>24)&0xff))
		} else {
			out.WriteString(fmt.Sprintf("%s dq %s\n", lab, formatDouble(f)))
		}
		return nil
	}
	val := int64(0)
	if v, ok := foldConstInit(g.Init); ok {
		val = v
	}
	out.WriteString(fmt.Sprintf("%s dq %d\n", lab, val))
	return nil
}

// isZeroInit reports whether a global's initialiser is entirely zero (or absent),
// so it can be placed in .bss instead of .data. This shrinks the on-disk image
// without changing run-time layout: the loader zero-fills .bss. Pointers are kept
// in .data to preserve the exact existing behaviour for address-initialised
// (stub-written) globals.
func isZeroInit(g *frontend.DeclStmt) bool {
	if g.Init == nil {
		return true
	}
	// A _BitInt global carries real words when its initialiser is a literal:
	// the generic aggregate rule below would call everything zero.
	if frontend.IsBig(g.Typ) {
		for _, w := range bigInitWords(g.Typ, g.Init) {
			if w != 0 {
				return false
			}
		}
		return true
	}
	if g.Typ != nil && g.Typ.IsPtr() {
		return false
	}
	if bi, ok := g.Init.(*frontend.BraceInit); ok {
		size := typeWidth(g.Typ)
		if size < 1 {
			size = 1
		}
		img := make([]byte, size)
		if err := fillBraceImage(g.Typ, bi, img, 0); err == nil {
			for _, b := range img {
				if b != 0 {
					return false
				}
			}
			return true
		}
		return false
	}
	// A char (or wchar_t) array initialised by a string literal carries real bytes.
	if g.Typ != nil && g.Typ.IsArray() {
		if sl, ok := g.Init.(*frontend.StrLit); ok &&
			((!sl.Wide && g.Typ.Elem.IsChar()) || (sl.Wide && g.Typ.Elem.Width == 2)) {
			return false
		}
	}
	if g.Typ != nil && g.Typ.Kind == frontend.KLongDouble {
		// Exact bit test: a value like 1e-4000L rounds to zero through the
		// float64 folder but is not zero in binary128, and .bss placement
		// would silently drop it.
		lo, hi, ok := foldTFInit(g.Init)
		if !ok {
			return false
		}
		return lo == 0 && hi == 0
	}
	if g.Typ != nil && g.Typ.IsFloating() {
		f := 0.0
		if v, ok := foldFloatInit(g.Init); ok {
			f = v
		}
		return f == 0.0
	}
	if g.Typ != nil && (g.Typ.IsArray() || IsAgg(g.Typ)) {
		// No braced initialiser and not a string: zero-filled (checked above for
		// the scalar/string cases that reached here).
		return true
	}
	val := int64(0)
	if v, ok := foldConstInit(g.Init); ok {
		val = v
	}
	return val == 0
}

// emitGlobalBSS emits an uninitialised global as a .bss reservation. The size is
// rounded up to 8 bytes so every global stays 8-aligned, matching the dq-block
// layout used in .data (the trailing padding is simply unused).
func emitGlobalBSS(out *strings.Builder, g *frontend.DeclStmt, lab string) {
	size := typeWidth(g.Typ)
	if size < 1 {
		size = 1
	}
	n := (size + 7) / 8
	out.WriteString(fmt.Sprintf("%s resq %d\n", lab, n))
}

// emitGlobalBrace lays out a global aggregate from a braced initialiser as a
// byte image and emits it as db bytes. Bytes not explicitly initialised stay
// zero. A char* member initialised by a string literal also stays zero here:
// walkGlobalInit registered the (object, offset, string) triple and the entry
// stub writes the string's address at startup (goa has no data relocations).
func emitGlobalBrace(out *strings.Builder, t *frontend.Type, bi *frontend.BraceInit, lab string) error {
	size := typeWidth(t)
	if size < 1 {
		size = 1
	}
	img := make([]byte, size)
	if err := fillBraceImage(t, bi, img, 0); err != nil {
		return err
	}
	out.WriteString(fmt.Sprintf("%s db %d", lab, img[0]))
	for _, b := range img[1:] {
		out.WriteString(fmt.Sprintf(", %d", b))
	}
	out.WriteString("\n")
	return nil
}

// fillBraceImage fills the image of a global aggregate at byte offset off
// from a braced initialiser, mirroring the layout the local emitter uses
// (arrays by element index, structs by member Offset, unions by first member).
func fillBraceImage(t *frontend.Type, bi *frontend.BraceInit, img []byte, off int) error {
	if t.IsArray() {
		ew := typeWidth(t.Elem)
		hasDesig := false
		for _, el := range bi.Elems {
			if el.DesigIdx >= 0 {
				hasDesig = true
				break
			}
		}
		for i, el := range bi.Elems {
			idx := el.DesigIdx
			if !hasDesig {
				idx = i
			}
			if idx < 0 || idx >= t.Len {
				continue
			}
			if err := fillBraceElem(t.Elem, el.E, img, off+idx*ew); err != nil {
				return err
			}
		}
		return nil
	}
	if t.IsStruct() {
		allDesig := len(bi.Elems) > 0
		for _, el := range bi.Elems {
			if el.Desig == "" {
				allDesig = false
				break
			}
		}
		if allDesig {
			for _, el := range bi.Elems {
				mi := frontend.MemberIndex(t, el.Desig)
				if mi < 0 {
					return fmt.Errorf("struct has no member %q", el.Desig)
				}
				m := t.Members[mi]
				if err := fillBraceElem(m.Type, el.E, img, off+m.Offset); err != nil {
					return err
				}
			}
			return nil
		}
		vis := frontend.PosMembers(t)
		for i, el := range bi.Elems {
			if i >= len(vis) {
				break
			}
			if el.DesigIdx >= 0 {
				return fmt.Errorf("array designator \"[%d] =\" is only valid in an array initialiser", el.DesigIdx)
			}
			if el.Desig != "" {
				return fmt.Errorf("cannot mix positional and designated (\".%s =\") initialisers", el.Desig)
			}
			m := vis[i]
			if err := fillBraceElem(m.Type, el.E, img, off+m.Offset); err != nil {
				return err
			}
		}
		return nil
	}
	if t.IsUnion() {
		if len(bi.Elems) == 0 {
			return nil
		}
		el := bi.Elems[0]
		var m *frontend.Member
		if el.Desig != "" {
			if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 {
				m = t.Members[mi]
			}
		} else if vis := frontend.PosMembers(t); len(vis) > 0 {
			m = vis[0]
		}
		if m == nil {
			return nil
		}
		if el.DesigIdx >= 0 {
			return fmt.Errorf("array designator \"[%d] =\" is only valid in an array initialiser", el.DesigIdx)
		}
		return fillBraceElem(m.Type, el.E, img, off+m.Offset)
	}
	if len(bi.Elems) != 1 {
		return fmt.Errorf("invalid braced initialiser for scalar type %s", t)
	}
	return fillBraceElem(t, bi.Elems[0].E, img, off)
}

// fillBraceElem writes one element into the image: a nested brace recurses, a
// string fills a char array, and scalars fold to their IEEE/integer bytes.
// Anything non-foldable stays zero, matching existing global behaviour.
func fillBraceElem(t *frontend.Type, e frontend.Expr, img []byte, off int) error {
	if nbi, ok := e.(*frontend.BraceInit); ok {
		return fillBraceImage(t, nbi, img, off)
	}
	w := typeWidth(t)
	if off+w > len(img) {
		return fmt.Errorf("initialiser overflows global of %d bytes", len(img))
	}
	if sl, ok := e.(*frontend.StrLit); ok {
		if t.IsArray() && ((sl.Wide && t.Elem.Width == 2) || (!sl.Wide && t.Elem.IsChar())) {
			b := append(append([]byte(nil), sl.Bytes...), 0)
			if sl.Wide {
				b = append(b, 0) // 2-byte NUL terminator for wchar_t[]
			}
			if len(b) > w {
				b = b[:w]
			}
			copy(img[off:], b)
			return nil
		}
		// A pointer slot initialised by a string leaves 8 zero bytes here:
		// walkGlobalInit has already registered the (object, offset, string)
		// triple, and the entry stub writes the string's address at startup.
		// goa has no data relocations, so the value cannot be stored in .data.
	}
	switch t.Kind {
	case frontend.KFloat:
		f, _ := foldFloatInit(e)
		bits := math.Float32bits(float32(f))
		for i := 0; i < 4; i++ {
			img[off+i] = byte(bits >> (8 * i))
		}
		return nil
	case frontend.KBool:
		// A _Bool keeps exactly 0 or 1 even as a global (C semantics: any
		// non-zero initialiser becomes 1).
		v, _ := foldConstInit(e)
		if v != 0 {
			v = 1
		}
		img[off] = byte(v)
		return nil
	case frontend.KDouble:
		f, _ := foldFloatInit(e)
		bits := math.Float64bits(f)
		for i := 0; i < 8; i++ {
			img[off+i] = byte(bits >> (8 * i))
		}
		return nil
	case frontend.KLongDouble:
		// binary128 in a brace: the exact 128-bit encoding, low word first.
		lo, hi, _ := foldTFInit(e)
		for i := 0; i < 8; i++ {
			img[off+i] = byte(lo >> (8 * i))
			img[off+8+i] = byte(hi >> (8 * i))
		}
		return nil
	}
	v, _ := foldConstInit(e)
	for i := 0; i < w && i < 8; i++ {
		img[off+i] = byte(v >> (8 * i))
	}
	return nil
}
