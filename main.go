package main

// goc32 builds one TinyOS user program in one step:
//
//   C -> goc front end -> gocl IR text -> native32 asm -> goc32/as -> goc32/ld
//     -> LZ4TOS envelope
//
// Everything after the C front end is in this repository: there is no clang,
// no ld.lld, no libLLVM and no cgo in the path. The result is the same hinted
// LZ4TOS blob `tos build` would place in littlefs.
//
// Examples:
//   goc32 user/hello.c
//   goc32 -target esp32c3 -o build/hello.tos user/hello.c
//   goc32 -target rp2040 user/hello.c
//   goc32 -S user/hello.c          # assembly only, for debugging

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goc/common"
	"goc/frontend"
	"goc32/internal/as"
	"goc32/internal/ld"
	"goc32/internal/lz4tos"
	"goc32/internal/obj"
	"gocl"
	"native32"
)

//go:embed tinyos/goc/usys.h tinyos/goc/syscall_rv32.S tinyos/goc/syscall_arm.S
var shimFS embed.FS

type target struct {
	Name   string
	Arch   string // rv32 | arm
	IRArch string // riscv32 | armel
	CPU    string // cortex-m0plus / cortex-m3 / cortex-m33
}

var targets = map[string]target{
	"rv32":    {Name: "rv32", Arch: "rv32", IRArch: "riscv32"},
	"esp32c3": {Name: "esp32c3", Arch: "rv32", IRArch: "riscv32"},
	"rv32nc":  {Name: "rv32nc", Arch: "rv32", IRArch: "riscv32"},
	"arm":     {Name: "arm", Arch: "arm", IRArch: "armel", CPU: "cortex-m3"},
	"rp2040":  {Name: "rp2040", Arch: "arm", IRArch: "armel", CPU: "cortex-m0plus"},
	"rp2350":  {Name: "rp2350", Arch: "arm", IRArch: "armel", CPU: "cortex-m33"},
}

// asmArch maps a TinyOS target to the instruction set and the encodings its
// core can execute.  The RP2040's Cortex-M0+ has only 16-bit Thumb, so a long
// branch there is an error rather than a silently widened instruction.
func asmArch(t target) as.Arch {
	if t.Arch == "rv32" {
		return as.RV32
	}
	if strings.HasPrefix(t.CPU, "cortex-m0") || strings.HasPrefix(t.CPU, "cortex-m1") {
		return as.Thumb1
	}
	return as.Thumb2
}

type options struct {
	target   string
	tos      string
	out      string
	incs     []string
	defs     []string
	asmOnly  bool
	irOnly   bool
	preOnly  bool
	packOnly bool
	fromIR   bool
	keep     bool
	verbose  bool
	inputs   []string
}

func main() {
	o := options{target: "rv32"}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			i++
			if i >= len(args) {
				fatalf("missing value for %s", a)
			}
			return args[i]
		}
		switch a {
		case "-target":
			o.target = val()
		case "-tos":
			o.tos = val()
		case "-o":
			o.out = val()
		case "-I":
			o.incs = append(o.incs, val())
		case "-D":
			o.defs = append(o.defs, val())
		case "-S":
			o.asmOnly = true
		case "-ir":
			o.irOnly = true
		case "-E":
			o.preOnly = true
		case "-pack":
			o.packOnly = true
		case "-ll":
			o.fromIR = true
		case "-keep":
			o.keep = true
		case "-v":
			o.verbose = true
		case "-h", "-help", "--help":
			usage()
			return
		default:
			if strings.HasPrefix(a, "-") {
				fatalf("unknown option %s", a)
			}
			o.inputs = append(o.inputs, a)
		}
	}
	if err := run(o); err != nil {
		fatalf("%v", err)
	}
}

