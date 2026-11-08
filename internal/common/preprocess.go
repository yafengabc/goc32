package common

// cpp.go — the C preprocessor for goc.
//
// It runs on the token stream produced by frontend.Lex (where '#' is a real token) and
// returns a fully-expanded token stream that frontend.Parse can consume. The design is
// token-based (not text-based), so macro arguments, '#' stringisation and '##'
// pasting are handled on real tokens rather than by string hacking.
//
// Supported:
//   - #include "file" / <file>   (local relative + search dirs)
//   - #define object- and function-like macros, with #, ## and __VA_ARGS__
//   - #embed "file" / <file>  (C23; limit/prefix/suffix/if_empty params)
//   - __VA_OPT__(x) (C23 variadic macro operator)
//   - #undef
//   - #if / #ifdef / #ifndef / #else / #elif / #endif  (constant expressions,
//     including the defined() operator)
//   - #error   (raised only when the branch is active)
//   - #line N ["file"] (and the GNU "# N ["file"]" form), affecting __LINE__,
//     __FILE__ and diagnostic line numbers
//   - predefined macros __FILE__, __LINE__, __goc__
//   - backslash line continuations inside macro definitions (and in any file,
//     main source or #include -- spliceContinuations runs per file in process)
//   - // and /* */ comments (stripped by frontend.Lex)
//
// Things deliberately left for a later stage: #pragma beyond ignoring it,
// and most of the hosted-header ecosystem.

