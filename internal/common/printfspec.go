package common

import (
	"goc/frontend"
	"reflect"
	"strings"
)

// Constant-format printf specialisation, shared by both back ends.
//
// The native generator and the IR front end must agree on this exactly. If they
// did not, a program's size would depend on which back end built it, and --
// worse -- the two reachability prunes would disagree about what the program
// references: the emitter would call the cheap formatter while the pruner
// still saw printf and pulled vfmt in with it. That is not a hypothetical
// hazard; it is exactly what happened when the IR front end had its own copy
// and the native one had this.
//
// So the decision lives here, parameterised by the two questions a caller has
// to answer about its own symbol tables, and neither back end re-derives it.

// DisablePrintfSpec turns off the constant-format printf specialisation. A
// freestanding target supplies its own printf (TinyOS's user/lib.c), so
// rewriting a call to goclib's __goclib_printf_lite would reference a runtime
// that is not there. The goc32 driver sets this before generating IR.
var DisablePrintfSpec bool

// printfQueries are the facts a specialisation needs about the caller's
// program. Both are asked only about the *user's* declarations, never about
// the C runtime's own copies: "is this name already taken" has to mean by the
// program, or every library function would look shadowed.
type PrintfQueries struct {
	// userDefines reports whether the program defines a function of this name.
	UserDefines func(name string) bool
	// shadowedByVar reports whether a *variable* of this name holds a function
	// pointer, which shadows the library function of the same spelling.
	ShadowedByVar func(name string) bool
}

// SpecializePrintfCall rewrites a printf/fprintf call with a literal format
// into the cheapest entry point that reproduces it exactly, or returns nil when
// nothing applies and the ordinary printf must handle it.
//
// Two rewrites, in the order the native generator applies them:
//
//   - A '%'-free literal is a pure echo, so it becomes a direct fwrite. This
//     keeps vfmt, the FILE layer and the whole format engine out.
//   - A "lite" literal -- only %s, %c, the integer conversions, %f and %%, with
//     no width, precision or flag -- goes to __goclib_printf_lite (or its _f
//     variant when a %f is present). Those write straight to the OS handle.
//
// The decision is made here, at compile time, and that is what makes the
// call-graph prune work. A run-time "try lite, fall back to vfmt" probe would
// leave vfmt reachable and nothing would be pruned; rejecting the call instead
// leaves the program correct through the ordinary printf, just larger.
func SpecializePrintfCall(n *frontend.Call, q PrintfQueries) *frontend.Call {
	if DisablePrintfSpec {
		return nil
	}
	if q.UserDefines == nil || q.ShadowedByVar == nil {
		return nil
	}
	// fprintf takes the stream first, so the format is the second argument.
	fmtIdx := 0
	switch n.Name {
	case "printf":
	case "fprintf":
		fmtIdx = 1
	default:
		return nil
	}
	if len(n.Args) < fmtIdx+1 {
		return nil
	}
	lit, ok := n.Args[fmtIdx].(*frontend.StrLit)
	if !ok {
		return nil // a run-time format string proves nothing at compile time
	}
	if q.UserDefines("fwrite") || q.ShadowedByVar("fwrite") {
		return nil
	}
	if !strings.Contains(string(lit.Bytes), "%") {
		// A '%'-free format is a pure echo, but only when nothing else is
		// passed: fwrite takes a fixed byte count, so a trailing argument
		// would be dropped and its evaluation lost. The lite branch below does
		// accept extra arguments -- that is where the values are destined.
		if len(n.Args) != fmtIdx+1 {
			return nil
		}
		stream := frontend.Expr(&frontend.Call{Name: "__goclib_stdout"})
		if fmtIdx == 1 {
			stream = n.Args[0]
		}
		// A single-byte echo would specialise to fwrite(ptr, 1, 1, stream).
		// LLVM's libcall simplification rewrites exactly that shape into
		// fputc(*ptr, stream), but the gocl back end had only emitted
		// fwrite's definition, so the link then failed with an undefined
		// fputc. Emit fputc directly here so neither the reachability prune
		// nor the emitter has to know about the rewrite the optimiser will
		// perform.
		if len(lit.Bytes) == 1 {
			first := &frontend.Index{Base: lit, Idx: &frontend.NumLit{Val: 0, Kind: frontend.TInt}}
			return &frontend.Call{Name: "fputc", Args: []frontend.Expr{first, stream}}
		}
		return &frontend.Call{Name: "fwrite", Args: []frontend.Expr{
			lit,
			&frontend.NumLit{Val: 1, Kind: frontend.TInt},
			&frontend.NumLit{Val: int64(len(lit.Bytes)), Kind: frontend.TInt},
			stream,
		}}
	}
	// The lite formatters take no stream, so there is no equivalent for
	// fprintf: routing it to one would write to the wrong FILE.
	if fmtIdx != 0 {
		return nil
	}
	target, ok := liteTargetFor(string(lit.Bytes))
	if !ok {
		return nil
	}
	if q.UserDefines(target) || q.ShadowedByVar(target) {
		return nil
	}
	args := make([]frontend.Expr, 0, len(n.Args))
	args = append(args, n.Args...)
	return &frontend.Call{Name: target, Args: args}
}

