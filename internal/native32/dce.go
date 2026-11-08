package native32

// Dead-code elimination for the typed IR.  The front end emits every
// expression of a dead store in SSA form, and the assembly peephole can drop
// the store but not the computation that fed it.  Removing a pure instruction
// with no uses also removes the uses of its operands, so this iterates.
//
// Loads and calls are kept even when their result is unused: a load can fault
// and a call has side effects.  The exception is a load from a non-escaping
// alloca, which reads our own stack frame.  Stores and terminators have no
// result and are never candidates.

func dceModule(m *Module) {
	for _, f := range m.Funcs {
		dceFunc(f)
	}
}

func dceFunc(f *Func) {
	direct := findDirectAllocas(f)
	for {
		uses := map[string]int{}
		for _, blk := range f.Blocks {
			for _, in := range blk.Insts {
				for _, op := range in.Ops {
					if op.Kind == VLocal {
						uses[op.Name]++
					}
				}
				for _, c := range in.Cases {
					if c.Val.Kind == VLocal {
						uses[c.Val.Name]++
					}
				}
				// An indirect call names its callee outside Ops.
				if in.Indirect && in.Callee != "" {
					uses[in.Callee]++
				}
			}
		}
		changed := false
		for _, blk := range f.Blocks {
			kept := blk.Insts[:0]
			for _, in := range blk.Insts {
				if in.Dst != "" && uses[in.Dst] == 0 && removable(in, direct) {
					changed = true
					continue
				}
				kept = append(kept, in)
			}
			blk.Insts = kept
		}
		if !changed {
			return
		}
	}
}

// removable reports instructions whose result is not observable.  A load is
// normally kept even when unused because it may fault, but a load from a
// non-escaping alloca reads our own stack frame and cannot.
func removable(in *Inst, direct map[string]bool) bool {
	if pureInst(in.Op) {
		return true
	}
	if in.Op == "load" && len(in.Ops) > 0 && in.Ops[0].Kind == VLocal {
		return direct[in.Ops[0].Name]
	}
	return false
}

func pureInst(op string) bool {
	switch op {
	case "add", "sub", "mul", "sdiv", "udiv", "srem", "urem",
		"fadd", "fsub", "fmul", "fdiv", "frem",
		"shl", "lshr", "ashr", "and", "or", "xor",
		"icmp", "fcmp",
		"trunc", "zext", "sext", "fptrunc", "fpext",
		"fptoui", "fptosi", "uitofp", "sitofp",
		"ptrtoint", "inttoptr", "bitcast", "addrspacecast",
		"select", "getelementptr", "alloca", "freeze",
		"extractvalue", "insertvalue", "extractelement",
		"insertelement", "shufflevector", "phi":
		return true
	}
	return false
}
