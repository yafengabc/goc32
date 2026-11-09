package native32

// A small, conservative peephole pass over the RV32 assembly.  The emitter is
// a stack machine: every SSA value lives in a frame slot, so without this the
// output is dominated by `sw`/`lw` pairs.  The pass only rewrites one basic
// block at a time (all state is dropped at labels and calls), which makes it
// easy to reason about:
//
//   - a load of a slot that is already in the destination register is deleted;
//   - a load of a slot whose value is known to be in another register becomes
//     a register move (which then lets the store become dead);
//   - `addi r, r, 0` disappears;
//   - a store to a slot that no remaining load reads is deleted.
//
// Stores marked `# goc32keep` are never deleted: outgoing stack arguments and
// the vararg register save area are read by the callee, not by a local load.

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	rv32LwSp     = regexp.MustCompile(`^lw\s+([a-z0-9]+),\s*(-?\d+)\(sp\)$`)
	rv32SwSp     = regexp.MustCompile(`^sw\s+([a-z0-9]+),\s*(-?\d+)\(sp\)$`)
	rv32ZeroAddi = regexp.MustCompile(`^addi\s+([a-z0-9]+),\s*([a-z0-9]+),\s*0$`)
	rv32Reg      = regexp.MustCompile(`^(x\d+|zero|ra|sp|gp|tp|t[0-6]|q[01]|s\d+|a[0-7])$`)
)

// Instructions whose first operand is not a destination.
var rv32NoDest = map[string]bool{
	"sw": true, "sb": true, "sh": true,
	"beq": true, "bne": true, "blt": true, "bge": true,
	"bltu": true, "bgeu": true, "beqz": true, "bnez": true,
	"j": true, "jr": true, "ret": true, "call": true, "tail": true,
	"fence": true, "ecall": true, "ebreak": true, "nop": true,
	"mret": true, "wfi": true, "unimp": true,
}

