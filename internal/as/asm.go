// Package as is goc32's own assembler.  It reads the small GNU-as subset
// native32 emits (plus the three hand-written .S files in the TinyOS tree) and
// produces an obj.Object.  There is no ELF, no clang, and no external
// assembler in the path.
//
// The subset is deliberately closed: RV32IM + the pseudo-instructions in
// native32/rv32.go, and Thumb-1/Thumb-2 as emitted by native32/arm.go.  An
// instruction the emitters do not produce is a bug here, not a feature
// request - the error says so with a file:line.
package as

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"goc32/internal/obj"
)

// Arch selects the instruction set and the encodings.
type Arch int

const (
	RV32   Arch = iota
	Thumb1      // ARMv6-M: 16-bit Thumb only
	Thumb2      // ARMv7-M / ARMv8-M: Thumb plus a handful of 32-bit forms
)

// Opt is the assembler's input.
type Opt struct {
	Arch Arch
	File string
}

// Assemble turns assembly text into one object.  It runs the usual two passes
// and repeats them while a long branch has to grow from 16 to 32 bits; each
// pass is cheap and the number of widenings is tiny.
func Assemble(src []byte, opt Opt) (*obj.Object, error) {
	lines, err := prepare(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opt.File, err)
	}
	widen := map[int]bool{}
	for attempt := 0; attempt < 12; attempt++ {
		a := &asm{arch: opt.Arch, file: opt.File, lines: lines, widen: widen}
		if err := a.pass(true); err != nil {
			return nil, err
		}
		if err := a.pass(false); err != nil {
			return nil, err
		}
		if len(a.needWiden) == 0 {
			return a.obj, nil
		}
		if opt.Arch == Thumb1 {
			return nil, fmt.Errorf("%s: branch out of Thumb-1 range (the M0+ has no 32-bit B): line %d",
				opt.File, a.needWiden[0])
		}
		for _, n := range a.needWiden {
			widen[n] = true
		}
	}
	return nil, fmt.Errorf("%s: branch relaxation did not converge", opt.File)
}

type srcLine struct {
	n     int    // source line, 1-based
	label string // label defined on this line, if any
	text  string // instruction or directive with the label removed
}

var (
	labelRe   = regexp.MustCompile(`^([A-Za-z_.$][A-Za-z0-9_.$]*):\s*(.*)$`)
	numDefRe  = regexp.MustCompile(`^\s*([0-9]+):`)
	numRefRe  = regexp.MustCompile(`\b([0-9]+)([fb])\b`)
	directive = regexp.MustCompile(`^\.[A-Za-z]`)
)

// prepare strips comments, renames numeric labels (1:, 1b, 1f), and splits
// labels off their lines.  Numeric labels are local to the file and only a
// handful of them exist (soft-float's tie comparisons), so renaming them up
// front keeps the rest of the assembler free of a special addressing mode.
func prepare(src string) ([]srcLine, error) {
	raw := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	// First pass: assign every numeric label a unique name, remembering the
	// line it appears on.
	names := map[int]string{}
	defs := map[string][]int{}
	for i, line := range raw {
		if m := numDefRe.FindStringSubmatch(line); m != nil {
			name := fmt.Sprintf(".Lnum%s_%d", m[1], i)
			names[i] = name
			defs[m[1]] = append(defs[m[1]], i)
		}
	}

	var out []srcLine
	inBlock := false
	for i, rawLine := range raw {
		line := rawLine
		// Block comments, including the multi-line ones in crt0*.S.
		for {
			if inBlock {
				j := strings.Index(line, "*/")
				if j < 0 {
					line = ""
					break
				}
				line = line[j+2:]
				inBlock = false
				continue
			}
			j := strings.Index(line, "/*")
			if j < 0 {
				break
			}
			k := strings.Index(line[j:], "*/")
			if k < 0 {
				line = line[:j]
				inBlock = true
				break
			}
			line = line[:j] + line[j+k+2:]
		}
		if j := strings.Index(line, "//"); j >= 0 {
			line = line[:j]
		}
		line = strings.TrimRight(line, " \t")

		if i < len(raw) && names[i] != "" {
			// Replace the numeric definition with its unique name.
			line = strings.Replace(line, strings.TrimSpace(strings.SplitN(line, ":", 2)[0])+":", names[i]+":", 1)
		}
		// Replace 1b/1f references.  A `b` suffix means the closest previous
		// definition, `f` the closest following one; both are resolved against
		// the line the reference is on.
		line = numRefRe.ReplaceAllStringFunc(line, func(s string) string {
			m := numRefRe.FindStringSubmatch(s)
			digit := m[1]
			list := defs[digit]
			if len(list) == 0 {
				return s
			}
			if m[2] == "b" {
				best := -1
				for _, d := range list {
					if d <= i {
						best = d
					}
				}
				if best >= 0 {
					return names[best]
				}
				return s
			}
			for _, d := range list {
				if d > i {
					return names[d]
				}
			}
			return s
		})

		// ARM's comment character.  Directives keep their @progbits / %note
		// suffixes, so only strip it from instruction lines.
		trimmed := strings.TrimSpace(line)
		label := ""
		text := trimmed

		// A label may be followed by an instruction on the same line.  Split
		// it off and recommit the remainder.  Directives never contain a ':'
		// in this subset, so the colon is what tells them apart.
		for {
			if text == "" {
				break
			}
			m := labelRe.FindStringSubmatch(text)
			if m == nil {
				break
			}
			label = m[1]
			text = strings.TrimSpace(m[2])
			break
		}
		if label == "" && !strings.HasPrefix(text, ".") {
			if j := strings.Index(text, "@"); j >= 0 {
				text = strings.TrimSpace(text[:j])
			}
			// RISC-V assembly uses # for comments; ARM immediates use #immm
			// directly, so only strip when # starts a comment word.
			if j := strings.Index(text, "# "); j >= 0 {
				text = strings.TrimSpace(text[:j])
			}
		}
		out = append(out, srcLine{n: i + 1, label: label, text: text})
	}
	return out, nil
}

