package main

// goc32 builds one TinyOS user program in one step:
//
//   C -> goc front end -> gocl IR text -> native32 asm -> clang -> ld.lld
//     -> LZ4TOS envelope
//
// The result is the same hinted LZ4TOS blob `tos build` would place in
// littlefs. No libLLVM.dll and no cgo are involved; clang/ld.lld are used only
// as the assembler and linker for this milestone.
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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"goc/common"
	"goc/frontend"
	"goc32/internal/lz4tos"
	"gocl"
	"native32"
)

//go:embed tinyos/goc/usys.h tinyos/goc/syscall_rv32.S tinyos/goc/syscall_arm.S
var shimFS embed.FS

type target struct {
	Name      string
	Arch      string // rv32 | arm
	IRArch    string // riscv32 | armel
	Triple    string
	CPU       string
	ArchFlags []string
	Emulation string
	TextReloc bool
}

var targets = map[string]target{
	"rv32": {
		Name: "rv32", Arch: "rv32", IRArch: "riscv32",
		Triple:    "riscv32-unknown-elf",
		ArchFlags: []string{"-march=rv32imc", "-mabi=ilp32"},
		Emulation: "elf32lriscv",
	},
	"esp32c3": {
		Name: "esp32c3", Arch: "rv32", IRArch: "riscv32",
		Triple:    "riscv32-unknown-elf",
		ArchFlags: []string{"-march=rv32imc", "-mabi=ilp32"},
		Emulation: "elf32lriscv",
	},
	"rv32nc": {
		Name: "rv32nc", Arch: "rv32", IRArch: "riscv32",
		Triple:    "riscv32-unknown-elf",
		ArchFlags: []string{"-march=rv32im", "-mabi=ilp32"},
		Emulation: "elf32lriscv",
	},
	"arm": {
		Name: "arm", Arch: "arm", IRArch: "armel",
		Triple: "thumbv7m-none-eabi", CPU: "cortex-m3",
		ArchFlags: []string{"-mcpu=cortex-m3", "-mfloat-abi=soft", "-mthumb"},
		Emulation: "armelf", TextReloc: true,
	},
	"rp2040": {
		Name: "rp2040", Arch: "arm", IRArch: "armel",
		Triple: "thumbv6m-none-eabi", CPU: "cortex-m0plus",
		ArchFlags: []string{"-mcpu=cortex-m0plus", "-mfloat-abi=soft", "-mthumb"},
		Emulation: "armelf", TextReloc: true,
	},
	"rp2350": {
		Name: "rp2350", Arch: "arm", IRArch: "armel",
		Triple: "thumbv8m.main-none-eabi", CPU: "cortex-m33",
		ArchFlags: []string{"-mcpu=cortex-m33", "-mfloat-abi=soft", "-mthumb"},
		Emulation: "armelf", TextReloc: true,
	},
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
	cc       string
	ld       string
	inputs   []string
}