func peepholeRV32(src string) string {
	lines := strings.Split(src, "\n")
	var out []string
	for i := 0; i < len(lines); {
		if isFunctionStart(lines[i]) {
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(lines[j], "\t.size ") {
				j++
			}
			if j < len(lines) {
				j++
			}
			out = append(out, peepholeRV32Func(lines[i:j])...)
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

func isFunctionStart(line string) bool {
	return strings.HasPrefix(line, "\t.type ") && strings.HasSuffix(strings.TrimSpace(line), ",@function")
}

// rv32Body strips the leading tab and a trailing comment.  ok is false for
// labels, directives and anything that is not a plain instruction.
func rv32Body(line string) (body, comment string, ok bool) {
	if !strings.HasPrefix(line, "\t") {
		return "", "", false
	}
	body = line[1:]
	if i := strings.IndexByte(body, '#'); i >= 0 {
		comment = body[i:]
		body = body[:i]
	}
	body = strings.TrimSpace(body)
	if body == "" || strings.HasPrefix(body, ".") {
		return "", "", false
	}
	return body, comment, true
}

func rv32Writes(line string) string {
	body, _, ok := rv32Body(line)
	if !ok {
		return ""
	}
	fields := strings.Fields(body)
	if len(fields) < 2 || rv32NoDest[fields[0]] {
		return ""
	}
	reg := strings.TrimSuffix(fields[1], ",")
	if !rv32Reg.MatchString(reg) {
		return ""
	}
	return reg
}

func peepholeRV32Func(lines []string) []string {
	lines = rv32ThreadBlocksFunc(lines)
	largeFrame := false
	for _, line := range lines {
		if strings.HasPrefix(line, "\tli t5,") || strings.HasPrefix(line, "\tli t6,") {
			largeFrame = true
			break
		}
	}

	out := make([]string, 0, len(lines))
	slotReg := map[int]string{} // frame offset -> register that holds it
	regSlot := map[string]int{} // register -> frame offset
	used := map[int]bool{}      // offsets read by a surviving lw

	clear := func() {
		slotReg = map[int]string{}
		regSlot = map[string]int{}
	}
	kill := func(reg string) {
		// A register can be the cached value of more than one slot (the same
		// value stored twice), so drop every entry, not just regSlot[reg].
		// q0/q1 are assembler pseudo-names for a4/a5: the program itself also
		// writes those registers (call arguments), so identity is canonical.
		for off, q := range slotReg {
			if sameReg(q, reg) {
				delete(slotReg, off)
			}
		}
		for r := range regSlot {
			if sameReg(r, reg) {
				delete(regSlot, r)
			}
		}
	}

	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			// A label (or a section directive outside the body): nothing
			// before it is valid on another path.
			clear()
			out = append(out, line)
			continue
		}
		body, _, ok := rv32Body(line)
		if !ok {
			out = append(out, line)
			continue
		}
		fields := strings.Fields(body)
		if len(fields) == 0 {
			out = append(out, line)
			continue
		}
		switch fields[0] {
		case "call", "jal", "jalr", "ret", "tail":
			clear()
			out = append(out, line)
			continue
		}
		if m := rv32SwSp.FindStringSubmatch(body); m != nil {
			off, _ := strconv.Atoi(m[2])
			slotReg[off] = m[1]
			regSlot[m[1]] = off
			out = append(out, line)
			continue
		}
		if m := rv32LwSp.FindStringSubmatch(body); m != nil {
			reg := m[1]
			off, _ := strconv.Atoi(m[2])
			if q, ok := slotReg[off]; ok && sameReg(q, reg) {
				// Already in the destination: drop the load.
				continue
			}
			kill(reg)
			if q, ok := slotReg[off]; ok {
				// Keep the value in a register; the store may now be dead.
				line = "\tmv " + reg + ", " + q
				slotReg[off] = reg
				regSlot[reg] = off
				out = append(out, line)
				continue
			}
			used[off] = true
			slotReg[off] = reg
			regSlot[reg] = off
			out = append(out, line)
			continue
		}
		if m := rv32ZeroAddi.FindStringSubmatch(body); m != nil && m[1] == m[2] {
			// A GEP with a zero byte offset.
			continue
		}
		if reg := rv32Writes(line); reg != "" {
			kill(reg)
		}
		out = append(out, line)
	}

	if largeFrame {
		// Slots over 2 KiB are addressed through t5/t6, not lw/sw(off), so
		// this pass cannot see their reads.  Keep every store.
		return out
	}
	res := out[:0]
	for _, line := range out {
		body, comment, ok := rv32Body(line)
		if ok && strings.Contains(comment, "goc32keep") {
			res = append(res, line)
			continue
		}
		if ok {
			if m := rv32SwSp.FindStringSubmatch(body); m != nil {
				off, _ := strconv.Atoi(m[2])
				if !used[off] {
					continue
				}
			}
		}
		res = append(res, line)
	}
	out = rv32FuseSetcc(rv32FoldImmediates(rv32FoldCopies(res)))
	out = rv32FoldCopies(out)
	out = rv32FoldAllocaAddrs(out)
	return rv32DropJumps(rv32InvertBranches(rv32ZeroStores(out)))
}

// rv32FuseSetcc removes the materialisation of a boolean that is only tested
// by the next branch, and folds the surrounding xori chains:
//
//	xori a, b, 0        ->  (no-op)
//	xori a, b, 0        ->  snez a, b
//	snez a, b           ->  ...
//	snez a, b           ->  beqz b, L
//	beqz a, L
//	slt a, b, c         ->  bge b, c, L
//	xori a, a, 1
//	beqz a, L
//
// The emitter lays out comparisons as setcc + xori + branch; a stack-machine
// code generator produces many of them and the intermediates are dead once
// the branch has read them.  A move immediately followed by a store of the
// same register is folded as well.
func rv32FuseSetcc(lines []string) []string {
	for pass := 0; pass < 4; pass++ {
		out := make([]string, 0, len(lines))
		changed := false
		for i := 0; i < len(lines); i++ {
			body, _, ok := rv32Body(lines[i])
			if !ok {
				out = append(out, lines[i])
				continue
			}
			// xori rd, rs, 0 is the front end's cast-to-bool no-op.
			if rd, rs, imm, ok := rv32ParseXori(body); ok && imm == 0 {
				if i+1 < len(lines) {
					if nb, _, ok := rv32Body(lines[i+1]); ok {
						if s, ok := rv32ParseSetcc(nb); ok &&
							(s.op == "snez" || s.op == "seqz") && s.rd == rd && s.ra == rd {
							out = append(out, "\t"+s.op+" "+rd+", "+rs)
							i++
							changed = true
							continue
						}
					}
				}
				changed = true
				if rd != rs {
					out = append(out, "\tmv "+rd+", "+rs)
				}
				continue
			}
			// A boolean NOT that only feeds a branch flips the branch.
			if rd, rs, imm, ok := rv32ParseXori(body); ok && imm == 1 && i+1 < len(lines) {
				if nb, _, ok := rv32Body(lines[i+1]); ok {
					if bop, breg, btgt, ok := rv32ParseBranch(nb); ok && breg == rd &&
						rv32VolatileReg(rd) && !rv32ReadBeforeWrite(lines[i+2:], rd) {
						inv := "beqz"
						if bop == "beqz" {
							inv = "bnez"
						}
						out = append(out, "\t"+inv+" "+rs+", "+btgt)
						i++
						changed = true
						continue
					}
				}
			}
			// xor of two registers tested by a branch is an equality test.
			if rd, ra, rb, ok := rv32ParseXor(body); ok && i+1 < len(lines) {
				if nb, _, ok := rv32Body(lines[i+1]); ok {
					if bop, breg, btgt, ok := rv32ParseBranch(nb); ok && breg == rd &&
						rv32VolatileReg(rd) && !rv32ReadBeforeWrite(lines[i+2:], rd) {
						br := "beq"
						if bop == "bnez" {
							br = "bne"
						}
						if rb == "zero" || rb == "x0" {
							if br == "beq" {
								out = append(out, "\tbeqz "+ra+", "+btgt)
							} else {
								out = append(out, "\tbnez "+ra+", "+btgt)
							}
						} else {
							out = append(out, "\t"+br+" "+ra+", "+rb+", "+btgt)
						}
						i++
						changed = true
						continue
					}
				}
			}
			// move followed by a branch on that register.
			if d, s, ok := rv32Move(lines[i]); ok && i+1 < len(lines) {
				if nb, _, ok := rv32Body(lines[i+1]); ok {
					if bop, breg, btgt, ok := rv32ParseBranch(nb); ok && breg == d &&
						rv32VolatileReg(d) && !rv32ReadBeforeWrite(lines[i+2:], d) {
						out = append(out, "\t"+bop+" "+s+", "+btgt)
						i++
						changed = true
						continue
					}
				}
			}
			// move followed by a store of that register.
			if d, s, ok := rv32Move(lines[i]); ok && i+1 < len(lines) {
				if nb, comment, ok := rv32Body(lines[i+1]); ok {
					if f := strings.Fields(strings.ReplaceAll(nb, ",", " ")); len(f) == 3 &&
						(f[0] == "sw" || f[0] == "sb" || f[0] == "sh") && f[1] == d &&
						rv32VolatileReg(d) && !rv32ReadBeforeWrite(lines[i+2:], d) {
						nl := "\t" + f[0] + " " + s + ", " + f[2]
						if comment != "" {
							nl += " " + comment
						}
						out = append(out, nl)
						i++
						changed = true
						continue
					}
				}
			}
			// compare + optional xori 1 + branch.
			if s, ok := rv32ParseSetcc(body); ok {
				fused := false
				for extra := 0; extra <= 1 && !fused; extra++ {
					if i+extra+1 >= len(lines) {
						break
					}
					nb, _, ok := rv32Body(lines[i+extra+1])
					if !ok {
						break
					}
					if extra == 1 {
						xr, xs, xi, ok := rv32ParseXori(nb)
						if !ok || xi != 1 || xr != s.rd || xs != s.rd {
							break
						}
						if i+extra+2 >= len(lines) {
							break
						}
						nb, _, ok = rv32Body(lines[i+extra+2])
						if !ok {
							break
						}
					}
					bop, breg, btgt, ok := rv32ParseBranch(nb)
					if !ok || breg != s.rd {
						break
					}
					after := i + extra + 2
					if !rv32VolatileReg(s.rd) || rv32ReadBeforeWrite(lines[after:], s.rd) {
						break
					}
					br, ok := rv32BranchFor(s, bop, extra == 1)
					if !ok {
						break
					}
					out = append(out, "\t"+br+", "+btgt)
					i += extra + 1
					changed = true
					fused = true
				}
				if fused {
					continue
				}
			}
			out = append(out, lines[i])
		}
		lines = out
		if !changed {
			break
		}
	}
	return lines
}

func rv32ParseAddi(body string) (dst, src string, off int, ok bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 4 || f[0] != "addi" {
		return "", "", 0, false
	}
	v, err := strconv.Atoi(f[3])
	if err != nil {
		return "", "", 0, false
	}
	return f[1], f[2], v, true
}

// rv32ParseAddiSelf parses "addi reg, reg, imm".
func rv32ParseAddiSelf(body, reg string) (int, bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 4 || f[0] != "addi" || f[1] != reg || f[2] != reg {
		return 0, false
	}
	v, err := strconv.Atoi(f[3])
	if err != nil {
		return 0, false
	}
	return v, true
}

// rv32FoldAddrAccess rewrites a load/store through a just-materialised
// sp+off pointer into a direct sp-relative access.
func rv32FoldAddrAccess(body, ptr, base string, off int) (string, bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 3 || f[2] != "0("+ptr+")" {
		return "", false
	}
	switch f[0] {
	case "lw", "sw":
		if off%4 != 0 {
			return "", false
		}
	case "lh", "lhu", "sh":
		if off%2 != 0 {
			return "", false
		}
	case "lb", "lbu", "sb":
	default:
		return "", false
	}
	if off < -2048 || off > 2047 {
		return "", false
	}
	if f[0] == "sw" || f[0] == "sh" || f[0] == "sb" {
		if f[1] == ptr {
			return "", false // the stored value is the pointer itself
		}
	}
	return "\t" + f[0] + " " + f[1] + ", " + strconv.Itoa(off) + "(" + base + ")", true
}

func rv32BranchOrCall(body string) bool {
	f := strings.Fields(body)
	if len(f) == 0 {
		return false
	}
	switch f[0] {
	case "j", "jr", "ret", "tail", "call", "jal", "jalr", "unreachable",
		"beqz", "bnez", "beq", "bne", "blt", "bge", "bltu", "bgeu", "ble", "bgt":
		return true
	}
	return false
}

// rv32FoldAllocaAddrs turns a materialised sp+off pointer and every access
// through it into direct sp-relative accesses:
//
//	addi q0, sp, OFF        OP   x, OFF(sp)
//	OP   x, 0(q0)           OP   y, OFF+ADD(sp)
//	addi q0, q0, ADD
//	OP   y, 0(q0)
//
// The run is abandoned if the pointer is read in any other way.  A load into
// the pointer or any other write ends the run; the pointer is then dead.
func rv32FoldAllocaAddrs(lines []string) []string {
	type fold struct {
		idx  int
		line string
	}
	replace := map[int]string{}
	drop := map[int]bool{}
	for i := 0; i < len(lines); i++ {
		body, _, ok := rv32Body(lines[i])
		if !ok {
			continue
		}
		p, base, off, ok := rv32ParseAddi(body)
		if !ok || p == base || !rv32VolatileReg(p) {
			continue
		}
		var folds []fold
		var chain []int
		cur := off
		abort := false
		for j := i + 1; j < len(lines); j++ {
			nb, _, ok := rv32Body(lines[j])
			if !ok || strings.HasPrefix(nb, ".") || rv32BranchOrCall(nb) {
				break
			}
			if n, ok := rv32ParseAddiSelf(nb, p); ok {
				cur += n
				chain = append(chain, j)
				continue
			}
			if nl, ok := rv32FoldAddrAccess(nb, p, base, cur); ok {
				folds = append(folds, fold{j, nl})
				if w := rv32Writes(lines[j]); w == p || w == base {
					break
				}
				continue
			}
			if w := rv32Writes(lines[j]); w == p || w == base {
				break
			}
			if rv32ReadsReg(nb, p) {
				abort = true
				break
			}
		}
		if abort || len(folds) == 0 {
			continue
		}
		for _, f := range folds {
			replace[f.idx] = f.line
		}
		drop[i] = true
		for _, c := range chain {
			drop[c] = true
		}
	}
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if drop[i] {
			continue
		}
		if nl, ok := replace[i]; ok {
			out = append(out, nl)
			continue
		}
		out = append(out, line)
	}
	return out
}

