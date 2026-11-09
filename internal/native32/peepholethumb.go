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
	return armJumpPeephole(armFoldMoves(armSlotForward(armShrinkTrampolines(src, wide)), wide))
}

var armWordSp = regexp.MustCompile(`^(str|ldr) (r(?:[0-9]|1[0-2])), \[sp(?:, #(-?[0-9]+))?\]$`)

var armSizeSp = regexp.MustCompile(`^((?:ldrb|ldrh|ldrsb|ldrsh)) (r(?:[0-9]|1[0-2])), \[sp(?:, #(-?[0-9]+))?\]$`)

var armMovRe = regexp.MustCompile(`^mov (r(?:[0-9]|1[0-2])), (r(?:[0-9]|1[0-2]))$`)
var armCmpRe = regexp.MustCompile(`^cmp (r(?:[0-9]|1[0-2])), (#-?[0-9]+|r(?:[0-9]|1[0-2]))$`)
var armStrOpRe = regexp.MustCompile(`^(str|strb|strh) (r(?:[0-9]|1[0-2])), (.+)$`)
var armAddR4 = regexp.MustCompile(`^add r4, sp, #(-?[0-9]+)$`)
var armR4Word = regexp.MustCompile(`^(str|ldr) (r(?:[0-9]|1[0-2])), \[r4\]$`)
var armR4Sized = regexp.MustCompile(`^(strb|ldrb|ldrsb|strh|ldrh|ldrsh) (r(?:[0-9]|1[0-2])), \[r4\]$`)

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

// armVolatile reports a register that is dead at labels and clobbered by
// calls.  The emitter keeps function-wide variables only in r5-r11.
func armVolatile(reg string) bool {
	switch reg {
	case "r0", "r1", "r2", "r3", "r12":
		return true
	}
	return false
}

func armLow(reg string) bool {
	return len(reg) == 2 && reg[0] == 'r' && reg[1] >= '0' && reg[1] <= '7'
}

func armLoadOp(op string) bool {
	return op == "ldrb" || op == "ldrh" || op == "ldrsb" || op == "ldrsh"
}

func armHasReg(s, reg string) bool {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if tok == reg {
			return true
		}
	}
	return false
}

// armReadBeforeWrite is conservative: a destination that also reads itself
// counts as a read, so a move may be kept longer than necessary, never shorter.
func armReadBeforeWrite(lines []string, reg string) bool {
	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			return false
		}
		body := armStripComment(strings.TrimSpace(line))
		if body == "" {
			continue
		}
		f := strings.Fields(body)
		if len(f) == 0 {
			continue
		}
		if f[0] == "bl" || f[0] == "blx" || f[0] == "bx" || f[0] == "b" || f[0] == "pop" || armInvert[f[0]] != "" {
			return false
		}
		for _, tok := range strings.Fields(strings.ReplaceAll(body, ",", " "))[1:] {
			if tok == reg {
				return true
			}
		}
	}
	return false
}