func usage() {
	fmt.Print(`usage: goc32 [options] file.c|dir ...

  -target NAME   rv32 (default), esp32c3, rv32nc, arm, rp2040, rp2350
  -tos DIR       TinyOS checkout (default: find user/lib.c walking up)
  -o FILE        output .tos / .s / .ll / .i (default: <base>.tos)
  -I DIR         extra include directory (repeatable)
  -D NAME[=VAL]  extra preprocessor define (repeatable)
  -S             emit assembly only
  -ir            emit the gocl IR only
  -E             preprocess only
  -pack          wrap an already linked ELF in LZ4TOS (no compile)
  -ll            treat the input as LLVM IR text and emit assembly
  -keep          keep the temporary build directory
  -v             print the pipeline steps
`)
}

func fatalf(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "goc32: "+f+"\n", a...)
	os.Exit(1)
}

func run(o options) error {
	if len(o.inputs) == 0 {
		usage()
		return fmt.Errorf("no input files")
	}
	t, ok := targets[o.target]
	if !ok {
		return fmt.Errorf("unknown target %q", o.target)
	}
	if o.packOnly {
		if len(o.inputs) != 1 {
			return fmt.Errorf("-pack takes exactly one ELF")
		}
		out := outputPath(o.out, o.inputs[0], ".tos")
		res, err := lz4tos.Pack(o.inputs[0], out)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d -> %d bytes (LZ4TOS, hint %d/%d/%d)\n",
			out, res.RawSize, res.CompSize, res.Hint.Span, res.Hint.StackAsk, res.Hint.HeapAsk)
		return nil
	}
	if o.fromIR {
		if len(o.inputs) != 1 {
			return fmt.Errorf("-ll takes exactly one LLVM IR file")
		}
		raw, err := os.ReadFile(o.inputs[0])
		if err != nil {
			return err
		}
		m, err := native32.ParseModule(string(raw))
		if err != nil {
			return fmt.Errorf("%s: IR parse: %w", o.inputs[0], err)
		}
		var asmText string
		if t.Arch == "arm" {
			asmText, err = native32.EmitARM(m, t.CPU)
		} else {
			asmText, err = native32.EmitRV32(m)
		}
		if err != nil {
			return err
		}
		return writeFile(outputPath(o.out, o.inputs[0], ".s"), []byte(asmText))
	}
	tos, err := findTOS(o.tos, o.inputs[0])
	if err != nil {
		return err
	}
	inputs, err := expandInputs(o.inputs)
	if err != nil {
		return err
	}
	incs := append([]string{}, o.incs...)
	incs = append(incs,
		filepath.Join(tos, "user"),
		filepath.Join(tos, "include"),
		filepath.Join(tos, "kernel"),
		filepath.Join(tos, "arch", t.Arch),
	)
	defs := runtimeDefines(t, o.defs)
	arch := asmArch(t)

	if o.asmOnly || o.irOnly || o.preOnly {
		for _, src := range inputs {
			out := o.out
			if out == "" || isDir(out) {
				ext := ".s"
				if o.irOnly {
					ext = ".ll"
				} else if o.preOnly {
					ext = ".i"
				}
				out = replaceExt(src, ext)
				if isDir(o.out) {
					out = filepath.Join(o.out, filepath.Base(out))
				}
			}
			text, err := compileOne(src, t, incs, defs, o, 0)
			if err != nil {
				return err
			}
			if err := writeFile(out, []byte(text)); err != nil {
				return err
			}
		}
		return nil
	}

	tmp, err := os.MkdirTemp("", "goc32-")
	if err != nil {
		return err
	}
	if o.keep {
		fmt.Printf("temp: %s\n", tmp)
	} else {
		defer os.RemoveAll(tmp)
	}

	shim, err := extractShim(tmp)
	if err != nil {
		return err
	}
	// The shim's usys.h must win over arch/<arch>/usys.h.
	incs = append([]string{shim}, incs...)

	var objs []*obj.Object
	build := func(src string, inline int) error {
		text, err := compileOne(src, t, incs, defs, o, inline)
		if err != nil {
			return err
		}
		if o.keep {
			_ = writeFile(filepath.Join(tmp, replaceExt(filepath.Base(src), ".s")), []byte(text))
		}
		if o.verbose {
			fmt.Printf("  as  %s (%d bytes)\n", src, len(text))
		}
		ob, err := as.Assemble([]byte(text), as.Opt{Arch: arch, File: src})
		if err != nil {
			return err
		}
		objs = append(objs, ob)
		return nil
	}

	// Runtime C: libc and the syscall wrappers.
	for _, src := range []string{
		filepath.Join(tos, "user", "lib.c"),
		filepath.Join(tos, "user", "syscall.c"),
	} {
		if err := build(src, 0); err != nil {
			return err
		}
	}
	// The program's own translation units.  Only C `static` functions reach
	// the inliner (they are `internal` in the IR), so this is safe even when
	// several input files are linked together: a sibling module cannot name a
	// static function from another translation unit.
	programInline := inlineLimit(t)
	for _, src := range inputs {
		if err := build(src, programInline); err != nil {
			return err
		}
	}
	// crt0, the goc syscall entry, and (ARM only) the EABI helpers.
	if err := buildAsm(filepath.Join(tos, "user", "crt0_"+t.Arch+".S"), "", arch, o, &objs); err != nil {
		return err
	}
	if err := buildAsm(filepath.Join(shim, "syscall_"+t.Arch+".S"), "", arch, o, &objs); err != nil {
		return err
	}
	if t.Arch == "arm" {
		if err := build(filepath.Join(tos, "user", "aeabi.c"), 0); err != nil {
			return err
		}
	}

	machine := uint16(ld.EM_RISCV)
	if t.Arch == "arm" {
		machine = ld.EM_ARM
	}
	image, err := ld.Link(objs, ld.Options{Machine: machine, Thumb: t.Arch == "arm", Entry: "_start"})
	if err != nil {
		return err
	}
	elf := filepath.Join(tmp, "prog.elf")
	if err := os.WriteFile(elf, image, 0o644); err != nil {
		return err
	}
	out := outputPath(o.out, inputs[0], ".tos")
	res, err := lz4tos.Pack(elf, out)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d -> %d bytes (LZ4TOS, hint %d/%d/%d)\n",
		out, res.RawSize, res.CompSize, res.Hint.Span, res.Hint.StackAsk, res.Hint.HeapAsk)
	return nil
}