func rv32ParseXor(body string) (rd, ra, rb string, ok bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 4 || f[0] != "xor" {
		return "", "", "", false
	}
	return f[1], f[2], f[3], true
}

func rv32ParseXori(body string) (rd, rs string, imm int64, ok bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 4 || f[0] != "xori" {
		return "", "", 0, false
	}
	v, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return f[1], f[2], v, true
}

func rv32ParseBranch(body string) (op, reg, target string, ok bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) != 3 || (f[0] != "beqz" && f[0] != "bnez") {
		return "", "", "", false
	}
	return f[0], f[1], f[2], true
}

type rv32Setcc struct {
	op     string
	rd     string
	ra     string
	rb     string
	hasImm bool
}

func rv32ParseSetcc(body string) (rv32Setcc, bool) {
	f := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(f) < 3 {
		return rv32Setcc{}, false
	}
	s := rv32Setcc{op: f[0], rd: f[1], ra: f[2]}
	switch f[0] {
	case "snez", "seqz":
		return s, len(f) == 3
	case "slt", "sltu":
		if len(f) != 4 {
			return rv32Setcc{}, false
		}
		s.rb = f[3]
		return s, true
	case "slti", "sltiu":
		if len(f) != 4 {
			return rv32Setcc{}, false
		}
		s.hasImm = true
		return s, true
	}
	return rv32Setcc{}, false
}

