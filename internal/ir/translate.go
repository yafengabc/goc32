package gocl

// Building the IR for a whole program.
//
// Initialisers for globals are constant-folded here; see constInit for the forms
// that are covered and what happens to the rest.

import (
	"fmt"
	"goc/common"
	"goc/common/link"
	"goc/frontend"
	"strconv"
	"strings"
)

//
// One generator owns every C function in the program -- the user's and the C
// runtime's alike -- and goa's own assembler is left with the entry stub, which
// is not C at all: it establishes the process stack per the platform ABI before
// main runs. The split is therefore between two different *kinds* of code rather
// than two compilers racing over the same symbols.
//
// That is a change from an earlier arrangement where each function went to
// whichever generator could handle it. Sharing a program that way needs a symbol
// table in both, and they disagreed: over a global's assembler-level name, over
// whether an undefined symbol was an import or something the other half already
// defined, and over the variadic calls the native path rewrites by inspecting a
// format string. Every one of those produced either a link error or a silently
// wrong answer.

// translateProgram lowers a whole program to one LLVM IR module: the user's
// globals, the user's functions, and every C runtime function the program reaches.
//
// lib is the parsed C runtime; a nil lib means "user code only" (used by the unit
// tests, which exercise fragments without the runtime present). Its function table
// is consulted for prototypes and for the reachability walk that decides which
// runtime code has to be emitted.
// softFloatBuiltins are the compiler-rt helpers a target without floating-point
// registers needs. LLVM names them when it lowers `double` arithmetic, a
// double-to-integer conversion, or a 64-bit division -- none of which appear as
// a call in the source, so they have to be requested by name rather than
// reached. The list is the set libgcc's soft-float library provides for the
// operations goc's own runtime performs; a name that is missing shows up as an
// undefined symbol naming it, which is a link error rather than a wrong answer.
//
// The `__*df2` family is the comparison set (each returns 0 or 1, matching what
// an `fcmp`/`cset` pair expects); the `__*df3` family is arithmetic; the
// conversions bridge both directions between `double` and the 64-bit integers.
var softFloatBuiltins = []string{
	// double
	"__adddf3", "__subdf3", "__muldf3", "__divdf3",
	"__eqdf2", "__nedf2", "__ltdf2", "__ledf2", "__gedf2", "__gtdf2",
	"__unorddf2",
	"__fixdfdi", "__fixdfsi", "__fixunsdfdi",
	"__floatsidf", "__floatdidf", "__floatunsidf", "__floatundidf",
	// float (a target with no FPU at all lowers `float` ops to these too)
	"__addsf3", "__subsf3", "__mulsf3", "__divsf3",
	"__eqsf2", "__nesf2", "__ltsf2", "__lesf2", "__gtsf2", "__gesf2",
	"__unordsf2",
	"__extendsfdf2", "__truncdfsf2",
	"__fixsfsi", "__fixsfdi", "__fixunssfsi", "__fixunssfdi",
	"__floatsisf", "__floatdisf", "__floatunsisf", "__floatundisf",
	"__udivdi3",
}

// doubleConvBuiltins are the conversions between a double and a 64-bit
// integer. A hard-float target needs these even though it has no use for the
// arithmetic above: VFP converts to and from a 32-bit int (vcvt.s32.f64), not
// a 64-bit one, so a `long` cast or a `long long` argument to printf names
// __fixdfdi, __fixunsdfdi and __floatdidf on armhf just as it does on a target
// with no FPU at all. Asking for the whole soft-float set there would link
// seventeen functions a VFP instruction already implements.
var doubleConvBuiltins = []string{"__fixdfdi", "__fixunsdfdi", "__floatdidf", "__floatundidf"}

// intBuiltins are the 64-bit integer multiply and divide helpers, for a target
// with no divide instruction. They are separate from softFloatBuiltins because
// they have nothing to do with floating point: a target can need these and have
// hardware doubles (RISC-V with the F extension), or need the float ones and
// divide in hardware.
var intBuiltins = []string{
	"__muldi3", "__divdi3", "__moddi3", "__udivdi3", "__umoddi3",
}

// int32Builtins are the 32-bit divide and remainder helpers. RV32 needs them
// and RV64 does not: the 64-bit ISA has `divw`/`divuw`/`remw`/`remuw`, so a
// 32-bit quotient there is an instruction, while the 32-bit baseline has no
// divide at any width and lowers every one of these to a call.
var int32Builtins = []string{"__divsi3", "__modsi3", "__udivsi3", "__umodsi3"}