import (
	"fmt"
	"goc/frontend"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Macro is a single #define'd entity.
type Macro struct {
	Name       string
	Params     []string // nil/empty for object-like; for variadic includes "__VA_ARGS__"
	IsFunc     bool
	IsVariadic bool
	Body       []frontend.Token
}

// condFrame tracks one #if/#else chain.
type condFrame struct {
	branchTaken bool // a branch in this block has already evaluated true
	active      bool // is the current branch selected for output
	seenElse    bool
}

// Preprocessor holds the expandable state shared across a translation unit,
// including files pulled in by #include.
type Preprocessor struct {
	// builtinOnly confines header resolution to the built-in library, skipping
	// every disk candidate. The C library itself is compiled with it set.
	//
	// Without it, a user who ships their own stdio.h in the working directory
	// -- or points -I at a directory containing one -- replaces the runtime's
	// header, and goclib then fails to compile against itself: file.c reaches
	// for FILE, does not find it, and the error names a library source rather
	// than the header the user shadowed. A real compiler has the same
	// isolation, under different spelling: the implementation's own sources
	// are never searched against the user's include path.
	//
	// Only the library sets it. User code resolves headers exactly as before,
	// so an override still works for the program being compiled.
	builtinOnly bool
	macros      map[string]*Macro
	inExpand    map[string]bool // macro names currently being expanded (recursion guard)
	condStack   []condFrame
	searchDirs  []string
	baseDir     string
	// #line state: a logical file name (empty = the real source file) and the
	// offset such that logicalLine = physicalLine + lineDelta. Reset per file
	// in process, so a #line inside an #include cannot leak into the includer.
	logicalFile string
	lineDelta   int
	// ceErr records a constant-expression evaluation error (division or
	// modulo by zero) inside #if/#elif. The ce* chain cannot return errors
	// without threading a signature through every level, so the directive
	// handlers check this flag and abort with a real diagnostic instead of
	// silently treating the expression as 0.
	ceErr bool
	// arityErr records the first function-like macro invoked with the wrong
	// number of arguments. Like ceErr it is a deferred flag: expansion runs
	// deep inside token scanning, which has no error return to thread a
	// diagnostic through, so the invocation is recorded here and reported
	// once the whole translation unit has been preprocessed.
	//
	// C11 6.10.3p4 requires a diagnostic for a mismatched invocation. Without
	// one the substitution indexed past the end of args and panicked when too
	// few were given, and silently dropped the surplus when too many were.
	arityErr error
	// condEval is set only while a #if/#elif condition expression is being
	// expanded and evaluated. During that window macro expansion must ignore
	// the enclosing branch-activity state: a "#elif X" following a false
	// "#if" still expands X normally (C 6.10.1), otherwise the condition is
	// evaluated against unexpanded identifiers and always comes out false.
	condEval bool
}

// Preprocess runs the full preprocessing pipeline on src (already read from
// filename) and returns the expanded token stream. The target platform
// defaults to Windows; PreprocessTarget selects it explicitly.
func Preprocess(src, filename string) ([]frontend.Token, error) {
	return PreprocessTarget(src, filename, false)
}

// PreprocessTarget is Preprocess with an explicit target platform. The target
// only decides which platform macros are predefined -- _WIN32/_WIN64 for the
// Windows x64 backend, __linux__ (and __linux) for the ELF backend -- so
// library sources and user programs can write portable
// "#if defined(_WIN32) ... #else ... #endif" branches.
func PreprocessTarget(src, filename string, linux bool, incDirs ...string) ([]frontend.Token, error) {
	return preprocess(src, filename, linux, false, incDirs...)
}

// PreprocessLibrary preprocesses one of the C library's own sources. It differs
// from PreprocessTarget in exactly one way: no header outside the built-in
// library can be reached. See Preprocessor.builtinOnly.
func PreprocessLibrary(src, filename string, linux bool) ([]frontend.Token, error) {
	ensureDisk()
	return preprocess(src, filename, linux, true)
}

func preprocess(src, filename string, linux, builtinOnly bool, incDirs ...string) ([]frontend.Token, error) {
	// The library source is filled in on first use (see ensureDisk). Doing it
	// here rather than only in PreprocessLibrary matters for every direct
	// caller of Preprocess: doInclude's fallback reads goclib headers through
	// libHeaders, and a nil Source there turns an unavailable header into a
	// nil-pointer panic instead of a skipped include.
	ensureDisk()
	src = spliceContinuations(src)
	p := &Preprocessor{
		macros:      map[string]*Macro{},
		inExpand:    map[string]bool{},
		searchDirs:  []string{},
		builtinOnly: builtinOnly,
	}
	// Platform macros, mirroring what real compilers predefine: object-like
	// macros with body "1", visible to #ifdef, defined() and plain expansion.
	if linux {
		p.macros["__linux__"] = &Macro{Name: "__linux__", Body: []frontend.Token{tokNum(1, 0)}}
		p.macros["__linux"] = &Macro{Name: "__linux", Body: []frontend.Token{tokNum(1, 0)}}
	} else {
		p.macros["_WIN32"] = &Macro{Name: "_WIN32", Body: []frontend.Token{tokNum(1, 0)}}
		p.macros["_WIN64"] = &Macro{Name: "_WIN64", Body: []frontend.Token{tokNum(1, 0)}}
	}
	// __goc__ marks "this source is being compiled by goc". It is the hook
	// goclib's headers use to tell the two apart: under goc the variadic
	// operations are code-generator builtins and the windows.h family is
	// spelled with goc's own inline-DLL syntax; under gcc/clang the same sources
	// must reach the host's __builtin_va_* builtins and libc instead. Real
	// compilers define their own such macro (__GNUC__, _MSC_VER, __clang__),
	// and none of them define this one, so the test is false there.
	//
	// It has to live in p.macros, not only in expandAt's switch: a header asks
	// with `#ifdef __goc__', and that consults the macro table. Handling it
	// solely as an expansion-time special case made every such test read false
	// and silently compiled goclib down the host path -- defining va_list as
	// __builtin_va_list, which goc does not have a type for.
	p.macros["__goc__"] = &Macro{Name: "__goc__", Body: []frontend.Token{tokNum(1, 0)}}
	// Standard predefined macros (C99 6.10.8 / C23 6.11). goc is a single-mode
	// compiler that accepts C89 through C23 source; __STDC_VERSION__ reports the
	// highest standard whose core features are implemented (C23) and also drives
	// visibility of C23-only declarations in goclib headers. __DATE__/__TIME__
	// are snapshotted once per compilation, mirroring real compilers.
	now := time.Now()
	p.macros["__STDC__"] = &Macro{Name: "__STDC__", Body: []frontend.Token{tokNum(1, 0)}}
	p.macros["__STDC_HOSTED__"] = &Macro{Name: "__STDC_HOSTED__", Body: []frontend.Token{tokNum(1, 0)}}
	p.macros["__STDC_VERSION__"] = &Macro{Name: "__STDC_VERSION__", Body: []frontend.Token{tokNum(202311, 0)}}
	p.macros["__DATE__"] = &Macro{Name: "__DATE__", Body: []frontend.Token{tokStr(now.Format("Jan _2 2006"), 0)}}
	p.macros["__TIME__"] = &Macro{Name: "__TIME__", Body: []frontend.Token{tokStr(now.Format("15:04:05"), 0)}}
	// GCC/Clang's memory-order constants. They are compiler-predefined rather
	// than declared anywhere -- GCC puts them in the compiler, not in
	// <stdatomic.h> -- and they are the only way to spell an order for the
	// __atomic_* builtins, which goc accepts (see src/frontend/atomic.go). The
	// values are the GCC enum's, so a program that passes __ATOMIC_ACQUIRE may
	// be read against GCC's own list.
	for name, val := range map[string]int64{
		"__ATOMIC_RELAXED": 0,
		"__ATOMIC_CONSUME": 1,
		"__ATOMIC_ACQUIRE": 2,
		"__ATOMIC_RELEASE": 3,
		"__ATOMIC_ACQ_REL": 4,
		"__ATOMIC_SEQ_CST": 5,
	} {
		p.macros[name] = &Macro{Name: name, Body: []frontend.Token{tokNum(val, 0)}}
	}
	// Vendor extension keywords that real-world headers use but that carry no
	// meaning for goc's code generator. They are predefined as macros that
	// expand to nothing, so a declaration such as
	// "__declspec(dllexport) void __stdcall f(void)" parses as plain C
	// instead of dying on the first unknown token. This is what lets a
	// portable library's Windows branch compile without patching its source.
	// __declspec/__attribute__ take one parenthesised argument list.
	p.macros["__declspec"] = &Macro{Name: "__declspec", Params: []string{"x"}, IsFunc: true}
	p.macros["__attribute__"] = &Macro{Name: "__attribute__", Params: []string{"x"}, IsFunc: true}
	for _, kw := range []string{"__stdcall", "__cdecl", "__fastcall", "__thiscall",
		"__inline", "__forceinline", "__restrict", "__restrict__", "__extension__"} {
		p.macros[kw] = &Macro{Name: kw}
	}
	p.baseDir = filepath.Dir(filename)
	// User -I directories take priority over the base dir and the cwd.
	p.searchDirs = append(p.searchDirs, incDirs...)
	p.searchDirs = append(p.searchDirs, p.baseDir, ".")
	toks, err := p.process(src, filename)
	if err != nil {
		return nil, err
	}
	if p.arityErr != nil {
		return nil, p.arityErr
	}
	return toks, nil
}

// SerializeTokens turns a preprocessed token stream back into C source text.
// This is the backend for the "-E" (preprocess-only) mode: the stream already
// has directives stripped and macros expanded/includes resolved, so what comes
// out is a single self-contained translation unit, much like `gcc -E`.
//
// Exact whitespace is not reconstructed (only the Space flag, which records
// whether a token was preceded by whitespace); line markers ("# 1 \"file\"")
// are intentionally omitted. That is enough for shell-level compatibility and
// for feeding the output back to goc, not for byte-exact gcc reproduction.
func SerializeTokens(toks []frontend.Token) string {
	var b strings.Builder
	for _, t := range toks {
		s := tokenText(t)
		if b.Len() > 0 && t.Space {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}

// tokenText renders one token as the C text it represents.
func tokenText(t frontend.Token) string {
	switch t.Kind {
	case frontend.TStr:
		return cEscape(t.Str)
	case frontend.TNum:
		if t.IsChar {
			return charLiteral(t.Num)
		}
		return t.Text // original numeric/character text is preserved verbatim
	default:
		return t.Text // frontend.TIdent, frontend.TKeyword, frontend.TPunct all carry their source text
	}
}

// charLiteral rebuilds a 'x' literal from its integer value, re-escaping the
// few control characters C source would spell out.
func charLiteral(v int64) string {
	switch v {
	case '\n':
		return "'\\n'"
	case '\t':
		return "'\\t'"
	case '\r':
		return "'\\r'"
	case 0:
		return "'\\0'"
	case '\'':
		return "'\\''"
	case '\\':
		return "'\\\\'"
	default:
		return fmt.Sprintf("'%c'", rune(v))
	}
}

// cEscape renders raw bytes as a C double-quoted string literal, re-spelling
// the control characters the way C source does (NUL -> \0, not \x00) so the
// -E output is itself valid C.
func cEscape(b []byte) string {
	var s strings.Builder
	s.WriteByte('"')
	for _, c := range b {
		switch c {
		case '\n':
			s.WriteString("\\n")
		case '\t':
			s.WriteString("\\t")
		case '\r':
			s.WriteString("\\r")
		case 0:
			s.WriteString("\\0")
		case '"':
			s.WriteString("\\\"")
		case '\\':
			s.WriteString("\\\\")
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&s, "\\x%02x", c)
			} else {
				s.WriteByte(c)
			}
		}
	}
	s.WriteByte('"')
	return s.String()
}

// spliceContinuations removes backslash-newline pairs (C translation phase 2),
// so multi-line macro definitions collapse onto one logical line.
//
// Phase 2 splices physical lines, it does not renumber them: C11 6.10.8p1
// makes __LINE__ the physical line of the current source line, and every
// diagnostic points at a physical line too. Simply deleting the newlines
// shifts every later line up by one per continuation, so instead the deleted
// newlines are *moved* to the end of the logical line they belong to. The
// line count is conserved, so a logical line starts on its first physical
// line and the text after it keeps its real line number.
func spliceContinuations(src string) string {
	// A UTF-8 BOM (EF BB BF) is permitted at the very start of a source file
	// (C11 5.1.1.2 translation phase 1); strip it here so it does not surface
	// as the lexer's "unexpected character 'ï'". spliceContinuations is the
	// single choke point every file -- the main TU and every #include target
	// -- passes through, so stripping here covers all of them.
	src = strings.TrimPrefix(src, "\xEF\xBB\xBF")
	var b strings.Builder
	i := 0
	n := len(src)
	held := 0 // newlines removed by continuations in the logical line so far
	for i < n {
		if src[i] == '\\' {
			if i+1 < n && src[i+1] == '\n' {
				held++
				i += 2
				continue
			}
			if i+2 < n && src[i+1] == '\r' && src[i+2] == '\n' {
				held++
				i += 3
				continue
			}
		}
		if src[i] == '\n' {
			// End of the logical line: pay back the newlines the
			// continuations swallowed, then this one.
			for k := 0; k < held; k++ {
				b.WriteByte('\n')
			}
			held = 0
			b.WriteByte('\n')
			i++
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	for k := 0; k < held; k++ {
		b.WriteByte('\n')
	}
	return b.String()
}

// process tokenises one file and scans it. It owns a fresh conditional-compile
// stack and #line state (an #if or #line inside an included file does not
// affect the includer), but shares the macro table so definitions are global.
//
// src is run through spliceContinuations here, not just at the Preprocess
// entry point, so that a file pulled in by #include gets its backslash-newline
// pairs removed too (phase 2 must apply to every file, not just the main one).
func (p *Preprocessor) process(src, filename string) ([]frontend.Token, error) {
	src = spliceContinuations(src)
	save := p.condStack
	saveFile := p.logicalFile
	saveDelta := p.lineDelta
	p.condStack = nil
	p.logicalFile = ""
	p.lineDelta = 0
	defer func() {
		p.condStack = save
		p.logicalFile = saveFile
		p.lineDelta = saveDelta
	}()

	raw, err := frontend.Lex(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", filename, err)
	}
	out, err := p.scan(raw, filename)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// active reports whether the current position is inside a selected branch of
// every enclosing #if.
func (p *Preprocessor) active() bool {
	for _, f := range p.condStack {
		if !f.active {
			return false
		}
	}
	return true
}

// scan walks the raw token stream, expands identifiers that are macros, and
// handles directives.
func (p *Preprocessor) scan(raw []frontend.Token, filename string) ([]frontend.Token, error) {
	var out []frontend.Token
	i := 0
	n := len(raw)
	for i < n {
		t := raw[i]
		if t.Kind == frontend.TEOF {
			break
		}
		if t.Kind == frontend.TPunct && t.Text == "#" && isDirectiveStart(raw, i) {
			hashLine := t.Line
			i++ // consume '#'
			if i >= n {
				continue
			}
			name := raw[i].Text
			i++ // consume directive name
			var rest []frontend.Token
			for i < n && raw[i].Line == hashLine {
				rest = append(rest, raw[i])
				i++
			}
			app, err := p.execDirective(name, rest, hashLine, filename)
			if err != nil {
				return nil, err
			}
			out = append(out, app...)
			continue
		}
		// Ordinary token (or inactive code): skip entirely when not active.
		if !p.active() {
			i++
			continue
		}
		e, ni := p.expandAt(raw, i, filename, raw[i].Line)
		out = append(out, e...)
		i = ni
	}
	out = append(out, frontend.Token{Kind: frontend.TEOF, Line: 0})
	return out, nil
}

// isDirectiveStart reports whether the '#' at index i begins a directive: it
// must be the first non-blank token on its line.
func isDirectiveStart(raw []frontend.Token, i int) bool {
	if raw[i].Text != "#" {
		return false
	}
	if i == 0 {
		return true
	}
	return raw[i-1].Line != raw[i].Line
}

// execDirective runs one directive and returns any tokens it produces (only
// #include produces tokens).
func (p *Preprocessor) execDirective(name string, rest []frontend.Token, line int, filename string) ([]frontend.Token, error) {
	// GNU extension: "# 100 \"file\"" is the same as "#line 100 \"file\"".
	if isAllDigits(name) {
		n, _ := strconv.ParseInt(name, 10, 64)
		return p.doLine(n, rest, line, filename)
	}
	switch name {
	case "include":
		if !p.active() {
			return nil, nil
		}
		return p.doInclude(rest, filename)
	case "embed":
		if !p.active() {
			return nil, nil
		}
		return p.doEmbed(rest, filename)
	case "define":
		if p.active() {
			p.doDefine(rest)
		}
	case "undef":
		if p.active() && len(rest) > 0 {
			delete(p.macros, rest[0].Text)
		}
	case "if":
		p.condEval = true
		expanded := p.expandTokens(rest, filename, line)
		cond := p.constExpr(expanded)
		p.condEval = false
		if p.ceErr {
			return nil, fmt.Errorf("%s:%d: division by zero in #if expression", p.logicalFileName(filename), p.logicalLine(line))
		}
		p.pushCond(cond != 0)
	case "ifdef":
		p.pushCond(p.macroDefined(rest))
	case "ifndef":
		p.pushCond(!p.macroDefined(rest))
	case "elif":
		p.condEval = true
		expanded := p.expandTokens(rest, filename, line)
		cond := p.constExpr(expanded)
		p.condEval = false
		if p.ceErr {
			return nil, fmt.Errorf("%s:%d: division by zero in #elif expression", p.logicalFileName(filename), p.logicalLine(line))
		}
		p.doElif(cond != 0)
	case "elifdef":
		p.doElifBool(p.macroDefined(rest))
	case "elifndef":
		p.doElifBool(!p.macroDefined(rest))
	case "else":
		p.doElse()
	case "endif":
		if len(p.condStack) > 0 {
			p.condStack = p.condStack[:len(p.condStack)-1]
		}
	case "error":
		if p.active() {
			return nil, fmt.Errorf("%s:%d: #error", p.logicalFileName(filename), p.logicalLine(line))
		}
	case "line":
		if !p.active() {
			return nil, nil
		}
		if len(rest) == 0 || rest[0].Kind != frontend.TNum {
			return nil, fmt.Errorf("%s:%d: #line needs a line number", p.logicalFileName(filename), p.logicalLine(line))
		}
		return p.doLine(rest[0].Num, rest[1:], line, filename)
	case "warning":
		if p.active() {
			fmt.Fprintf(os.Stderr, "%s:%d: warning: %s\n",
				p.logicalFileName(filename), p.logicalLine(line), tokensText(rest))
		}
	case "pragma":
		// Ignored for now.
	default:
		if p.active() {
			return nil, fmt.Errorf("%s:%d: unknown preprocessing directive #%s", p.logicalFileName(filename), p.logicalLine(line), name)
		}
	}
	return nil, nil
}

// isAllDigits reports whether s is a non-empty digit sequence (the GNU
// "# <digits>" directive name).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// logicalLine and logicalFileName apply the current #line state to a physical
// source position, for __LINE__ / __FILE__ and for diagnostics.
func (p *Preprocessor) logicalLine(line int) int { return line + p.lineDelta }

func (p *Preprocessor) logicalFileName(filename string) string {
	if p.logicalFile != "" {
		return p.logicalFile
	}
	return filename
}

// doLine implements #line N ["file"]: from the next source line on, __LINE__
// reports N+1 for the line after the directive, and __FILE__ (if given) the
// new name. Applies only when the branch is active.
func (p *Preprocessor) doLine(n int64, rest []frontend.Token, line int, filename string) ([]frontend.Token, error) {
	if !p.active() {
		return nil, nil
	}
	if n <= 0 {
		return nil, fmt.Errorf("%s:%d: #line line number must be positive", p.logicalFileName(filename), p.logicalLine(line))
	}
	// The directive sits on physical line `line`; the next physical line is
	// logically N+1, so the offset is N - line.
	p.lineDelta = int(n) - line
	if len(rest) > 0 && rest[0].Kind == frontend.TStr {
		p.logicalFile = string(rest[0].Str)
	}
	return nil, nil
}

// macroDefined handles #ifdef / #ifndef, both "X" and "(X)" forms.
func (p *Preprocessor) macroDefined(rest []frontend.Token) bool {
	if len(rest) == 0 {
		return false
	}
	name := rest[0].Text
	if name == "(" && len(rest) >= 3 {
		name = rest[1].Text
	}
	_, ok := p.macros[name]
	return ok
}

// tokensText renders a token slice as a single space-separated string, for use
// in diagnostics such as #warning messages. String literals carry their content
// in .Str (mirroring doLine), not .Text.
func tokensText(ts []frontend.Token) string {
	s := ""
	for i, t := range ts {
		if i > 0 {
			s += " "
		}
		if t.Kind == frontend.TStr {
			s += string(t.Str)
		} else {
			s += t.Text
		}
	}
	return s
}

func (p *Preprocessor) pushCond(cond bool) {
	p.condStack = append(p.condStack, condFrame{branchTaken: cond, active: cond})
}

func (p *Preprocessor) doElse() {
	if len(p.condStack) == 0 {
		return
	}
	f := p.condStack[len(p.condStack)-1]
	if f.seenElse {
		return
	}
	f.seenElse = true
	f.active = !f.branchTaken
	p.condStack[len(p.condStack)-1] = f
}

// doElif updates the top conditional frame for a #elif whose condition was
// already evaluated by the caller (which also checked the division-by-zero
// flag); cond is the branch's own condition.
func (p *Preprocessor) doElif(cond bool) {
	if len(p.condStack) == 0 {
		return
	}
	f := p.condStack[len(p.condStack)-1]
	if f.seenElse {
		return
	}
	// This #elif branch is selected only when no earlier branch in the chain
	// fired and its own condition holds. Every other case must leave the
	// frame INACTIVE -- including the case where an earlier #if/#elif fired,
	// which left the frame active: without the explicit deactivation the
	// #elif body would be emitted in addition to the taken branch.
	if !f.branchTaken && cond {
		f.active = true
	} else {
		f.active = false
	}
	f.branchTaken = f.branchTaken || cond
	p.condStack[len(p.condStack)-1] = f
}

// doElifBool is like doElif but takes the already-resolved condition, so that
// #elifdef / #elifndef can reuse the chain logic with a defined() test.
func (p *Preprocessor) doElifBool(cond bool) {
	if len(p.condStack) == 0 {
		return
	}
	f := p.condStack[len(p.condStack)-1]
	if f.seenElse {
		return
	}
	if !f.branchTaken && cond {
		f.active = true
	} else {
		f.active = false
	}
	f.branchTaken = f.branchTaken || cond
	p.condStack[len(p.condStack)-1] = f
}

// doDefine parses a #define directive from its body tokens (everything after
// "define").
func (p *Preprocessor) doDefine(rest []frontend.Token) {
	if len(rest) == 0 {
		return
	}
	name := rest[0].Text
	m := &Macro{Name: name}
	i := 1
	// A function-like macro has '(' immediately after the name (no space).
	if i < len(rest) && rest[i].Text == "(" && !rest[i].Space {
		m.IsFunc = true
		i++ // skip '('
		for i < len(rest) && rest[i].Text != ")" {
			if rest[i].Text == "..." {
				m.IsVariadic = true
				i++
				if i < len(rest) && rest[i].Text == "," {
					i++
				}
				continue
			}
			if rest[i].Kind == frontend.TIdent {
				m.Params = append(m.Params, rest[i].Text)
			}
			i++
			if i < len(rest) && rest[i].Text == "," {
				i++
			}
		}
		if m.IsVariadic {
			m.Params = append(m.Params, "__VA_ARGS__")
		}
		if i < len(rest) && rest[i].Text == ")" {
			i++
		}
	}
	m.Body = rest[i:]
	p.macros[name] = m
}

// doInclude resolves and expands an #include.
func (p *Preprocessor) doInclude(rest []frontend.Token, filename string) ([]frontend.Token, error) {
	if len(rest) == 0 {
		return nil, fmt.Errorf("%s: #include needs a filename", filename)
	}
	t := rest[0]
	var path string
	angled := false
	if t.Kind == frontend.TStr {
		path = string(t.Str)
	} else if t.Text == "<" {
		var sb strings.Builder
		for i := 1; i < len(rest) && rest[i].Text != ">"; i++ {
			sb.WriteString(rest[i].Text)
		}
		path = sb.String()
		angled = true
	} else {
		return nil, fmt.Errorf("%s: malformed #include", filename)
	}

	full, err := p.resolveInclude(path, filename, angled)
	if err != nil {
		// Headers that goc ships (stdio.h, stddef.h, ...) are embedded as
		// real files under goclib/ and injected directly, with no disk
		// lookup. The fallback serves BOTH the <file> and "file" spellings:
		// sources that live only inside the embedded FS -- the built-in
		// goclib itself -- include their own headers by name, and a quoted
		// include from an on-disk file must still resolve when the compiler
		// runs outside the source tree. An unavailable <file> is skipped
		// rather than fatal.
		// goclib ships one flat header directory, so a path with a POSIX
		// prefix such as <sys/stat.h> matches on its basename (goclib/stat.h).
		// That keeps the standard spelling working without a goclib/sys tree.
		if src, rerr := libHeaders.ReadFile("goclib/" + path); rerr == nil {
			inc, perr := p.process(string(src), "<builtin:"+path+">")
			if perr != nil {
				return nil, perr
			}
			if len(inc) > 0 && inc[len(inc)-1].Kind == frontend.TEOF {
				inc = inc[:len(inc)-1]
			}
			return inc, nil
		}
		if base := filepath.Base(path); base != path {
			if src, rerr := libHeaders.ReadFile("goclib/" + base); rerr == nil {
				inc, perr := p.process(string(src), "<builtin:"+path+">")
				if perr != nil {
					return nil, perr
				}
				if len(inc) > 0 && inc[len(inc)-1].Kind == frontend.TEOF {
					inc = inc[:len(inc)-1]
				}
				return inc, nil
			}
		}
		if angled {
			fmt.Fprintf(os.Stderr, "%s: note: skipping unavailable system header <%s>\n", filename, path)
			return nil, nil
		}
		return nil, err
	}
	src, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read %s: %v", filename, path, err)
	}
	inc, err := p.process(string(src), full)
	if err != nil {
		return nil, err
	}
	// Drop the trailing frontend.TEOF that process() appends; the includer's scan owns
	// its own terminator.
	if len(inc) > 0 && inc[len(inc)-1].Kind == frontend.TEOF {
		inc = inc[:len(inc)-1]
	}
	return inc, nil
}

// doEmbed handles the C23 #embed directive. "#embed <file>" / "#embed "file""
// expands to a comma-separated list of integer constants -- the file's bytes
// (0..255) -- optionally restricted by limit(N) and surrounded by prefix(...)/suffix(...)
// embed parameters, or replaced by if_empty(...) when the file is empty. The
// produced tokens are spliced into the stream wherever an initializer list is
// expected.
func (p *Preprocessor) doEmbed(rest []frontend.Token, filename string) ([]frontend.Token, error) {
	if len(rest) == 0 {
		return nil, fmt.Errorf("%s: #embed needs a filename", filename)
	}
	t := rest[0]
	var path string
	angled := false
	start := 1
	if t.Kind == frontend.TStr {
		path = string(t.Str)
	} else if t.Text == "<" {
		var sb strings.Builder
		for i := 1; i < len(rest) && rest[i].Text != ">"; i++ {
			sb.WriteString(rest[i].Text)
		}
		path = sb.String()
		angled = true
		start = 1
		for start < len(rest) && rest[start].Text != ">" {
			start++
		}
		if start < len(rest) {
			start++ // consume '>'
		}
	} else {
		return nil, fmt.Errorf("%s: malformed #embed", filename)
	}

	// Embed parameters: limit(N), prefix(...), suffix(...), if_empty(...).
	limit := -1
	var prefixToks, suffixToks, ifEmptyToks []frontend.Token
	j := start
	for j < len(rest) {
		par := rest[j]
		if par.Kind == frontend.TIdent {
			switch par.Text {
			case "limit":
				if j+3 < len(rest) && rest[j+1].Text == "(" &&
					rest[j+3].Text == ")" && rest[j+2].Kind == frontend.TNum {
					limit = int(rest[j+2].Num)
					j += 4
					continue
				}
			case "prefix":
				if toks, nj, ok := parseParenList(rest, j); ok {
					prefixToks = toks
					j = nj
					continue
				}
			case "suffix":
				if toks, nj, ok := parseParenList(rest, j); ok {
					suffixToks = toks
					j = nj
					continue
				}
			case "if_empty":
				if toks, nj, ok := parseParenList(rest, j); ok {
					ifEmptyToks = toks
					j = nj
					continue
				}
			}
		}
		j++
	}

	full, err := p.resolveInclude(path, filename, angled)
	if err != nil {
		return nil, fmt.Errorf("%s: #embed cannot find %s: %v", filename, path, err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, fmt.Errorf("%s: #embed cannot read %s: %v", filename, path, err)
	}
	if limit >= 0 && len(data) > limit {
		data = data[:limit]
	}
	if len(data) == 0 && len(ifEmptyToks) > 0 {
		return stripEndCommas(ifEmptyToks), nil
	}

	// The prefix/suffix lists are comma-separated token lists written by the
	// user; trim any stray leading/trailing comma so groups join with a single
	// separator. The byte list is joined by commas between elements only.
	prefixToks = stripEndCommas(prefixToks)
	suffixToks = stripEndCommas(suffixToks)

	var out []frontend.Token
	sep := false
	if len(prefixToks) > 0 {
		out = append(out, prefixToks...)
		sep = true
	}
	for _, b := range data {
		if sep {
			out = append(out, frontend.Token{Kind: frontend.TPunct, Text: ",", Line: t.Line})
		} else {
			sep = true
		}
		out = append(out, frontend.Token{Kind: frontend.TNum, Num: int64(b), Text: strconv.Itoa(int(b)), Line: t.Line})
	}
	if len(suffixToks) > 0 {
		if sep {
			out = append(out, frontend.Token{Kind: frontend.TPunct, Text: ",", Line: t.Line})
		}
		out = append(out, suffixToks...)
	}
	return out, nil
}

// stripEndCommas removes any leading/trailing ',' tokens from a #embed prefix/
// suffix list so the groups join with a single separator.
func stripEndCommas(toks []frontend.Token) []frontend.Token {
	for len(toks) > 0 && toks[0].Kind == frontend.TPunct && toks[0].Text == "," {
		toks = toks[1:]
	}
	for len(toks) > 0 && toks[len(toks)-1].Kind == frontend.TPunct && toks[len(toks)-1].Text == "," {
		toks = toks[:len(toks)-1]
	}
	return toks
}

// parseParenList parses a parameter of the form name(...) beginning at rest[j],
// returning the inner tokens (excluding the parentheses) and the index just past
// the closing ')'. Used by #embed's prefix/suffix/if_empty parameters.
func parseParenList(rest []frontend.Token, j int) ([]frontend.Token, int, bool) {
	if j+1 >= len(rest) || rest[j+1].Text != "(" {
		return nil, j + 1, false
	}
	depth := 0
	var inner []frontend.Token
	k := j + 1 // at '('
	for k+1 < len(rest) {
		k++
		tk := rest[k]
		if tk.Text == "(" {
			depth++
			inner = append(inner, tk)
		} else if tk.Text == ")" {
			if depth == 0 {
				return inner, k + 1, true
			}
			depth--
			inner = append(inner, tk)
		} else {
			inner = append(inner, tk)
		}
	}
	return nil, k, false
}

func (p *Preprocessor) resolveInclude(path, fromFile string, angled bool) (string, error) {
	if p.builtinOnly {
		// The library's own headers live in the built-in library and nowhere
		// else; there is nothing on disk to find.
		return "", fmt.Errorf("include file not found: %s", path)
	}
	var candidates []string
	if !angled {
		candidates = append(candidates, filepath.Join(filepath.Dir(fromFile), path))
	}
	for _, d := range p.searchDirs {
		candidates = append(candidates, filepath.Join(d, path))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("include file not found: %s", path)
}

// headerExists reports whether a #include-style path would resolve, mirroring
// the resolution used by doInclude: disk candidates (via resolveInclude) plus
// the embedded goclib/ headers, matched on the basename for <sys/x.h> spellings.
// It backs the __has_include(x) preprocessor operator.
func (p *Preprocessor) headerExists(path string) bool {
	if _, err := p.resolveInclude(path, filepath.Join(p.baseDir, "_"), false); err == nil {
		return true
	}
	if _, err := libHeaders.ReadFile("goclib/" + path); err == nil {
		return true
	}
	if base := filepath.Base(path); base != path {
		if _, err := libHeaders.ReadFile("goclib/" + base); err == nil {
			return true
		}
	}
	return false
}

// expandAt expands the token at index i (handling macros and predefined
// macros) and returns the resulting tokens plus the next index to read. line
// is the source line of the token currently being processed (the invocation
// site), so __LINE__ resolves to where the macro is used, not where it was
// defined.
func (p *Preprocessor) expandAt(raw []frontend.Token, i int, filename string, line int) ([]frontend.Token, int) {
	t := raw[i]
	if t.Kind == frontend.TIdent {
		switch t.Text {
		case "__FILE__":
			return []frontend.Token{tokStr(p.logicalFileName(filename), t.Line)}, i + 1
		case "__LINE__":
			return []frontend.Token{tokNum(int64(p.logicalLine(line)), t.Line)}, i + 1
		case "__goc__":
			return []frontend.Token{tokNum(1, t.Line)}, i + 1
		case "defined":
			// Pass "defined NAME" / "defined(NAME)" through verbatim: the
			// name operand must NOT be macro-expanded (C standard, #if
			// rules). Without this guard "defined(FOO)" expands to
			// "defined(1)" and cePrimary looks up the macro "1" -- always
			// false -- so every defined() test on a real macro broke.
			out := []frontend.Token{t}
			i++
			if i < len(raw) && raw[i].Text == "(" {
				out = append(out, raw[i])
				i++
				if i < len(raw) {
					out = append(out, raw[i]) // the name, unexpanded
					i++
				}
				if i < len(raw) && raw[i].Text == ")" {
					out = append(out, raw[i])
					i++
				}
			} else if i < len(raw) && raw[i].Kind == frontend.TIdent {
				out = append(out, raw[i])
				i++
			}
			return out, i
		}
		if m, ok := p.macros[t.Text]; ok && !p.inExpand[t.Text] && (p.condEval || p.active()) {
			if !m.IsFunc {
				p.inExpand[t.Text] = true
				e := p.expandTokens(m.Body, filename, line)
				delete(p.inExpand, t.Text)
				return e, i + 1
			}
			// Function-like: needs an adjacent '('.
			if i+1 < len(raw) && raw[i+1].Text == "(" {
				args, next, err := p.readArgs(raw, i+1)
				if err == nil {
					p.inExpand[t.Text] = true
					e := p.expandFunc(m, args, filename, line)
					delete(p.inExpand, t.Text)
					return e, next
				}
			}
		}
	}
	return []frontend.Token{t}, i + 1
}

// expandTokens fully expands a token slice.
func (p *Preprocessor) expandTokens(toks []frontend.Token, filename string, line int) []frontend.Token {
	var out []frontend.Token
	j := 0
	for j < len(toks) {
		e, nj := p.expandAt(toks, j, filename, line)
		out = append(out, e...)
		j = nj
	}
	return out
}

// readArgs collects the comma-separated arguments of a function-like macro
// invocation starting at the '(' at openIdx. Nested parentheses are tracked;
// the top-level comma separates arguments.
func (p *Preprocessor) readArgs(raw []frontend.Token, openIdx int) ([][]frontend.Token, int, error) {
	if openIdx >= len(raw) || raw[openIdx].Text != "(" {
		return nil, openIdx, fmt.Errorf("expected '('")
	}
	args := [][]frontend.Token{}
	depth := 0
	i := openIdx + 1
	n := len(raw)
	cur := []frontend.Token{}
	for i < n {
		t := raw[i]
		switch t.Text {
		case "(":
			depth++
			cur = append(cur, t)
			i++
		case ")":
			if depth == 0 {
				args = append(args, cur)
				return args, i + 1, nil
			}
			depth--
			cur = append(cur, t)
			i++
		case ",":
			if depth == 0 {
				args = append(args, cur)
				cur = []frontend.Token{}
				i++
			} else {
				cur = append(cur, t)
				i++
			}
		default:
			cur = append(cur, t)
			i++
		}
	}
	return nil, i, fmt.Errorf("unterminated macro arguments")
}

// expandFunc performs parameter substitution, '#' stringisation and '##'
// pasting for a function-like macro, then re-expands the result.
func (p *Preprocessor) expandFunc(m *Macro, args [][]frontend.Token, filename string, line int) []frontend.Token {
	// `F()` for a macro with no parameters supplies one empty argument slot,
	// which is the right way round for substitution but the wrong count for
	// the arity check: it is zero arguments, not one.
	if len(m.Params) == 0 && len(args) == 1 && len(args[0]) == 0 {
		args = nil
	}
	np := len(m.Params)
	if m.IsVariadic {
		// __VA_ARGS__ is a parameter that may legitimately receive nothing.
		if len(args) < np-1 {
			p.arityError(m, len(args), np-1, line)
			for len(args) < np-1 {
				args = append(args, []frontend.Token{})
			}
		}
	} else if len(args) != np {
		p.arityError(m, len(args), np, line)
		for len(args) < np {
			args = append(args, []frontend.Token{})
		}
		if len(args) > np {
			args = args[:np]
		}
	}
	if m.IsVariadic {
		vaIdx := np - 1
		// Pad up to np so __VA_ARGS__ is always addressable, even when the
		// macro is invoked with no variadic arguments (it then becomes empty).
		for len(args) < np {
			args = append(args, []frontend.Token{})
		}
		if len(args) > np {
			merged := []frontend.Token{}
			for k := vaIdx; k < len(args); k++ {
				if k > vaIdx {
					merged = append(merged, frontend.Token{Kind: frontend.TPunct, Text: ","})
				}
				merged = append(merged, args[k]...)
			}
			args = append(args[:vaIdx], merged)
		}
	}

	// __VA_ARGS__ is empty (a placemarker) when no variadic argument was given.
	vaNonEmpty := false
	if m.IsVariadic {
		vaNonEmpty = len(args[len(args)-1]) > 0
	}

	out := p.substTokens(m.Body, m, args, filename, line, vaNonEmpty)
	return p.expandTokens(out, filename, line)
}

// arityError records a function-like macro invoked with the wrong number of
// arguments. Only the first one is kept: a single diagnostic is enough to
// point the user at the problem, and later expansions stay well-formed.
func (p *Preprocessor) arityError(m *Macro, got, want, line int) {
	if p.arityErr != nil {
		return
	}
	p.arityErr = fmt.Errorf("macro %q requires %d argument(s), but %d given (line %d)",
		m.Name, want, got, line)
}

// substTokens performs argument substitution, '#' stringisation and '##' pasting
// for a slice of body tokens, returning the substituted tokens WITHOUT the final
// re-expansion (the caller does that). It is used both for the whole macro body
// and for the contents of a __VA_OPT__(...) construct.
func (p *Preprocessor) substTokens(body []frontend.Token, m *Macro, args [][]frontend.Token, filename string, line int, vaNonEmpty bool) []frontend.Token {
	var out []frontend.Token
	i := 0
	n := len(body)
	for i < n {
		bt := body[i]
		// Stringisation: # param
		if bt.Kind == frontend.TPunct && bt.Text == "#" && i+1 < n && isParam(body[i+1], m) {
			idx := paramIndex(body[i+1].Text, m)
			out = append(out, stringize(args[idx], bt.Line)...)
			i += 2
			continue
		}
		// Pasting: a ## b  (b may be a __VA_OPT__ operand)
		if bt.Kind == frontend.TPunct && bt.Text == "##" {
			if len(out) == 0 {
				i++
				continue
			}
			i++
			if i >= n {
				break
			}
			nxt := body[i]
			if m.IsVariadic && isVAOpt(nxt) && i+1 < n && body[i+1].Text == "(" {
				content, next, ok := parseVAOpt(body, i)
				if ok {
					if vaNonEmpty {
						sub := p.substTokens(content, m, args, filename, line, vaNonEmpty)
						if len(sub) > 0 {
							merged := lexOne(out[len(out)-1].Text + tokenSpelling(sub[0]))
							out[len(out)-1] = merged
							out = append(out, sub[1:]...)
						}
						// Empty substitution keeps out[len-1] unchanged
						// (placemarker semantics of __VA_OPT__).
					}
					i = next
					continue
				}
			}
			var nxtToks []frontend.Token
			if isParam(nxt, m) {
				nxtToks = args[paramIndex(nxt.Text, m)]
			} else {
				nxtToks = []frontend.Token{nxt}
			}
			if len(nxtToks) > 0 {
				merged := lexOne(out[len(out)-1].Text + tokenSpelling(nxtToks[0]))
				out[len(out)-1] = merged
				out = append(out, nxtToks[1:]...)
			}
			i++
			continue
		}
		// __VA_OPT__(x): when __VA_ARGS__ is non-empty, expand x (with full
		// substitution); otherwise expand to a placemarker (no tokens).
		if m.IsVariadic && isVAOpt(bt) && i+1 < n && body[i+1].Text == "(" {
			content, next, ok := parseVAOpt(body, i)
			if ok {
				if vaNonEmpty {
					out = append(out, p.substTokens(content, m, args, filename, line, vaNonEmpty)...)
				}
				i = next
				continue
			}
			out = append(out, bt)
			i++
			continue
		}
		if isParam(bt, m) {
			idx := paramIndex(bt.Text, m)
			// An argument used as a '##' operand is not pre-expanded.
			if i+1 < n && body[i+1].Kind == frontend.TPunct && body[i+1].Text == "##" {
				out = append(out, args[idx]...)
			} else {
				out = append(out, p.expandTokens(args[idx], filename, line)...)
			}
			i++
			continue
		}
		out = append(out, bt)
		i++
	}
	return out
}

// isVAOpt reports whether a token is the __VA_OPT__ preprocessing operator.
func isVAOpt(t frontend.Token) bool {
	return (t.Kind == frontend.TIdent || t.Kind == frontend.TKeyword) && t.Text == "__VA_OPT__"
}

// parseVAOpt parses a __VA_OPT__(...) construct beginning at body[i]
// (body[i].Text == "__VA_OPT__"). It returns the inner tokens (excluding the
// parentheses), the index just past the closing ')', and whether parsing
// succeeded.
func parseVAOpt(body []frontend.Token, i int) (content []frontend.Token, nextIdx int, ok bool) {
	if i+1 >= len(body) || body[i+1].Text != "(" {
		return nil, i + 1, false
	}
	depth := 0
	j := i + 1 // index of '('
	k := j + 1
	for k < len(body) {
		t := body[k]
		if t.Text == "(" {
			depth++
			content = append(content, t)
		} else if t.Text == ")" {
			if depth == 0 {
				return content, k + 1, true
			}
			depth--
			content = append(content, t)
		} else {
			content = append(content, t)
		}
		k++
	}
	return nil, k, false
}

func isParam(t frontend.Token, m *Macro) bool {
	return t.Kind == frontend.TIdent && paramIndex(t.Text, m) >= 0
}

func paramIndex(name string, m *Macro) int {
	for i, p := range m.Params {
		if p == name {
			return i
		}
	}
	return -1
}

// stringize turns a token list into a string-literal token (the C '#' operator).
func stringize(toks []frontend.Token, line int) []frontend.Token {
	var sb strings.Builder
	for idx, t := range toks {
		if idx > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(tokenSpelling(t))
	}
	return []frontend.Token{{Kind: frontend.TStr, Str: []byte(sb.String()), Line: line}}
}

// tokenSpelling returns the textual form of a token, used for pasting and
// stringisation.
func tokenSpelling(t frontend.Token) string {
	switch t.Kind {
	case frontend.TStr:
		return "\"" + string(t.Str) + "\""
	case frontend.TNum, frontend.TIdent, frontend.TKeyword:
		return t.Text
	case frontend.TPunct:
		return t.Text
	}
	return t.Text
}

// lexOne re-lexes a piece of text into a single token (used by '##' pasting).
func lexOne(s string) frontend.Token {
	toks, err := frontend.Lex(s)
	if err == nil && len(toks) > 0 && toks[0].Kind != frontend.TEOF {
		return toks[0]
	}
	return frontend.Token{Kind: frontend.TPunct, Text: s}
}

func tokStr(s string, line int) frontend.Token {
	return frontend.Token{Kind: frontend.TStr, Str: []byte(s), Line: line}
}

func tokNum(v int64, line int) frontend.Token {
	return frontend.Token{Kind: frontend.TNum, Num: v, Text: strconv.FormatInt(v, 10), Line: line}
}

// ceVal is the value of a #if / #elif constant expression. C11 6.10.1p4
// evaluates those in intmax_t / uintmax_t, so a value has to carry both its
// bit pattern and which of the two types it is: `#if -1 < 0u` is false
// because -1 converts to uintmax_t and becomes the largest possible value.
type ceVal struct {
	u   uint64 // bit pattern of an intmax_t or uintmax_t
	sgn bool   // true: intmax_t (signed); false: uintmax_t
}

func ceSigned(v int64) ceVal    { return ceVal{u: uint64(v), sgn: true} }
func ceUnsigned(v uint64) ceVal { return ceVal{u: v, sgn: false} }

// i reinterprets the bit pattern as an intmax_t.
func (v ceVal) i() int64 { return int64(v.u) }

// truth reports whether a constant expression selects its branch.
func (v ceVal) truth() bool { return v.u != 0 }

// ceCommon applies the usual arithmetic conversions to a pair of operands and
// returns their bit patterns plus the signedness of the common type. Because
// the bit pattern is stored as-is, converting just means flipping the flag:
// a signed value's uint64 is already its two's complement in 64 bits.
func ceCommon(l, r ceVal) (uint64, uint64, bool) {
	return l.u, r.u, l.sgn && r.sgn
}

// constExpr evaluates a (pre-expanded) integer constant expression used by
// #if / #elif. Undefined identifiers and string constants are 0; character
// literals carry their byte value (the lexer emits them as frontend.TNum).
func (p *Preprocessor) constExpr(toks []frontend.Token) int64 {
	if len(toks) == 0 {
		return 0
	}
	p.ceErr = false // fresh expression: clear any stale error flag
	v, i := p.ceCond(toks, 0)
	if i < len(toks) {
		// Tokens left over mean the expression was only partly understood.
		// Every operator level must be present in the chain below: when one
		// was missing the walk stopped early and quietly took the leftmost
		// operand as the whole condition, so `#if (1 << 3) == 9` came out
		// true. Report it rather than guess.
		p.ceErr = true
	}
	return v.i()
}

// ceCond is the conditional-expression level: `a ? b : c`.
func (p *Preprocessor) ceCond(toks []frontend.Token, i int) (ceVal, int) {
	cond, i := p.ceOr(toks, i)
	if i < len(toks) && toks[i].Text == "?" {
		i++
		thenV, ni := p.ceCond(toks, i)
		i = ni
		if i < len(toks) && toks[i].Text == ":" {
			i++
			elseV, ni := p.ceCond(toks, i)
			i = ni
			if cond.truth() {
				return thenV, i
			}
			return elseV, i
		}
	}
	return cond, i
}

func (p *Preprocessor) ceOr(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceAnd(toks, i)
	for i < len(toks) && toks[i].Text == "||" {
		i++
		right, ni := p.ceAnd(toks, i)
		i = ni
		if left.truth() || right.truth() {
			left = ceSigned(1)
		} else {
			left = ceSigned(0)
		}
	}
	return left, i
}

func (p *Preprocessor) ceAnd(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceBitOr(toks, i)
	for i < len(toks) && toks[i].Text == "&&" {
		i++
		right, ni := p.ceBitOr(toks, i)
		i = ni
		if left.truth() && right.truth() {
			left = ceSigned(1)
		} else {
			left = ceSigned(0)
		}
	}
	return left, i
}

func (p *Preprocessor) ceBitOr(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceBitXor(toks, i)
	for i < len(toks) && toks[i].Text == "|" {
		i++
		right, ni := p.ceBitXor(toks, i)
		i = ni
		_, _, sgn := ceCommon(left, right)
		left = ceVal{u: left.u | right.u, sgn: sgn}
	}
	return left, i
}

func (p *Preprocessor) ceBitXor(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceBitAnd(toks, i)
	for i < len(toks) && toks[i].Text == "^" {
		i++
		right, ni := p.ceBitAnd(toks, i)
		i = ni
		_, _, sgn := ceCommon(left, right)
		left = ceVal{u: left.u ^ right.u, sgn: sgn}
	}
	return left, i
}

func (p *Preprocessor) ceBitAnd(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceEq(toks, i)
	for i < len(toks) && toks[i].Text == "&" {
		i++
		right, ni := p.ceEq(toks, i)
		i = ni
		_, _, sgn := ceCommon(left, right)
		left = ceVal{u: left.u & right.u, sgn: sgn}
	}
	return left, i
}

func (p *Preprocessor) ceEq(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceRel(toks, i)
	for i < len(toks) && (toks[i].Text == "==" || toks[i].Text == "!=") {
		op := toks[i].Text
		i++
		right, ni := p.ceRel(toks, i)
		i = ni
		var r int64
		if op == "==" {
			if left.u == right.u {
				r = 1
			}
		} else {
			if left.u != right.u {
				r = 1
			}
		}
		left = ceSigned(r)
	}
	return left, i
}

func (p *Preprocessor) ceRel(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceShift(toks, i)
	for i < len(toks) && (toks[i].Text == "<" || toks[i].Text == ">" ||
		toks[i].Text == "<=" || toks[i].Text == ">=") {
		op := toks[i].Text
		i++
		right, ni := p.ceShift(toks, i)
		i = ni
		lu, ru, sgn := ceCommon(left, right)
		// A signed operand converts to uintmax_t when the other side is
		// unsigned; that is what makes `#if -1 < 0u` false.
		var r int64
		if sgn {
			switch op {
			case "<":
				if int64(lu) < int64(ru) {
					r = 1
				}
			case ">":
				if int64(lu) > int64(ru) {
					r = 1
				}
			case "<=":
				if int64(lu) <= int64(ru) {
					r = 1
				}
			case ">=":
				if int64(lu) >= int64(ru) {
					r = 1
				}
			}
		} else {
			switch op {
			case "<":
				if lu < ru {
					r = 1
				}
			case ">":
				if lu > ru {
					r = 1
				}
			case "<=":
				if lu <= ru {
					r = 1
				}
			case ">=":
				if lu >= ru {
					r = 1
				}
			}
		}
		left = ceSigned(r)
	}
	return left, i
}

// ceShift is the shift level: `<<` / `>>`. The result keeps the promoted left
// operand's type, and a count outside [0, width) is undefined (C11 6.5.7p3).
func (p *Preprocessor) ceShift(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceAdd(toks, i)
	for i < len(toks) && (toks[i].Text == "<<" || toks[i].Text == ">>") {
		op := toks[i].Text
		i++
		right, ni := p.ceAdd(toks, i)
		i = ni
		n := right.u
		if (right.sgn && right.i() < 0) || n > 63 {
			// Undefined in a constant expression, so a constraint violation:
			// flag it and pick a defined result rather than trapping.
			p.ceErr = true
			n = 0
		}
		switch {
		case op == "<<":
			left = ceVal{u: left.u << n, sgn: left.sgn}
		case left.sgn:
			// An intmax_t shifts arithmetically and keeps its sign.
			left = ceVal{u: uint64(left.i() >> n), sgn: true}
		default:
			left = ceVal{u: left.u >> n, sgn: false}
		}
	}
	return left, i
}

func (p *Preprocessor) ceAdd(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceMul(toks, i)
	for i < len(toks) && (toks[i].Text == "+" || toks[i].Text == "-") {
		op := toks[i].Text
		i++
		right, ni := p.ceMul(toks, i)
		i = ni
		lu, ru, sgn := ceCommon(left, right)
		var u uint64
		if op == "+" {
			u = lu + ru
		} else {
			u = lu - ru
		}
		left = ceVal{u: u, sgn: sgn}
	}
	return left, i
}

func (p *Preprocessor) ceMul(toks []frontend.Token, i int) (ceVal, int) {
	left, i := p.ceUnary(toks, i)
	for i < len(toks) && (toks[i].Text == "*" || toks[i].Text == "/" || toks[i].Text == "%") {
		op := toks[i].Text
		i++
		right, ni := p.ceUnary(toks, i)
		i = ni
		lu, ru, sgn := ceCommon(left, right)
		switch op {
		case "*":
			left = ceVal{u: lu * ru, sgn: sgn}
		case "/":
			if ru == 0 {
				// Division by zero in a #if/#elif constant expression is a
				// constraint violation (C11 6.6p4 / 6.5.5p5); the caller
				// reports it via p.ceErr instead of silently skipping.
				p.ceErr = true
				left = ceVal{u: 0, sgn: sgn}
			} else if sgn {
				left = ceVal{u: uint64(int64(lu) / int64(ru)), sgn: true}
			} else {
				left = ceVal{u: lu / ru, sgn: false}
			}
		case "%":
			if ru == 0 {
				p.ceErr = true
				left = ceVal{u: 0, sgn: sgn}
			} else if sgn {
				left = ceVal{u: uint64(int64(lu) % int64(ru)), sgn: true}
			} else {
				left = ceVal{u: lu % ru, sgn: false}
			}
		}
	}
	return left, i
}

func (p *Preprocessor) ceUnary(toks []frontend.Token, i int) (ceVal, int) {
	if i >= len(toks) {
		return ceSigned(0), i
	}
	t := toks[i]
	switch t.Text {
	case "!":
		v, ni := p.ceUnary(toks, i+1)
		if v.truth() {
			return ceSigned(0), ni
		}
		return ceSigned(1), ni
	case "-":
		v, ni := p.ceUnary(toks, i+1)
		return ceVal{u: -v.u, sgn: v.sgn}, ni
	case "+":
		return p.ceUnary(toks, i+1)
	case "~":
		v, ni := p.ceUnary(toks, i+1)
		return ceVal{u: ^v.u, sgn: v.sgn}, ni
	}
	return p.cePrimary(toks, i)
}

func (p *Preprocessor) cePrimary(toks []frontend.Token, i int) (ceVal, int) {
	if i >= len(toks) {
		return ceSigned(0), i
	}
	t := toks[i]
	switch {
	case t.Kind == frontend.TNum:
		// A u/U suffix makes the constant unsigned, which changes how it
		// combines with signed operands further up the chain.
		if t.IsUnsig {
			return ceUnsigned(uint64(t.Num)), i + 1
		}
		return ceSigned(t.Num), i + 1
	case t.Kind == frontend.TIdent && t.Text == "defined":
		i++
		name := ""
		if i < len(toks) && toks[i].Text == "(" {
			i++
			if i < len(toks) {
				name = toks[i].Text
				i++
			}
			if i < len(toks) && toks[i].Text == ")" {
				i++
			}
		} else if i < len(toks) {
			name = toks[i].Text
			i++
		}
		if _, ok := p.macros[name]; ok {
			return ceSigned(1), i
		}
		// __has_* operators are lexer keywords handled directly in cePrimary,
		// not macros. The standard requires defined(__has_c_attribute) and
		// defined(__has_include) to yield 1 (C23 6.10.10; gcc/clang agree),
		// otherwise the portable guard "#if defined(__has_c_attribute) && ..."
		// always falls through to the #else branch.
		switch name {
		case "__has_c_attribute", "__has_include":
			return ceSigned(1), i
		}
		return ceSigned(0), i
	case t.Text == "__has_include":
		// __has_include(<h>) / __has_include("h"): 1 if the header resolves, else 0.
		i++
		var path string
		if i < len(toks) && toks[i].Text == "(" {
			i++
			if i < len(toks) && toks[i].Kind == frontend.TStr {
				path = string(toks[i].Str)
				i++
			} else if i < len(toks) && toks[i].Text == "<" {
				i++
				var sb strings.Builder
				for i < len(toks) && toks[i].Text != ">" {
					sb.WriteString(toks[i].Text)
					i++
				}
				path = sb.String()
				i++ // consume '>'
			}
			if i < len(toks) && toks[i].Text == ")" {
				i++
			}
		}
		if p.headerExists(path) {
			return ceSigned(1), i
		}
		return ceSigned(0), i
	case t.Text == "__has_c_attribute":
		// __has_c_attribute(name): 202311 if supported, else 0.
		i++
		var name string
		if i < len(toks) && toks[i].Text == "(" {
			i++
			if i < len(toks) && toks[i].Text != ")" {
				name = toks[i].Text
				i++
				if i < len(toks) && toks[i].Text == "::" {
					i++
					if i < len(toks) && toks[i].Text != ")" {
						name = toks[i].Text
						i++
					}
				}
			}
			if i < len(toks) && toks[i].Text == ")" {
				i++
			}
		}
		switch name {
		case "deprecated", "nodiscard", "maybe_unused", "fallthrough",
			"noreturn", "unsequenced", "reproducible", "_Noreturn",
			"alignas", "alignof", "thread_local", "char8_t":
			return ceSigned(202311), i
		default:
			return ceSigned(0), i
		}
	case t.Kind == frontend.TIdent:
		return ceSigned(0), i + 1
	case t.Text == "(":
		v, ni := p.ceCond(toks, i+1)
		if ni < len(toks) && toks[ni].Text == ")" {
			ni++
		}
		return v, ni
	}
	return ceSigned(0), i + 1
}
