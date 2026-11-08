package common

// The C library: locating it, reading it, and compiling it into the shape a
// back end links against.
//
// Two sources are supported and they are interchangeable. Disk is the
// development default: goclib/ sits in the source tree, so editing a library
// header takes effect without rebuilding the compiler. An embed.FS is the
// distribution form, where the library travels inside the executable. Both
// satisfy Source, and nothing below knows which one is in use.

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"goc/frontend"
)

// Program is the compiled form of the C library for one target: what it
// defines, what it declares, and what it defines at file scope. A back end
// walks these to decide what a program actually needs -- which is why a
// hello-world does not pay for malloc.
type Program struct {
	// Funcs holds the library's definitions, keyed by name. Order is separate
	// because a map cannot be walked deterministically, and the output must
	// not depend on map iteration.
	Funcs   map[string]*frontend.FuncDecl
	Order   []string
	Protos  []*frontend.FuncDecl
	Globals []*frontend.DeclStmt
}

// DLLNames maps a function name to the Windows DLL it is imported from, for
// every prototype in the library that named one inline
// ("extern void ExitProcess(DWORD), kernel32;"). It is filled while the library
// is compiled, which happens before any user program is generated, so a back
// end can consult it for any call it emits.
//
// The name says DLL but the map is populated on both targets: it is simply
// empty for a Linux build, where the imports are syscalls rather than PE
// imports.
var DLLNames = map[string]string{}

// The library sources, resolved through whichever Source is in effect.
// A nil Source is the signal that SetLibrary has not run; ensureDisk fills in
// the on-disk default on first use.
var (
	libC       Source
	libHeaders Source
)

// SetLibrary installs the library source, replacing the on-disk default. It
// must be called before the first compile.
//
// The load is deferred rather than done here because of Go's initialisation
// order: a package's init() runs before its importer's, so a library injected
// from the entry point's init() would arrive after this package had already
// committed to the on-disk one -- and the injection would silently do nothing.
//
// Installing a source does NOT mean the build can be skipped. Build reads
// through libC/libHeaders, so an injected library still has to be compiled --
// prepareLoad installs the source and leaves the build in place. Replacing
// load with a no-op here left both targets nil, and the failure surfaced far
// from its cause: every call in every program came out as
// "unknown function X: not in goclib ()", with the empty list the only hint
// that the library itself had never been built at all.
func SetLibrary(src Source) {
	libC, libHeaders = src, src
	load = prepareLoad
}

// prepareLoad is the body of loadOnce for an installed (non-disk) library.
// It differs from the on-disk default only in that it skips ensureDisk: the
// source is already set, and ensureDisk would be a no-op anyway.
func prepareLoad() {
	// Both targets are attempted even if the first fails, so a problem
	// confined to one platform does not hide the other. The first error
	// is the one reported.
	Win, Err = Build(false, "")
	if Err == nil {
		Linux, Err = Build(true, "")
	}
}

func ensureDisk() {
	if libC == nil {
		libC, libHeaders = Disk(), Disk()
	}
}

var (
	loadOnce sync.Once
	// load is the deferred body of loadOnce. SetLibrary swaps in prepareLoad,
	// the same build without the on-disk search a caller-supplied library does
	// not need.
	load = func() {
		ensureDisk()
		prepareLoad()
	}
)

// The compiled library, one program per target. A build failure is reported
// through Err when a compile runs; a target that failed simply has nil here.
var (
	Win   *Program
	Linux *Program
	Err   error
)

// Ensure compiles the library if it has not been compiled yet. Every entry
// point calls it before its first use: the preprocessor resolves
// "#include <stdio.h>" out of the library, so reaching it with no library is a
// nil dereference in the middle of a header lookup rather than a diagnostic.
func Ensure() {
	loadOnce.Do(load)
}

// Store returns the compiled library for a target, or nil when that target
// failed to build. Callers must have called Ensure first.
func Store(linux bool) *Program {
	if linux {
		return Linux
	}
	return Win
}

// Build compiles the C library for one target.
//
// The umbrella header goes first -- its includes pull in the standard headers,
// so definitions placed there are collected too -- then the .c files in name
// order. The library has no main(), so that one checker diagnostic is expected
// and filtered; anything else is a hard error, because the library must compile
// for every program.
//
// defines (which may be empty) is prepended to every source, so a single macro
// reaches each translation unit. The library files are compiled independently
// of each other, which is what makes that possible -- and it is also why each
// one includes what it needs.
func Build(linux bool, defines string) (*Program, error) {
	ensureDisk()
	lib := &Program{Funcs: map[string]*frontend.FuncDecl{}}
	compile := func(name, src string) error {
		if defines != "" {
			src = defines + src
		}
		toks, err := PreprocessLibrary(src, "goclib/"+name, linux)
		if err != nil {
			return fmt.Errorf("goclib/%s: %v", name, err)
		}
		prog, err := frontend.Parse(toks)
		if err != nil {
			return fmt.Errorf("goclib/%s: %v", name, err)
		}
		for _, e := range frontend.Check(prog) {
			if strings.Contains(e.Error(), "program has no main()") {
				continue // the library is not a program
			}
			return fmt.Errorf("goclib/%s: %v", name, e)
		}
		lib.Protos = append(lib.Protos, prog.Prototypes...)
		// A prototype may name its import library ("..., user32"); record it
		// here too, because the library itself is parsed at start-up and only
		// reaches code generation later, when the user program is emitted.
		for _, pr := range prog.Prototypes {
			if pr.DLL != "" {
				DLLNames[pr.Name] = pr.DLL
			}
		}
		lib.Globals = append(lib.Globals, prog.Globals...)
		// Definitions: later files win over earlier ones, so a .c definition
		// overrides a header definition of the same name.
		for _, f := range prog.Funcs {
			if _, dup := lib.Funcs[f.Name]; !dup {
				lib.Order = append(lib.Order, f.Name)
			}
			lib.Funcs[f.Name] = f
		}
		return nil
	}

	umbrella, err := libHeaders.ReadFile("goclib/goclib.h")
	if err != nil {
		return nil, err
	}
	if err := compile("goclib.h", string(umbrella)); err != nil {
		return nil, err
	}
	entries, err := libC.ReadDir("goclib")
	if err != nil {
		return nil, err
	}
	for _, f := range entries {
		if !strings.HasSuffix(f, ".c") {
			continue
		}
		b, err := libC.ReadFile("goclib/" + f)
		if err != nil {
			return nil, err
		}
		if err := compile(f, string(b)); err != nil {
			return nil, err
		}
	}
	return lib, nil
}

// PublicNames lists the library functions a program may call: everything
// defined, minus the internal __goclib_* primitives. Sorted, so a caller that
// reports "available functions" does not vary between runs.
func PublicNames(linux bool) []string {
	var out []string
	if lib := Store(linux); lib != nil {
		for _, n := range lib.Order {
			if strings.HasPrefix(n, "__") {
				continue
			}
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