func translateProgram(prog *frontend.Program, lib *common.Program, linux bool, opt int, arch string) (string, map[string]bool, []string, error) {
	m := newIRMod(linux, arch)
	// Whether this build links through gocld alone decides if the module owns
	// the entry point and the raw syscalls. It is recorded before any body is
	// generated, because a call the C library makes to `write` is turned into a
	// generated stub or left as a declare depending on the answer.
	m.nativeLink = UsesNativeLink(linux, arch, prog, lib)
	// -Os is a property of the module rather than of one function: it decides
	// which pipeline runs, and the optsize attribute is what that pipeline reads.
	m.optSize = opt == 2
	// Thread-local globals live in the linker-owned .tls section, reached
	// through the __goc_tls_slot helper. Record each one's offset up front so
	// every reference in the IR uses the same number the .tls image reserves.
	for _, tv := range ComputeTLSLayout(prog, lib, linux) {
		m.tlsOffsets[tv.Name] = int64(tv.Offset)
	}
	defined := map[string]bool{}
	tr := &typeResolver{
		funcDefs:   map[string]*frontend.FuncDecl{},
		globalTyp:  map[string]*frontend.Type{},
		staticVars: map[string]string{},
		lib:        lib,
	}

	// A file-scope static in the user's program can share a spelling with one
	// in the C runtime: phase1.c declares its own `static unsigned long
	// rand_state` and stdlib.c has one too. Both are internal to their own
	// translation unit, so they are two different objects -- but this front
	// end names every global "G_<name>", which emitted one symbol twice and
	// LLVM rejected the module with "redefinition of global". Rename the
	// user's copy the same way mergePrograms does when two user files declare
	// the same static, so each side keeps its own storage.
	if lib != nil {
		renames := map[string]string{}
		for _, lg := range lib.Globals {
			for _, gl := range prog.Globals {
				if gl.Name != lg.Name {
					continue
				}
				cand := gl.Name + "__tu0"
				for i := 1; irNameTaken(prog, lib, cand); i++ {
					cand = fmt.Sprintf("%s__tu0_%d", gl.Name, i)
				}
				renames[gl.Name] = cand
				break
			}
		}
		common.RenameInProgram(prog, renames)
	}

	// The function table has to hold both halves before anything is generated, so
	// a call can be resolved whether its target is the user's or the runtime's.
	tr.userDefs = map[string]bool{}
	for _, f := range prog.Funcs {
		tr.funcDefs[f.Name] = f
		tr.userDefs[f.Name] = true
		defined[f.Name] = true
	}
	// What a name in a static initialiser can refer to. It has to be collected
	// before any initialiser is lowered, because lowering happens while the
	// definitions are still being built: "&g" has to know whether g is an
	// object (emitted as G_g) or a function (emitted as @g).
	m.symKind = map[string]string{}
	for _, f := range prog.Funcs {
		m.symKind[f.Name] = "func"
	}
	if lib != nil {
		for _, f := range lib.Funcs {
			m.symKind[f.Name] = "func"
		}
		for _, p := range lib.Protos {
			m.symKind[p.Name] = "func"
		}
	}
	for _, gl := range prog.Globals {
		m.symKind[gl.Name] = "global"
	}
	if lib != nil {
		for _, lg := range lib.Globals {
			if _, shadowed := m.symKind[lg.Name]; !shadowed {
				m.symKind[lg.Name] = "global"
			}
		}
	}
	// The runtime's globals, when the program's own do not shadow them, are part
	// of the module for the same reason its functions are -- and they have to be
	// *defined* here, not merely given a type. Registering the type alone left
	// every reference to a C runtime global pointing at a symbol nobody emitted:
	// genWith skips these names because `claimed` says LLVM owns them, so neither
	// half defined them and the program died on the first access. stdout is the
	// one every program touches, which is why writing anything at all crashed.
	//
	// Only the *types* are registered here. The definitions come later, once
	// reachability is known: the runtime declares about 25 file-scope
	// variables and a program that only calls write() needs none of them, yet
	// emitting all of them cost a whole extra 1 KB of .data -- enough to make
	// `print("hello world")` larger under -fllvm than under the native
	// generator, which prunes by useLibGlobal. See emitLibGlobals.
	if lib != nil {
		for _, lg := range lib.Globals {
			if _, dup := tr.globalTyp[lg.Name]; dup {
				continue
			}
			tr.globalTyp[lg.Name] = lg.Typ
			// Claimed whatever happens below, including the case where nothing
			// is emitted. genWith skips these names on the strength of this
			// same map, so the symbol keeps exactly one owner: if the reachability
			// walk were to miss a reference, the symptom is a link error naming
			// a global, not two definitions of one.
			defined["G_"+lg.Name] = true
		}
	}

	// --- globals ---
	// A definition needs its initialiser lowered to an LLVM constant, which this
	// front end does for the forms C programs actually use: a scalar constant, a
	// string, and a brace-initialised array or struct of them. A global whose
	// initialiser is not one of those keeps the native generator's definition
	// and is declared external here.
	for _, gl := range prog.Globals {
		// Register the type first, whatever happens to the definition below.
		// Expression typing looks a name up in tr.globalTyp, and a global that
		// was never registered there resolved to no type at all -- which the
		// emitters read as i32. A file-scope array then loaded its first
		// element instead of decaying to its address, so every subscript of a
		// global array came out as "ptrtoint ptr <i32>" and LLVM rejected the
		// module; a global scalar in a comparison was compared against the
		// symbol itself instead of being loaded. Registering the type is what
		// makes both of those decay and load correctly.
		//
		// The program's own globals are registered after the runtime's, so a
		// user definition shadows a runtime name of the same spelling.
		if gl.Typ != nil {
			tr.globalTyp[gl.Name] = gl.Typ
		}
		if gl.Typ == nil || typUnsupported(gl.Typ) {
			continue
		}
		ty := m.llirType(gl.Typ)
		// Thread-local storage is owned by the linker: it lays the variable out
		// in the .tls section and provides __goc_tls_slot to reach it per
		// thread. The IR references it through that helper (ident / lvalue),
		// never as a direct global, so no IR symbol is declared here.
		if gl.IsTLS {
			continue
		}
		init, ok := m.constInit(gl.Init, gl.Typ)
		if !ok {
			m.noteExternGlobal("G_"+gl.Name, ty)
			continue
		}
		m.globals = append(m.globals, irGlobal{
			name: "G_" + gl.Name, ty: ty, init: init,
		})
		defined["G_"+gl.Name] = true
	}

	// --- functions ---
	// The order does not matter: LLVM resolves calls to definitions that appear
	// later, because every call names its argument types explicitly.
	var wanted []*frontend.FuncDecl
	for _, f := range prog.Funcs {
		if !llvmEligible(f) {
			// Naming the constructs rather than a flag: the caller did not ask
			// for a back end, gocl is the back end, so the only thing worth
			// saying is which part of the language is not modelled yet.
			return "", nil, nil, fmt.Errorf(
				"%s uses a construct the LLVM front end does not model yet "+
					"(inline assembly, bit-fields or _BitInt)", f.Name)
		}
		wanted = append(wanted, f)
	}
	// The runtime's own functions: only the ones the program actually reaches.
	// Emitting every function in the runtime (~380 of them) is what made the
	// -fllvm binary 100k+ next to the native build's ~13; the native generator
	// prunes by reachability (c.need fixed-point), and the IR front end must do
	// the same or it ships a whole C library the program never calls.
	// llvmRoots returns that reachable set, seeded from the program's own calls
	// and the few helpers the entry stub names directly.
	if lib != nil {
		need, needGlobals := llvmRoots(prog, lib, linux, arch, tr)
		m.emitLibGlobals(lib, needGlobals)
		// lib.Order can name a function more than once -- the runtime's
		// sources are concatenated, so a name that two of them define reaches
		// the order list twice. Emitting it twice gave LLVM "invalid
		// redefinition of function"; one definition per name is all a module
		// may hold, and a name the user's own program defines is already
		// spoken for.
		emitted := map[string]bool{}
		for _, f := range prog.Funcs {
			emitted[f.Name] = true
		}
		for _, name := range lib.Order {
			if !need[name] || emitted[name] {
				continue
			}
			emitted[name] = true
			f := lib.Funcs[name]
			if f.Body == nil {
				continue // a prototype, not a definition
			}
			if !llvmEligible(f) {
				return "", nil, nil, fmt.Errorf(
					"the C runtime function %s uses a construct the LLVM front "+
						"end does not model yet", name)
			}
			wanted = append(wanted, f)
		}
	}

	// Every name this module will define is marked BEFORE any body is
	// generated, not as each one is reached. A call to a function defined later
	// in the list would otherwise emit a `declare` for it during the earlier
	// body's generation, and the later `define` would collide with it: LLVM
	// reads a declare followed by a matching define as a redefinition.
	for _, f := range wanted {
		m.defined[f.Name] = true
		defined[f.Name] = true
		tr.funcDefs[f.Name] = f
	}

	for _, f := range wanted {
		// The body still needs the name in `defined` so a recursive call emits
		// no declare; and `defined` tells the assembler half that this name
		// belongs to the LLVM object, so goa emits only the entry stub and
		// never re-defines it.
		body, err := genIRFunc(tr, m, f)
		if err != nil {
			return "", nil, nil, err
		}
		m.funcBodies = append(m.funcBodies, body)
	}
	return m.String(), defined, m.Externals(), nil
}

