// Turning C source files into one checked program.
//
// This is the front half of a build, and both compilers do it identically:
// read each source, inject the -D macros, preprocess for the target, parse,
// and -- when there is more than one file -- merge the translation units into
// a single Program. What differs between them is only what happens to the
// result.
//
// It lives here rather than in either back end because the merging rules are
// not a matter of taste. Two files that each declare `static int helper` get
// distinct symbols, an enumerator defined twice with different values is an
// error rather than a silent last-writer-wins, and a duplicate non-static
// definition is a duplicate definition. Those three answers have to be the
// same no matter which back end is asked, or the same source would compile
// under one compiler and fail under the other for no visible reason.

package common

import (
	"fmt"
	"os"
	"strings"

	"goc/frontend"
)

// InjectDefines prepends a #define line for each -D, in the spelling gcc
// accepts: NAME=VALUE, or NAME on its own for 1.
//
// The macros go in front of the source text rather than into the
// preprocessor's table, so they reach every header the file includes --
// which is what makes -D visible to code the user did not write. A macro
// defined only in the top-level stream would not be.
func InjectDefines(src string, defines []string) string {
	if len(defines) == 0 {
		return src
	}
	var b strings.Builder
	for _, d := range defines {
		if eq := strings.IndexByte(d, '='); eq >= 0 {
			b.WriteString("#define ")
			b.WriteString(d[:eq])
			b.WriteByte(' ')
			b.WriteString(d[eq+1:])
			b.WriteByte('\n')
		} else {
			b.WriteString("#define ")
			b.WriteString(d)
			b.WriteString(" 1\n")
		}
	}
	b.WriteString(src)
	return b.String()
}

// PreprocessFile reads one source file, injects the -D macros and runs the
// target-aware preprocessor over it.
func PreprocessFile(path string, defines []string, linux bool, incDirs ...string) ([]frontend.Token, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return PreprocessTarget(InjectDefines(string(src), defines), path, linux, incDirs...)
}

// Translate turns source files into one checked program: preprocess, parse,
// merge, check. The result is everything a back end needs and nothing about
// how the image will be built.
//
// Several files are several translation units -- each has its own macros and
// its own type names -- and the merged program is what one image is built
// from. A single file skips the merge entirely, which is not just an
// optimisation: it keeps the common case identical to what a one-file build
// has always done.
func Translate(paths []string, defines []string, linux bool, incDirs ...string) (*frontend.Program, error) {
	units := make([]*parsedUnit, 0, len(paths))
	for _, path := range paths {
		toks, err := PreprocessFile(path, defines, linux, incDirs...)
		if err != nil {
			return nil, err
		}
		// frontend.Parse resets typedefs, structs and frontend.EnumConsts on
		// entry, which gives this file a clean type namespace -- but it also
		// leaves behind this file's enumerators, which the next Parse wipes.
		// Snapshot them here and merge them back once every unit is parsed.
		prog, err := frontend.Parse(toks)
		if err != nil {
			return nil, fmt.Errorf("%s: parse error: %w", path, err)
		}
		u := &parsedUnit{path: path, prog: prog, enums: map[string]int64{}}
		for k, v := range frontend.EnumConsts {
			u.enums[k] = v
		}
		units = append(units, u)
	}
	// Restore every unit's enumerators: frontend.Check resolves case-label
	// constants from this table, and constant folding reads it too.
	for k := range frontend.EnumConsts {
		delete(frontend.EnumConsts, k)
	}
	for _, u := range units {
		for k, v := range u.enums {
			if old, dup := frontend.EnumConsts[k]; dup && old != v {
				return nil, fmt.Errorf("%s: enumerator %q is also defined with a different value in another file", u.path, k)
			}
			frontend.EnumConsts[k] = v
		}
	}
	if len(units) == 1 {
		return units[0].prog, nil
	}
	return MergeUnits(units)
}

// parsedUnit is one file's parse result plus the enumerators it declared.
type parsedUnit struct {
	path string
	prog *frontend.Program
	// enums holds the enumerators this unit declared; frontend.Parse clears the global
	// table before every unit, so the values are collected here and merged
	// back once all units are parsed.
	enums map[string]int64
}

// unitSym is one file-scope definition, recorded so that merging can tell a
// duplicate (the same non-static name in two files) from an intentional
// private copy (the same static name in two files).