// rv32BranchFor maps a setcc result tested by beqz/bnez to the direct
// branch.  negated selects the xori 1 (inverted comparison) form.
func rv32BranchFor(s rv32Setcc, branch string, negated bool) (string, bool) {
	if s.hasImm {
		return "", false
	}
	beqz := branch == "beqz"
	switch s.op {
	case "snez":
		if negated {
			beqz = !beqz
		}
		if beqz {
			return "beqz " + s.ra, true
		}
		return "bnez " + s.ra, true
	case "seqz":
		if negated {
			beqz = !beqz
		}
		if beqz {
			return "bnez " + s.ra, true
		}
		return "beqz " + s.ra, true
	case "slt", "sltu":
		if negated {
			beqz = !beqz
		}
		bge, blt := "bge", "blt"
		if s.op == "sltu" {
			bge, blt = "bgeu", "bltu"
		}
		if beqz {
			return bge + " " + s.ra + ", " + s.rb, true
		}
		return blt + " " + s.ra + ", " + s.rb, true
	}
	return "", false
}

// rv32FoldImmediates sinks a small constant into the ALU instruction that
// consumes it:
//
//	li t1, 4          ->   addi t0, t0, 4
//	add t0, t0, t1
//
// Only t0-t6 are folded: a0-a7 may be live across the following call, where
// the liveness scan treats a call as not reading a temporary.
func rv32FoldImmediates(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) {
			if reg, val, ok := rv32ParseLi(lines[i]); ok && rv32TmpReg(reg) {
				if next, ok := rv32FoldImmInto(lines[i+1], reg, val); ok &&
					!rv32ReadBeforeWrite(lines[i+2:], reg) {
					out = append(out, next)
					i++ // drop the li
					continue
				}
			}
		}
		out = append(out, lines[i])
	}
	return out
}