// irNameTaken reports whether a global spelling is already in use by the
// program or by the C runtime, so a rename cannot land on a second collision.
func irNameTaken(prog *frontend.Program, lib *common.Program, name string) bool {
	for _, g := range prog.Globals {
		if g.Name == name {
			return true
		}
	}
	if lib != nil {
		for _, g := range lib.Globals {
			if g.Name == name {
				return true
			}
		}
	}
	return false
}

// llvmRoots returns the set of C-runtime functions the IR front end must
// compile in, found by a fixed-point reachability walk over the program and
// the runtime's own bodies. It is the IR front end's counterpart of the
// native generator's c.need closure: a runtime function is pulled in only
// when something reachable actually calls it (or takes its address, which
// counts as a use), plus the few helpers the entry stub names directly.
//
// Emitting the whole runtime (every one of its ~380 functions) is what made
// the -fllvm binary 100k+ next to the native build's ~13; pruning to what the
// program reaches brings it back in line.
func llvmRoots(prog *frontend.Program, lib *common.Program, linux bool, arch string, tr *typeResolver) (map[string]bool, map[string]bool) {
	need := map[string]bool{}
	isLib := func(name string) bool {
		f, ok := lib.Funcs[name]
		return ok && f.Body != nil
	}
	// The runtime's file-scope variables are pruned on the same evidence as its
	// functions: a name goes in needGlobals when a reachable body mentions it.
	// isLibGlobal has to exclude function names, because the walk reports calls
	// and identifiers alike and most identifiers it reports are functions.
	needGlobals := map[string]bool{}
	isLibGlobal := map[string]bool{}
	for _, lg := range lib.Globals {
		isLibGlobal[lg.Name] = true
	}
	markGlobal := func(name string) {
		if isLibGlobal[name] {
			needGlobals[name] = true
		}
	}
	// The questions specializePrintfCall needs. A nil tr means there was no
	// emitter context; the specialisation is skipped then, which costs size but
	// never correctness.
	var q common.PrintfQueries
	if tr != nil {
		q = common.PrintfQueries{
			UserDefines:   func(name string) bool { return tr.userDefs[name] },
			ShadowedByVar: func(name string) bool { _, _, ok := tr.fnPtrVar(name); return ok },
		}
	}
	markReachable := func(name string) {
		if isLib(name) {
			need[name] = true
		}
	}
	// 1. References in the user's own code: a direct call, or an identifier
	//    used as a value (taking a runtime function's address, e.g.
	//    "fp = memcpy;"). In both the definition has to be in this object.
	//
	// A printf call is counted as whatever it will *become*. The emitter
	// rewrites printf("lit") to fwrite, so counting the name as written would
	// keep printf -- and through it vfmt and the float exponent machine --
	// reachable, and the prune would undo the rewrite, making the emitter's work
	// invisible. Both kinds of reference therefore go through one walk: a
	// second, plain walk over the same tree would reinstate the name the
	// rewrite just removed.
	for _, f := range prog.Funcs {
		frontend.WalkStmts(f.Body, func(s frontend.Stmt) {
			for _, e := range stmtExprs(s) {
				walkExpr(e, func(n frontend.Expr) {
					switch x := n.(type) {
					case *frontend.Call:
						if repl := common.SpecializePrintfCall(x, q); repl != nil {
							markReachable(repl.Name)
							// The rewrite introduces references of its own.
							// fwrite is a library function, and the stdout
							// accessor it is handed is a *call* the original
							// printf never contained -- nothing else pulls it
							// in, so the specialisation has to add both or the
							// link fails on a missing symbol.
							markReachable("fwrite")
							markReachable("__goclib_stdout")
							return
						}
						markReachable(x.Name)
					case *frontend.Ident:
						markReachable(x.Name)
						markGlobal(x.Name)
					}
				})
			}
		})
	}
	// 1b. The binary128 runtime, which long double is lowered to calls into.
	//     Those calls are created by the code generator, so no source-level
	//     reference exists for the sweep above to find -- the same blind spot
	//     the soft-float helpers have, and handled the same way.
	if usesLongDouble(prog) {
		for _, n := range tfRuntime {
			need[n] = true
		}
	}
	// 2. Entry-stub helpers. goa's own assembler still builds the entry stub,
	//    and it calls into the runtime for argument parsing and process
	//    termination; those names must exist in the LLVM object so the stub
	//    resolves against it rather than going undefined.
	need["__goclib_exit"] = true // stub terminator whenever the runtime is present
	// The soft-float helpers are referenced by the *compiler*, not by the
	// source: LLVM lowers a double operation on a target with no floating-point
	// registers into a call to __adddf3 & co, and a 64-bit division into a call
	// to __udivdi3. Nothing in the program mentions those names, so the
	// reachability sweep above cannot see them and the link would fail on
	// undefined symbols the moment a float or a long long appeared.
	//
	// They are marked unconditionally for that target rather than on demand,
	// because the reference does not exist yet at this point -- it is created
	// later, by the code generator.
	//
	// Which targets need them:
	//
	//   x86-64 / AArch64 -- SSE2 and the FP register file implement the whole
	//     set, including the conversions.
	//
	//   armel, riscv64, riscv32 -- no FPU. The RISC-V triples name no F or D
	//     extension, so a double there is soft: it arrives in an integer
	//     register and every operation is a call. That is slower than the
	//     hardware a real RV64 has, and it is also what keeps the flat va_list
	//     cursor correct, because a soft double takes one integer slot in the
	//     save area exactly like a long.
	//
	//   armhf (plain "arm") -- VFP does the arithmetic, but not the 64-bit
	//     conversions, so it asks for those three alone.
	switch arch {
	case "armel", "riscv64", "riscv32":
		for _, n := range softFloatBuiltins {
			need[n] = true
		}
	case "arm":
		for _, n := range doubleConvBuiltins {
			need[n] = true
		}
	}
	// RISC-V has no divide instruction in the baseline ISA, so a `long long`
	// quotient or product becomes a call to one of these. Unlike the soft-float
	// list this is not about floating point at all -- the linker asks for
	// __muldi3 and __udivdi3 on riscv64 as readily as on riscv32, because the
	// 64-bit divide needs a 128-bit intermediate either way.
	//
	// These are integer-only and cheap to get right, so they live in their own
	// file rather than alongside the float helpers.
	// armel needs the float ones too; it is listed above.
	//
	// armhf (plain "arm") is here as well: VFP gives it hardware doubles, but
	// nothing gives it a 64-bit integer divide, so a `long long` quotient or
	// remainder in the C library names __udivdi3 just as it does on RISC-V.
	if arch == "riscv64" || arch == "riscv32" || arch == "arm" || arch == "armel" {
		for _, n := range intBuiltins {
			need[n] = true
		}
	}
	if arch == "riscv32" {
		for _, n := range int32Builtins {
			need[n] = true
		}
	}
	if !linux {
		mainTakesArgs, gui, wide := false, false, false
		for _, f := range prog.Funcs {
			switch f.Name {
			case "main":
				mainTakesArgs = len(f.ParamTypes) >= 1
			case "WinMain":
				gui = true
			case "wWinMain":
				gui, wide = true, true
			}
		}
		if mainTakesArgs {
			need["__goclib_get_args"] = true
		}
		if gui {
			if wide {
				need["__goclib_lp_cmdline_w"] = true
			} else {
				need["__goclib_lp_cmdline_a"] = true
			}
		}
	}
	// 3. Fixed-point closure over the runtime's own bodies: any runtime
	//    function reachable from the above pulls in the runtime functions it
	//    calls in turn.
	for {
		changed := false
		for name := range need {
			f, ok := lib.Funcs[name]
			if !ok || f.Body == nil {
				continue
			}
			collectLibRefs(f.Body, func(callee string) {
				markGlobal(callee)
				if isLib(callee) && !need[callee] {
					need[callee] = true
					changed = true
				}
			})
		}
		if !changed {
			break
		}
	}
	// 3b. One more sweep for globals, now that `need` has stopped growing. The
	//     loop above only marks a global while it is visiting the body that
	//     mentioned it, and it visits each body once per round -- so a global
	//     first reached in the last round is marked, but this pass makes the
	//     "every reachable body was scanned" property explicit rather than an
	//     accident of the iteration order.
	for name := range need {
		if f, ok := lib.Funcs[name]; ok && f.Body != nil {
			collectLibRefs(f.Body, markGlobal)
		}
	}
	// 4. Full-exit upgrade, mirroring the native generator's needsFullExit:
	//    if the program touched the stdio streams, registered atexit, or
	//    called exit itself, the bare __goclib_exit is not enough -- the C
	//    standard's atexit/flush chain is required, and that lives in exit.
	//    The same fact decides the entry stub's terminator; see step 5.
	fullExit := false
	for _, name := range [...]string{"__goclib_stdout", "__goclib_stderr", "atexit", "exit"} {
		if need[name] {
			fullExit = true
			if isLib("exit") && !need["exit"] {
				need["exit"] = true
				// One more pass so exit's own callees come in.
				for {
					changed := false
					for en := range need {
						ef, ok := lib.Funcs[en]
						if !ok || ef.Body == nil {
							continue
						}
						collectLibRefs(ef.Body, func(callee string) {
							markGlobal(callee)
							if isLib(callee) && !need[callee] {
								need[callee] = true
								changed = true
							}
						})
					}
					if !changed {
						break
					}
				}
			}
			break
		}
	}
	// The exit upgrade above widened `need` again, so the globals of everything
	// it pulled in have to be swept as well.
	for name := range need {
		if f, ok := lib.Funcs[name]; ok && f.Body != nil {
			collectLibRefs(f.Body, markGlobal)
		}
	}
	// 5. The entry stub's terminator. The stub is written against the bare
	//    __goclib_exit (entryStubC) and is upgraded here, where step 4 has
	//    just established whether the program needs the full C exit. Without
	//    this the stub terminates the process directly: an atexit handler
	//    registered by main never runs, because nothing between the stub and
	//    exit_group walks the handler chain. The goa path, which builds its
	//    stub in assembly, reads the same decision off the object in linkData
	//    -- a symbol the object defines is the only one a stub may call, and
	//    exit is defined exactly when this upgrade fired.
	if fullExit {
		setStubExit(prog, "exit")
	}
	// A global the program itself declares is emitted by the loop over
	// prog.Globals, not here, but a *reference* to one from runtime code still
	// has to resolve. Runtime bodies only ever mention the runtime's own
	// globals, so nothing extra is needed for that; the sweep is here to make
	// the invariant checkable rather than assumed.
	return need, needGlobals
}

