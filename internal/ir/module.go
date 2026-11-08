package gocl

import (
	"fmt"
	"goa"
	"goc/frontend"
	"sort"
	"strconv"
	"strings"
)

// LLVM IR module scaffolding: type rendering, name mangling and the global
// table.
//
// The IR is emitted as text rather than built through LLVM's C API. That is not
// a shortcut -- it is what makes the backend testable without a shared library
// present, keeps the output readable for debugging, and lets the same module go
// straight to LLVMVerifyModule for a real syntax check before anything is
// emitted. LLVM parses the text with the same parser it uses for .ll files, so
// what is verified here is exactly what would be compiled.

// irMod accumulates one LLVM IR module.
type irMod struct {
	structs map[string]bool           // struct type names already defined
	structN map[*frontend.Type]string // C struct type -> LLVM name
	seq     int

	globals []irGlobal
	// prototypes are emitted as `declare` so a call can be resolved even when
	// the definition lives in the object goa's own assembler produces (the C
	// runtime) or in another translation unit.
	protos map[string]bool
	// prototypeLines holds the `declare` statements, in insertion order, so a
	// call can be resolved before the definition appears later in the file.
	prototypeLines []string
	// typeLines holds the struct type definitions, emitted before anything
	// that might refer to them.
	typeLines []string
	// extSig de-duplicates external declarations by name and arity.
	extSig map[string]bool
	// extRet remembers a declared return type per external.
	extRet map[string]*frontend.Type
	// strings interns string constants, mapping their contents to a global.
	strings map[string]string
	// consts interns every folded constant by its rendered form, so two globals
	// initialised identically share one object instead of storing it twice.
	consts map[string]string
	// funcBodies holds each finished definition; they are appended after the
	// declarations so a function may call one defined later in the file.
	funcBodies []string
	// skipped names functions this module deliberately does not define
	// because another generator does. They still get declared, so calls to
	// them type-check and link.
	defined map[string]bool
	// symKind says what a C identifier written in a static initialiser names:
	// "global" for a file-scope or runtime object (emitted as G_<name>),
	// "func" for a function or a prototype. It is the only way "&g" in an
	// initialiser can be rendered as the address of the right symbol -- the
	// initialiser is lowered before the definitions exist, and a name that is
	// not in here is not something whose address can be taken.
	symKind map[string]string
	// optSize asks the optimiser for a size-oriented result. LLVM 21 removed
	// the `Os` pipeline and replaced it with the optsize attribute on the
	// functions to shrink, run under O2 -- so this is what -Os now means, and
	// the pipeline string alone can no longer ask for it.
	optSize bool
	// linux selects the ELF target triple and the SysV x86-64 data layout. The
	// IR front end owns this so the module it emits names the machine LLVM must
	// lower to -- goc's own code generator switches its whole register file on
	// the same flag, and the LLVM backend has to agree or the object describes a
	// different ABI than the C runtime it is linked against.
	linux bool
	// arch names the instruction set: "x86_64" (default), "aarch64", "arm"
	// (armv7), "riscv64", "riscv32". It chooses the target triple and the data
	// layout; the libLLVM build linked here ships all of them, so selecting one
	// is purely a matter of naming the right triple rather than recompiling
	// anything.
	arch string
	// tlsOffsets maps a C thread-local global's name to its byte offset inside
	// the .tls section. The linker lays the variable out there and provides the
	// __goc_tls_slot helper to reach it; the IR references the variable through
	// that helper with this offset, so the access code and the storage always
	// agree on where the variable lives.
	tlsOffsets map[string]int64
	// nativeLink records that this build links through gocld with no assembler
	// in the loop, which means this module also owns the entry point and every
	// raw syscall (see UsesNativeLink). When it is false the x86-64/goa path
	// is in use: the entry stub, the syscall stubs and the TLS storage all
	// arrive from goa's assembler, and the IR must leave them alone rather than
	// define a second copy of a symbol that would then be defined twice.
	nativeLink bool
	// syscallStubs names the raw Linux syscalls this module DEFINES rather than
	// declares. On a target whose entry point and syscalls are both generated
	// here (every non-x86_64 Linux target: see injectEntryStub) there is no goa
	// assembler in the loop to synthesise `mov rax,N; syscall`, so the call the
	// C runtime makes to, say, `write` has to be satisfied by a body in this
	// module. noteExtern records the name here and emitSyscallStub writes the
	// body.
	syscallStubs map[string]bool
	// stubRet / stubParams keep the signature the first call site used, which
	// is the C runtime's own prototype for that syscall. The stub must match
	// the call exactly (LLVM checks operand count and types against a defined
	// function), so the signature is recorded rather than guessed from the
	// syscall number.
	stubRet    map[string]*frontend.Type
	stubParams map[string][]*frontend.Type
}

type irGlobal struct {
	name string
	ty   string
	init string // constant expression, or "" for zero
	// external marks a global this module only references; it is emitted as a
	// declaration and the definition comes from the other generator.
	external bool
	// constant marks a private object holding folded initialiser bytes, not a
	// variable the program names.
	constant bool
}

func newIRMod(linux bool, arch string) *irMod {
	if arch == "" {
		arch = "x86_64"
	}
	m := &irMod{
		structs:      map[string]bool{},
		structN:      map[*frontend.Type]string{},
		protos:       map[string]bool{},
		defined:      map[string]bool{},
		extSig:       map[string]bool{},
		extRet:       map[string]*frontend.Type{},
		strings:      map[string]string{},
		consts:       map[string]string{},
		linux:        linux,
		arch:         arch,
		tlsOffsets:   map[string]int64{},
		syscallStubs: map[string]bool{},
		stubRet:      map[string]*frontend.Type{},
		stubParams:   map[string][]*frontend.Type{},
	}
	return m
}

// dso returns the "dso_local" attribute for the Linux target. A static ELF
// image is one non-preemptible unit: every global and function the module
// defines or references lives in the same executable, so marking it dso_local
// tells LLVM to bind references PC-relative (R_X86_64_PC32) instead of through a
// GOT (R_X86_64_GOTPCRELX), which a no-GOT static image cannot satisfy.
func (m *irMod) dso() string {
	if m.linux {
		return "dso_local "
	}
	return ""
}

// String returns the assembled module: type definitions, then globals, then
// prototypes, then the function bodies.
// Externals lists the globals this module declared but did not define, which
// is exactly the set the entry stub has to give storage to.
//
// A global lands here when its initialiser is a relocation -- the address of
// another global, or of a function -- because a COFF or ELF object cannot carry
// a relocation in an initialiser without a fixup, and goa emits none. The
// alternative would be a linker script; emitting the slot and letting the stub
// write it at startup keeps the object self-contained.
//
// It matters that this set is small and specific. A global the module *does*
// define must not be given storage by the stub as well: two definitions of one
// symbol, and the program reads whichever the linker ordered first.
func (m *irMod) Externals() []string {
	var out []string
	for _, g := range m.globals {
		if g.external {
			out = append(out, g.name)
		}
	}
	sort.Strings(out)
	return out
}