func rv32ParseLi(line string) (reg string, val int64, ok bool) {
	body, _, ok := rv32Body(line)
	if !ok || !strings.HasPrefix(body, "li ") {
		return "", 0, false
	}
	parts := strings.SplitN(body, ",", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	reg = strings.TrimSpace(strings.TrimPrefix(parts[0], "li"))
	if !rv32Reg.MatchString(reg) {
		return "", 0, false
	}
	val, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return reg, val, true
}

func rv32FoldImmInto(line, creg string, val int64) (string, bool) {
	body, comment, ok := rv32Body(line)
	if !ok {
		return "", false
	}
	op, ops := rv32Operands(body)
	if len(ops) != 3 {
		return "", false
	}
	rd := ops[0]
	if rd == "sp" || rd == "ra" || rd == "zero" {
		return "", false
	}
	at := -1
	for i := 1; i <= 2; i++ {
		if ops[i] == creg {
			at = i
		}
	}
	if at < 0 {
		return "", false
	}
	rs := ops[3-at]
	if rs == creg {
		return "", false // both operands are the same constant
	}
	newOp, imm := "", val
	checkRange := true
	switch op {
	case "add":
		newOp = "addi"
	case "sub":
		if at != 2 {
			return "", false
		}
		newOp, imm = "addi", -val
	case "and":
		newOp = "andi"
	case "or":
		newOp = "ori"
	case "xor":
		newOp = "xori"
	case "slt":
		if at != 2 {
			return "", false
		}
		newOp = "slti"
	case "sltu":
		if at != 2 {
			return "", false
		}
		newOp = "sltiu"
	case "sll", "srl", "sra":
		if at != 2 || val < 0 || val > 31 {
			return "", false
		}
		newOp = map[string]string{"sll": "slli", "srl": "srli", "sra": "srai"}[op]
		imm = val
		checkRange = false
	case "mul":
		k, ok := rv32Pow2(val)
		if !ok {
			return "", false
		}
		newOp, imm, checkRange = "slli", int64(k), false
	case "divu":
		if at != 2 {
			return "", false
		}
		k, ok := rv32Pow2(val)
		if !ok {
			return "", false
		}
		newOp, imm, checkRange = "srli", int64(k), false
	case "remu":
		if at != 2 {
			return "", false
		}
		if _, ok := rv32Pow2(val); !ok {
			return "", false
		}
		newOp, imm = "andi", val-1
	default:
		return "", false
	}
	if checkRange && (imm < -2048 || imm > 2047) {
		return "", false
	}
	nb := "\t" + newOp + " " + rd + ", " + rs + ", " + strconv.FormatInt(imm, 10)
	if comment != "" {
		nb += " " + comment
	}
	return nb, true
}

func rv32Pow2(v int64) (int, bool) {
	if v <= 0 || v&(v-1) != 0 {
		return 0, false
	}
	k := 0
	for v > 1 {
		v >>= 1
		k++
	}
	return k, k <= 31
}

// rv32ZeroStores turns `li tX, 0; sw tX, ...` into `sw zero, ...`.
func rv32ZeroStores(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) {
			if reg, ok := rv32LiZero(lines[i]); ok && rv32TmpReg(reg) {
				body, comment, ok := rv32Body(lines[i+1])
				if ok {
					op, ops := rv32Operands(body)
					if op == "sw" && len(ops) >= 2 && ops[0] == reg &&
						!rv32HasReg(ops[1], reg) &&
						!rv32ReadBeforeWrite(lines[i+2:], reg) {
						nb := strings.Replace(body, "sw "+reg+",", "sw zero,", 1)
						next := "\t" + nb
						if comment != "" {
							next += " " + comment
						}
						out = append(out, next)
						i++ // drop the li
						continue
					}
				}
			}
		}
		out = append(out, lines[i])
	}
	return out
}