// emitLibGlobals defines the C runtime's file-scope variables that the
// reachability walk actually found a use for. The others are left out of the
// module entirely.
//
// The alternative -- defining all ~25 of them -- is not a rounding error. The
// runtime's variables are the big ones in the library (a 1 KB stdio buffer
// among them), and they land in .data whether or not the program touches them,
// so every image carried them. `print("hello world")` came out 1024 bytes
// larger under -fllvm than under the native generator for exactly this reason:
// the native generator's useLibGlobal marks a library global when an expression
// names it, and the IR front end had no equivalent.
//
// Types were registered earlier, for every global, and are deliberately not
// pruned: a name's type is what makes a subscript decay and a scalar load, and
// getting that wrong produces malformed IR rather than a missing symbol. Only
// the storage is pruned.
func (m *irMod) emitLibGlobals(lib *common.Program, need map[string]bool) {
	for _, lg := range lib.Globals {
		if !need[lg.Name] {
			continue
		}
		if lg.Typ == nil || typUnsupported(lg.Typ) {
			continue
		}
		if lg.IsTLS {
			// Owned by the linker's .tls section, like a program-scope TLS
			// global; reached through __goc_tls_slot, not declared here.
			continue
		}
		init, ok := m.constInit(lg.Init, lg.Typ)
		if !ok {
			m.noteExternGlobal("G_"+lg.Name, m.llirType(lg.Typ))
			continue
		}
		m.globals = append(m.globals, irGlobal{
			name: "G_" + lg.Name, ty: m.llirType(lg.Typ), init: init,
		})
	}
}