// archLayoutTriple returns the LLVM data layout and target triple for an
// architecture and OS. The data layout must match what the rest of the compiler
// (and goa's assembler, on x86-64) assumes, or struct-by-value returns and
// >8-byte arguments land at the wrong offset. The libLLVM build linked here
// ships x86_64, aarch64, arm, riscv64 and riscv32, so any of these triples is
// valid.
func archLayoutTriple(arch string, linux bool) (string, string) {
	vendorOS := "-pc-windows-msvc"
	if linux {
		vendorOS = "-pc-linux-gnu"
	}
	switch arch {
	case "aarch64":
		// AArch64 is LP64 and uniform: every integer/vector type aligns to its
		// size, and the n32:64 rule keeps 64-bit accesses naturally aligned.
		if linux {
			return "e-m:e-i8:8:32-i16:16:32-i64:64-i128:128-n32:64-S128", "aarch64" + vendorOS
		}
		return "e-m:w-i8:8:32-i16:16:32-i64:64-i128:128-n32:64-S128", "aarch64" + vendorOS
	case "arm", "armel":
		// ARMv7: a 32-bit pointer machine. The layout differs from AArch64 in
		// the explicit p:32:32 and the S64 stack alignment.
		//
		// The two float ABIs are separate targets, not a detail of one. "arm"
		// is armhf -- the hard-float ABI every current ARM Linux distribution
		// uses -- where the triple ends in `hf` and a `double` operation is a
		// VFP instruction. "armel" is the older soft-float ABI, where the
		// target has no FPU and every double operation becomes a *call* to
		// __adddf3 and its relatives in the C runtime (see softfloat.c).
		//
		// The distinction is not cosmetic and not detectable after the fact:
		// the two ABIs disagree about which registers hold a float argument,
		// so a binary built for one is not merely slower under the other, it
		// passes the wrong values. Naming them separately is the only way a
		// program can ask for the one it needs.
		if linux {
			if arch == "armel" {
				return "e-m:e-p:32:32-i64:64-v128:64:128-a:0:32-n32-S64", "arm" + vendorOS
			}
			return "e-m:e-p:32:32-i64:64-v128:64:128-a:0:32-n32-S64", "armv7" + vendorOS + "hf"
		}
		return "e-m:w-p:32:32-i64:64-v128:64:128-a:0:32-n32-S64", "arm" + vendorOS
	case "riscv64":
		// RV64 is LP64 with a RISC-V-specific layout string.
		if linux {
			return "e-m:e-p:64:64-i64:64-i128:128-n32:64-S128", "riscv64" + vendorOS
		}
		return "e-m:w-p:64:64-i64:64-i128:128-n32:64-S128", "riscv64" + vendorOS
	case "riscv32":
		// RV32 is ILP32: the only difference from RV64 is the 32-bit pointer
		// (p:32:32) and the native 32-bit alignment (n32, no 64-bit GPRs).
		if linux {
			return "e-m:e-p:32:32-i64:64-i128:128-n32-S128", "riscv32" + vendorOS
		}
		return "e-m:w-p:32:32-i64:64-i128:128-n32-S128", "riscv32" + vendorOS
	default: // x86_64
		if linux {
			return "e-m:e-p270:32:32-p271:32:32-p272:64:64-i64:64-i128:128-f80:128-n8:16:32:64-S128", "x86_64" + vendorOS
		}
		return "e-m:w-p270:32:32-p271:32:32-p272:64:64-i64:64-i128:128-f80:128-n8:16:32:64-S128", "x86_64" + vendorOS
	}
}

func (m *irMod) String() string {
	// The layout string describes the machine goa's own assembler targets, so a
	// struct laid out by one half of the compiler is read identically by the
	// other. On Windows it is the m:w (MS ABI) layout; on Linux the SysV one
	// (m:e), because the two differ in struct-by-value passing and aggregate
	// alignment -- getting this wrong makes a struct return or a >8-byte
	// argument land at the wrong offset.
	// The layout string and triple name the machine the module lowers to. Each
	// architecture has its own data layout, and within an architecture Windows
	// and Linux differ in the aggregate ABI (m:w vs m:e) and the vendor/OS
	// field. Getting the layout wrong makes a struct-by-value return or a
	// >8-byte argument land at the wrong offset -- the same failure mode goa's
	// own assembler would hit, so the two halves must agree.
	layout, triple := archLayoutTriple(m.arch, m.linux)

	var b strings.Builder
	b.WriteString("target datalayout = \"" + layout + "\"\n")
	b.WriteString("target triple = \"" + triple + "\"\n\n")
	// Module-level assembly has to follow the two target lines: the parser
	// reads a `module asm` as a top-level entity and refuses it before
	// `target datalayout` has been seen.
	for _, line := range m.entryAlignAsm() {
		b.WriteString(line + "\n")
	}
	if len(m.entryAlignAsm()) > 0 {
		b.WriteString("\n")
	}
	for _, t := range m.typeLines {
		b.WriteString(t + "\n")
	}
	if len(m.typeLines) > 0 {
		b.WriteString("\n")
	}
	for _, g := range m.globals {
		if g.external {
			b.WriteString("@" + g.name + " = external " + m.dso() + "global " + g.ty + "\n")
			continue
		}
		init := g.init
		if init == "" {
			init = "zeroinitializer"
		}
		b.WriteString("@" + g.name + " = " + m.dso() + "global " + g.ty + " " + init + ", align " +
			itoa(alignOfLlir(g.ty)) + "\n")
	}
	if len(m.globals) > 0 {
		b.WriteString("\n")
	}
	if s := m.windowsFltused(); s != "" {
		b.WriteString(s)
		b.WriteString("\n")
	}
	// A thread-local variable is reached through the linker-provided
	// __goc_tls_slot helper (see call.go lvalue / expression.go ident), which
	// returns the variable's per-thread address from its .tls offset. The
	// helper's body is assembled by the entry stub; this declares it so the IR
	// can call it.
	if len(m.tlsOffsets) > 0 {
		b.WriteString("declare ptr @__goc_tls_slot(i64)\n\n")
	}
	for _, p := range m.prototypeLines {
		b.WriteString(p + "\n")
	}
	if len(m.prototypeLines) > 0 {
		b.WriteString("\n")
	}
	for _, f := range m.funcBodies {
		b.WriteString(f)
		b.WriteString("\n")
	}
	// The generated syscall bodies go after the C functions. They are appended
	// as `define` (not `declare`), so they must be part of the module rather
	// than something the linker supplies -- that is the whole point on the
	// targets where gocld links the object without goa.
	for _, name := range m.sortedSyscallStubs() {
		b.WriteString(m.emitSyscallStub(name))
		b.WriteString("\n")
	}
	// A static ELF image has no GOT or PLT, so the Linux module must ask LLVM for
	// non-PIE code: a PIC Level and PIE Level of 0 make every reference to a
	// static symbol a plain PC-relative relocation (R_X86_64_PC32) instead of a
	// GOTPCRELX one. Without this, x86_64-pc-linux-gnu defaults to PIE and
	// `&"..string.."` in the codegen becomes a GOT-relative load gocld cannot
	// resolve (it builds no GOT), failing the link with "unsupported relocation
	// type 42".
	if m.linux {
		b.WriteString("!0 = !{i32 1, !\"PIC Level\", i32 0}\n")
		b.WriteString("!1 = !{i32 1, !\"PIE Level\", i32 0}\n")
		b.WriteString("!llvm.module.flags = !{!0, !1}\n")
	}
	return b.String()
}

// --- types ------------------------------------------------------------------

// llirType renders a C type as an LLVM type.
//
// Integer widths and signedness are preserved because C's usual arithmetic
// conversions depend on both. A pointer is the opaque `ptr`: goc's C subset has
// no typed pointers, and the front end inserts the casts that need. A float
// rides in an i32, matching what the native code generator does, so a value
// means the same thing on both paths.
// boolIr is the IR type of a value that is already a condition.
func boolIr() *frontend.Type { return &frontend.Type{Kind: frontend.KBool} }