func rv32LiZero(line string) (string, bool) {
	body, _, ok := rv32Body(line)
	if !ok || !strings.HasPrefix(body, "li ") {
		return "", false
	}
	parts := strings.SplitN(body, ",", 2)
	if len(parts) != 2 {
		return "", false
	}
	reg := strings.TrimSpace(strings.TrimPrefix(parts[0], "li"))
	if !rv32Reg.MatchString(reg) {
		return "", false
	}
	return reg, strings.TrimSpace(parts[1]) == "0"
}

// rv32DropJumps removes a jump whose target is the very next label.  The
// emitter lays blocks out in order, so roughly half of its branch tails end
// with such a jump.
// rv32InvertBranches turns
//
//	bnez r, L1
//	j    L2
//
// L1:
//
// into `beqz r, L2`, dropping the jump.  Conditional branches have a smaller
// range than `j`, so only nearby targets are inverted; large functions keep
// the long jump.
func rv32InvertBranches(lines []string) []string {
	labelPos := map[string]int{}
	for i, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			s := strings.TrimSpace(line)
			if strings.HasSuffix(s, ":") {
				labelPos[strings.TrimSuffix(s, ":")] = i
			}
		}
	}
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		body, _, ok := rv32Body(lines[i])
		if ok && (strings.HasPrefix(body, "bnez ") || strings.HasPrefix(body, "beqz ")) && i+2 < len(lines) {
			f := strings.Fields(body)
			if len(f) == 3 {
				cond := strings.TrimSuffix(f[1], ",")
				l1 := f[2]
				next, _, ok2 := rv32Body(lines[i+1])
				if ok2 && strings.HasPrefix(next, "j ") {
					l2 := strings.TrimSpace(strings.TrimPrefix(next, "j "))
					if strings.TrimSpace(lines[i+2]) == l1+":" {
						if pos, ok3 := labelPos[l2]; ok3 && absInt(pos-i) <= 300 {
							inv := "beqz"
							if f[0] == "beqz" {
								inv = "bnez"
							}
							out = append(out, "\t"+inv+" "+cond+", "+l2)
							i++ // drop the jump
							continue
						}
					}
				}
			}
		}
		out = append(out, lines[i])
	}
	return out
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rv32DropJumps removes a jump whose target is the very next label.  The
// emitter lays blocks out in order, so roughly half of its branch tails end
// with such a jump.
func rv32DropJumps(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		body, _, ok := rv32Body(line)
		if ok && strings.HasPrefix(body, "j ") {
			target := strings.TrimSpace(strings.TrimPrefix(body, "j "))
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j < len(lines) && strings.TrimSpace(lines[j]) == target+":" {
				continue
			}
		}
		out = append(out, line)
	}
	return out
}