// ComputeTLSLayout assigns every thread-local global a byte offset inside the
// .tls section, in declaration order across the user program's globals and then
// the C runtime's reachable globals. The rule mirrors the native generator's
// tlsPlace exactly -- 8-byte alignment and link.TLSAlignedSize slots -- because
// two halves of the build (the IR that emits the access code and the linker
// stub that lays out the section) must agree on where each variable sits or the
// declared label lands off its own bytes.
//
// It is the single source of truth consumed by both translateProgram (which
// records the offsets in irMod.tlsOffsets for the IR emitter) and the linker
// stub (which fills link.Data.TLSVars); calling it once here keeps them in
// lockstep. A name that appears in both the program and the runtime resolves to
// the program's spelling, so the offset is assigned only once.
func ComputeTLSLayout(prog *frontend.Program, lib *common.Program, linux bool) []link.TLSVar {
	var vars []link.TLSVar
	seen := map[string]bool{}
	off := 0
	add := func(g *frontend.DeclStmt) {
		if g == nil || seen[g.Name] {
			return
		}
		seen[g.Name] = true
		off = (off + 7) &^ 7
		vars = append(vars, link.TLSVar{
			Name:   g.Name,
			Decl:   g,
			Offset: off,
			Label:  "TL_" + g.Name,
		})
		off += link.TLSAlignedSize(g.Typ)
	}
	for _, g := range prog.Globals {
		if g.IsTLS {
			add(g)
		}
	}
	if lib != nil {
		for _, g := range lib.Globals {
			if g.IsTLS {
				add(g)
			}
		}
	}
	return vars
}

// collectLibRefs visits every expression in a function body and reports the
// names of C-runtime functions it references: a direct call, or an identifier
// used as a value that names a runtime function (taking its address). Indirect
// calls contribute their target expression, so "fp = lib_fn; fp();" pulls
// lib_fn in through the assignment.
func collectLibRefs(body frontend.Stmt, fn func(string)) {
	if body == nil {
		return
	}
	frontend.WalkStmts(body, func(s frontend.Stmt) {
		for _, e := range stmtExprs(s) {
			walkExpr(e, func(n frontend.Expr) {
				switch x := n.(type) {
				case *frontend.Call:
					fn(x.Name)
				case *frontend.Ident:
					fn(x.Name)
				}
			})
		}
	})
}

// constInit lowers a global's initialiser to an LLVM constant.
//
// Only the forms C programs actually use are handled: a scalar constant, a
// string literal, and brace-initialised aggregates of those. Anything else --
// an address computed from another global, a cast, a function pointer -- reports
// false, and the caller then leaves the definition to the native generator and
// declares it external. That keeps a construct this front end cannot yet
// constant-fold compiling, at the cost of a second owner for that one symbol;
// the alternative, emitting a wrong constant, would be far worse.
func (m *irMod) constInit(e frontend.Expr, t *frontend.Type) (string, bool) {
	return m.constInitAt(e, t, true)
}

