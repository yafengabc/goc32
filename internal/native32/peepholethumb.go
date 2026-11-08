package native32

import (
	"regexp"
	"strconv"
	"strings"
)

// armPeephole is the small text pass over the finished Thumb assembly.  The
// emitter lays blocks out in order, so a jump to the next label is redundant.
// A conditional branch followed by an unconditional jump can be inverted when
// the branch target is the fallthrough, which removes the jump; the distance
// guard respects Thumb's small conditional range.  Blocks that still need a
// literal pool keep their `.ltorg` between the two, so they are left alone.

var armInvert = map[string]string{
	"beq": "bne", "bne": "beq",
	"bhs": "blo", "blo": "bhs",
	"bmi": "bpl", "bpl": "bmi",
	"bvs": "bvc", "bvc": "bvs",
	"bhi": "bls", "bls": "bhi",
	"bge": "blt", "blt": "bge",
	"bgt": "ble", "ble": "bgt",
}

func armPeephole(src string, wide bool) string {
	return armJumpPeephole(armSlotForward(armShrinkTrampolines(src, wide)))
}

var armWordSp = regexp.MustCompile(`^(str|ldr) (r(?:[0-9]|1[0-2])), \[sp(?:, #(-?[0-9]+))?\]$`)

var armSizeSp = regexp.MustCompile(`^((?:ldrb|ldrh|ldrsb|ldrsh)) (r(?:[0-9]|1[0-2])), \[sp(?:, #(-?[0-9]+))?\]$`)