type asm struct {
	arch  Arch
	file  string
	lines []srcLine

	obj  *obj.Object
	cur  *obj.Sec
	off  int
	offs map[*obj.Sec]int

	global    map[string]bool
	funcs     map[string]bool
	thumbNext bool
	emit      bool

	widen     map[int]bool
	needWiden []int
	line      int
}

// encodeInsn dispatches to the instruction set selected at startup.
func (a *asm) encodeInsn(text string, off int, sizeOnly bool) ([]byte, bool, error) {
	if a.arch == RV32 {
		return a.rv32Insn(text, off, sizeOnly)
	}
	return a.thumbInsn(text, off, sizeOnly)
}

func (a *asm) errf(format string, args ...interface{}) error {
	return fmt.Errorf("%s:%d: %s", a.file, a.line, fmt.Sprintf(format, args...))
}

func (a *asm) pass(define bool) error {
	if define {
		a.obj = obj.New(a.file)
		a.global = map[string]bool{}
		a.funcs = map[string]bool{}
	} else {
		// Keep the symbols and sections from the sizing pass; only their
		// contents and relocations are produced now.
		for _, s := range a.obj.Secs {
			s.Data = nil
			s.Relocs = nil
		}
	}
	a.offs = map[*obj.Sec]int{}
	a.emit = !define
	a.thumbNext = false
	a.cur = nil
	a.off = 0

	for _, l := range a.lines {
		a.line = l.n
		if l.label != "" && define {
			if err := a.define(l.label); err != nil {
				return err
			}
		}
		if l.label != "" && !define {
			// Keep track of which symbol a .thumb_func applies to in this pass
			// as well: nothing is defined here, but the flag must not leak into
			// the next label's attributes.
		}
		text := strings.TrimSpace(l.text)
		if text == "" {
			continue
		}
		if directive.MatchString(text) {
			if err := a.doDirective(text, define); err != nil {
				return err
			}
			continue
		}
		if a.cur == nil {
			return a.errf("instruction outside any section: %s", text)
		}
		data, widen, err := a.encodeInsn(text, a.off, define)
		if err != nil {
			return err
		}
		if widen && !a.widen[a.line] {
			a.needWiden = append(a.needWiden, a.line)
		}
		if !define {
			a.put(data)
		}
		a.advance(len(data))
	}
	if a.cur != nil {
		a.finishSec()
	}
	return nil
}

func (a *asm) define(name string) error {
	if a.cur == nil {
		return a.errf("label %s outside any section", name)
	}
	s := a.obj.Sym(name)
	if s.Sec != nil {
		return a.errf("duplicate label %s", name)
	}
	fn := a.funcs[name]
	if a.arch != RV32 && a.thumbNext {
		fn = true
	}
	s.Sec = a.cur
	s.Value = a.off
	s.Global = a.global[name]
	s.Func = fn
	a.thumbNext = false
	return nil
}