func main() {
	o := options{target: "rv32", cc: "clang", ld: "ld.lld"}
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
		case "-cc":
			o.cc = val()
		case "-ld":
			o.ld = val()
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
  -cc PATH       clang path (default: clang)
  -ld PATH       ld.lld path (default: ld.lld)
  -v             print every external command
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
		var asm string
		if t.Arch == "arm" {
			asm, err = native32.EmitARM(m, t.CPU)
		} else {
			asm, err = native32.EmitRV32(m)
		}
		if err != nil {
			return err
		}
		return writeFile(outputPath(o.out, o.inputs[0], ".s"), []byte(asm))
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
			if err := compileOne(src, out, t, incs, defs, o); err != nil {
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

	var objs []string
	addObj := func(p string) { objs = append(objs, p) }

	// Runtime C: libc and the syscall wrappers.
	for _, src := range []string{
		filepath.Join(tos, "user", "lib.c"),
		filepath.Join(tos, "user", "syscall.c"),
	} {
		obj := filepath.Join(tmp, replaceExt(filepath.Base(src), ".o"))
		if err := buildObject(src, obj, t, incs, defs, o, shim); err != nil {
			return err
		}
		addObj(obj)
	}
	// The program's own translation units.
	for _, src := range inputs {
		obj := filepath.Join(tmp, replaceExt(filepath.Base(src), ".o"))
		// Two inputs with the same base name would collide; make it unique.
		if _, err := os.Stat(obj); err == nil {
			obj = filepath.Join(tmp, fmt.Sprintf("%d_%s", len(objs), replaceExt(filepath.Base(src), ".o")))
		}
		if err := buildObject(src, obj, t, incs, defs, o, shim); err != nil {
			return err
		}
		addObj(obj)
	}
	// crt0, the goc syscall entry, and (ARM only) the EABI helpers.
	crt0 := filepath.Join(tos, "user", "crt0_"+t.Arch+".S")
	crt0Obj := filepath.Join(tmp, "crt0.o")
	if err := assemble(crt0, crt0Obj, t, o); err != nil {
		return err
	}
	objs = append([]string{crt0Obj}, objs...)

	sysObj := filepath.Join(tmp, "gocsys.o")
	if err := assemble(filepath.Join(shim, "syscall_"+t.Arch+".S"), sysObj, t, o); err != nil {
		return err
	}
	addObj(sysObj)

	if t.Arch == "arm" {
		aeabi := filepath.Join(tos, "user", "aeabi.c")
		aeabiObj := filepath.Join(tmp, "aeabi.o")
		if err := compileHost(aeabi, aeabiObj, t, incs, o); err != nil {
			return err
		}
		addObj(aeabiObj)
	}

	elf := filepath.Join(tmp, "prog.elf")
	if err := link(objs, elf, t, tos, o); err != nil {
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

func compileOne(src, out string, t target, incs, defs []string, o options) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// GCC basic/extended asm is not parsed by the front end. The TinyOS
	// userland uses exactly two shapes: an empty compiler barrier and the
	// illegal-instruction macro. Both are rewritten to a runtime trap helper
	// (or dropped, for the empty barrier) before parsing.
	srcText := "void __goc32_trap(void);\n" + string(raw)
	text := common.InjectDefines(srcText, defs)
	toks, err := common.PreprocessTarget(text, src, false, incs...)
	if err != nil {
		return fmt.Errorf("%s: preprocess: %w", src, err)
	}
	toks, err = rewriteAsm(toks)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if o.preOnly {
		return writeFile(out, []byte(common.SerializeTokens(toks)))
	}
	prog, err := frontend.Parse(toks)
	if err != nil {
		return fmt.Errorf("%s: parse: %w", src, err)
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
			return fmt.Errorf("%s: type errors:\n  %s", src, strings.Join(real, "\n  "))
		}
	}
	ir, err := gocl.TranslateIR(prog, t.IRArch)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if o.irOnly {
		return writeFile(out, []byte(ir))
	}
	m, err := native32.ParseModule(ir)
	if err != nil {
		return fmt.Errorf("%s: IR parse: %w", src, err)
	}
	var asm string
	if t.Arch == "rv32" {
		asm, err = native32.EmitRV32(m)
	} else {
		asm, err = native32.EmitARM(m, t.CPU)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	return writeFile(out, []byte(asm))
}

func buildObject(src, obj string, t target, incs, defs []string, o options, shim string) error {
	asm := obj[:len(obj)-2] + ".s"
	if err := compileOne(src, asm, t, incs, defs, o); err != nil {
		return err
	}
	return assemble(asm, obj, t, o)
}

func assemble(src, obj string, t target, o options) error {
	args := []string{"--target=" + t.Triple}
	args = append(args, t.ArchFlags...)
	args = append(args, "-c", src, "-o", obj)
	return runCmd(o.cc, args, o.verbose)
}

func compileHost(src, obj string, t target, incs []string, o options) error {
	args := []string{"--target=" + t.Triple}
	args = append(args, t.ArchFlags...)
	for _, d := range incs {
		args = append(args, "-I", d)
	}
	args = append(args, "-c", src, "-o", obj)
	return runCmd(o.cc, args, o.verbose)
}

func link(objs []string, out string, t target, tos string, o options) error {
	args := []string{"-m", t.Emulation,
		"-T", filepath.Join(tos, "user", "user.ld"),
		"-nostdlib", "-pie", "--gc-sections",
		"-z", "max-page-size=16", "-z", "now", "-s",
		"-o", out,
	}
	if t.TextReloc {
		args = append(args, "-z", "notext")
	}
	args = append(args, objs...)
	return runCmd(o.ld, args, o.verbose)
}

func runCmd(name string, args []string, verbose bool) error {
	if verbose {
		fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	}
	cmd := exec.Command(name, args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %v\n%s", name, err, strings.TrimSpace(out.String()))
	}
	return nil
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
// trap helper, or removes an empty barrier. It works on the preprocessed token
// stream so macros such as TOS_ILLEGAL_INSN() are already expanded.
func rewriteAsm(toks []frontend.Token) ([]frontend.Token, error) {
	var out []frontend.Token
	for i := 0; i < len(toks); i++ {
		tk := toks[i]
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
		depth := 0
		close := -1
		var asmText []byte
		seenStr := false
		for k := open; k < len(toks); k++ {
			switch toks[k].Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					close = k
				}
			}
			if depth == 1 && !seenStr && toks[k].Kind == frontend.TStr {
				asmText = toks[k].Str
				seenStr = true
			}
			if close >= 0 {
				break
			}
		}
		if close < 0 {
			return nil, fmt.Errorf("unterminated inline asm")
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
		body := strings.TrimSpace(string(asmText))
		if body == "" {
			// An empty asm is a compiler barrier; the stack-machine back ends
			// do not optimise, so dropping it is equivalent.
			i = close
			continue
		}
		if !isTrapAsm(body) {
			return nil, fmt.Errorf("unsupported inline asm %q", body)
		}
		line := tk.Line
		out = append(out,
			frontend.Token{Kind: frontend.TIdent, Text: "__goc32_trap", Line: line},
			frontend.Token{Kind: frontend.TPunct, Text: "(", Line: line},
			frontend.Token{Kind: frontend.TPunct, Text: ")", Line: line},
		)
		i = close
	}
	return out, nil
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
