package native32

import "strings"

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

func armPeephole(src string) string {
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