func (a *asm) setSec(name string, flags, typ uint32, align int) {
	for _, s := range a.obj.Secs {
		if s.Name == name {
			a.switchSec(s)
			return
		}
	}
	a.switchSec(a.obj.Sec(name, flags, typ, align))
}

func (a *asm) switchSec(s *obj.Sec) {
	if a.cur != nil {
		a.finishSec()
	}
	a.cur = s
	if a.emit && s.Type != obj.SHT_NOBITS {
		a.off = len(s.Data)
	} else {
		a.off = a.offs[s]
	}
	if a.emit && s.Type != obj.SHT_NOBITS && len(s.Data) < a.off {
		s.Data = append(s.Data, make([]byte, a.off-len(s.Data))...)
	}
}

func (a *asm) finishSec() {
	if a.cur == nil {
		return
	}
	if a.emit && a.cur.Type != obj.SHT_NOBITS {
		a.off = len(a.cur.Data)
	}
	a.offs[a.cur] = a.off
	if a.off > a.cur.Size {
		a.cur.Size = a.off
	}
}

func (a *asm) advance(n int) { a.off += n }

func (a *asm) put(b []byte) {
	if a.cur == nil || len(b) == 0 {
		return
	}
	if a.cur.Type == obj.SHT_NOBITS {
		return
	}
	if len(a.cur.Data) < a.off {
		a.cur.Data = append(a.cur.Data, make([]byte, a.off-len(a.cur.Data))...)
	}
	a.cur.Data = append(a.cur.Data, b...)
}

func (a *asm) addReloc(k obj.RelKind, off int, sym string, add int64) {
	if !a.emit || a.cur == nil {
		return
	}
	a.cur.Relocs = append(a.cur.Relocs, obj.Reloc{Off: off, Kind: k, Sym: sym, Addend: add})
}

// doDirective handles the directives the emitters and the hand-written .S
// files use.  Unsupported ones are an error rather than being ignored: a
// silently dropped .section would put code in the wrong place, and a silently
// dropped .zero would corrupt every following offset.
func (a *asm) doDirective(text string, define bool) error {
	fields := strings.Fields(text)
	d := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(text, d))

	switch d {
	case ".text":
		a.setSec(".text", obj.SHF_ALLOC|obj.SHF_EXECINSTR, obj.SHT_PROGBITS, 4)
	case ".data":
		a.setSec(".data", obj.SHF_ALLOC|obj.SHF_WRITE, obj.SHT_PROGBITS, 4)
	case ".bss":
		a.setSec(".bss", obj.SHF_ALLOC|obj.SHF_WRITE, obj.SHT_NOBITS, 4)
	case ".section":
		name, rest2 := cutField(rest, ',')
		name = strings.TrimSpace(name)
		if name == "" {
			return a.errf("bad .section")
		}
		flags := uint32(0)
		typ := uint32(obj.SHT_PROGBITS)
		fl, rest3 := cutField(rest2, ',')
		fl = strings.Trim(strings.TrimSpace(fl), `"`)
		if strings.Contains(fl, "a") {
			flags |= obj.SHF_ALLOC
		}
		if strings.Contains(fl, "w") {
			flags |= obj.SHF_WRITE
		}
		if strings.Contains(fl, "x") {
			flags |= obj.SHF_EXECINSTR
		}
		t := strings.TrimSpace(rest3)
		t = strings.TrimPrefix(t, "%")
		t = strings.TrimPrefix(t, "@")
		switch t {
		case "nobits":
			typ = obj.SHT_NOBITS
		case "note":
			typ = obj.SHT_NOTE
		}
		align := 1
		if flags&obj.SHF_EXECINSTR != 0 {
			align = 4
		}
		a.setSec(name, flags, typ, align)
	case ".balign", ".p2align", ".align":
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return a.errf("bad %s operand %q", d, rest)
		}
		if d == ".p2align" {
			n = 1 << n
		}
		if n < 1 {
			n = 1
		}
		pad := (n - a.off%n) % n
		if a.cur != nil && n > a.cur.Align {
			a.cur.Align = n
		}
		if a.emit {
			a.put(make([]byte, pad))
		}
		a.advance(pad)
	case ".globl", ".global":
		for _, name := range strings.Fields(strings.ReplaceAll(rest, ",", " ")) {
			a.global[name] = true
		}
	case ".type":
		name, tail := cutField(rest, ',')
		name = strings.TrimSpace(name)
		if strings.Contains(tail, "function") {
			a.funcs[name] = true
		}
	case ".thumb_func":
		a.thumbNext = true
	case ".size", ".syntax", ".thumb", ".arm", ".ltorg", ".cpu", ".arch",
		".fpu", ".eabi_attribute", ".file", ".ident", ".code":
		// No effect on the bytes this assembler emits.
	case ".byte", ".hword", ".short", ".word", ".long", ".zero", ".space":
		vals := strings.Split(rest, ",")
		for _, v := range vals {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if d == ".zero" || d == ".space" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return a.errf("bad %s operand %q", d, v)
				}
				if a.emit {
					a.put(make([]byte, n))
				}
				a.advance(n)
				continue
			}
			e, err := parseExpr(v)
			if err != nil {
				return a.errf("%s: %v", d, err)
			}
			size := 4
			switch d {
			case ".byte":
				size = 1
			case ".hword", ".short":
				size = 2
			}
			if e.hasSym {
				if size != 4 {
					return a.errf("%s with a symbol needs four bytes", d)
				}
				a.addReloc(obj.Abs32, a.off, e.sym, e.add)
				if a.emit {
					a.put(u32(uint32(e.add)))
				}
			} else if a.emit {
				switch size {
				case 1:
					a.put([]byte{byte(e.val)})
				case 2:
					a.put([]byte{byte(e.val), byte(e.val >> 8)})
				default:
					a.put(u32(uint32(e.val)))
				}
			}
			a.advance(size)
		}
	default:
		return a.errf("unsupported directive %s", d)
	}
	return nil
}

