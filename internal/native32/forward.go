package native32

// Local load forwarding for scalar allocas whose address never escapes.
//
// findDirectAllocas proves that such an object is only ever the pointer operand
// of a load or a store, so every write to it is visible in the IR and a call
// cannot change it.  Inside one basic block a store followed by a load (or two
// loads with no store in between) therefore reads exactly the SSA value the
// previous access left there, and the load can be replaced by that value.
//
// This is the value-level half of mem2reg: the address half already removes
// the pointer arithmetic, and this removes the redundant loads the front end
// emits when a C local is read more than once.  The hottest locals are left
// alone because planRegVars keeps them in a register for the whole function;
// forwarding their loads would only move work from the load path to the
// liveness path.

func forwardDirectLoads(m *Module, regVars, minCount int) {
	for _, f := range m.Funcs {
		forwardLoadsFunc(f, regVars, minCount)
	}
}

func forwardLoadsFunc(f *Func, regVars, minCount int) {
	direct := findDirectAllocas(f)
	if len(direct) == 0 {
		return
	}
	skip := map[string]bool{}
	for i, c := range allocaCandidates(f, minCount) {
		if i >= regVars {
			break
		}
		skip[c.name] = true
	}
	size := map[string]int{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op == "alloca" && in.Dst != "" && in.Ty != nil && direct[in.Dst] && !skip[in.Dst] {
				size[in.Dst] = in.Ty.Size()
			}
		}
	}
	if len(size) == 0 {
		return
	}

	subst := map[string]Value{} // load result -> value known to be in memory
	drop := map[*Inst]bool{}
	for _, blk := range f.Blocks {
		avail := map[string]Value{} // alloca -> value its cell holds now
		for _, in := range blk.Insts {
			switch in.Op {
			case "load":
				if in.Dst == "" || in.Ty == nil || len(in.Ops) == 0 {
					continue
				}
				p := in.Ops[0]
				if p.Kind != VLocal || size[p.Name] == 0 || in.Ty.Size() != size[p.Name] {
					continue
				}
				if v, ok := avail[p.Name]; ok {
					subst[in.Dst] = v
					drop[in] = true
					continue
				}
				avail[p.Name] = Value{Kind: VLocal, Name: in.Dst}
			case "store":
				if in.Ty == nil || len(in.Ops) < 2 {
					continue
				}
				p, v := in.Ops[1], in.Ops[0]
				// Only SSA values are substituted: a constant has no single
				// IR name and the consumer's operand type is not recorded.
				if p.Kind != VLocal || size[p.Name] == 0 || in.Ty.Size() != size[p.Name] || v.Kind != VLocal {
					continue
				}
				avail[p.Name] = v
			}
		}
	}
	if len(subst) == 0 {
		return
	}
	resolve := func(v Value) Value {
		for i := 0; i < 64 && v.Kind == VLocal; i++ {
			n, ok := subst[v.Name]
			if !ok {
				break
			}
			v = n
		}
		return v
	}
	for _, blk := range f.Blocks {
		kept := blk.Insts[:0]
		for _, in := range blk.Insts {
			if drop[in] {
				continue
			}
			for i := range in.Ops {
				in.Ops[i] = resolve(in.Ops[i])
			}
			for i := range in.Cases {
				in.Cases[i].Val = resolve(in.Cases[i].Val)
			}
			if in.Indirect && in.Callee != "" {
				in.Callee = resolve(Value{Kind: VLocal, Name: in.Callee}).Name
			}
			kept = append(kept, in)
		}
		blk.Insts = kept
	}
}