func (m *irMod) llirType(t *frontend.Type) string {
	if t == nil {
		return "i32"
	}
	switch t.Kind {
	case frontend.KVoid:
		return "void"
	case frontend.KBool:
		// C's _Bool is a byte, but the IR front end uses this type for the
		// result of a comparison and for a reduced controlling expression,
		// which LLVM models as i1. A _Bool stored to memory is written through
		// an i8 slot by the store path, so nothing else depends on this.
		return "i1"
	case frontend.KBitInt:
		// Not modelled as a distinct type. goc's own generator handles these
		// and the caller keeps such functions off this path.
		return "i64"
	case frontend.KInt:
		switch t.Width {
		case 1:
			return "i8"
		case 2:
			return "i16"
		case 8:
			return "i64"
		default:
			return "i32"
		}
	case frontend.KFloat:
		return "float"
	case frontend.KDouble:
		return "double"
	case frontend.KLongDouble:
		// A long double is carried as 128 raw bits: see tfIRType in fp128.go.
		// It is not LLVM's fp128, which would have LLVM legalise every
		// operation into a libgcc-named helper this runtime does not provide.
		m.tfPairDecl()
		return tfIRType
	case frontend.KPtr, frontend.KFunc:
		return "ptr"
	case frontend.KArr:
		return "[" + itoa(lenOfTy(t)) + " x " + m.llirType(t.Elem) + "]"
	case frontend.KStruct, frontend.KUnion:
		return "%" + m.structName(t)
	}
	return "i32"
}

func isArrayTy(s string) bool { return len(s) > 0 && s[0] == '[' }

func alignOfLlir(ty string) int {
	switch {
	case len(ty) > 1 && ty[0] == 'i':
		n := 0
		for i := 1; i < len(ty); i++ {
			if ty[i] < '0' || ty[i] > '9' {
				return 8
			}
			n = n*10 + int(ty[i]-'0')
		}
		// i1/_Bool is a byte in memory; n/8 would give 0, and LLVM rejects
		// an alignment of 0.
		if a := n / 8; a >= 1 {
			return a
		}
		return 1
	case ty == "float":
		return 4
	case ty == "double":
		return 8
	case ty == "ptr":
		return 8
	}
	// An array or a struct is as aligned as its elements. Reporting 1 for
	// "[1024 x i8]" put a char buffer the C runtime hands to a Win32 API on an
	// odd address, and WriteFile wrote through it faulted: the API assumes the
	// alignment its own prototype implies, not the alignment of the element.
	// "[3 x i64]" is the Windows x64 va_list, which is only ever accessed
	// through pointers, so it keeps its own tighter answer above.
	if strings.HasPrefix(ty, "[") {
		if i := strings.Index(ty, " x "); i > 0 {
			return alignOfLlir(ty[i+3 : len(ty)-1])
		}
	}
	if strings.HasPrefix(ty, "%") {
		return 8
	}
	return 1
}

// structName defines the LLVM type for a struct or union on first use and
// returns its name. A tagged struct uses its tag so the IR stays readable; an
// anonymous one is numbered, since nothing else can name it.
func (m *irMod) structName(t *frontend.Type) string {
	if n, ok := m.structN[t]; ok {
		return n
	}
	base := "anon"
	if t.Tag != "" {
		base = sanitize(t.Tag)
	}
	// frontend.Structs can refer to one another, so reserve the name before recursing.
	name := base
	if _, taken := m.structs[name]; taken {
		m.seq++
		name = base + "." + itoa(m.seq)
	}
	m.structN[t] = name
	m.structs[name] = true

	var body string
	if t.Kind == frontend.KUnion {
		// A union is as wide as its widest member, and its first member
		// already occupies that width -- only the remainder, if any, needs an
		// explicit pad. Adding the whole width on top of the member doubled
		// it: "union U { char *p; int x; }" came out as "{ ptr, [8 x i8] }",
		// 16 bytes for what C calls 8.
		first, pad := unionLayout(t)
		ty := "i8"
		if first != nil {
			ty = m.llirType(first)
		}
		body = "{ " + ty + " }"
		if pad > 0 {
			body = "{ " + ty + ", [" + itoa(pad) + " x i8] }"
		}
	} else {
		// The fields are the members that own storage: an ordinary member,
		// or an anonymous struct/union member's shell. A sub-member the
		// shell promoted out of itself (AnonBase != nil) is NOT a field --
		// it lives inside the shell at an offset of its own, and C reaches
		// it through the shell. Listing it as well double-counted the
		// storage: %S1 came out 40 bytes for a 24-byte struct, with the
		// promoted fields at LLVM's idea of their offsets rather than C's.
		// Every access in this back end is a byte getelementptr off the
		// object, so the mislayout stayed invisible -- until a *static*
		// initialiser had to name a field, and then LLVM rejected the
		// shell's scalar ("%anon 2") before the program could run at all.
		parts := make([]string, 0, len(t.Members))
		for _, mem := range t.Members {
			if mem.AnonBase != nil {
				continue
			}
			parts = append(parts, m.llirType(mem.Type))
		}
		if len(parts) == 0 {
			body = "{ i8 }"
		} else {
			body = "{ " + joinStrings(parts, ", ") + " }"
		}
	}
	// No trailing pad member. LLVM lays a struct out from its members per the
	// target datalayout -- inserting the same internal and trailing padding the
	// C ABI does -- so "{ ptr, i32 }" is already 16 bytes with the right
	// alignment. Adding a pad member of our own both inflates the size (a
	// pad rounded up to the struct's alignment made it 24) and puts a member
	// the C source never mentions in the type, which every brace initialiser
	// then fails to fill ("initializer with struct type has wrong # elements").
	m.typeLines = append(m.typeLines, "%"+name+" = type "+body)
	return name
}

// isAnonShell reports whether mem is an anonymous struct/union member: it
// owns storage in its parent but promotes its named sub-members into the
// parent's member list, so the parent reaches them by name while the member
// itself stays unnamed. promoteAnonMembers leaves exactly these members
// behind, and posMembers skips them for the same reason.
func isAnonShell(mem *frontend.Member) bool {
	return mem != nil && mem.Name == "" &&
		(mem.Type != nil && (mem.Type.Kind == frontend.KStruct || mem.Type.Kind == frontend.KUnion))
}

// unionOwner picks the member a union's LLVM type is spelled with. An
// anonymous struct/union member is a poor choice: its promoted sub-members
// are initialised on their own (".whole = ..." or ".lo = ..."), and only the
// first field of the LLVM type can carry an initialiser, so the value would
// land in a member the C source never names. The first ordinary member is
// the one C itself initialises when a union is written without a designator.
func unionOwner(t *frontend.Type) *frontend.Member {
	for _, mem := range t.Members {
		if mem.AnonBase != nil {
			continue // promoted sub-member: lives inside its shell
		}
		if !isAnonShell(mem) {
			return mem
		}
	}
	for _, mem := range t.Members {
		if mem.AnonBase == nil {
			return mem
		}
	}
	return nil
}

// unionLayout describes how a union is laid out in LLVM: the owner member
// keeps its own type (so member access can use it directly) and pad is the
// number of extra bytes needed to reach the union's C size. The type emitter
// and the constant initialiser both go through this so they cannot disagree
// about how many members a union has.
func unionLayout(t *frontend.Type) (first *frontend.Type, pad int) {
	if len(t.Members) == 0 {
		return nil, 0
	}
	owner := unionOwner(t)
	if owner == nil {
		return nil, 0
	}
	first = owner.Type
	wide := frontend.Sizeof(first)
	for _, mem := range t.Members {
		if mem.AnonBase != nil {
			continue // promoted sub-member: already inside the shell's size
		}
		if s := frontend.Sizeof(mem.Type); s > wide {
			wide = s
		}
	}
	size := t.Size
	if size <= 0 {
		size = wide
	}
	if size > wide {
		wide = size
	}
	if pad = wide - frontend.Sizeof(first); pad < 0 {
		pad = 0
	}
	return first, pad
}

