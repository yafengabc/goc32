package native32

import (
	"regexp"
	"strings"
)

// Threading pass for empty Thumb blocks.  The emitter creates labels whose
// whole body is `b TARGET` (edge blocks, empty join blocks).  On ARMv7-M and
// ARMv8-M a branch can reach any point in the function, so every reference to
// such a label can target the final destination directly; the empty block is
// then unreachable and its branch disappears.
//
// Only blocks with no fallthrough predecessor and no remaining references are
// removed, so no implicit path is broken.  ARMv6-M's short conditional range
// makes arbitrary retargeting unsafe, so the pass is a no-op there for now.

var armBranchRe = regexp.MustCompile(`^(b|b(?:eq|ne|lt|ge|gt|le|lo|hs|hi|ls|mi|pl|vs|vc)) ([\w.]+)$`)

func armThreadBlocks(src string, wide bool) string {
	for pass := 0; pass < 8; pass++ {
		lines := strings.Split(src, "\n")
		labelAt := map[string]int{}
		for i, line := range lines {
			if lab, ok := armBlockName(line); ok {
				labelAt[lab] = i
			}
		}
		// label -> target of a block that contains exactly one `b`.
		tramp := map[string]string{}
		for lab, pos := range labelAt {
			if pos+2 > len(lines) {
				continue
			}
			if pos > 0 {
				if _, ok := armBlockName(lines[pos-1]); ok {
					continue // another label shares this block
				}
			}
			b := strings.TrimSpace(lines[pos+1])
			m := armBranchRe.FindStringSubmatch(b)
			if m == nil || m[1] != "b" {
				continue
			}
			if pos+2 < len(lines) && strings.HasPrefix(lines[pos+2], "\t") {
				continue // more than one instruction in the block
			}
			tramp[lab] = m[2]
		}
		if len(tramp) == 0 {
			return src
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
			m := armBranchRe.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			if d, ok := dest[m[2]]; ok {
				if !wide {
					pos, okp := labelAt[d]
					if !okp {
						continue
					}
					limit := 45 // Thumb-1 conditional range is +/-256 bytes
					if m[1] == "b" {
						limit = 200 // and the unconditional range is +/-2 KiB
					}
					if absInt(pos-i) > limit {
						continue
					}
				}
				lines[i] = "\t" + m[1] + " " + d
				changed = true
			}
		}
		if changed {
			src = strings.Join(lines, "\n")
			continue
		}
		// Stable: remove trampolines that are no longer referenced and cannot
		// be entered by fallthrough.
		referenced := map[string]bool{}
		worded := map[string]bool{}
		for _, line := range lines {
			b := strings.TrimSpace(line)
			if m := armBranchRe.FindStringSubmatch(b); m != nil {
				referenced[m[2]] = true
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
			m := armBranchRe.FindStringSubmatch(strings.TrimSpace(lines[pos+1]))
			if m == nil || m[1] != "b" || m[2] == lab {
				continue
			}
			if pos > 0 {
				if _, ok := armBlockName(lines[pos-1]); ok {
					continue
				}
			}
			if pos+2 < len(lines) && strings.HasPrefix(lines[pos+2], "\t") {
				continue
			}
			if referenced[lab] || worded[lab] {
				continue
			}
			if armHasFallthrough(lines, pos) {
				continue
			}
			dead[pos] = true
			dead[pos+1] = true
		}
		if len(dead) == 0 {
			return src
		}
		out := make([]string, 0, len(lines))
		for i, line := range lines {
			if !dead[i] {
				out = append(out, line)
			}
		}
		src = strings.Join(out, "\n")
	}
	return src
}

func armBlockName(line string) (string, bool) {
	if line == "" || line[0] == '\t' || line[0] == ' ' || !strings.HasSuffix(line, ":") {
		return "", false
	}
	c := line[0]
	if !(c == '.' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		return "", false
	}
	return strings.TrimSuffix(line, ":"), true
}

// armHasFallthrough reports whether a block at line pos can be entered by
// falling out of the preceding block.  Anything unexpected counts as a
// possible fallthrough so the caller keeps the block.
func armHasFallthrough(lines []string, pos int) bool {
	for i := pos - 1; i >= 0; i-- {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			return true // another label: keep it
		}
		b := strings.TrimSpace(line)
		if strings.HasPrefix(b, ".") {
			return true // directives/pools: keep it
		}
		f := strings.Fields(b)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "b" || f[0] == "ret" || f[0] == "bx":
			return false
		case f[0] == "pop" && strings.Contains(b, "pc"):
			return false
		case f[0] == "mov" && strings.HasPrefix(f[1], "pc,"):
			return false
		}
		return true
	}
	return true
}