func buildAsm(path, text string, arch as.Arch, o options, objs *[]*obj.Object) error {
	var src []byte
	if text != "" {
		src = []byte(text)
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src = b
	}
	if o.verbose {
		fmt.Printf("  as  %s\n", path)
	}
	ob, err := as.Assemble(src, as.Opt{Arch: arch, File: path})
	if err != nil {
		return err
	}
	*objs = append(*objs, ob)
	return nil
}

// inlineLimit is the instruction budget for single-call-site inlining.  It is
// deliberately small: the stack-machine frame grows when a callee's values are
// merged into the caller, and on ARMv6-M the extra slots cost more than the
// removed call.  The pass is also limited to a single input file, where a
// function cannot be referenced from a sibling translation unit.
func inlineLimit(t target) int {
	if t.Arch == "rv32" {
		return 40
	}
	if t.Arch == "arm" && !strings.HasPrefix(t.CPU, "cortex-m0") && !strings.HasPrefix(t.CPU, "cortex-m1") {
		return 40
	}
	return 0
}

func runtimeDefines(t target, user []string) []string {
	d := []string{
		"__builtin_va_list=va_list",
		"__builtin_va_start=va_start",
		"__builtin_va_arg=va_arg",
		"__builtin_va_end=va_end",
		"__builtin_va_copy=va_copy",
		"__TINYOS__=1",
	}
	if t.Arch == "arm" {
		d = append(d, "__arm__=1")
	} else {
		d = append(d, "__riscv=1")
	}
	return append(d, user...)
}

