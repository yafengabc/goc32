// Package link emits the parts of a goc-generated assembly file that are not
// C: the entry stub, the data sections, and the startup code that binds
// pointer slots which the assembler cannot relocate.
//
// It exists because two back ends need exactly this and neither should own it.
// The native generator (src/goc) and the LLVM one (src/gocl) differ in how
// they produce code -- hand-written x86-64 versus IR through libLLVM -- but
// they produce the same kind of artifact: assembly for the same assembler,
// goa, with the same calling convention and the same process start-up. The
// stub is not C, so neither generator can express it; it was ending up inside
// the native one, which made the LLVM compiler unable to produce a runnable
// program without it.
//
// The division is data against behaviour. A back end supplies what the program
// contains -- its strings, its globals, its thread-local variables -- and this
// package decides where each goes in the image and what has to happen before
// main runs.
package link

import (
	"fmt"
	"sort"
	"strings"

	"goc/frontend"
)

// StringSlot is a pointer slot that must hold the address of a string constant
// by the time main runs.
//
// It exists because goa has no data relocations. A C program can write
// `char *p = "text";` and expect the linker to put the address in .data; goc
// emits the .data image itself and has no relocation to express, so the slot
// stays zero and the entry stub computes the address with lea and stores it.
// That covers every case C allows: a top-level pointer, a static local, and a
// char* member nested anywhere inside a braced initialiser.
type StringSlot struct {
	// Global is the .data label of the object holding the slot.
	Global string
	// String is the .rdata label of the string constant.
	String string
	// Offset is the slot's byte offset within that object.
	Offset int
}

// FuncSlot is a pointer slot holding the address of a function, for the same
// reason StringSlot exists: `static H hooks = { malloc, free };` needs a code
// address in .data and goa cannot put one there.
type FuncSlot struct {
	// Global is the .data label of the object holding the slot.
	Global string
	// Func is the label of the function whose address belongs in it.
	Func string
	// Offset is the slot's byte offset within that object.
	Offset int
}

// ArraySlot is a pointer slot holding the address of a file-scope array or
// other global object. C decays the array to a pointer, so `const char *p =
// json_words;` is an ordinary pointer initialiser -- but the address still has
// to be computed at startup.
type ArraySlot struct {
	// Global is the .data label of the object holding the slot.
	Global string
	// Array is the .data label of the target.
	Array string
	// Offset is the slot's byte offset within the holding object.
	Offset int
}

// StaticLocal is a `static` variable declared inside a function. Each gets a
// unique label so two functions may both name theirs `x`, and it persists
// across calls.
type StaticLocal struct {
	// Label is the unique assembly label for this one.
	Label string
	// Decl is the declaration, which carries the type and the initialiser.
	Decl *frontend.DeclStmt
}

// TLSVar is one thread-local variable's layout in the .tls section. One
// instance exists per thread, so these cannot live in .data.
type TLSVar struct {
	// Name is the source-level name.
	Name string
	// Decl is the declaration, carrying the type.
	Decl *frontend.DeclStmt
	// Offset is the byte offset from the start of the .tls section.
	Offset int
	// Label is the assembly label, which is also the rip-resolvable address
	// on Linux.
	Label string
}

// StringConst is a string literal in the program, with the .rdata label that
// addresses it.
type StringConst struct {
	// Label is the .rdata label.
	Label string
	// Text is the literal's value, already decoded from its source escapes.
	Text string
	// Wide marks an L"..." literal, whose Text holds UTF-16LE code units
	// rather than UTF-8 bytes. It gets a 2-byte NUL terminator instead of one.
	Wide bool
}

// WordArray is a C23 _BitInt literal too wide for an inline mov, emitted as
// its little-endian 64-bit word image in .rdata and addressed by label.
type WordArray struct {
	Label string
	Words []uint64
}

// DoubleConst is a floating-point literal, emitted as an 8-byte .rdata image
// addressed by label.
type DoubleConst struct {
	Label string
	Value float64
}

// Entry describes the program's entry point. goc synthesises one: there is no
// C main signature to rely on, and the platform's expectation differs
// (WinMain for a GUI subsystem, plain main otherwise).
type Entry struct {
	// Func is the assembly symbol for the entry function.
	Func string
	// Exit is the platform's termination call: ExitProcess on Windows, the
	// exit syscall stub on Linux.
	Exit string
	// IsGUI selects the Windows GUI entry (WinMain) over the console one.
	IsGUI bool
	// Wide selects the wide (W) entry variant, for a wWinMain.
	Wide bool
	// TakesArgs records whether the entry reads argc/argv. The stub passes
	// them on Windows and does not on Linux, where they already sit on the
	// stack the kernel built.
	TakesArgs bool
}