// hasGlobal reports whether name is a module-level variable.
func (m *irMod) hasGlobal(name string) bool {
	for _, g := range m.globals {
		if g.name == name {
			return true
		}
	}
	return false
}

// declareFunc records a `declare` for a function this module calls but does not
// define -- the C runtime and the entry stub come from goa's own assembler, so
// their bodies live in the other half of the link. The signature here is
// deliberately loose (everything as i32) because the call sites that matter go
// through the runtime's real prototypes; what matters for correctness is that
// the symbol is declared exactly once and with a fixed type.
func (m *irMod) declareFunc(name, ret string, params []string) {
	if m.protos[name] {
		return
	}
	m.protos[name] = true
	ps := ""
	if len(params) > 0 {
		ps = strings.Join(params, ", ")
	}
	m.prototypeLines = append(m.prototypeLines,
		"declare "+m.dso()+ret+" @"+name+"("+ps+")")
}

// --- small helpers ----------------------------------------------------------

func itoa(n int) string { return strconv.Itoa(n) }

func joinStrings(a []string, sep string) string {
	var b strings.Builder
	for i, s := range a {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}

// sanitize turns a C identifier or tag into an LLVM identifier. Names that are
// not already valid -- a leading digit, say -- are prefixed rather than
// rejected, because a mangled name is better than a failed compile.
func sanitize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '.' || c == '$' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if ok {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// llvmMisrecognizedMaxMin lists the C library names LLVM's TargetLibraryInfo
// claims for its own libcall expansion, and which therefore must not appear
// verbatim as an LLVM symbol.
//
// The mechanism is name-based and it overrides the body: InstCombine sees
// `call double @fmaximum_num(...)`, decides from the name alone that this is a
// libfunc, and replaces the call with the libcall's semantics -- which for
// fmaximum_num is fmax's, not its own. C23 splits the maxima family in two:
// fmax SKIPS a NaN and returns the other operand, fmaximum_num PROPAGATES it and
// raises a domain error. Collapsing the two turns fmaximum_num(NaN, 1) into 1.0,
// and the miscompile is silent -- no diagnostic, no trap, just a wrong number in
// the program's output.
//
// The substitution is worse than it looks, because it does not need the body to
// be the libfunc's. A definition of `@fmaximum_num` whose body is a bare
// `ret double %p0` -- which returns its first argument and so must answer NaN --
// still gets replaced, and answers 1.0. Nothing about the IR says "max"; only
// the spelling of the symbol does.
//
// Measured on LLVM 22.1.8 (the libLLVM gocl links is 23.x, which behaves the
// same way), and reproducible outside gocl: clang -O2 on
// `fmaximum_num(0.0/0.0, 1.0)` prints 1.000000, because clang lowers the call to
// @llvm.maximumnum.f64 and folds that with the same wrong rule. gcc -O2 gets it
// right. The upstream fix is in flight in the min/max SimplifyDemandedFPClass
// work; until it lands, the portable answer is to keep the name LLVM keys on out
// of the IR entirely.
//
// Only the `_num` pair is renamed. fmax and fmin are *supposed* to skip a NaN, so
// the expansion LLVM performs for them is the semantics the standard asks for,
// and a program that calls them through a function pointer still wants a real
// symbol of that name. The two families also behave identically for every input
// that is not a NaN, so nothing else in the library can tell them apart.
var llvmMisrecognizedMaxMin = map[string]string{
	"fmaximum_num": "goclib_fmaximum_num",
	"fminimum_num": "goclib_fminimum_num",
}

// goclibLibmRenames lists the C math-library names whose bodies goclib ships
// itself, and which must therefore not appear verbatim as an LLVM symbol.
//
// Unlike fmaximum_num (see llvmMisrecognizedMaxMin), the bug here is not a wrong
// semantic expansion of one function. It is that gocl's default<O2> pipeline
// runs SimplifyLibCalls, which keys on the callee name alone and decides that
// `call double @pow(...)` is the C library pow. It then folds the call with the
// *hardware-accurate* math LLVM assumes the C library provides. That value
// differs from goclib's self-contained software implementation by up to a few
// ULPs, and -- worse -- mishandles domain edges: pow(-2.0, 0.5) folds to a
// finite number instead of NaN, where goclib's software pow returns NaN. The
// native goc backend does no such folding and matches the golden exactly, so the
// golden values are by definition goclib's software output.
//
// Renaming the symbol out of LLVM's vocabulary is what stops the recognition:
// SimplifyLibCalls never inspects the body, only the name. goclim's own rename
// of fmaximum_num already uses this mechanism; here it is widened to the whole
// math family goclib provides. Every such function is renamed to a goclib_
// prefix, and because gocl compiles a whole program into one LLVM module the
// definition, every call site, every declaration and every address-of reference
// all go through irFuncSym and agree on the spelling.
//
// The names below are exactly the libm functions goclib defines (double- and
// long-double precision; there are no float-suffixed variants in goclib).
var goclibLibmRenames = map[string]string{
	// double precision
	"fabs": "goclib_fabs", "floor": "goclib_floor", "ceil": "goclib_ceil",
	"fmod": "goclib_fmod", "sqrt": "goclib_sqrt", "trunc": "goclib_trunc",
	"round": "goclib_round", "fmin": "goclib_fmin", "fmax": "goclib_fmax",
	"exp": "goclib_exp", "exp2": "goclib_exp2", "expm1": "goclib_expm1",
	"log": "goclib_log", "log2": "goclib_log2", "log10": "goclib_log10",
	"log1p": "goclib_log1p", "pow": "goclib_pow", "sin": "goclib_sin",
	"cos": "goclib_cos", "tan": "goclib_tan", "atan": "goclib_atan",
	"asin": "goclib_asin", "acos": "goclib_acos", "atan2": "goclib_atan2",
	"sinh": "goclib_sinh", "cosh": "goclib_cosh", "tanh": "goclib_tanh",
	"hypot": "goclib_hypot", "cbrt": "goclib_cbrt", "copysign": "goclib_copysign",
	"fdim": "goclib_fdim", "rint": "goclib_rint", "nearbyint": "goclib_nearbyint",
	"remainder": "goclib_remainder", "fma": "goclib_fma", "scalbn": "goclib_scalbn",
	// special functions used by mathlib2: erf/erfc/tgamma/lgamma/ilogb/logb/nan
	"erf": "goclib_erf", "erfc": "goclib_erfc", "tgamma": "goclib_tgamma",
	"lgamma": "goclib_lgamma", "ilogb": "goclib_ilogb", "logb": "goclib_logb",
	"nan": "goclib_nan",
	// long double precision
	"fabsl": "goclib_fabsl", "floorl": "goclib_floorl", "ceill": "goclib_ceill",
	"truncl": "goclib_truncl", "roundl": "goclib_roundl", "rintl": "goclib_rintl",
	"nearbyintl": "goclib_nearbyintl", "fmodl": "goclib_fmodl",
	"scalbnl": "goclib_scalbnl", "sqrtl": "goclib_sqrtl", "cbrtl": "goclib_cbrtl",
	"hypotl": "goclib_hypotl", "fmaxl": "goclib_fmaxl", "fminl": "goclib_fminl",
	"copysignl": "goclib_copysignl", "fdiml": "goclib_fdiml", "expl": "goclib_expl",
	"expm1l": "goclib_expm1l", "exp2l": "goclib_exp2l", "logl": "goclib_logl",
	"log1pl": "goclib_log1pl", "log2l": "goclib_log2l", "log10l": "goclib_log10l",
	"powl": "goclib_powl", "sinl": "goclib_sinl", "cosl": "goclib_cosl",
	"tanl": "goclib_tanl", "asinl": "goclib_asinl", "acosl": "goclib_acosl",
	"atanl": "goclib_atanl", "atan2l": "goclib_atan2l", "sinhl": "goclib_sinhl",
	"coshl": "goclib_coshl", "tanhl": "goclib_tanhl",
}

// irFuncSym returns the LLVM symbol a C function name is emitted as. It is the
// C name unchanged for almost everything, and a private spelling for the libfuncs
// LLVM would otherwise rewrite -- see the tables above.
//
// Renaming is safe here because gocl compiles a whole program into one LLVM
// module: goclib's definitions and the user's calls are in the same module, so
// both sides go through this function and agree. Nothing links against these
// symbols from another object, and the image gocld builds keeps whatever spelling
// the IR used.
func irFuncSym(cName string) string {
	if alt, ok := llvmMisrecognizedMaxMin[cName]; ok {
		return alt
	}
	if alt, ok := goclibLibmRenames[cName]; ok {
		return alt
	}
	return cName
}

// lenOfTy returns the element count of an array type, or 1.
func lenOfTy(t *frontend.Type) int {
	if t.Len <= 0 {
		return 1
	}
	return t.Len
}

// --- extern tracking --------------------------------------------------------

// noteExtern records a function this module calls but does not define. The C
// runtime and the entry stub come from goa's own assembler, so their bodies
// arrive through the other half of the link; all that is needed here is a
// declaration with the right shape.
func (m *irMod) noteExtern(name string, ret *frontend.Type, params []*frontend.Type) {
	// The return type is recorded on every call, not only the ones that need a
	// `declare` line: callExpr reads it back through externRet to type the
	// `call` instruction, and that has to be void for a void function however
	// this module came to define it. Missing this makes every void callee
	// resolve to int, so the call reads "call i32 @f" against the "void @f"
	// the module defines -- which LLVM reports as a redefinition.
	m.setExternRet(name, ret)
	if m.defined[name] {
		return
	}
	// A raw Linux syscall is not something the C library defines and not
	// something this module is willing to leave undefined: on the targets
	// where this back end also owns the entry point there is no assembler in
	// the link to synthesise the `mov rax,N; syscall` stub that goa used to
	// provide, so the body is generated here (see emitSyscallStub) instead.
	//
	// The number has to be the target's, not x86-64's: `write` is 1 on x86-64
	// and 64 on AArch64, and a stub that loads the wrong one runs to completion
	// producing nothing. A name this architecture has no number for is left as
	// a plain declare, so the link reports the missing symbol by name instead
	// of the program trapping inside a wrong syscall.
	//
	// The signature is taken from the first call site, which is the C
	// runtime's own prototype for that syscall -- goclib calls write() through
	// a real declaration, and every call site agrees on it. Recording it rather
	// than deriving one from the syscall number is what keeps the generated
	// body type-compatible with the call that reaches it.
	if m.linux && m.nativeLink && goa.IsLinuxSyscall(name) {
		if _, ok := archSyscallNumber(m.arch, name); ok {
			if !m.syscallStubs[name] {
				m.syscallStubs[name] = true
				m.stubRet[name] = ret
				m.stubParams[name] = params
			}
			return
		}
	}
	key := name + "/" + itoa(len(params))
	if m.extSig[key] {
		return
	}
	m.extSig[key] = true
	var ps []string
	for _, p := range params {
		ps = append(ps, m.llirType(p))
	}
	r := "i32"
	if ret != nil {
		r = m.llirType(ret)
	}
	// irFuncSym: a declaration has to spell the symbol the call site names. For
	// the renamed libfuncs that is not the C name, and a `declare` under the C
	// name would leave the module with a declaration nobody defines and a call
	// to a symbol nobody declared.
	m.prototypeLines = append(m.prototypeLines,
		"declare "+m.dso()+r+" @"+irFuncSym(name)+"("+strings.Join(ps, ", ")+")")
}

// noteExternGlobal declares a global this module reads or writes but does not
// define. The definition comes from the other generator; without the
// declaration a reference would be an undefined symbol at link time.
func (m *irMod) noteExternGlobal(name, ty string) {
	m.globals = append(m.globals, irGlobal{name: name, ty: ty, external: true})
}

// noteIntrinsic declares one of LLVM's own intrinsics. They are not library
// functions and have no body; the exact signature matters, because a mismatch is
// accepted at parse time and rejected at codegen.
func (m *irMod) noteIntrinsic(name, ret string, params []string) {
	if m.protos[name] {
		return
	}
	m.protos[name] = true
	if ret == "" {
		ret = "void"
	}
	m.prototypeLines = append(m.prototypeLines,
		"declare "+m.dso()+ret+" @"+name+"("+strings.Join(params, ", ")+")")
}

// externRet reports the return type a previous noteExtern recorded, defaulting
// to int so a call whose prototype was never seen still type-checks.
func (m *irMod) externRet(name string) *frontend.Type {
	if t, ok := m.extRet[name]; ok {
		return t
	}
	return frontend.IntType()
}

// setExternRet records a declared return type.
func (m *irMod) setExternRet(name string, t *frontend.Type) {
	if m.extRet == nil {
		m.extRet = map[string]*frontend.Type{}
	}
	m.extRet[name] = t
}

// addString interns a string constant and returns its global name.
func (m *irMod) addString(b []byte) string {
	if m.strings == nil {
		m.strings = map[string]string{}
	}
	key := string(b)
	if n, ok := m.strings[key]; ok {
		return n
	}
	name := ".str." + itoa(len(m.strings))
	m.strings[key] = name
	ty := "[" + itoa(len(b)) + " x i8]"
	m.globals = append(m.globals, irGlobal{name: name, ty: ty, init: cString(b)})
	return name
}

// cStringN renders n bytes as an LLVM [n x i8] constant, NUL-padding or
// truncating the input to fit. An initialiser for `char s[4] = "abc"` has to
// produce exactly four elements, and one for `char *p = "abc"` exactly the
// string's own length plus its terminator.
func cStringN(b []byte, n int) string {
	if n < 0 {
		n = 0
	}
	if len(b) > n {
		b = b[:n]
	}
	padded := make([]byte, n)
	copy(padded, b)
	return cString(padded)
}

// cString renders a byte sequence as an LLVM [N x i8] constant.
func cString(b []byte) string {
	var sb strings.Builder
	sb.WriteString("c\"")
	for _, c := range b {
		switch {
		case c == '"':
			sb.WriteString("\\22")
		case c == '\\':
			sb.WriteString("\\5C")
		case c >= 0x20 && c < 0x7f:
			sb.WriteByte(c)
		default:
			sb.WriteString("\\")
			const hex = "0123456789ABCDEF"
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0xf])
		}
	}
	sb.WriteString("\"")
	return sb.String()
}