// constInitAt renders a global's initialiser. `top` says whether the value sits
// directly after the type in a "@g = global <ty> <value>" line.
//
// It matters for arrays. A constant in value position spells the type only when
// it is nested inside a struct or array -- "[2 x i8] c\"ab\"" -- but at the top
// level the type has already been written, and repeating it yields
// "@tzname = global [2 x ptr] [2 x ptr][...]", which LLVM rejects outright
// ("expected type"). frontend.Structs and scalars are unaffected: "{ ... }" carries no
// prefix in either position.
func (m *irMod) constInitAt(e frontend.Expr, t *frontend.Type, top bool) (string, bool) {
	if e == nil {
		return "zeroinitializer", true
	}
	if bi, ok := e.(*frontend.BraceInit); ok {
		return m.constAggregate(bi, t, top)
	}
	if sl, ok := e.(*frontend.StrLit); ok {
		return m.constString(sl.Bytes, t)
	}
	if n, ok := e.(*frontend.NumLit); ok {
		return m.constScalar(n, t)
	}
	// A cast of a constant folds away rather than becoming a runtime value.
	if c, ok := e.(*frontend.CastExpr); ok && c.Typ != nil {
		return m.constInitAt(c.E, c.Typ, top)
	}
	if u, ok := e.(*frontend.Unary); ok && (u.Op == "-" || u.Op == "+") {
		v, ok2 := m.constInitAt(u.E, t, false)
		if !ok2 {
			return "", false
		}
		if u.Op == "+" {
			return v, true
		}
		// A floating-point constant is spelled by its bit pattern in LLVM IR
		// (e.g. "double 0x4000000000000000"), and LLVM rejects a leading "-"
		// on one: "@g = global double -0x4000000000000000" fails to parse with
		// "expected value token". Negation flips the sign bit instead of
		// prepending "-". Integers have no such restriction -- "i64 -5" is
		// fine, and "i64 neg (...)" is what LLVM objects to -- so they keep
		// negateConst.
		if t != nil && (t.Kind == frontend.KFloat || t.Kind == frontend.KDouble) {
			return negateFPConst(v), true
		}
		return negateConst(v), true
	}
	// The address of an object, optionally behind a cast: "&g" and
	// "(NimStrPayload*)&g". C writes the address of a global in a static
	// initialiser constantly, and LLVM can hold it directly as a constant
	// expression -- so there is no reason to fall back to zeroinitializer,
	// which is what left Nim's string constants pointing at nothing.
	if name := constInitAddrName(e); name != "" {
		switch m.symKind[name] {
		case "global":
			return "@G_" + name, true
		case "func":
			// irFuncSym for the same reason a call site uses it: the address
			// has to name the symbol the function was *defined* under.
			return "@" + irFuncSym(name), true
		}
	}
	// An enumeration constant used as an initialiser ("enum Color g = GREEN",
	// "int n = TWO") is a compile-time integer with no storage of its own: it
	// lives only in frontend.EnumConsts. Without folding it here the global's
	// initialiser resolved to nothing, so the definition was left to the native
	// generator and the symbol came out `external`, and the link then failed
	// with "undefined symbol(s): G_g_color, G_g_num". An enumerator is never a
	// global, so this cannot collide with the array-decay case below.
	if id, ok := e.(*frontend.Ident); ok {
		if v, ok := frontend.EnumConsts[id.Name]; ok {
			return m.constScalar(&frontend.NumLit{Val: v, Kind: frontend.TInt}, t)
		}
	}
	// A bare identifier naming an array decays to the address of its first
	// element, the same way it does in an expression.
	if id, ok := e.(*frontend.Ident); ok && m.symKind[id.Name] == "global" {
		return "@G_" + id.Name, true
	}
	return "", false
}

// constInitAddrName reports the object an "&name" in a static initialiser
// names, peeling the casts C puts in front of it. It answers "" when the
// expression is not an address-of; a bare identifier is only an address when it
// names an array, which the caller checks against the symbol table.
func constInitAddrName(e frontend.Expr) string {
	for {
		c, ok := e.(*frontend.CastExpr)
		if !ok {
			break
		}
		e = c.E
	}
	u, ok := e.(*frontend.Unary)
	if !ok || u.Op != "&" {
		return ""
	}
	if id, ok := u.E.(*frontend.Ident); ok {
		return id.Name
	}
	return ""
}
func negateConst(v string) string {
	if strings.HasPrefix(v, "-") {
		return v[1:]
	}
	if v == "0" {
		return v
	}
	return "-" + v
}

// negateFPConst negates a floating-point constant that is spelled by its 64-bit
// bit pattern ("0xHHHHHHHHHHHHHHHH"), as constScalar emits for every FP literal.
// LLVM IR rejects a leading "-" on an FP constant, so negation must flip the sign
// bit rather than prepend "-": -2.0 is 0xC000000000000000, not -0x4000000000000000.
// Every FP constant here is widened to a 64-bit pattern (a float is the bits of
// its double widening), so the sign bit is always bit 63.
func negateFPConst(v string) string {
	if !strings.HasPrefix(v, "0x") {
		return "-" + v
	}
	bits, err := strconv.ParseUint(v[2:], 16, 64)
	if err != nil {
		return "-" + v
	}
	bits ^= 0x8000000000000000
	return fmt.Sprintf("0x%016x", bits)
}

func (m *irMod) constScalar(n *frontend.NumLit, t *frontend.Type) (string, bool) {
	// Kind == frontend.TDouble is what marks a literal as floating point at all.
	// IsFloat only says WHICH width -- "1.5f" is float, "1.5" is double -- so
	// testing it alone sent a plain double literal down the integer path and
	// emitted "double 0" for "3.14159".
	if n.Kind == frontend.TDouble {
		// LLVM spells FP constants in the 16-digit double form even for
		// float, and rejects one the type cannot hold exactly: a float
		// constant is therefore the bit pattern of the double that the float
		// widens to, not the 8-digit float pattern (which it refuses).
		bits := f64bits(n.Fval)
		if n.IsFloat || (t != nil && t.Kind == frontend.KFloat) {
			bits = f64bits(float64(float32(n.Fval)))
		}
		return fmt.Sprintf("0x%016x", bits), true
	}
	if n.BigWords != nil {
		// A _BitInt constant is a word array; the front end does not model the
		// type, so this stays with the native generator.
		return "", false
	}
	// A narrow type keeps only the low bits of the value, which is what C says a
	// conversion to that type does.
	v := n.Val
	if t != nil && t.Kind == frontend.KInt {
		switch t.Width {
		case 1:
			v = int64(int8(v))
		case 2:
			v = int64(int16(v))
		case 4:
			v = int64(int32(v))
		}
	}
	// A zero integer constant in pointer position is a null pointer; "ptr 0"
	// is not a thing LLVM accepts.
	if v == 0 && t != nil && (t.Kind == frontend.KPtr || t.Kind == frontend.KFunc) {
		return "null", true
	}
	return strconv.FormatInt(v, 10), true
}

func (m *irMod) constString(b []byte, t *frontend.Type) (string, bool) {
	// A char array is initialised by copying the bytes; a char pointer takes
	// the address of a private copy of them.
	//
	// The terminator is ONE element wide, not one byte: an L"..." literal's
	// Bytes hold UTF-16LE code units, so `wchar_t *p = L"ab"` needs two zero
	// bytes after them. A single byte there left the last wchar_t
	// half-initialised and %ls read straight past the terminator into
	// whatever constant followed it.
	w := 1
	if t != nil && t.Elem != nil && t.Elem.Width > 1 {
		w = t.Elem.Width
	}
	if t == nil || t.Kind == frontend.KArr {
		n := t.Len
		if n <= 0 {
			n = len(b) + w
		}
		body := cStringN(b, n)
		name := m.internConst(body, "["+strconv.Itoa(n)+" x i8]")
		if t != nil && t.Kind == frontend.KArr {
			return body, true
		}
		return "getelementptr inbounds ([" + strconv.Itoa(n) +
			" x i8], ptr @" + name + ", i64 0, i64 0)", true
	}
	if t.Kind == frontend.KPtr {
		n := len(b) + w
		name := m.internConst(cStringN(b, n), "["+strconv.Itoa(n)+" x i8]")
		return "getelementptr inbounds ([" + strconv.Itoa(n) +
			" x i8], ptr @" + name + ", i64 0, i64 0)", true
	}
	return "", false
}