// Data is everything a back end tells the linker about the program. The
// back end has already decided what the program contains; this package decides
// where each piece goes in the image.
type Data struct {
	// Program is the checked AST, read for its globals and their initialisers.
	Program *frontend.Program

	// Body is the function-body assembly the back end generated, spliced in
	// after the entry stub and before the data sections.
	Body string

	Entry Entry

	// UnitPrefix distinguishes this unit's compiler-minted labels -- string
	// literals, statics -- from another unit's. A whole program has one unit and
	// needs none; a unit on its way to becoming an object file does, or two
	// objects in one link would both define LC0.
	UnitPrefix string

	// Linux selects an ELF image rather than a PE.
	Linux bool
	// WinGUI selects the GUI subsystem on a PE, which is what makes the
	// linker emit `subsystem windows`. It is independent of Entry.IsGUI:
	// a program can be built as a GUI with a plain main().
	WinGUI bool
	// Opt is the optimisation level, which reaches the linker only so that the
	// image it produces matches what the back end optimised for.
	Opt int

	// Imports are the external symbols the stub must declare, already
	// formatted for the assembler's `extern` directive.
	Imports []string

	// Globals maps a global's name to the .data label holding it. A name
	// absent from the map belongs to another back end and is not emitted
	// here. It doubles as the lookup table for `int *p = &g;` and
	// `const char *p = json_words;` in a static initialiser.
	Globals map[string]string

	// StaticVars maps a static local's source name to its .data label, for
	// the same address-of-an-initialiser lookup. Only the function currently
	// being generated contributes: a static local's name is not visible
	// outside its function, so an initialiser that names one is a reference
	// the walk cannot resolve and the slot stays unbound.
	StaticVars map[string]string

	// FuncAddr resolves a function designator in a static initialiser to the
	// symbol whose address it decays to, reporting false when that function's
	// code is not in this image. Registering the name is also what pulls a
	// C-library function into the emitted set when only its address is taken.
	FuncAddr func(name string) (string, bool)

	// Strings is the program's string-literal pool, in the order the .rdata
	// section must emit it. StrLabs maps a literal node to its label so a
	// literal reached twice keeps one definition; the linker appends to both
	// when a static initialiser introduces a literal the body never mentioned.
	Strings []StringConst
	StrLabs map[*frontend.StrLit]string

	// BigWords are the _BitInt literals too wide for an inline mov, each
	// emitted as a word array in .rdata.
	BigWords []WordArray

	// Doubles are the floating-point literals, as 8-byte .rdata images.
	Doubles []DoubleConst

	// StringSlots, FuncSlots and ArraySlots are the pointer slots the startup
	// code has to bind. The linker appends the ones it discovers while
	// walking initialisers to whatever the back end already found.
	StringSlots []StringSlot
	FuncSlots   []FuncSlot
	ArraySlots  []ArraySlot

	// Statics are the function-scope statics, laid out in .data.
	Statics []StaticLocal

	// TLSVars are the thread-local variables, laid out in .tls in this order.
	// The back end supplies each one's offset and label because it is the one
	// that generated the access code reading them.
	TLSVars []TLSVar

	// LibGlobals are the C library's file-scope variables (rand_state and
	// friends). They share the global pool.
	LibGlobals []*frontend.DeclStmt
	// LibGlobalsUsed names the subset the program actually reaches, so an
	// unreferenced one costs nothing.
	LibGlobalsUsed map[string]bool

	// IsZeroInit reports whether a declaration's initialiser writes nothing
	// but zeros, which decides whether it needs a .data image or only .bss.
	// Nil means the built-in folding in this package, which is what a back end
	// that has not been asked to fold differently wants.
	IsZeroInit func(*frontend.DeclStmt) bool

	// SymbolInObject reports whether the other back end put a definition of
	// this name into the object, so this one must not define it again. A
	// duplicate definition is not a performance loss: which of the two wins
	// depends on link order rather than on anything the programmer wrote.
	//
	// The distinction is per name, not a mode. Under the LLVM back end most
	// globals are defined in the object, but one whose address a static
	// initialiser takes -- "int *gp = &g" -- is emitted as an `external` global
	// with no storage, because the value is a relocation the object cannot
	// contain on its own. That one still needs a .data image here, and asking
	// per name is what tells the two apart.
	//
	// Nil means the native generator's arrangement: nothing is defined in an
	// object, so every global is emitted here.
	SymbolInObject func(name string) bool

	// NeedsSlotBinding reports whether the object left a global's storage for
	// the stub to fill, and is what decides which pointer slots get bound.
	//
	// The IR front end fills an initialiser itself whenever it can: a string
	// literal becomes a getelementptr, a zero becomes zeroinitializer. What it
	// cannot express is the address of a global -- that is a relocation, so it
	// emits the slot as `external global` with no storage and leaves the value
	// to startup code. Those, and only those, are what the stub writes.
	//
	// Binding a slot the object already initialised is not harmless. The stub
	// writes to the global's address, which is a second definition of storage
	// the object owns, and which definition wins depends on link order rather
	// than on anything the programmer wrote.
	//
	// Nil means every global, which is the native generator's arrangement.
	NeedsSlotBinding func(label string) bool
}

