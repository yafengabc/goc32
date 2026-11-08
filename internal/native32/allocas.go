package native32

import "sort"

// findDirectAllocas reports the scalar allocas whose address never escapes.
// Such a local is only ever the pointer operand of a load or a store, so the
// back end can address its storage directly as sp+offset instead of
// materialising the pointer at every access.  This is the low-risk half of
// mem2reg: values still live in memory across blocks, but the address
// computation and the pointer load/store pair disappear.

func findDirectAllocas(f *Func) map[string]bool {
	direct, _, _ := analyzeAllocas(f)
	out := map[string]bool{}
	for name, ok := range direct {
		if ok {
			out[name] = true
		}
	}
	return out
}

// planRegVars picks the hottest non-escaping scalar locals and gives each a
// callee-saved register for the whole function.  Because the address never
// escapes, every load and store of that object is visible here, so a register
// is a faithful replacement for its memory cell across calls and blocks.
func planRegVars(f *Func, regs []string, minCount int) (map[string]string, []string) {
	direct, count, size := analyzeAllocas(f)
	type cand struct {
		name string
		n    int
	}
	var cs []cand
	for name, ok := range direct {
		if ok && size[name] == 4 && count[name] >= minCount {
			cs = append(cs, cand{name, count[name]})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].n != cs[j].n {
			return cs[i].n > cs[j].n
		}
		return cs[i].name < cs[j].name
	})
	vars := map[string]string{}
	var used []string
	for i, c := range cs {
		if i >= len(regs) {
			break
		}
		vars[c.name] = regs[i]
		used = append(used, regs[i])
	}
	return vars, used
}

func analyzeAllocas(f *Func) (map[string]bool, map[string]int, map[string]int) {
	type info struct {
		direct bool
		count  int
		size   int
	}
	allocas := map[string]*info{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "alloca" || in.Dst == "" || in.Ty == nil {
				continue
			}
			// Direct sp-relative loads/stores handle the scalar sizes the
			// back end can address without a pointer: byte, half, word and
			// the two-word pair.  Aggregates and arrays keep the pointer path.
			sz := in.Ty.Size()
			allocas[in.Dst] = &info{
				direct: sz == 1 || sz == 2 || sz == 4 || sz == 8,
				size:   sz,
			}
		}
	}
	if len(allocas) == 0 {
		return nil, nil, nil
	}
	escape := func(v Value) {
		if v.Kind != VLocal {
			return
		}
		if inf := allocas[v.Name]; inf != nil {
			inf.direct = false
		}
	}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			for i, op := range in.Ops {
				if op.Kind != VLocal {
					continue
				}
				inf := allocas[op.Name]
				if inf == nil {
					continue
				}
				switch {
				case in.Op == "load" && i == 0:
					inf.count++
				case in.Op == "store" && i == 1:
					inf.count++
				default:
					inf.direct = false
				}
			}
			for _, c := range in.Cases {
				escape(c.Val)
			}
			if in.Indirect && in.Callee != "" {
				escape(Value{Kind: VLocal, Name: in.Callee})
			}
		}
	}
	direct := map[string]bool{}
	count := map[string]int{}
	size := map[string]int{}
	for name, inf := range allocas {
		direct[name] = inf.direct
		count[name] = inf.count
		size[name] = inf.size
	}
	return direct, count, size
}
