package native32

import "strings"

// Threading pass for empty RV32 blocks.  A label whose whole body is `j T`
// (edge and join blocks) can be skipped: every reference to the label is
// retargeted to T, and the trampoline disappears once nothing references it
// and no path falls through into it.

func rv32BranchTarget(body string) (op, target string, ok bool) {
	f := strings.Fields(body)
	if len(f) == 2 && f[0] == "j" {
		return "j", f[1], true
	}
	if len(f) == 0 {
		return "", "", false
	}
	switch f[0] {
	case "beqz", "bnez":
		if len(f) == 3 {
			return f[0], f[2], true
		}
	case "beq", "bne", "blt", "bge", "bltu", "bgeu", "ble", "bgt":
		if len(f) == 4 {
			return f[0], f[3], true
		}
	}
	return "", "", false
}

func rv32ThreadBlocksFunc(lines []string) []string {
	for pass := 0; pass < 8; pass++ {
		labelAt := map[string]int{}
		for i, line := range lines {
			if lab, ok := rv32BlockName(line); ok {
				labelAt[lab] = i
			}
		}
		tramp := map[string]string{}
		for lab, pos := range labelAt {
			if pos+2 > len(lines) {
				continue
			}
			if pos > 0 {
				if _, ok := rv32BlockName(lines[pos-1]); ok {
					continue
				}
			}
			if !strings.HasPrefix(lines[pos+1], "\t") {
				continue
			}
			op, target, ok := rv32BranchTarget(strings.TrimSpace(lines[pos+1]))
			if !ok || op != "j" {
				continue
			}
			if pos+2 < len(lines) && strings.HasPrefix(lines[pos+2], "\t") {
				continue
			}
			tramp[lab] = target
		}
		if len(tramp) == 0 {
			return lines
		}
		resolve := func(l string) (string, bool) {
			seen := map[string]bool{}
			for {
				if seen[l] {
					return "", false
				}
				seen[l] = true
				t, ok := tramp[l]
				if !ok {
					return l, true
				}
				l = t
			}
		}
		dest := map[string]string{}
		for l := range tramp {
			if d, ok := resolve(l); ok && d != l {
				dest[l] = d
			}
		}
		changed := false
		for i, line := range lines {
			if !strings.HasPrefix(line, "\t") {
				continue
			}
			_, target, ok := rv32BranchTarget(strings.TrimSpace(line))
			if !ok {
				continue
			}
			d, ok := dest[target]
			if !ok || !strings.HasSuffix(line, target) {
				continue
			}
			lines[i] = line[:len(line)-len(target)] + d
			changed = true
		}
		if changed {
			continue
		}
		referenced := map[string]bool{}
		worded := map[string]bool{}
		for _, line := range lines {
			b := strings.TrimSpace(line)
			if _, target, ok := rv32BranchTarget(b); ok {
				referenced[target] = true
			}
			if f := strings.Fields(b); len(f) == 2 && f[0] == ".word" {
				worded[f[1]] = true
			}
		}
		dead := map[int]bool{}
		for lab, pos := range labelAt {
			if pos+2 > len(lines) {
				continue
			}
			op, target, ok := rv32BranchTarget(strings.TrimSpace(lines[pos+1]))
			if !ok || op != "j" || target == lab {
				continue
			}
			if pos > 0 {
				if _, ok := rv32BlockName(lines[pos-1]); ok {
					continue
				}
			}
			if pos+2 < len(lines) && strings.HasPrefix(lines[pos+2], "\t") {
				continue
			}
			if referenced[lab] || worded[lab] {
				continue
			}
			if rv32HasFallthrough(lines, pos) {
				continue
			}
			dead[pos] = true
			dead[pos+1] = true
		}
		if len(dead) == 0 {
			return lines
		}
		out := make([]string, 0, len(lines))
		for i, line := range lines {
			if !dead[i] {
				out = append(out, line)
			}
		}
		lines = out
	}
	return lines
}

func rv32BlockName(line string) (string, bool) {
	if line == "" || line[0] == '\t' || line[0] == ' ' || !strings.HasSuffix(line, ":") {
		return "", false
	}
	c := line[0]
	if !(c == '.' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return "", false
	}
	return strings.TrimSuffix(line, ":"), true
}

func rv32HasFallthrough(lines []string, pos int) bool {
	for i := pos - 1; i >= 0; i-- {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			return true
		}
		b := strings.TrimSpace(line)
		if strings.HasPrefix(b, ".") {
			return true
		}
		f := strings.Fields(b)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "j", "ret", "jr", "tail", "unreachable":
			return false
		}
		return true
	}
	return true
}