// rv32FoldCopies retargets a producer whose result is immediately copied into
// another register, when the source is a temporary and is dead afterwards:
//
//	lw  t0, 12(sp)      ->   lw  a0, 12(sp)
//	mv  a0, t0
//
// This is the common shape of a call argument load.  Only t0-t6 are used as
// the source: the ABI argument registers may be live across the following
// call, while a temporary copied into one cannot be.
func rv32FoldCopies(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if d, s, ok := rv32Move(lines[i]); ok && d == s {
			continue
		}
		if i+1 < len(lines) {
			if d, s, ok := rv32Move(lines[i+1]); ok && rv32TmpReg(s) {
				if nd, ok := rv32Retarget(lines[i], s, d); ok && !rv32ReadBeforeWrite(lines[i+2:], s) {
					out = append(out, nd)
					i++ // drop the move
					continue
				}
			}
		}
		out = append(out, lines[i])
	}
	return out
}

func rv32TmpReg(reg string) bool {
	if reg == "q0" || reg == "q1" {
		return true
	}
	return len(reg) == 2 && reg[0] == 't' && reg[1] >= '0' && reg[1] <= '6'
}

// canonReg maps the assembler pseudo-temporaries q0/q1 to the physical
// registers they become.  They are a4/a5, which the emitter also uses for
// call arguments, so every peephole register-identity check must see through
// the alias; otherwise a cached value survives an argument write and the
// cache hands the wrong value to a later use.
func canonReg(reg string) string {
	switch reg {
	case "q0":
		return "a4"
	case "q1":
		return "a5"
	}
	return reg
}

func sameReg(a, b string) bool { return canonReg(a) == canonReg(b) }