func armStripComment(s string) string {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

var armRegRe = regexp.MustCompile(`^r(?:[0-9]|1[0-2])$`)

// armNoDest lists the instructions whose first operand is not a destination.
var armNoDest = map[string]bool{
	"str": true, "strb": true, "strh": true, "strd": true,
	"cmp": true, "cmn": true, "tst": true, "teq": true,
	"b": true, "bl": true, "bx": true, "blx": true,
	"nop": true, "svc": true, "dmb": true, "dsb": true, "isb": true,
}

func armWrites(body string) string {
	f := strings.Fields(body)
	if len(f) < 2 || armNoDest[f[0]] {
		return ""
	}
	reg := strings.TrimSuffix(f[1], ",")
	if !armRegRe.MatchString(reg) {
		return ""
	}
	return reg
}

// armSlotForward tracks word-sized sp-relative stores within a basic block and
// turns a later load of the same slot into a register move (or removes it).
// Thumb has no equivalent of the RV32 slot cache, and the stack-machine
// emitter stores many SSA results that are read again a few instructions
// later.  Calls, labels and directives end the block and clear the cache.
//
// Once a function has been processed, word stores to slots that no surviving
// load reads are deleted.  Stores marked with @ goc32keep (outgoing stack
// arguments and the vararg register save area) are never removed.
func armSlotForward(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	slotReg := map[int]string{}
	used := map[int]bool{}
	funcStart := 0
	clear := func() {
		slotReg = map[int]string{}
	}
	kill := func(reg string) {
		for off, q := range slotReg {
			if q == reg {
				delete(slotReg, off)
			}
		}
	}
	spOff := func(m []string) int {
		if m[3] == "" {
			return 0
		}
		off, _ := strconv.Atoi(m[3])
		return off
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			clear()
			out = append(out, line)
			continue
		}
		body := strings.TrimSpace(line)
		if body == "" {
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".section .text.") {
			used = map[int]bool{}
			funcStart = len(out)
			clear()
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".size ") {
			kept := out[:funcStart]
			for _, l := range out[funcStart:] {
				b := armStripComment(strings.TrimSpace(l))
				if m := armWordSp.FindStringSubmatch(b); m != nil && m[1] == "str" {
					off := spOff(m)
					if !used[off] && !strings.Contains(l, "goc32keep") {
						continue
					}
				}
				kept = append(kept, l)
			}
			out = kept
			used = map[int]bool{}
			funcStart = len(out)
			clear()
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".") {
			if body == ".ltorg" {
				clear()
			}
			out = append(out, line)
			continue
		}
		if m := armWordSp.FindStringSubmatch(body); m != nil {
			off := spOff(m)
			reg := m[2]
			if m[1] == "str" {
				slotReg[off] = reg
				out = append(out, line)
				continue
			}
			if q, ok := slotReg[off]; ok {
				if q == reg {
					continue
				}
				out = append(out, "\tmov "+reg+", "+q)
				kill(reg)
				slotReg[off] = reg
				continue
			}
			// The load overwrites reg, so it can no longer hold a cached
			// value for another slot; the loaded value is now its slot's.
			kill(reg)
			slotReg[off] = reg
			used[off] = true
			out = append(out, line)
			continue
		}
		// Byte/halfword loads are not forwarded but still read their slot.
		if m := armSizeSp.FindStringSubmatch(body); m != nil {
			used[spOff(m)] = true
		}
		f := strings.Fields(body)
		if f[0] == "bl" || f[0] == "blx" || f[0] == "bx" || f[0] == "b" || f[0] == "pop" || armInvert[f[0]] != "" {
			clear()
		}
		if reg := armWrites(body); reg != "" {
			kill(reg)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// armShrinkTrampolines replaces the emitter's long-branch trampoline
//
//	ldr r3, .LbrN
//	mov pc, r3
//	.balign 4
//
// .LbrN:
//
//	.word TARGET
//
// with a direct branch when the target is in range.  On ARMv7-M/ARMv8-M a
// plain `b` assembles to the 32-bit form at any distance; on Thumb-1 the
// line-distance guard keeps it inside the short branch range.
func armShrinkTrampolines(src string, wide bool) string {
	lines := strings.Split(src, "\n")
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
		if i+4 < len(lines) {
			f := strings.Fields(strings.TrimSpace(lines[i]))
			g := strings.Fields(strings.TrimSpace(lines[i+1]))
			if len(f) == 3 && f[0] == "ldr" && strings.TrimSuffix(f[1], ",") == "r3" && strings.HasPrefix(f[2], ".Lbr") &&
				len(g) == 3 && g[0] == "mov" && strings.TrimSuffix(g[1], ",") == "pc" && g[2] == "r3" &&
				strings.TrimSpace(lines[i+2]) == ".balign 4" &&
				strings.TrimSpace(lines[i+3]) == f[2]+":" {
				w := strings.Fields(strings.TrimSpace(lines[i+4]))
				if len(w) == 2 && w[0] == ".word" {
					target := w[1]
					if pos, ok := labelPos[target]; wide || (ok && absInt(pos-i) <= 200) {
						out = append(out, "\tb "+target)
						i += 4
						continue
					}
				}
			}
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

func armJumpPeephole(src string) string {
	lines := strings.Split(src, "\n")
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
		line := lines[i]
		body := strings.TrimSpace(line)
		if strings.HasPrefix(line, "\t") {
			f := strings.Fields(body)
			// movs r0, r0 / adds r0, #0
			if len(f) == 3 && f[0] == "movs" && strings.TrimSuffix(f[1], ",") == f[2] {
				continue
			}
			if len(f) == 3 && (f[0] == "adds" || f[0] == "subs") && f[2] == "#0" {
				continue
			}
			// b L followed by L:
			if len(f) == 2 && f[0] == "b" && i+1 < len(lines) {
				if strings.TrimSpace(lines[i+1]) == f[1]+":" && !isLtorg(line) {
					continue
				}
			}
			// b<cond> L1; b L2; L1:
			if len(f) == 2 && armInvert[f[0]] != "" && i+2 < len(lines) {
				if nf := strings.Fields(strings.TrimSpace(lines[i+1])); len(nf) == 2 && nf[0] == "b" {
					if strings.TrimSpace(lines[i+2]) == f[1]+":" {
						if pos, ok := labelPos[nf[1]]; ok && absInt(pos-i) <= 40 {
							out = append(out, "\t"+armInvert[f[0]]+" "+nf[1])
							i++ // drop the unconditional jump
							continue
						}
					}
				}
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isLtorg(line string) bool { return strings.TrimSpace(line) == ".ltorg" }