type unitSym struct {
	name   string
	static bool
	kind   string // "function" or "variable"
	unit   int
	path   string
}

func MergeUnits(units []*parsedUnit) (*frontend.Program, error) {
	// 1. Collect every file-scope definition, grouped by name.
	defs := map[string][]unitSym{}
	for i, u := range units {
		for _, f := range u.prog.Funcs {
			defs[f.Name] = append(defs[f.Name], unitSym{f.Name, f.Storage == "static", "function", i, u.path})
		}
		for _, g := range u.prog.Globals {
			if g.Storage == "extern" {
				continue // a declaration, not a definition
			}
			defs[g.Name] = append(defs[g.Name], unitSym{g.Name, g.Storage == "static", "variable", i, u.path})
		}
	}

	// 2. Decide the renames. A name defined by two units is a hard error
	// unless at least one of the definitions is static: then every *static*
	// definition of that name gets a per-unit unique name and the external
	// one keeps its own.
	renames := make([]map[string]string, len(units))
	for i := range units {
		renames[i] = map[string]string{}
	}
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	// Sorted iteration keeps diagnostics deterministic.
	sortStrings(names)
	for _, n := range names {
		syms := defs[n]
		if len(syms) < 2 {
			continue
		}
		byUnit := map[int]bool{}
		for _, s := range syms {
			byUnit[s.unit] = true
		}
		if len(byUnit) < 2 {
			// The same file defines the name twice. frontend.Check catches a repeated
			// global ("redefinition in the same scope") but not a repeated
			// function, which would silently emit the label twice -- e.g.
			// `goc a.c a.c`.
			if syms[0].kind == "function" {
				return nil, fmt.Errorf("duplicate definition of function %q in %s", n, syms[0].path)
			}
			continue
		}
		anyStatic := false
		for _, s := range syms {
			if s.static {
				anyStatic = true
			}
		}
		if !anyStatic {
			return nil, fmt.Errorf("duplicate definition of %s %q: %s and %s",
				syms[0].kind, n, syms[0].path, syms[len(syms)-1].path)
		}
		for _, s := range syms {
			if s.static {
				renames[s.unit][n] = uniqueRename(n, s.unit, defs)
			}
		}
	}

	// 3. Apply the renames (declaration + every reference) and merge.
	merged := &frontend.Program{}
	globals := map[string]*frontend.DeclStmt{}
	var globalOrder []string
	protos := map[string]*frontend.FuncDecl{}
	for i, u := range units {
		if len(renames[i]) > 0 {
			RenameInProgram(u.prog, renames[i])
		}
		merged.Funcs = append(merged.Funcs, u.prog.Funcs...)
		for _, p := range u.prog.Prototypes {
			if prev, dup := protos[p.Name]; dup {
				// Keep the first prototype but remember an import library
				// named on any of them (`extern long MessageBoxA(...), user32`).
				if prev.DLL == "" && p.DLL != "" {
					prev.DLL = p.DLL
				}
				continue
			}
			protos[p.Name] = p
			merged.Prototypes = append(merged.Prototypes, p)
		}
		for _, g := range u.prog.Globals {
			prev, dup := globals[g.Name]
			if !dup {
				globals[g.Name] = g
				globalOrder = append(globalOrder, g.Name)
				continue
			}
			// Same name in two files: two plain declarations of the same
			// header-declared global are one object; a real definition wins
			// over an extern declaration and two definitions were rejected
			// above (or renamed, for statics).
			prevDef := prev.Storage != "extern"
			thisDef := g.Storage != "extern"
			if thisDef && !prevDef {
				globals[g.Name] = g
				for j, nm := range globalOrder {
					if nm == g.Name {
						globalOrder[j] = g.Name // keep the position
						break
					}
				}
			}
		}
	}
	for _, n := range globalOrder {
		merged.Globals = append(merged.Globals, globals[n])
	}
	return merged, nil
}

// uniqueRename picks a file-unique replacement for a static symbol.
func uniqueRename(name string, unit int, defs map[string][]unitSym) string {
	for i := 0; ; i++ {
		cand := fmt.Sprintf("%s__tu%d", name, unit)
		if i > 0 {
			cand = fmt.Sprintf("%s__tu%d_%d", name, unit, i)
		}
		if _, taken := defs[cand]; !taken {
			return cand
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
