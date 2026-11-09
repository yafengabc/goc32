package native32

import "sort"

// Block-local register allocation for RV32.
//
// The emitter is a stack machine: values are computed in q0/q1 and spilled to
// a frame slot, and the assembly peephole recovers some of that traffic by
// forwarding a store into later loads.  Forwarding is linear, though: it can
// only keep one register per slot and it cannot hold several values at once.
//
// A value whose whole live range stays inside one basic block does not need a
// slot at all.  Such a value gets a caller-saved argument register (a0-a3,
// a6, a7) from a linear scan.  The allocation is limited to blocks without a
// call, so nothing else clobbers those registers and the function pays no
// prologue save/restore.  The emitter then writes producers straight to the
// allocated register, which removes both the store at the definition and the
// reload/copy at every use.
//
// Values that cross a block boundary, phi destinations (copied on edges),
// allocas and values loaded from a register-promoted local keep their slots:
// the front end keeps C locals in allocas, and the existing reg-var and alias
// paths already handle them better than a register here.

func planBlockAlloc(f *Func, cross map[string]bool, regVar, paramReg map[string]string) map[string]string {
	alloc := map[string]string{}
	for _, blk := range f.Blocks {
		hasCall := false
		for _, in := range blk.Insts {
			if in.Op == "call" {
				hasCall = true
				break
			}
		}
		if hasCall {
			continue
		}
		defIdx := map[string]int{}
		def := map[string]*Inst{}
		last := map[string]int{}
		moveUses := map[string]int{}
		for i, in := range blk.Insts {
			for oi, op := range in.Ops {
				if op.Kind != VLocal {
					continue
				}
				last[op.Name] = i
				if rv32OperandCostsMove(in.Op, oi) {
					moveUses[op.Name]++
				}
			}
			for _, c := range in.Cases {
				if c.Val.Kind == VLocal {
					last[c.Val.Name] = i
				}
			}
			if in.Indirect && in.Callee != "" {
				last[in.Callee] = i
			}
			if in.Dst != "" {
				defIdx[in.Dst] = i
				def[in.Dst] = in
			}
		}
		type iv struct {
			name       string
			start, end int
		}
		var ivs []iv
		for name, in := range def {
			if cross[name] || regVar[name] != "" || paramReg[name] != "" {
				continue
			}
			if in.Ty == nil || in.Ty.Size() != 4 || in.Ty.Align() > 4 {
				continue
			}
			// Only values whose uses would need a copy out of q0 profit:
			// the stack machine already keeps a value in q0 for free when
			// every use reads that register.
			if moveUses[name] == 0 {
				continue
			}
			switch in.Op {
			case "alloca", "phi", "call",
				// Casts are copies in this backend; the copy folder handles
				// them better than a register allocation.
				"trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr":
				continue
			case "load":
				// The reg-var alias path is free; a register would only add
				// a copy at the definition.
				if in.Ops[0].Kind == VLocal && regVar[in.Ops[0].Name] != "" {
					continue
				}
			}
			end, ok := last[name]
			if !ok {
				continue
			}
			ivs = append(ivs, iv{name, defIdx[name], end})
		}
		if len(ivs) == 0 {
			continue
		}
		sort.Slice(ivs, func(i, j int) bool {
			if ivs[i].start != ivs[j].start {
				return ivs[i].start < ivs[j].start
			}
			return ivs[i].name < ivs[j].name
		})
		type active struct {
			end int
			reg string
		}
		var act []active
		// a0-a3 are in the compressed-register range; a6/a7 are the fallback.
		avail := []string{"a7", "a6", "a3", "a2", "a1", "a0"}
		for _, v := range ivs {
			kept := act[:0]
			for _, a := range act {
				if a.end < v.start {
					avail = append(avail, a.reg)
					continue
				}
				kept = append(kept, a)
			}
			act = kept
			if len(avail) == 0 {
				continue
			}
			r := avail[len(avail)-1]
			avail = avail[:len(avail)-1]
			alloc[v.name] = r
			act = append(act, active{v.end, r})
		}
	}
	return alloc
}

// rv32OperandCostsMove reports whether the stack machine would have to copy a
// value out of q0 for operand i.  The first operand of most instructions is
// already requested in q0, so keeping it there costs nothing; a second operand
// is loaded into q1 and becomes a move once the peephole forwards the store.
func rv32OperandCostsMove(op string, i int) bool {
	switch op {
	case "add", "sub", "mul", "sdiv", "udiv", "srem", "urem",
		"shl", "lshr", "ashr", "and", "or", "xor", "icmp", "getelementptr":
		return i >= 1
	case "store":
		return i == 1
	case "ret":
		return true
	case "load", "br", "switch", "select", "unreachable",
		"trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr",
		"alloca", "phi", "call":
		return false
	}
	return true
}