// armFoldMoves folds the register copies introduced by slot forwarding (and
// by the emitter) into the next comparison or store when the copy destination
// is a scratch register that is dead afterwards.
func armFoldMoves(src string, wide bool) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		body := armStripComment(strings.TrimSpace(line))
		if strings.HasPrefix(line, "\t") {
			if m := armMovRe.FindStringSubmatch(body); m != nil {
				rd, rs := m[1], m[2]
				if rd == rs {
					continue
				}
				if armVolatile(rd) && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t") &&
					!armReadBeforeWrite(lines[i+2:], rd) {
					nb := armStripComment(strings.TrimSpace(lines[i+1]))
					if m2 := armCmpRe.FindStringSubmatch(nb); m2 != nil && m2[1] == rd && m2[2] != rd {
						out = append(out, "\tcmp "+rs+", "+m2[2])
						i++
						continue
					}
					if m3 := armStrOpRe.FindStringSubmatch(nb); m3 != nil && m3[2] == rd &&
						!armHasReg(m3[3], rd) && (wide || armLow(rs)) {
						nl := "\t" + m3[1] + " " + rs + ", " + m3[3]
						if strings.Contains(lines[i+1], "@") {
							nl += " " + lines[i+1][strings.Index(lines[i+1], "@"):]
						}
						out = append(out, nl)
						i++
						continue
					}
				}
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// armSlotForward tracks word-sized sp-relative stores within a basic block and
// turns a later load of the same slot into a register move (or removes it).
// Thumb has no equivalent of the RV32 slot cache, and the stack-machine
// emitter stores many SSA results that are read again a few instructions
// later.  Calls, labels and directives end the block and clear the cache.
//
// ARMv6-M addresses slots beyond 124 bytes (and byte/halfword slots) through
// r4; `add r4, sp, #off` plus `[r4]` is treated as the same slot as
// `[sp,#off]`, with r4's known offset invalidated by any other write to r4.
//
// Once a function has been processed, word stores to slots that no surviving
// load reads are deleted.  Stores marked with @ goc32keep (outgoing stack
// arguments and the vararg register save area) are never removed.
func armSlotForward(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	slotReg := map[int]string{}
	used := map[int]bool{}
	storeOff := map[int]int{} // output index -> frame offset
	funcStart := 0
	r4Off := -1
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
	offsetOf := func(m []string) int {
		if m[3] == "" {
			return 0
		}
		off, _ := strconv.Atoi(m[3])
		return off
	}
	// r4 address calculations are emitted lazily so that a load which is
	// folded away does not leave a dead `add r4, sp, #off` behind.
	pendR4 := ""
	flush := func() {
		if pendR4 != "" {
			out = append(out, pendR4)
			pendR4 = ""
		}
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			flush()
			clear()
			out = append(out, line)
			continue
		}
		body := strings.TrimSpace(line)
		if body == "" {
			flush()
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".section .text.") {
			flush()
			used = map[int]bool{}
			storeOff = map[int]int{}
			r4Off = -1
			funcStart = len(out)
			clear()
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".size ") {
			pendR4 = ""
			dead := map[int]bool{}
			for idx := funcStart; idx < len(out); idx++ {
				if off, ok := storeOff[idx]; ok && !used[off] {
					dead[idx] = true
				}
			}
			// A dead store also makes its address calculation dead when the
			// next surviving line recomputes r4 or leaves the block.
			for idx := funcStart; idx < len(out); idx++ {
				if !dead[idx] || idx <= funcStart || dead[idx-1] {
					continue
				}
				prev := strings.TrimSpace(out[idx-1])
				if !armAddR4.MatchString(prev) || strings.Contains(out[idx-1], "goc32keep") {
					continue
				}
				n := idx + 1
				for n < len(out) && dead[n] {
					n++
				}
				safe := true
				if n < len(out) {
					nb := strings.TrimSpace(out[n])
					if strings.HasPrefix(out[n], "\t") && !strings.HasPrefix(nb, ".") &&
						!armAddR4.MatchString(nb) && armHasReg(nb, "r4") {
						safe = false
					}
				}
				if safe {
					dead[idx-1] = true
				}
			}
			kept := out[:funcStart]
			for idx := funcStart; idx < len(out); idx++ {
				if dead[idx] {
					continue
				}
				kept = append(kept, out[idx])
			}
			out = kept
			used = map[int]bool{}
			storeOff = map[int]int{}
			funcStart = len(out)
			r4Off = -1
			clear()
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(body, ".") {
			flush()
			if body == ".ltorg" {
				clear()
			}
			out = append(out, line)
			continue
		}
		if m := armAddR4.FindStringSubmatch(body); m != nil {
			r4Off, _ = strconv.Atoi(m[1])
			kill("r4")
			pendR4 = line
			continue
		}
		// Sp-relative word slot.
		if m := armWordSp.FindStringSubmatch(body); m != nil {
			flush()
			off := offsetOf(m)
			reg := m[2]
			if m[1] == "str" {
				slotReg[off] = reg
				storeOff[len(out)] = off
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
		// Byte/halfword sp loads are not forwarded but still read their slot.
		if m := armSizeSp.FindStringSubmatch(body); m != nil {
			used[offsetOf(m)] = true
		}
		// r4-addressed slot.
		if r4Off >= 0 {
			off := r4Off
			if m := armR4Word.FindStringSubmatch(body); m != nil {
				reg := m[2]
				if m[1] == "str" {
					flush()
					slotReg[off] = reg
					storeOff[len(out)] = off
					out = append(out, line)
					continue
				}
				if q, ok := slotReg[off]; ok {
					// The address calculation is dead once the load folds.
					pendR4 = ""
					if q == reg {
						continue
					}
					out = append(out, "\tmov "+reg+", "+q)
					kill(reg)
					slotReg[off] = reg
					if reg == "r4" {
						r4Off = -1
					}
					continue
				}
				flush()
				kill(reg)
				slotReg[off] = reg
				used[off] = true
				if reg == "r4" {
					r4Off = -1
				}
				out = append(out, line)
				continue
			}
			if m := armR4Sized.FindStringSubmatch(body); m != nil {
				flush()
				if armLoadOp(m[1]) {
					used[off] = true
					kill(m[2])
				}
				if m[2] == "r4" {
					r4Off = -1
				}
				out = append(out, line)
				continue
			}
		}
		flush()
		f := strings.Fields(body)
		if f[0] == "bl" || f[0] == "blx" || f[0] == "bx" || f[0] == "b" || f[0] == "pop" || armInvert[f[0]] != "" {
			clear()
		}
		if reg := armWrites(body); reg != "" {
			kill(reg)
			if reg == "r4" {
				r4Off = -1
			}
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