// convertTo converts a value to a named LLVM type. It is the counterpart of
// convert for the places where the target is already rendered as text -- a
// function's return type, for instance.
func (e *irEmitter) convertTo(op string, from *frontend.Type, toIR string) string {
	// An array operand has already decayed to a pointer to its first element by
	// the time it is a value. Converting from the array type itself renders
	// "[256 x i8]", which is not what the value is: the operand is the slot's
	// address. Reading it as an array produced "bitcast [256 x i8] %p to ptr"
	// against a pointer, which LLVM rejects.
	if from != nil && from.Kind == frontend.KArr {
		from = frontend.PtrType(from.Elem)
	}
	// Same interception as convert's: a long double's i128 reads as an integer
	// to every branch below, so it has to be taken out before the integer
	// width logic gets it and truncates the pattern instead of converting the
	// value.
	if isLongDouble(from) || toIR == tfIRType {
		if isLongDouble(from) && toIR == tfIRType {
			return op
		}
		if isLongDouble(from) {
			return e.tfFromLD(op, toIR)
		}
		return e.tfToLD(op, from)
	}
	f := e.ty(from)
	if f == toIR {
		return op
	}
	if isFloatTy(from) {
		v := e.newTmp()
		if f == "double" && toIR == "float" {
			e.line("%s = fptrunc double %s to float", v, op)
			return v
		}
		if f == "float" && toIR == "double" {
			e.line("%s = fpext float %s to double", v, op)
			return v
		}
	}
	// Integer <-> floating point is a value conversion, not a reinterpretation,
	// so it has to happen before the integer-width branch below -- an i1 and a
	// double are both "different shapes", and left to the store-and-load
	// fallback the 1-byte i1 was stored into an 8-byte slot and read back as a
	// double, yielding whatever the adjacent bytes happened to hold. That is
	// how "return (x != x);" in a function returning double came to answer
	// 5368713217 instead of 0.0 or 1.0.
	//
	// An i1 source is signed: an LLVM i1 carries the values 0 and -1, so sitofp
	// turns true into 1.0 and uitofp would turn it into 1.0 as well by luck of
	// the zero-extension, but false into 0.0 either way. sitofp states the
	// intent.
	// An i1 source is neither signed nor unsigned: LLVM's i1 holds 0 and -1,
	// so sitofp yields -1.0 for true and uitofp is not available on i1 at all.
	// Both are wrong here, because C means 1.0. The three-step widen is what
	// clang does and what keeps the value honest: zext to i64 turns the -1 bit
	// pattern into 1, and uitofp then gives 1.0. (sitofp straight off the i1
	// is not merely imprecise, it is actively wrong -- LLVM legalises it into
	// "cmpunordsd + andpd <sign mask>", which hands back -1.0.)
	if isIntIr(f) && isFloatIr(toIR) {
		src := op
		if f == "i1" {
			w := e.newTmp()
			e.line("%s = zext i1 %s to i64", w, op)
			src, f = w, "i64"
		}
		v := e.newTmp()
		opc := "uitofp"
		if from != nil && from.Signed {
			opc = "sitofp"
		}
		e.line("%s = %s %s %s to %s", v, opc, f, src, toIR)
		return v
	}
	if isFloatIr(f) && isIntIr(toIR) {
		v := e.newTmp()
		e.line("%s = %s %s %s to %s", v, fptoiOp(toIR), f, op, toIR)
		return v
	}
	// Integers: same width and different signedness needs no instruction, since
	// both live in the same register and C only reinterprets the bits.
	if len(f) > 1 && f[0] == 'i' && len(toIR) > 1 && toIR[0] == 'i' {
		fw, tw := irIntWidth(f), irIntWidth(toIR)
		switch {
		case fw < tw:
			v := e.newTmp()
			// An i1 source is zero-extended whatever C's signedness says.
			// A _Bool is Signed in the front end, but LLVM's i1 does not
			// carry C's 0 and 1 -- it carries 0 and -1, the all-ones
			// pattern -- so "sext i1 true to i32" is -1 and every cast of
			// a true _Bool came out negative: (int)b was -1, (long)b was
			// -1, (unsigned)b was 4294967295, and print(b) printed -1.
			// zext turns the pattern into the 1 C means.
			if from != nil && from.Signed && f != "i1" {
				e.line("%s = sext %s %s to %s", v, f, op, toIR)
			} else {
				e.line("%s = zext %s %s to %s", v, f, op, toIR)
			}
			return v
		case fw > tw:
			// Narrowing to _Bool is not a truncation. C 6.3.1.2 says the
			// result is 0 if the value compares equal to zero and 1
			// otherwise, while "trunc i32 100 to i1" keeps the low bit and
			// answers false for 100, 4 and 256 -- so "b = 100" stored 0 and
			// "bb = 256" was false. Every other narrow target really is a
			// modulo-2^n truncation, so only i1 takes the comparison.
			if toIR == "i1" {
				v := e.newTmp()
				e.line("%s = icmp ne %s %s, 0", v, f, op)
				return v
			}
			v := e.newTmp()
			e.line("%s = trunc %s %s to %s", v, f, op, toIR)
			return v
		}
		return op
	}
	// A pointer and an integer are not interchangeable by truncation: LLVM has
	// dedicated opcodes for that, and using trunc between them is rejected with
	// "invalid cast opcode". A pointer reaching an integer slot is C's pointer
	// arithmetic on this target (an int is wide enough to hold it).
	if f == "ptr" && len(toIR) > 1 && toIR[0] == 'i' {
		v := e.newTmp()
		e.line("%s = ptrtoint ptr %s to %s", v, op, toIR)
		return v
	}
	if len(f) > 1 && f[0] == 'i' && toIR == "ptr" {
		v := e.newTmp()
		e.line("%s = inttoptr %s %s to ptr", v, f, op)
		return v
	}
	if f == "ptr" || toIR == "ptr" {
		v := e.newTmp()
		if f == "ptr" {
			e.line("%s = bitcast ptr %s to %s", v, op, toIR)
		} else {
			e.line("%s = bitcast %s %s to ptr", v, f, op)
		}
		return v
	}
	// Anything else moves through memory: the two types have different shapes,
	// so a store and a load of the right width is the general answer. The
	// staging slot has to hold the whole source object, which for an aggregate
	// is more than the eight bytes a scalar needs.
	n := 8
	if from != nil {
		if sz := frontend.Sizeof(from); sz > n {
			n = sz
		}
	}
	slot := e.scratchSlot(n)
	e.line("store %s %s, ptr %s, align %d", f, op, slot, alignOfIr(f))
	v := e.newTmp()
	e.line("%s = load %s, ptr %s, align %d", v, toIR, slot, alignOfIr(toIR))
	return v
}

