package native32

import "sort"

// Block-local register allocation for the ARM/Thumb emitter.
//
// The ARM back end is a stack machine too, but it reserves r4 for address
// scratch and r0/r1 for expression evaluation.  r2 is caller-saved and unused
// by scalar code, so a value that stays inside a call-free block can live
// there.  The stack machine is already good at keeping a value in r0 when
// every use reads that register (the first operand, a store value, a return
// value); allocation only pays off when a use would otherwise need a copy out
// of r0, which in this back end means a second operand or a store address.
//
// r2 is clobbered by 64-bit arithmetic, aggregate/8-byte memory operations,
// division helpers and a few intrinsics, so blocks containing those keep their
// slots.  r3 is not used: every internal branch materialises its target there
// for the range-independent Thumb-1 jump.
func planARMBlockAlloc(f *Func, cross map[string]bool, regVar map[string]string) map[string]string {
	alloc := map[string]string{}
	for _, blk := range f.Blocks {
		if armBlockClobbersR2(blk) {
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
				if armOperandCostsMove(in.Op, oi) {
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
			if cross[name] || regVar[name] != "" {
				continue
			}
			if in.Ty == nil || in.Ty.Size() != 4 || in.Ty.Align() > 4 {
				continue
			}
			if moveUses[name] == 0 {
				continue
			}
			switch in.Op {
			case "alloca", "phi", "call",
				"getelementptr", "select", "unreachable",
				"trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr":
				continue
			case "load":
				// The reg-var alias path is free.
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
		var activeEnd = -1
		var activeReg string
		for _, v := range ivs {
			if activeReg != "" && activeEnd < v.start {
				activeReg = ""
			}
			if activeReg != "" {
				continue
			}
			alloc[v.name] = "r2"
			activeReg = "r2"
			activeEnd = v.end
		}
	}
	return alloc
}

// armBlockClobbersR2 reports a block whose instruction selection uses r2 as a
// scratch register.
func armBlockClobbersR2(blk *Block) bool {
	for _, in := range blk.Insts {
		switch in.Op {
		case "call", "sdiv", "udiv", "srem", "urem":
			return true
		case "add", "sub", "mul", "shl", "lshr", "ashr", "and", "or", "xor",
			"icmp", "select",
			"trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr":
			if in.Ty != nil && in.Ty.Size() > 4 {
				return true
			}
		case "load":
			// An 8-byte load takes the high word in r2; larger aggregates
			// only use r1.
			if in.Ty.Size() == 8 {
				return true
			}
		case "store":
			if in.Ty.Size() > 4 {
				return true
			}
		case "getelementptr":
			// A non-power-of-two element size is scaled through r2.
			if sz := in.Ty.Size(); sz > 0 && sz&(sz-1) != 0 {
				for _, op := range in.Ops[1:] {
					if op.Kind == VLocal {
						return true
					}
				}
			}
		}
	}
	return false
}

// armOperandCostsMove mirrors rv32OperandCostsMove for the ARM emitter.  A
// return value is read from r0, so it costs nothing; only second operands and
// store addresses need a copy.
func armOperandCostsMove(op string, i int) bool {
	switch op {
	case "add", "sub", "mul", "sdiv", "udiv", "srem", "urem",
		"shl", "lshr", "ashr", "and", "or", "xor", "icmp", "getelementptr":
		return i >= 1
	case "store":
		return i == 1
	case "load", "br", "switch", "select", "ret", "unreachable",
		"trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr",
		"alloca", "phi", "call":
		return false
	}
	return true
}