// liteTargetFor picks the cheapest lite formatter that covers a format
// literal: the integer-only entry when there is no %f, the float one when
// there is. Splitting them is what keeps double_to_buf/floor/fmod/signbit and
// friends out of a program that never prints a double -- a single vfmt_lite
// with an unconditional %f branch referenced them even for printf("%d").
// Returns ok = false when the format is not lite at all.
func liteTargetFor(f string) (string, bool) {
	has, hasFloat := ScanLiteFormat(f)
	if !has {
		return "", false
	}
	if hasFloat {
		return "__goclib_printf_lite_f", true
	}
	return "__goclib_printf_lite", true
}

// scanLiteFormat reports whether every conversion in a printf format literal is
// one the lite formatters reproduce exactly, whether there is at least one
// conversion (a format with none is constantFormatFwrite's echo case, and a
// separate function keeps each rewrite to one job), and whether any of them is
// %f/%F -- which decides the integer-only or the float entry point.
//
// The walk follows the printf grammar vfmt parses, so it rejects exactly what
// lite cannot do. Any of these after a '%' disqualify the format:
//
//   - a flag character  -  0  +  space  #
//   - a width: '*' or any digit
//   - a precision: '.', optionally followed by '*' or digits
//   - a length modifier: h l L q j z t
//   - a specifier outside s c d i u o x X f F
//
// '%%' is a literal percent, not a conversion; a 'f' or '%' anywhere outside a
// conversion is plain text. A trailing lone '%' is malformed but harmless, so
// it is treated as text rather than a reason to reject.
func ScanLiteFormat(f string) (has, hasFloat bool) {
	has = false
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		if i >= len(f) {
			break // trailing '%': not a conversion
		}
		if f[i] == '%' {
			continue // "%%" is a literal percent
		}
		// From here on the conversion must be bare: no flags, width,
		// precision or length modifier may precede the specifier.
		if strings.IndexByte("-+0 #.", f[i]) >= 0 {
			return false, false
		}
		if f[i] == '*' || (f[i] >= '0' && f[i] <= '9') {
			return false, false // field width
		}
		if strings.IndexByte("hlLqjzt", f[i]) >= 0 {
			return false, false // length modifier
		}
		switch f[i] {
		case 'f', 'F':
			has = true
			hasFloat = true
		case 's', 'c', 'd', 'i', 'u', 'o', 'x', 'X':
			has = true
		default:
			// %p, %e, %g, %a and anything unknown stay on the full vfmt.
			return false, false
		}
	}
	return has, hasFloat
}

// RenameInProgram renames file-scope symbols throughout one unit's AST: the
// declarations themselves and every frontend.Ident that refers to them. The walk is
// reflection-based so it cannot silently miss a node type (an unhandled node
// shape would be a missed rename, which is a wrong-reference bug).
func RenameInProgram(prog *frontend.Program, renames map[string]string) {
	if len(renames) == 0 {
		return
	}
	for _, f := range prog.Funcs {
		if nn, ok := renames[f.Name]; ok {
			f.Name = nn
		}
		renameInValue(f.Body, renames)
	}
	for _, f := range prog.Prototypes {
		if nn, ok := renames[f.Name]; ok {
			f.Name = nn
		}
	}
	for _, g := range prog.Globals {
		if nn, ok := renames[g.Name]; ok {
			g.Name = nn
		}
		renameInValue(g.Init, renames)
	}
}

func renameInValue(v any, renames map[string]string) {
	if v == nil {
		return
	}
	renameReflect(reflect.ValueOf(v), renames)
}

func renameReflect(v reflect.Value, renames map[string]string) {
	switch v.Kind() {
	case reflect.Invalid:
		return
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return
		}
		if v.CanInterface() {
			switch x := v.Interface().(type) {
			case *frontend.Ident:
				if nn, ok := renames[x.Name]; ok {
					x.Name = nn
				}
				return
			case *frontend.Call:
				// A direct call names its callee as a string, not an *frontend.Ident.
				// (An indirect call goes through frontend.IndirectCall.Fn/UFCS and is
				// reached by the ordinary walk.)
				if nn, ok := renames[x.Name]; ok {
					x.Name = nn
				}
			case *frontend.Type:
				return // types carry tags and member names, never symbols
			case frontend.Type:
				return
			}
		}
		renameReflect(v.Elem(), renames)
	case reflect.Struct:
		if v.CanInterface() {
			if _, ok := v.Interface().(frontend.Type); ok {
				return
			}
		}
		for i := 0; i < v.NumField(); i++ {
			renameReflect(v.Field(i), renames)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			renameReflect(v.Index(i), renames)
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			renameReflect(it.Value(), renames)
		}
	}
}