// needsDefinition reports whether this package has to emit storage for a
// global: either because no other back end defined it, or because no back end
// did (the native generator, where every global is emitted here).
func (d *Data) needsDefinition(label string) bool {
	return d.SymbolInObject == nil || !d.SymbolInObject(label)
}

// zeroInit applies the caller's folding when it supplied one and this
// package's own otherwise.
func (d *Data) zeroInit(g *frontend.DeclStmt) bool {
	if d.IsZeroInit != nil {
		return d.IsZeroInit(g)
	}
	return isZeroInit(g)
}

// Error formats a diagnostic that names a function the assembler cannot
// resolve, listing what was available. The back end supplies the candidate
// list because only it knows which library functions the program can reach.
type UnknownFuncError struct {
	// Name is the unresolved function.
	Name string
	// Available lists what could have been called instead, for the message.
	Available []string
	// Linux distinguishes the two cases: no DLL, or no syscall goa knows.
	Linux bool
}

func (e *UnknownFuncError) Error() string {
	if e.Linux {
		return fmt.Sprintf("unknown function %q: not in the C library (%s), and not a Linux syscall goa knows",
			e.Name, strings.Join(e.Available, ", "))
	}
	return fmt.Sprintf("unknown function %q: not in the C library (%s), and no DLL named on its prototype (declare it as 'extern ret %s(args), dllname;')",
		e.Name, strings.Join(e.Available, ", "), e.Name)
}

// sortImports puts the extern declarations in a fixed order, so the same
// program always produces the same stub.
func sortImports(imports []string) []string {
	out := append([]string(nil), imports...)
	sort.Strings(out)
	return out
}

// ResolveEntry picks the program's entry point and its exit call.
//
// main is the C standard name. A Windows GUI program has none and instead
// defines wWinMain (Unicode) or WinMain (ANSI) -- both are just as much a
// language-level entry point, so the compiler accepts them rather than making
// the caller write a main() shim. wWinMain wins if present, matching the
// /SUBSYSTEM:WINDOWS convention of preferring the wide API. Neither is valid
// on Linux, where the entry really is main(argc, argv, envp).
//
// TakesArgs records that the entry reads argc/argv, which decides whether the
// stub has to parse the command line before calling it (main(void) does not).
//
// It lives here because a program must not get a different entry point
// depending on which compiler built it: the same source that runs under goc
// has to run under gocl, and an entry chosen in one and not the other is a
// link error with no obvious cause.
func ResolveEntry(funcs map[string]*frontend.FuncDecl, linux bool) (Entry, error) {
	e := Entry{Exit: "exit"}
	if fn, ok := funcs["main"]; ok {
		e.Func = "main"
		e.TakesArgs = len(fn.Params) > 0
	} else if !linux {
		if _, ok := funcs["wWinMain"]; ok {
			e.Func, e.IsGUI, e.Wide = "wWinMain", true, true
		} else if _, ok := funcs["WinMain"]; ok {
			e.Func, e.IsGUI = "WinMain", true
		}
	}
	if e.Func == "" {
		return e, fmt.Errorf("no entry point: define main (or, on Windows, wWinMain or WinMain)")
	}
	return e, nil
}