// isIntIr reports whether an IR type name is an integer ("i1", "i32", "i64").
func isIntIr(s string) bool {
	return len(s) > 1 && s[0] == 'i' && s[1] >= '0' && s[1] <= '9'
}

// isFloatIr reports whether an IR type name is a floating-point type.
func isFloatIr(s string) bool {
	return s == "float" || s == "double"
}

// fptoiOp picks the signed or unsigned float-to-integer opcode. An i1 target
// has no width to speak of and only ever holds 0 or -1, so signed is the only
// spelling that can work.
func fptoiOp(toIR string) string {
	if toIR == "i1" {
		return "fptosi"
	}
	if toIR == "i32" {
		return "fptosi"
	}
	return "fptoui"
}

// irIntWidth returns the bit width of an "iN" type, 64 when it cannot be read.
func irIntWidth(ty string) int {
	if len(ty) < 2 || ty[0] != 'i' {
		return 64
	}
	n := 0
	for i := 1; i < len(ty); i++ {
		if ty[i] < '0' || ty[i] > '9' {
			return 64
		}
		n = n*10 + int(ty[i]-'0')
	}
	return n
}

// doGoto branches to a C label. A backward target is already known; a forward
// one is not, so the branch is written through a placeholder and patched once
// the label has been seen.
func (e *irEmitter) doGoto(n *frontend.GotoStmt) {
	if l, ok := e.userLabels[n.Label]; ok {
		e.term("br label %%%s", l)
		return
	}
	// Emit through a named placeholder block so the text can be rewritten in
	// place when the target appears; a real target later replaces the whole
	// line.
	ph := e.newLabel()
	off := e.bodyLen()
	e.term("br label %%%s", ph)
	e.forwards = append(e.forwards, forwardGoto{
		label: n.Label, at: off, placeholder: ph,
	})
}

// blockLen reports the current length of the body buffer, which is where a
// forward goto's branch text will need patching.
func (e *irEmitter) bodyLen() int { return e.body.Len() }

