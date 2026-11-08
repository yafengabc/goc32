package common

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
//
// The C runtime on disk.
//
// goc used to carry the whole C library inside the executable: //go:embed
// baked goclib/*.c and goclib/*.h into the binary, and the loader read them
// back out of an embed.FS. That made goc a single self-contained file -- carry
// the exe and you can compile C -- and it is the reason goclib/ and the
// compiler could never be separated into two projects.
//
// The split into goc and gocl needs the library to be an input both compilers
// read the same way, so it is loaded from disk instead. The cost is stated
// plainly because it is real: **goc.exe is no longer self-contained.** Copied
// away from its goclib/ directory it fails on the first program that calls
// anything, with a diagnostic naming the missing library -- not a silent wrong
// answer, but not a working compiler either. goclib/ therefore has to travel
// with the binary, and any packaging that ships a compiler has to ship it too.
//
// The lookup order is deliberate. GOCLIB_PATH wins so a distribution can point
// at a shared library copy, and so the test suite can build a second compiler
// against a different tree. After that the search runs from the directory
// holding the running executable, and then from the working directory -- a
// developer running `go run ./goc` or `go test` gets a different origin than
// one running the installed binary, and both have to work.
//
// Each origin is searched by probe, which alternates descending and climbing.
// See probe for why one direction alone is not enough.
//
// Everything below the found root is read as a plain directory, and the calls
// keep the "goclib/<name>" spelling an embed.FS would use, so a caller can swap
// this for an embedded library without changing a single call site.

// RootEnv names the environment variable that overrides the search.
const RootEnv = "GOCLIB_PATH"

// Source is where the C library comes from. Two implementations satisfy it:
// sourceFS below, which reads a goclib/ directory from disk, and the embed.FS
// an entry point injects so its binary needs nothing beside it.
//
// The interface is deliberately narrow -- read one file, list one directory --
// and its ReadDir returns names, not fs.DirEntry. That is what lets an
// embed.FS satisfy it without a wrapper: embed.FS's directory entries and the
// disk ones are different types, and normalising to a name list is the point
// at which they become interchangeable.
//
// Errors keep the shape the callers already handle: the preprocessor skips an
// unavailable header rather than failing, and it does that by testing
// err == nil. A missing file on disk and a missing file in an embed are
// indistinguishable there, which is the behaviour we want.
// diskSource reads the library from a directory on disk.
type diskSource struct {
	// root is the directory that holds goclib/. A path handed to ReadFile is
	// joined onto it, so callers keep passing "goclib/<name>".
	root string
	// err records why the library could not be located. The loader checks it
	// once and reports it, rather than each ReadFile failing on its own and
	// the first caller to notice producing a confusing message.
	err error
}

// ReadFile reads one library file. The name is a slash-separated path
// relative to root, matching embed.FS conventions.
func (f diskSource) ReadFile(name string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return os.ReadFile(filepath.Join(f.root, filepath.FromSlash(name)))
}