// constAggregate renders a brace initialiser for an array or struct.
//
// Inside an aggregate, a scalar element must carry its own type:
// "[ptr getelementptr(...), ptr null]", not "[getelementptr(...), null]". LLVM
// only lets a struct body ("{ ... }") and a nested array omit it, and a global
// line already has the aggregate's own type written -- so an element without one
// is read as the start of a type and the module is rejected with "expected
// type". See constInitAt for what `top` means.
func (m *irMod) constAggregate(b *frontend.BraceInit, t *frontend.Type, top bool) (string, bool) {
	if t == nil {
		return "", false
	}
	switch t.Kind {
	case frontend.KArr:
		parts := make([]string, 0, t.Len)
		byIdx := map[int]frontend.Expr{}
		order := []int{}
		for _, el := range b.Elems {
			idx := len(order)
			if el.DesigIdx >= 0 {
				idx = el.DesigIdx
			}
			if _, dup := byIdx[idx]; !dup {
				order = append(order, idx)
			}
			byIdx[idx] = el.E
		}
		for i := 0; i < t.Len; i++ {
			e, ok := byIdx[i]
			if !ok {
				// A skipped element still needs its type: "[i32 1, i32 2,
				// 0, 0]" is read as a type where a value belongs.
				parts = append(parts, m.typedInAggregate(m.zeroOf(t.Elem), t.Elem))
				continue
			}
			v, ok := m.constInitAt(e, t.Elem, false)
			if !ok {
				return "", false
			}
			parts = append(parts, m.typedInAggregate(v, t.Elem))
		}
		body := "[" + joinStrings(parts, ", ") + "]"
		if top {
			return body, true
		}
		return "[" + strconv.Itoa(t.Len) + " x " + m.llirType(t.Elem) + "]" + body, true
	case frontend.KStruct, frontend.KUnion:
		// Which member each element initialises is the front end's
		// decision, and it is not the one a naive pairing makes. A
		// positional element pairs with posMembers(t)[i]: an anonymous
		// struct/union member is transparent, so the members it promoted
		// pair with the initialisers directly ("{1,2,3,10,20,'Z'}" fills x,
		// y, a, b and c -- the two shells consume nothing). A designator
		// names any member of the list, a promoted one included, so
		// ".b = 55" has to reach the b that lives inside a shell. Counting
		// the shells as positional slots put the value into the shell and
		// left the field the C source names at zero, and pairing Elems[i]
		// with Members[i] ignored a designator altogether.
		byTarget := map[*frontend.Member]frontend.Expr{}
		vis := frontend.PosMembers(t)
		for i, el := range b.Elems {
			if el.Desig != "" {
				mi := frontend.MemberIndex(t, el.Desig)
				if mi < 0 || mi >= len(t.Members) {
					continue // the checker has already reported it
				}
				byTarget[t.Members[mi]] = el.E
				continue
			}
			idx := i
			if el.DesigIdx >= 0 {
				idx = el.DesigIdx
			}
			if idx < 0 || idx >= len(vis) {
				continue // out of range: the checker has already reported it
			}
			byTarget[vis[idx]] = el.E
		}
		if t.Kind == frontend.KUnion {
			// C initialises only one member of a union: the first element's
			// target -- the first visible member, or the one a designator
			// names. The shell is transparent here too, so "= {5}" over
			// "union {struct{short lo,hi;}; int whole;}" fills whole. The
			// LLVM type spells that member's storage as its single first
			// field, and the members share offset 0, so the value is
			// rendered in the field's type and the low bytes are the ones C
			// reads back through the member it named.
			if len(b.Elems) == 0 {
				return "zeroinitializer", true
			}
			el := b.Elems[0]
			var mem *frontend.Member
			if len(vis) > 0 {
				mem = vis[0]
			}
			if el.Desig != "" {
				if mi := frontend.MemberIndex(t, el.Desig); mi >= 0 && mi < len(t.Members) {
					mem = t.Members[mi]
				}
			}
			owner := unionOwner(t)
			if mem == nil || mem.Type == nil || owner == nil || owner.Type == nil {
				return "", false
			}
			// The value has to sit at the union's offset 0 and fit inside
			// the field. A member that starts further in (".hi = 3" of a
			// {short lo, short hi} shell) or an aggregate target cannot be
			// spelled as a constant expression, so the definition is left
			// to the native generator, which writes it with a store.
			if mem.Offset != 0 || frontend.Sizeof(mem.Type) > frontend.Sizeof(owner.Type) ||
				isAggType(mem.Type) || isAggType(owner.Type) {
				return "", false
			}
			var v string
			var ok bool
			if isAnonShell(owner) {
				// Every storage owner is a shell, so the field is the
				// shell's own type and the value goes back inside it, aimed
				// at the member the element named.
				v, ok = m.constInitAt(anonShellBrace(owner,
					map[string]frontend.Expr{mem.Name: el.E}), owner.Type, false)
			} else {
				v, ok = m.constInitAt(el.E, owner.Type, false)
			}
			if !ok {
				return "", false
			}
			parts := []string{m.typedInAggregate(v, owner.Type)}
			// The union's extra bytes are a member the C source never
			// names, so append them here to match the type (see
			// unionLayout).
			if _, pad := unionLayout(t); pad > 0 {
				parts = append(parts, "["+strconv.Itoa(pad)+" x i8] zeroinitializer")
			}
			return "{" + joinStrings(parts, ", ") + "}", true
		}
		// One part per storage owner -- the members that own storage, which
		// is exactly what the LLVM type lists as fields (see structType in
		// module.go). A promoted sub-member is not one: it lives inside its
		// shell and is rendered as part of the shell's nested brace.
		parts := make([]string, 0, len(t.Members))
		for _, mem := range t.Members {
			if mem.AnonBase != nil {
				continue
			}
			if isAnonShell(mem) {
				// One field of the LLVM type, but the values C aimed at
				// the members this shell promoted have to be gathered back
				// into it.
				v, ok := m.constInitAt(anonShellBrace(mem, shellVals(t, mem, byTarget)),
					mem.Type, false)
				if !ok {
					return "", false
				}
				parts = append(parts, m.typedInAggregate(v, mem.Type))
				continue
			}
			if x, ok := byTarget[mem]; ok && x != nil {
				v, ok := m.constInitAt(x, mem.Type, false)
				if !ok {
					return "", false
				}
				parts = append(parts, m.typedInAggregate(v, mem.Type))
				continue
			}
			parts = append(parts, m.typedInAggregate(m.zeroOf(mem.Type), mem.Type))
		}
		if len(parts) == 0 {
			return "zeroinitializer", true
		}
		return "{" + joinStrings(parts, ", ") + "}", true
	}
	// A braced scalar.
	if len(b.Elems) > 0 {
		return m.constInitAt(b.Elems[0].E, t, top)
	}
	return "zeroinitializer", true
}