// globalSym returns the assembler-level symbol for a C global, or "" if the
// module does not reference it. The two generators have to name a global
// identically or they end up referring to different storage: goa's assembler
// prefixes every global's label with "G_" so it cannot collide with a function
// of the same name.
func (m *irMod) globalSym(cName string) string {
	sym := "G_" + cName
	for _, g := range m.globals {
		if g.name == sym {
			return sym
		}
	}
	return ""
}

// tlsOffset reports the .tls-section offset of a thread-local global, or false
// if cName is not one. The offset is exactly the one ComputeTLSLayout assigned
// and the linker's .tls image used, so the IR's access and the section's storage
// cannot drift apart.
func (m *irMod) tlsOffset(cName string) (int64, bool) {
	off, ok := m.tlsOffsets[cName]
	return off, ok
}

// --- generated syscall stubs -------------------------------------------------

// sortedSyscallStubs lists the syscalls this module defines, sorted so the same
// program always lowers to the same bytes of IR (and so a diff of two dumps is
// readable).
// windowsFltused defines the one symbol a Windows module that touches floating
// point is expected to provide, or "" where the target does not ask for it.
//
// LLVM names `_fltused` when it lowers a COFF module that uses the SSE
// registers: the reference is how MSVC's linker decides whether to pull the
// floating-point support out of the CRT, and it is emitted whether or not
// anything is going to satisfy it. MinGW answers it from libmingwex. A gocl
// program links nothing but the object LLVM just wrote, so on Windows every
// program that formats a double died at
//
//	gocl: linking the LLVM object: coff: undefined symbol(s): _fltused
//
// while the same source ran fine on Linux -- and, because the native back end
// never names the symbol, fine under `goc` too.
//
// It is a global in the IR rather than a C variable in goclib because the name
// has to reach the COFF symbol table exactly as spelled: goc prefixes a C
// global's symbol with "G_", so `int _fltused;` would define G__fltused and
// leave the reference unresolved. Four bytes, and only off Linux.
func (m *irMod) windowsFltused() string {
	if m.linux {
		return ""
	}
	return "@_fltused = " + m.dso() + "global i32 1, align 4"
}

// entryAlignAsm is the module-level assembly that becomes the ELF entry point
// in place of _start, or nil when the entry point is not ours to wrap.
//
// It exists because of an eight-byte disagreement about the stack. The SysV
// ABI has the CALLER push the return address, so a function entered by `call`
// sees rsp congruent to 8 modulo 16 -- and LLVM writes its prologues for
// exactly that: the C `_start` opens with `pushq %rax` to bring rsp back onto
// a 16-byte boundary. The kernel pushes nothing. It enters with rsp already
// 16-byte aligned, so that same `pushq` leaves it at 8 modulo 16, and every
// call `_start` goes on to make hands its callee a stack that is off by eight.
//
// Nothing notices until a callee spills an SSE register: `movaps` faults on an
// unaligned address, so a program whose printf formats a double -- on x86-64
// va_arg reads the float save area, which the variadic prologue fills with
// movaps -- dies with SIGSEGV before printing anything. Integer-only programs
// ran for months because no instruction on that path cares, which is also why
// no amount of integer testing would have found this.
//
// Realigning before the call is the whole fix: `_start` then sees exactly the
// stack shape LLVM compiled it for. `and` rather than a fixed subtraction so
// the stub is right whether or not the kernel's alignment promise holds.
//
// It is x86-64-only on purpose. The eight bytes come from `call` pushing a
// return address; AArch64, ARM and RISC-V enter a function with the stack
// pointer the caller left, so for them the kernel's entry already is the shape
// the prologue expects and there is nothing to correct.
func (m *irMod) entryAlignAsm() []string {
	if !m.linux || !m.nativeLink || m.arch != "x86_64" {
		return nil
	}
	return []string{
		`module asm ".text"`,
		`module asm ".globl __goc_entry"`,
		`module asm ".p2align 4, 0x90"`,
		`module asm "__goc_entry:"`,
		`module asm "  andq $-16, %rsp"`,
		`module asm "  callq _start"`,
		// _start ends in __goclib_exit and does not return. This is the
		// landing for the case it ever did, and it exits through the C
		// library rather than through a hard-coded syscall number so the
		// trap number stays in one place (syscalls.go).
		`module asm "  xorl %edi, %edi"`,
		`module asm "  callq __goclib_exit"`,
	}
}