// ReadDir lists the .c and .h files in one library directory, sorted. The sort
// is not cosmetic: it fixes the order the library's sources are compiled in,
// and therefore the order their definitions reach the symbol table, so the
// output does not depend on how the file system happens to enumerate.
func (f diskSource) ReadDir(name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	ents, err := os.ReadDir(filepath.Join(f.root, filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".c") || strings.HasSuffix(n, ".h") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// probe searches outward from one origin, alternating between descending and
// climbing so the two directions interleave.
//
// The alternation is not decoration. goclib/ is at src/goclib while the
// binary is at bin/goc.exe: the library is neither an ancestor of the binary
// (climb from bin/ goes to the repo root, the parent, the volume, and never
// through src) nor a child of it (descend from bin/ reaches the goc-out*
// build directories). Only going up one level and then back down finds it, and
// a search that tries all of one direction before starting the other cannot
// express that.
//
// Each level is tried as descend-then-climb, so a nearer hit always wins over
// a farther one regardless of direction.
func probe(dir string, consider func(string) (string, bool)) (string, bool) {
	for depth := 0; depth < 4; depth++ {
		if root, ok := descend(dir, consider); ok {
			return root, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // reached the volume root
		}
		dir = parent
	}
	return "", false
}

// descend tries dir, then its children. One level is enough here because
// probe alternates: a deeper library is found by climbing to its parent first
// and descending from there.
func descend(dir string, consider func(string) (string, bool)) (string, bool) {
	if root, ok := consider(dir); ok {
		return root, true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	// Sorted so the choice is deterministic when two subdirectories both
	// carry a library, e.g. a stale copy beside a current one.
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if root, ok := consider(filepath.Join(dir, n)); ok {
			return root, true
		}
	}
	return "", false
}

// findGoclibRoot locates the directory that holds goclib/.
//
// The candidates are tried in order and the first one containing goclib/goclib.h
// wins; a directory that exists but has no library in it is skipped rather than
// accepted, so a stray GOCLIB_PATH does not silently produce an empty runtime.
func FindRoot() (string, error) {
	var tried []string
	consider := func(dir string) (string, bool) {
		if dir == "" {
			return "", false
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		abs = filepath.Clean(abs)
		tried = append(tried, abs)
		// goclib.h is the umbrella header every include path goes through, so
		// its presence is the one signal that this is a real library tree and
		// not a directory that merely shares the name.
		if st, err := os.Stat(filepath.Join(abs, "goclib", "goclib.h")); err == nil && !st.IsDir() {
			return abs, true
		}
		return "", false
	}

	if env := os.Getenv(RootEnv); env != "" {
		if root, ok := consider(env); ok {
			return root, nil
		}
	}
	// Walk up from the executable, then from the working directory.
	//
	// Both are loops over filepath.Dir rather than a fixed number of ".."
	// hops, because the library has moved more than once (src/goclib, then the
	// repository root, now src/ again) and each move broke a hardcoded depth.
	// Two details make this more than a loop:
	//
	//   - filepath.Join(dir, "..") is NOT the way up. Join cleans the ".." away
	//     and returns the parent, so a loop written with it never climbs at all.
	//     An earlier version asked for the parent of the parent and silently
	//     got the parent, which is why the library went missing from src/goc.
	//   - an installed layout has goclib/ beside the exe, while the repository
	//     has it under src/. Climbing covers both without a special case.
	//
	// The executable is tried before the working directory so a copy of the
	// toolchain run from an unrelated project still finds its own library.
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if root, ok := probe(filepath.Dir(exe), consider); ok {
			return root, nil
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if root, ok := probe(wd, consider); ok {
			return root, nil
		}
	}
	return "", fmt.Errorf("cannot find the goclib C library (looked in: %s); "+
		"set %s to the directory that contains goclib/", strings.Join(tried, ", "),
		RootEnv)
}

// The library's two sources, declared together because they are two
// implementations of one idea. goclibCFS and goclibHeaders always hold the same
// value; they are two names so the call sites read as "the .c files" and "the
// headers", the way the code generator and the preprocessor think about them.
//
// Source is an interface rather than embed.FS so the on-disk implementation
// satisfies it too. embed.FS could not be used directly: its ReadDir returns
// []fs.DirEntry, so a wrapper would need the same adaptation anyway.
// Normalising to a sorted list of names is where the two become
// interchangeable.
type Source interface {
	// ReadFile reads one library file, named as in embed.FS: a slash-separated
	// path relative to the library root, so "goclib/stdio.h".
	ReadFile(name string) ([]byte, error)
	// ReadDir lists the .c and .h files in one library directory, sorted.
	ReadDir(name string) ([]string, error)
}

var (
	goclibCFS     Source
	goclibHeaders Source
)

// Disk opens the on-disk library, or returns a source whose every call fails
// with the reason. The loader checks the error once, so a missing library is
// reported once with an actionable message instead of as a read error on an
// unrelated header deep inside the front end.
func Disk() Source {
	root, err := FindRoot()
	if err != nil {
		return diskSource{err: err}
	}
	return diskSource{root: root}
}