// isAggType reports whether t is an array, struct or union -- the kinds
// whose initialiser is a brace rather than a single scalar value.
func isAggType(t *frontend.Type) bool {
	return t != nil && (t.Kind == frontend.KArr ||
		t.Kind == frontend.KStruct || t.Kind == frontend.KUnion)
}

// shellVals collects the values aimed at the members an anonymous
// struct/union member promoted into t's member list, keyed by name.
// promoteAnonMembers copies each promoted member into the parent list
// keeping only its name, type and offset, so pointer identity does not
// survive the trip -- and the shell's own member list still knows them by
// name. A name that two different shells promoted is ambiguous in C itself;
// the first one wins, which is what the checker's memberIndex picks too.
func shellVals(t *frontend.Type, shell *frontend.Member, byTarget map[*frontend.Member]frontend.Expr) map[string]frontend.Expr {
	vals := map[string]frontend.Expr{}
	for _, sub := range t.Members {
		if sub.AnonBase != shell || sub.Name == "" {
			continue
		}
		if _, dup := vals[sub.Name]; dup {
			continue
		}
		if x, ok := byTarget[sub]; ok {
			vals[sub.Name] = x
		}
	}
	return vals
}

// anonShellBrace builds the brace the shell's own type would have been
// written with, out of the values aimed at its promoted members. Every
// element is designated: only a subset of the shell's members has a value,
// and a positional brace would pair them with the wrong fields.
func anonShellBrace(shell *frontend.Member, vals map[string]frontend.Expr) *frontend.BraceInit {
	sub := &frontend.BraceInit{}
	if shell == nil || shell.Type == nil {
		return sub
	}
	for _, vm := range frontend.PosMembers(shell.Type) {
		if x, ok := vals[vm.Name]; ok && x != nil {
			sub.Elems = append(sub.Elems, frontend.InitElem{Desig: vm.Name, E: x})
		}
	}
	return sub
}

// typedInAggregate gives a constant element its type when LLVM wants one.
//
// A struct body and a nested array are "{ ... }" and "[N x T] ...", which
// already say what they are. Everything else -- an integer, a pointer, a
// getelementptr, a null -- has to be written "<ty> <value>" when it sits inside
// an aggregate, or the reader takes the value for a type. A value that is
// already a compound constant, or a string literal (which is typed by its own
// c"..." form only when its type is stated), is passed through.
func (m *irMod) typedInAggregate(v string, t *frontend.Type) string {
	if v == "" {
		return v
	}
	if v == "zeroinitializer" {
		// A skipped element still has to say what it is: inside an
		// aggregate a bare "zeroinitializer" is read as a type. It is only
		// at the top of a global ("@g = global [3 x %point]
		// zeroinitializer") that the type is already written.
		return m.llirType(t) + " zeroinitializer"
	}
	if strings.HasPrefix(v, "c\"") {
		// A bare c"..." does NOT: inside an aggregate LLVM wants
		// "[6 x i8] c\"hello\\00\"".
		return m.llirType(t) + " " + v
	}
	switch t.Kind {
	case frontend.KStruct, frontend.KUnion:
		// A nested aggregate is spelled "<type> { ... }". A bare "{ ... }" is
		// taken for the enclosing body and the reader reports "expected '}' at
		// end of struct" -- and flattening it out ("{i32 1, i32 2, i32 3,
		// i32 4}") is rejected as having the wrong number of elements.
		return m.llirType(t) + " " + v
	case frontend.KArr:
		return v // "[N x T] ..." already carries its type
	case frontend.KPtr, frontend.KFunc:
		// Everything here needs the prefix, including null: "ptr null" and
		// "ptr getelementptr(...)" are accepted, while a bare null is read as
		// the start of a type ("expected type").
		return "ptr " + v
	}
	if t.Kind == frontend.KInt && (v == "true" || v == "false") {
		return "i1 " + v
	}
	return m.llirType(t) + " " + v
}

// zeroOf renders a zero value of a type, for an element the initialiser skips.
func (m *irMod) zeroOf(t *frontend.Type) string {
	if t == nil {
		return "0"
	}
	switch t.Kind {
	case frontend.KFloat:
		return "0.0"
	case frontend.KDouble:
		return "0.0"
	case frontend.KArr, frontend.KStruct, frontend.KUnion:
		return "zeroinitializer"
	case frontend.KPtr, frontend.KFunc:
		return "null"
	}
	return "0"
}

// internConst gives a constant a private global and returns its name, so the
// same bytes are stored once however many pointers refer to them.
func (m *irMod) internConst(body, ty string) string {
	if m.consts == nil {
		m.consts = map[string]string{}
	}
	if n, ok := m.consts[body]; ok {
		return n
	}
	name := ".const." + strconv.Itoa(len(m.consts))
	m.consts[body] = name
	m.globals = append(m.globals, irGlobal{name: name, ty: ty, init: body, constant: true})
	return name
}