func (m *irMod) sortedSyscallStubs() []string {
	out := make([]string, 0, len(m.syscallStubs))
	for name := range m.syscallStubs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// emitSyscallStub writes one raw Linux syscall as a real function body.
//
// goclib reaches the kernel through plain C calls -- `write(fd, buf, n)`,
// `_exit(code)` -- and on x86-64 those were satisfied by goa emitting a
// `mov rax,N; syscall` stub. For the targets whose entry point is generated
// here there is no assembler in the link, so the stub is generated here
// instead, as inline assembly in the module LLVM compiles.
//
// Two things make it correct rather than merely plausible:
//
//   - The signature is the C library's own prototype, recorded from the call
//     site (see noteExtern), so this is an ordinary call-compatible function
//     rather than a variadic trampoline. LLVM checks a call against a defined
//     function's real operand list, so a guessed `i64(i64,i64,i64,i64,i64,i64)`
//     would not match the call the library makes.
//
//   - The arguments are named as inline-assembly inputs but never moved. On
//     every Unix Linux ABI the C calling convention already places the first
//     six integer/pointer arguments in exactly the registers the trap reads
//     them from, so the body only has to load the number and trap. Declaring
//     the inputs is still necessary: an asm block that does not name an
//     operand is free to have the register allocator reuse that register.
//
// On a 32-bit target (arm, riscv32) one extra step is required: the front
// end types long as 64-bit regardless of target, so a syscall prototype
// carries i64 arguments even here, and an i64 value occupies a register
// pair on a 32-bit machine -- it cannot sit in the single register an asm
// constraint names. Left alone, LLVM silently honours the constraint for
// the low half only and the register the syscall actually reads gets a
// value that was never intended (a `write` whose buffer came out as NULL
// and a link that still succeeded -- the fault appears only at run time,
// inside the trap). The stub therefore truncates every i64 argument to i32
// first; the low half is exactly the value the kernel reads, and the asm
// registers are all 32-bit singles again.
//
// The result comes back in the same register the first argument went in, which
// is the `=r,0` read-write idiom rather than a shared-register pair, so the
// first input is bound with a matching constraint instead of naming x0 twice.
func (m *irMod) emitSyscallStub(name string) string {
	num, ok := archSyscallNumber(m.arch, name)
	if !ok {
		num = 0
	}
	ret := m.stubRet[name]
	if ret == nil {
		ret = frontend.IntType()
	}
	params := m.stubParams[name]
	retTy := m.llirType(ret)
	// 32-bit targets: i64 parameters cannot bind to a single asm register
	// (see the comment above), so they are truncated to i32 before the call.
	is32 := m.arch == "arm" || m.arch == "armel" || m.arch == "riscv32"

	var ps, ins []string
	var pre []string
	for i, p := range params {
		if p == nil {
			p = frontend.IntType()
		}
		pty := m.llirType(p)
		// A function parameter is written "<type> %name". The "%0: <type>"
		// form belongs to instructions and allocas, and using it here made
		// LLVM read "%0" as a (nonexistent) type and report "invalid type for
		// function argument" against the parameter.
		ps = append(ps, pty+" %"+itoa(i))
		op := pty + " %" + itoa(i)
		if is32 && pty == "i64" {
			pre = append(pre, fmt.Sprintf("  %%a%d = trunc i64 %%%d to i32", i, i))
			op = "i32 %a" + itoa(i)
		}
		// The operand type is spelled out, not left to be inferred. LLVM
		// rejects an inline-asm call whose operand list has a bare "%0" with
		// "invalid type for function argument": there is no declaration to
		// infer from when the constraint pins the operand to a register, so
		// the type has to be there.
		ins = append(ins, op)
	}
	tmpl, cons, numOperand := syscallAsm(m.arch, num, len(params))
	// x86-64 has to pass the syscall number in as an immediate operand rather
	// than baking it into the template: LLVM inline asm reads a literal "$N" in
	// the template as a reference to operand N, so a hardcoded "$60" is an
	// operand 60 that does not exist ("Invalid $ operand number"). The number
	// therefore arrives as the first input and the template names it "$1" --
	// operand 0 is the output, so the first input is operand 1.
	if numOperand != "" {
		ins = append([]string{numOperand}, ins...)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "define %s%s @%s(%s) {\n", m.dso(), retTy, name, strings.Join(ps, ", "))
	// The truncation instructions go before the asm call: the kernel reads the
	// 32-bit low half, which is exactly what the truncated value is.
	for _, p := range pre {
		b.WriteString(p)
		b.WriteString("\n")
	}
	// The template is embedded as-is: its \0A is the IR escape for the newline
	// that separates the two assembler statements, and running it through %q
	// would turn that into a literal backslash and hand the assembler the four
	// characters "\0A" instead of a line break.
	asmTy := "i64"
	if is32 {
		asmTy = "i32"
	}
	fmt.Fprintf(&b, "  %%r = call %s asm sideeffect \"%s\", \"%s\"(%s)\n",
		asmTy, tmpl, cons, strings.Join(ins, ", "))
	// A syscall reports -errno as a negative long. Narrowing to the
	// prototype's return type keeps the value a libc caller expects: the low
	// half of a sign-extended 64-bit -errno is the same negative int, and a
	// successful count is positive in both.
	switch {
	case retTy == "void":
		// A void syscall (exit_group, exit, _exit) is the common one on the
		// termination path: the trap does not come back, and the wrapper has
		// nothing to hand its own caller. The asm still yields a value, which
		// is simply not returned.
		b.WriteString("  ret void\n")
	case retTy == "i64":
		if is32 {
			// The result is a 32-bit value in r0; sign-extend back to the
			// prototype's 64-bit long (ssize_t semantics: -errno stays
			// negative in 64 bits, a count stays positive).
			b.WriteString("  %r64 = sext i32 %r to i64\n  ret i64 %r64\n")
		} else {
			b.WriteString("  ret i64 %r\n")
		}
	case retTy == "ptr":
		if is32 {
			// A 32-bit pointer result (mmap and friends): zero-extend the
			// low half to 64 bits before inttoptr.
			b.WriteString("  %r64 = zext i32 %r to i64\n  %p = inttoptr i64 %r64 to ptr\n  ret ptr %p\n")
		} else {
			b.WriteString("  %p = inttoptr i64 %r to ptr\n  ret ptr %p\n")
		}
	case strings.HasPrefix(retTy, "i"):
		fmt.Fprintf(&b, "  %%n = trunc %s %%r to %s\n  ret %s %%n\n", asmTy, retTy, retTy)
	default:
		fmt.Fprintf(&b, "  ret %s %%r\n", retTy)
	}
	b.WriteString("}\n")
	return b.String()
}

// syscallAsm returns the inline-assembly template, the constraint string and
// the immediate operand (empty when the number is baked into the template) for
// one syscall on the given target.
//
// The template loads the syscall number into the register the kernel reads it
// from and executes the trap. The constraints name the argument registers (so
// the allocator cannot move an argument out from under the trap) and declare
// the scratch registers the kernel destroys -- a stub that omitted the clobbers
// would let the caller read whatever the allocator happened to leave there.
func syscallAsm(arch string, num int64, nargs int) (tmpl, cons, numOperand string) {
	// asmNL is the IR escape for the newline that separates the two assembler
	// statements in an inline-asm template. It has to reach LLVM as the three
	// characters \ 0 A: in a double-quoted Go string "\0A" would be a NUL
	// octal escape followed by 'A', and the assembler would receive that
	// instead of a line break.
	const asmNL = `\0A`
	n := strconv.FormatInt(num, 10)
	// Per architecture: where the arguments arrive, where the result comes
	// back, which registers the kernel destroys, and the instruction pair.
	argRegs, outReg, scratch := []string(nil), "", []string(nil)
	// arg0AliasOut records that the first argument arrives in the very
	// register the result is returned in -- true everywhere except x86-64,
	// where arguments start at rdi and the result is rax. That one register
	// has to be bound with a matching constraint; naming it twice is a
	// constraint the verifier rejects.
	arg0AliasOut := false
	// numIsImm records that the number has to be passed in rather than written
	// into the template. x86-64 needs it: LLVM inline asm reads a literal "$N"
	// in the template as a reference to operand N, so `movq $60, %rax` is read
	// as "operand 60", which does not exist. The immediate therefore arrives as
	// the first input and the template names it "$1" (operand 0 is the output,
	// so the first input is operand 1).
	numIsImm := false
	switch arch {
	case "x86_64":
		// SysV hands the first six integer/pointer arguments to rdi, rsi, rdx,
		// r10, r8, r9 -- r10, not rcx, because the `syscall` instruction
		// itself destroys rcx and r11. The number goes in rax and the kernel
		// returns the result in rax, so the number and the result share it.
		argRegs = []string{"rdi", "rsi", "rdx", "r10", "r8", "r9"}
		outReg, scratch = "rax", []string{"rcx", "r11"}
		tmpl = "movq $1, %rax" + asmNL + "syscall"
		numIsImm = true
	case "aarch64":
		argRegs = []string{"x0", "x1", "x2", "x3", "x4", "x5"}
		outReg = "x0"
		scratch = []string{"x8", "x9", "x10", "x11", "x12", "x13", "x14", "x15", "x16", "x17"}
		tmpl = "movz x8, #" + n + asmNL + "svc #0"
		arg0AliasOut = true
	case "riscv64", "riscv32":
		// The RISC-V kernel returns in a0 and preserves everything else the
		// caller can see, so only the number register and the temporaries the
		// kernel borrows are clobbered. `li` is a pseudo-instruction the
		// assembler expands, so the whole 16-bit range needs no special case.
		argRegs = []string{"a0", "a1", "a2", "a3", "a4", "a5"}
		outReg = "a0"
		scratch = []string{"a7", "t0", "t1", "t2", "t3", "t4", "t5", "t6"}
		tmpl = "li a7, " + n + asmNL + "ecall"
		arg0AliasOut = true
	case "arm", "armel":
		// ARM's EABI passes at most four arguments in registers and the rest
		// on the stack, so this is right for the syscalls the C library
		// actually calls with four or fewer operands (all but mmap/select).
		// The register convention is the same in both float ABIs -- only the
		// floating-point *argument* passing differs, and no raw syscall here
		// takes a float.
		argRegs = []string{"r0", "r1", "r2", "r3", "r4", "r5"}
		outReg = "r0"
		scratch = []string{"r7", "r12"}
		tmpl = "mov r7, #" + n + asmNL + "svc #0"
		arg0AliasOut = true
	default:
		return "", "", ""
	}
	cl := []string{"={" + outReg + "}"}
	if numIsImm {
		// The number is the first input, so the arguments shift down by one and
		// the template refers to it as "$1".
		cl = append(cl, "i")
		numOperand = "i64 " + n
	}
	for i := 0; i < nargs && i < len(argRegs); i++ {
		if i == 0 && arg0AliasOut {
			cl = append(cl, "0")
			continue
		}
		cl = append(cl, "{"+argRegs[i]+"}")
	}
	for _, r := range scratch {
		cl = append(cl, "~{"+r+"}")
	}
	return tmpl, strings.Join(cl, ","), numOperand
}