func cutField(s string, sep byte) (string, string) {
	i := strings.IndexByte(s, sep)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// expr is a .word operand: a symbol with an optional addend, or a number.
type expr struct {
	sym    string
	add    int64
	hasSym bool
	val    uint64
}

func parseExpr(s string) (expr, error) {
	var e expr
	s = strings.TrimSpace(s)
	if s == "" {
		return e, fmt.Errorf("empty expression")
	}
	// Find a +/- that is not the leading sign.
	cut := -1
	for i := 1; i < len(s); i++ {
		if s[i] == '+' || s[i] == '-' {
			cut = i
			break
		}
	}
	name := s
	add := ""
	if cut >= 0 {
		name, add = s[:cut], s[cut:]
	}
	if name[0] >= '0' && name[0] <= '9' || name[0] == '-' && len(name) > 1 {
		v, err := strconv.ParseInt(name, 0, 64)
		if err != nil {
			u, uerr := strconv.ParseUint(strings.TrimPrefix(name, "+"), 0, 64)
			if uerr != nil {
				return e, fmt.Errorf("bad number %q", name)
			}
			e.val = u
		} else {
			e.val = uint64(v)
		}
		if add != "" {
			n, err := strconv.ParseInt(add, 0, 64)
			if err != nil {
				return e, fmt.Errorf("bad addend %q", add)
			}
			e.val = uint64(int64(e.val) + n)
		}
		return e, nil
	}
	if !isSymName(name) {
		return e, fmt.Errorf("bad expression %q", s)
	}
	e.hasSym = true
	e.sym = name
	if add != "" {
		n, err := strconv.ParseInt(add, 0, 64)
		if err != nil {
			return e, fmt.Errorf("bad addend %q", add)
		}
		e.add = n
	}
	return e, nil
}

func isSymName(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '.' || c == '$') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c = s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == '$') {
			return false
		}
	}
	return true
}

func u16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

func u32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

// splitOps splits an operand list on commas that are not inside [] or {}.
func splitOps(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// lookup resolves a label for an instruction.  In the sizing pass there are no
// targets yet, so callers get same=true and a zero address and must not range
// check.
func (a *asm) lookup(name string) (addr int, same, ok bool) {
	if !a.emit {
		return 0, true, true
	}
	s, ok := a.obj.Syms[name]
	if !ok || s.Sec == nil {
		return 0, false, false
	}
	return s.Value, s.Sec == a.cur, true
}

// branchTarget returns the byte offset from the end of the instruction
// (PC-relative base for Thumb) to a label, or a relocation when the target is
// in another section.
func (a *asm) branchTarget(name string, pcBase int) (int, bool) {
	addr, same, ok := a.lookup(name)
	if !ok {
		return 0, false
	}
	if !same {
		return 0, false
	}
	return addr - pcBase, true
}
