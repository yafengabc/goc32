package native32

// Load forwarding for scalar allocas whose address never escapes.
//
// findDirectAllocas proves that such an object is only ever the pointer operand
// of a load or a store, so every write to it is visible in the IR and a call
// cannot change it.  A load can therefore be replaced by the SSA value the
// previous access left in that cell: inside one block this is a plain forward
// scan, and across blocks a must-analysis propagates a value only when every
// predecessor agrees on the exact same SSA name.
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

// storeTarget reports the tracked alloca written by a store and the SSA value
// that store leaves there.  Constants are not tracked: the consumer's operand
// type is not recorded, and an i64 constant would lose its high word.
func storeTarget(in *Inst, size map[string]int) (string, Value, bool) {
	if in.Op != "store" || in.Ty == nil || len(in.Ops) < 2 {
		return "", Value{}, false
	}
	p, v := in.Ops[1], in.Ops[0]
	if p.Kind != VLocal || size[p.Name] == 0 || in.Ty.Size() != size[p.Name] || v.Kind != VLocal {
		return "", Value{}, false
	}
	return p.Name, v, true
}

func sameAvail(a, b map[string]Value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || w.Kind != v.Kind || w.Name != v.Name {
			return false
		}
	}
	return true
}

func blockSuccessors(b *Block) []string {
	if len(b.Insts) == 0 {
		return nil
	}
	last := b.Insts[len(b.Insts)-1]
	switch last.Op {
	case "br":
		return last.Labels
	case "switch":
		out := append([]string{}, last.Labels...)
		for _, c := range last.Cases {
			out = append(out, c.Dest)
		}
		return out
	}
	return nil
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

	// Must-analysis over the CFG: in[b][a] is the value every predecessor
	// agrees the cell a holds on entry to b.  Blocks with no predecessors
	// start empty, and a loop back edge with no value keeps the header empty,
	// which only loses an optimisation, never correctness.
	preds := map[string][]*Block{}
	for _, blk := range f.Blocks {
		for _, s := range blockSuccessors(blk) {
			preds[s] = append(preds[s], blk)
		}
	}
	inAvail := map[string]map[string]Value{}
	outAvail := map[string]map[string]Value{}
	for pass := 0; pass <= len(f.Blocks); pass++ {
		changed := false
		for _, blk := range f.Blocks {
			cur := map[string]Value{}
			if ps := preds[blk.Label]; len(ps) > 0 {
				for name, v := range outAvail[ps[0].Label] {
					agrees := true
					for _, p := range ps[1:] {
						w, ok := outAvail[p.Label][name]
						if !ok || w.Kind != v.Kind || w.Name != v.Name {
							agrees = false
							break
						}
					}
					if agrees {
						cur[name] = v
					}
				}
			}
			if !sameAvail(cur, inAvail[blk.Label]) {
				inAvail[blk.Label] = cur
				changed = true
			}
			next := make(map[string]Value, len(cur))
			for k, v := range cur {
				next[k] = v
			}
			for _, in := range blk.Insts {
				if name, v, ok := storeTarget(in, size); ok {
					next[name] = v
				}
			}
			if !sameAvail(next, outAvail[blk.Label]) {
				outAvail[blk.Label] = next
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	subst := map[string]Value{} // load result -> value known to be in memory
	drop := map[*Inst]bool{}
	for _, blk := range f.Blocks {
		avail := map[string]Value{}
		for k, v := range inAvail[blk.Label] {
			avail[k] = v
		}
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
				if name, v, ok := storeTarget(in, size); ok {
					avail[name] = v
				}
			}
		}
	}
	if len(subst) == 0 {
		dceFunc(f)
		dropDeadAllocas(f)
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
	// The dropped loads may have been the only users of a chain of pure
	// instructions, so clean up immediately rather than waiting for a pass
	// that runs before this one.
	dceFunc(f)
	dropDeadAllocas(f)
}

// dropDeadAllocas removes a non-escaping local that is never read.  Its
// address cannot appear anywhere except the pointer operand of a store, so
// once forwarding has removed every load, the cell and its stores are dead.
func dropDeadAllocas(f *Func) {
	direct := findDirectAllocas(f)
	if len(direct) == 0 {
		return
	}
	loads := map[string]int{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op == "load" && len(in.Ops) == 1 &&
				in.Ops[0].Kind == VLocal && direct[in.Ops[0].Name] {
				loads[in.Ops[0].Name]++
			}
		}
	}
	dead := map[string]bool{}
	for name := range direct {
		if loads[name] == 0 {
			dead[name] = true
		}
	}
	if len(dead) == 0 {
		return
	}
	for _, blk := range f.Blocks {
		kept := blk.Insts[:0]
		for _, in := range blk.Insts {
			if in.Op == "alloca" && dead[in.Dst] {
				continue
			}
			if in.Op == "store" && len(in.Ops) == 2 &&
				in.Ops[1].Kind == VLocal && dead[in.Ops[1].Name] {
				continue
			}
			kept = append(kept, in)
		}
		blk.Insts = kept
	}
}
