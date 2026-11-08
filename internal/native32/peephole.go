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
	rv32Reg      = regexp.MustCompile(`^(x\d+|zero|ra|sp|gp|tp|t[0-6]|s\d+|a[0-7])$`)
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
		for off, q := range slotReg {
			if q == reg {
				delete(slotReg, off)
			}
		}
		delete(regSlot, reg)
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
			if q, ok := slotReg[off]; ok && q == reg {
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
	return rv32FoldCopies(res)
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
	return len(reg) == 2 && reg[0] == 't' && reg[1] >= '0' && reg[1] <= '6'
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
	if rv32Writes(line) != old {
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
	if len(head) != 2 || head[1] != old {
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
		if rv32Writes(line) == reg {
			return false
		}
		if rv32HasReg(body, reg) {
			return true
		}
		switch fields[0] {
		case "j", "jr", "ret", "tail", "jal", "jalr":
			return false
		}
	}
	return false
}

func rv32HasReg(body, reg string) bool {
	for _, tok := range strings.FieldsFunc(body, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_'
	}) {
		if tok == reg {
			return true
		}
	}
	return false
}
