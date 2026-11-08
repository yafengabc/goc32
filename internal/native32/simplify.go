package native32

// No-op casts are pure copies in a 32-bit address space: ptrtoint/inttoptr
// between pointers and i32, bitcasts of same-width integers, and trunc/zext/
// sext whose source and destination have the same size.  gocl emits ptrtoint
// for every pointer argument that TinyOS declares as an i32, so removing these
// copies deletes a load/store pair each time.
//
// Replacing a cast result with its operand is always safe: the operand
// dominates the cast, and the back end has a single 32-bit register class, so
// the IR types do not affect code generation.

func simplifyModule(m *Module) {
	for _, f := range m.Funcs {
		simplifyFunc(f)
	}
}

func simplifyFunc(f *Func) {
	subst := map[string]Value{}
	drop := map[*Inst]bool{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst == "" || len(in.Ops) == 0 || !noopCast(in) {
				continue
			}
			subst[in.Dst] = in.Ops[0]
			drop[in] = true
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

func noopCast(in *Inst) bool {
	dst := 4
	if in.Ty != nil {
		dst = in.Ty.Size()
	}
	src := 4
	if in.SrcTy != nil {
		src = in.SrcTy.Size()
	}
	if src != dst {
		return false
	}
	switch in.Op {
	case "ptrtoint", "inttoptr":
		return true
	case "bitcast":
		// Equal-sized aggregates also reach 4 bytes, but the back end only
		// carries scalar values; leave those casts to it.
		return in.Ty != nil && in.SrcTy != nil &&
			scalar32(in.Ty.Kind) && scalar32(in.SrcTy.Kind)
	case "trunc", "zext", "sext":
		return true
	}
	return false
}

func scalar32(k TypeKind) bool {
	return k == KInt || k == KPtr || k == KFloat || k == KDouble
}