// rv32VolatileReg reports a register that is dead at block boundaries and
// clobbered by calls: the scratch temporaries and the caller-saved a-regs.
// The fusion rules may only use it for liveness, because rv32ReadBeforeWrite
// treats a call as a kill and a label as a block end.
func rv32VolatileReg(reg string) bool {
	switch reg {
	case "q0", "q1":
		return true
	}
	if len(reg) == 2 {
		if reg[0] == 't' && reg[1] >= '0' && reg[1] <= '6' {
			return true
		}
		if reg[0] == 'a' && reg[1] >= '0' && reg[1] <= '7' {
			return true
		}
	}
	return false
}

var rv32QReg = regexp.MustCompile(`\bq[01]\b`)

// rv32RenameScratch maps the pseudo-temporaries to the compressed-capable
// argument registers.  The rename happens after the peephole so its temp
// classification stays unambiguous: a4/a5 are also call arguments, and the
// peephole must be able to tell the two roles apart.
func rv32RenameScratch(src string) string {
	return rv32QReg.ReplaceAllStringFunc(src, func(r string) string {
		if r == "q0" {
			return "a4"
		}
		return "a5"
	})
}

func rv32Move(line string) (dst, src string, ok bool) {
	body, _, ok := rv32Body(line)
	if !ok || !strings.HasPrefix(body, "mv ") {
		return "", "", false
	}
	parts := strings.SplitN(body, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	dst = strings.TrimSpace(strings.TrimPrefix(parts[0], "mv"))
	src = strings.TrimSpace(parts[1])
	if !rv32Reg.MatchString(dst) || !rv32Reg.MatchString(src) {
		return "", "", false
	}
	return dst, src, true
}

// rv32Retarget rewrites the destination of line from old to dst.
func rv32Retarget(line, old, dst string) (string, bool) {
	body, comment, ok := rv32Body(line)
	if !ok {
		return "", false
	}
	if !sameReg(rv32Writes(line), old) {
		return "", false
	}
	switch dst {
	case "sp", "ra", "zero":
		return "", false
	}
	op := strings.Fields(body)[0]
	switch op {
	case "call", "jal", "jalr", "ret", "tail":
		return "", false
	}
	parts := strings.SplitN(body, ",", 2)
	head := strings.Fields(parts[0])
	if len(head) != 2 || !sameReg(head[1], old) {
		return "", false
	}
	head[1] = dst
	nb := head[0] + " " + head[1]
	if len(parts) == 2 {
		nb += "," + parts[1]
	}
	if comment != "" {
		nb += " " + comment
	}
	return "\t" + nb, true
}

// rv32ReadBeforeWrite reports whether reg is read before being redefined or
// before control leaves the block.  Calls do not read t0-t6, and a successor
// block reloads SSA values from their slots.
func rv32ReadBeforeWrite(lines []string, reg string) bool {
	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			return false // label
		}
		body, _, ok := rv32Body(line)
		if !ok {
			continue
		}
		fields := strings.Fields(body)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "call" {
			return false
		}
		// An instruction can read its own destination (mul t0, t0, t1),
		// so check sources before treating a write as a kill.
		if rv32ReadsReg(body, reg) {
			return true
		}
		if sameReg(rv32Writes(line), reg) {
			return false
		}
		switch fields[0] {
		case "j", "jr", "ret", "tail", "jal", "jalr":
			return false
		}
	}
	return false
}

// rv32ReadsReg reports whether any source operand of body mentions reg.
func rv32ReadsReg(body, reg string) bool {
	op, ops := rv32Operands(body)
	start := 0
	if !rv32NoDest[op] && len(ops) > 0 {
		start = 1
	}
	for i := start; i < len(ops); i++ {
		if rv32HasReg(ops[i], reg) {
			return true
		}
	}
	return false
}

func rv32Operands(body string) (op string, ops []string) {
	parts := strings.Split(body, ",")
	head := strings.Fields(parts[0])
	if len(head) == 0 {
		return "", nil
	}
	op = head[0]
	if len(head) > 1 {
		ops = append(ops, head[1])
	}
	for _, p := range parts[1:] {
		ops = append(ops, strings.TrimSpace(p))
	}
	return op, ops
}

func rv32HasReg(body, reg string) bool {
	for _, tok := range strings.FieldsFunc(body, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	}) {
		if sameReg(tok, reg) {
			return true
		}
	}
	return false
}