// compileOne runs the C front end and the native32 back end and returns the
// assembly text. File-scope __asm__ blocks (user/aeabi.c's divmod helpers) are
// lifted out of the token stream and appended verbatim: they are already
// assembly and the assembler will consume them in the same object.
func compileOne(src string, t target, incs, defs []string, o options, inlineLimit int) (string, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	// GCC basic/extended asm is not parsed by the front end. The TinyOS
	// userland uses exactly two shapes: an empty compiler barrier and the
	// illegal-instruction macro. Both are rewritten to a runtime trap helper
	// (or dropped, for the empty barrier) before parsing.
	srcText := "void __goc32_trap(void);\n" + string(raw)
	text := common.InjectDefines(srcText, defs)
	toks, err := common.PreprocessTarget(text, src, false, incs...)
	if err != nil {
		return "", fmt.Errorf("%s: preprocess: %w", src, err)
	}
	toks, fileAsm, err := rewriteAsm(toks)
	if err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	if o.preOnly {
		return common.SerializeTokens(toks), nil
	}
	prog, err := frontend.Parse(toks)
	if err != nil {
		return "", fmt.Errorf("%s: parse: %w", src, err)
	}
	if errs := frontend.CheckUnit(prog); len(errs) > 0 {
		var real []string
		for _, e := range errs {
			if strings.Contains(e.Error(), "program has no main()") {
				continue
			}
			real = append(real, e.Error())
		}
		if len(real) > 0 {
			return "", fmt.Errorf("%s: type errors:\n  %s", src, strings.Join(real, "\n  "))
		}
	}
	ir, err := gocl.TranslateIR(prog, t.IRArch)
	if err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	if o.irOnly {
		return ir, nil
	}
	m, err := native32.ParseModule(ir)
	if err != nil {
		return "", fmt.Errorf("%s: IR parse: %w", src, err)
	}
	if inlineLimit > 0 {
		native32.InlineSmall(m, inlineLimit)
	}
	var asmText string
	if t.Arch == "rv32" {
		asmText, err = native32.EmitRV32(m)
	} else {
		asmText, err = native32.EmitARM(m, t.CPU)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", src, err)
	}
	if len(fileAsm) > 0 {
		asmText += "\n" + strings.Join(fileAsm, "\n") + "\n"
	}
	return asmText, nil
}

func extractShim(dir string) (string, error) {
	shim := filepath.Join(dir, "shim")
	if err := os.MkdirAll(shim, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"usys.h", "syscall_rv32.S", "syscall_arm.S"} {
		data, err := shimFS.ReadFile("tinyos/goc/" + name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(shim, name), data, 0o644); err != nil {
			return "", err
		}
	}
	return shim, nil
}

func findTOS(explicit, firstInput string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("TOS_ROOT"); env != "" {
		candidates = append(candidates, env)
	}
	if firstInput != "" {
		if abs, err := filepath.Abs(firstInput); err == nil {
			candidates = append(candidates, abs)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	for _, c := range candidates {
		dir := c
		if st, err := os.Stat(dir); err == nil && !st.IsDir() {
			dir = filepath.Dir(dir)
		}
		for {
			if isTOS(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("cannot find the TinyOS checkout (pass -tos DIR; it needs user/lib.c)")
}

func isTOS(dir string) bool {
	need := []string{
		filepath.Join(dir, "user", "lib.c"),
		filepath.Join(dir, "user", "user.ld"),
		filepath.Join(dir, "include", "tos.h"),
		filepath.Join(dir, "kernel"),
	}
	for _, p := range need {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

func expandInputs(args []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		abs, _ := filepath.Abs(p)
		if !seen[abs] {
			seen[abs] = true
			out = append(out, p)
		}
	}
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			matches, err := filepath.Glob(filepath.Join(a, "*.c"))
			if err != nil {
				return nil, err
			}
			sort.Strings(matches)
			for _, m := range matches {
				add(m)
			}
			continue
		}
		add(a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no C sources found")
	}
	return out, nil
}

func outputPath(explicit, firstInput, ext string) string {
	if explicit != "" {
		if isDir(explicit) {
			return filepath.Join(explicit, replaceExt(filepath.Base(firstInput), ext))
		}
		return explicit
	}
	return replaceExt(firstInput, ext)
}

func replaceExt(p, ext string) string {
	return strings.TrimSuffix(p, filepath.Ext(p)) + ext
}

func isDir(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	if strings.HasSuffix(p, ".ll") {
		fmt.Printf("IR written to %s\n", p)
	} else if strings.HasSuffix(p, ".s") {
		fmt.Printf("assembly written to %s\n", p)
	}
	return nil
}

// rewriteAsm replaces GCC __asm__/asm constructs with a call to the runtime
// trap helper, or removes an empty barrier. A file-scope __asm__ block is
// returned separately and assembled as-is. It works on the preprocessed token
// stream so macros such as TOS_ILLEGAL_INSN() are already expanded.
func rewriteAsm(toks []frontend.Token) ([]frontend.Token, []string, error) {
	var out []frontend.Token
	var fileAsm []string
	depth := 0
	for i := 0; i < len(toks); i++ {
		tk := toks[i]
		if tk.Text == "{" {
			depth++
			out = append(out, tk)
			continue
		}
		if tk.Text == "}" {
			if depth > 0 {
				depth--
			}
			out = append(out, tk)
			continue
		}
		if !isAsmToken(tk) {
			out = append(out, tk)
			continue
		}
		j := i + 1
		volatileSeen := false
		for j < len(toks) && (toks[j].Text == "volatile" || toks[j].Text == "__volatile__") {
			volatileSeen = true
			j++
		}
		if j >= len(toks) || toks[j].Text != "(" {
			out = append(out, tk)
			continue
		}
		open := j
		d := 0
		close := -1
		var body strings.Builder
		for k := open; k < len(toks); k++ {
			switch toks[k].Text {
			case "(":
				d++
			case ")":
				d--
				if d == 0 {
					close = k
				}
			}
			if d == 1 && toks[k].Kind == frontend.TStr {
				body.Write(toks[k].Str)
			}
			if close >= 0 {
				break
			}
		}
		if close < 0 {
			return nil, nil, fmt.Errorf("unterminated inline asm")
		}
		if depth == 0 {
			// A file-scope asm block is a piece of an assembly file written
			// inside a C file.  Hand it to the assembler instead of trying to
			// parse it as C.
			fileAsm = append(fileAsm, body.String())
			i = close
			if i+1 < len(toks) && toks[i+1].Text == ";" {
				i++
			}
			continue
		}
		// register int r0 __asm__("r0") = ... — a register-binding
		// declarator, not an asm statement. The back end treats register as a
		// no-op, so drop the binding and let normal allocation happen.
		if !volatileSeen && len(out) > 0 && out[len(out)-1].Kind == frontend.TIdent {
			after := ""
			if close+1 < len(toks) {
				after = toks[close+1].Text
			}
			switch after {
			case "=", ",", ";", "[":
				i = close
				continue
			}
		}
		asmBody := strings.TrimSpace(body.String())
		if asmBody == "" {
			// An empty asm is a compiler barrier; the stack-machine back ends
			// do not optimise, so dropping it is equivalent.
			i = close
			continue
		}
		if !isTrapAsm(asmBody) {
			return nil, nil, fmt.Errorf("unsupported inline asm %q", asmBody)
		}
		line := tk.Line
		out = append(out,
			frontend.Token{Kind: frontend.TIdent, Text: "__goc32_trap", Line: line},
			frontend.Token{Kind: frontend.TPunct, Text: "(", Line: line},
			frontend.Token{Kind: frontend.TPunct, Text: ")", Line: line},
		)
		i = close
	}
	return out, fileAsm, nil
}

func isAsmToken(t frontend.Token) bool {
	if t.Kind != frontend.TIdent && t.Kind != frontend.TKeyword {
		return false
	}
	return t.Text == "__asm__" || t.Text == "__asm" || t.Text == "asm"
}

func isTrapAsm(s string) bool {
	return strings.Contains(s, "udf") ||
		strings.Contains(s, "ud2") ||
		strings.Contains(s, ".word 0") ||
		strings.Contains(s, "unimp") ||
		strings.Contains(s, "ebreak")
}
