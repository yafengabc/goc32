package native32

import "fmt"

// Single-call-site inlining.
//
// The front end emits every static helper as a real function, while clang -O2
// inlines the ones that are used once in a translation unit.  Those helpers
// pay a call, a prologue/epilogue and register saves for nothing.  Inlining a
// function with exactly one call site cannot duplicate code: the body moves to
// the call site, and the out-of-line copy is dropped once it has no callers and
// its address is never taken (the linker cannot drop it by itself because the
// emitter marks every function global).
//
// Only direct calls to definitions inside the same module are considered.  A
// recursive function is skipped, as are variadic functions and helpers the
// backend emits calls to directly (`__aeabi_*`, `memcpy`, ...).

func InlineSmall(m *Module, limit int) {
	if limit <= 0 {
		return
	}
	in := &irInliner{
		m:       m,
		fmap:    map[string]*Func{},
		inlined: map[string]bool{},
		limit:   limit,
	}
	for _, f := range m.Funcs {
		in.fmap[f.Name] = f
	}
	for pass := 0; pass < 256; pass++ {
		if !in.step() {
			break
		}
	}
	in.removeInlinedAway()
	dceModule(m)
}

type irInliner struct {
	m       *Module
	fmap    map[string]*Func
	seq     int
	inlined map[string]bool
	limit   int
}

func (in *irInliner) step() bool {
	counts := in.callCounts()
	recursive := in.recursiveSet()
	for _, caller := range in.m.Funcs {
		for bi, blk := range caller.Blocks {
			for ii, call := range blk.Insts {
				if call.Op != "call" || call.Indirect {
					continue
				}
				callee := in.fmap[call.Callee]
				if callee == nil || len(callee.Blocks) == 0 || callee == caller {
					continue
				}
				if callee.Variadic || callee.Name == "main" || inlineProtected(callee.Name) {
					continue
				}
				if counts[callee.Name] != 1 || recursive[callee.Name] {
					continue
				}
				if funcInsts(callee) > in.limit {
					continue
				}
				if in.inlineOne(caller, bi, ii, callee) {
					in.inlined[callee.Name] = true
					return true
				}
			}
		}
	}
	return false
}

func funcInsts(f *Func) int {
	n := 0
	for _, b := range f.Blocks {
		n += len(b.Insts)
	}
	return n
}

func (in *irInliner) callCounts() map[string]int {
	out := map[string]int{}
	for _, f := range in.m.Funcs {
		for _, b := range f.Blocks {
			for _, i := range b.Insts {
				if i.Op == "call" && !i.Indirect {
					out[i.Callee]++
				}
			}
		}
	}
	return out
}

func (in *irInliner) recursiveSet() map[string]bool {
	edges := map[string][]string{}
	for _, f := range in.m.Funcs {
		for _, b := range f.Blocks {
			for _, i := range b.Insts {
				if i.Op == "call" && !i.Indirect {
					if _, ok := in.fmap[i.Callee]; ok {
						edges[f.Name] = append(edges[f.Name], i.Callee)
					}
				}
			}
		}
	}
	out := map[string]bool{}
	for name := range in.fmap {
		seen := map[string]bool{}
		var reaches func(string) bool
		reaches = func(n string) bool {
			for _, e := range edges[n] {
				if e == name {
					return true
				}
				if seen[e] {
					continue
				}
				seen[e] = true
				if reaches(e) {
					return true
				}
			}
			return false
		}
		if reaches(name) {
			out[name] = true
		}
	}
	return out
}

func (in *irInliner) addressTaken() map[string]bool {
	out := map[string]bool{}
	var mark func(Value)
	mark = func(v Value) {
		switch v.Kind {
		case VGlobal, VGlobalOff:
			if _, ok := in.fmap[v.Name]; ok {
				out[v.Name] = true
			}
		case VAgg:
			for _, e := range v.Elems {
				mark(e)
			}
		}
	}
	for _, f := range in.m.Funcs {
		for _, b := range f.Blocks {
			for _, i := range b.Insts {
				for _, v := range i.Ops {
					mark(v)
				}
				for _, c := range i.Cases {
					mark(c.Val)
				}
			}
		}
	}
	for _, g := range in.m.Globals {
		mark(g.Init)
	}
	return out
}

// removeInlinedAway drops the out-of-line copy of a function that was inlined
// and has no callers left.  A function whose address is taken stays, and so do
// helpers the emitter names directly (division helpers, memcpy).
func (in *irInliner) removeInlinedAway() {
	counts := in.callCounts()
	addr := in.addressTaken()
	keep := map[*Func]bool{}
	for _, f := range in.m.Funcs {
		if !in.inlined[f.Name] || f.Name == "main" || inlineProtected(f.Name) || addr[f.Name] || counts[f.Name] > 0 {
			keep[f] = true
		}
	}
	out := in.m.Funcs[:0]
	for _, f := range in.m.Funcs {
		if keep[f] {
			out = append(out, f)
		}
	}
	in.m.Funcs = out
}

func inlineProtected(name string) bool {
	switch name {
	case "main", "memcpy", "memmove", "memset":
		return true
	}
	if len(name) > len("__aeabi_") && name[:len("__aeabi_")] == "__aeabi_" {
		return true
	}
	return false
}

type inlineCtx struct {
	contLab  string
	valMap   map[string]string
	labMap   map[string]string
	paramMap map[string]Value
}

func (c *inlineCtx) label(l string) string {
	if n, ok := c.labMap[l]; ok {
		return n
	}
	return l
}

func (c *inlineCtx) value(v Value) (Value, bool) {
	if v.Kind != VLocal {
		return v, true
	}
	if pv, ok := c.paramMap[v.Name]; ok {
		return pv, true
	}
	if n, ok := c.valMap[v.Name]; ok {
		v.Name = n
		return v, true
	}
	return v, true
}

func (c *inlineCtx) inst(in *Inst) (*Inst, bool) {
	out := &Inst{
		Op:       in.Op,
		Dst:      in.Dst,
		Ty:       in.Ty,
		SrcTy:    in.SrcTy,
		Pred:     in.Pred,
		Indirect: in.Indirect,
		Callee:   in.Callee,
		Align:    in.Align,
	}
	if in.Dst != "" {
		out.Dst = c.valMap[in.Dst]
	}
	for _, v := range in.Ops {
		nv, ok := c.value(v)
		if !ok {
			return nil, false
		}
		out.Ops = append(out.Ops, nv)
	}
	out.OpTys = append([]*Type{}, in.OpTys...)
	for _, cs := range in.Cases {
		nv, ok := c.value(cs.Val)
		if !ok {
			return nil, false
		}
		out.Cases = append(out.Cases, Case{Val: nv, Dest: c.label(cs.Dest)})
	}
	for _, l := range in.Labels {
		out.Labels = append(out.Labels, c.label(l))
	}
	if in.Indirect && in.Callee != "" {
		if pv, ok := c.paramMap[in.Callee]; ok {
			if pv.Kind != VLocal {
				return nil, false
			}
			out.Callee = pv.Name
		} else if n, ok := c.valMap[in.Callee]; ok {
			out.Callee = n
		}
	}
	return out, true
}

func (in *irInliner) inlineOne(caller *Func, bi, ii int, callee *Func) bool {
	blk := caller.Blocks[bi]
	call := blk.Insts[ii]
	if len(call.Ops) != len(callee.Params) {
		return false
	}
	hot := call.Dst != ""
	if hot && callee.Ret == nil {
		return false
	}
	if hot && callee.Ret.Kind == KVoid {
		return false
	}

	prefix := fmt.Sprintf("inl%d.", in.seq)
	in.seq++
	ctx := &inlineCtx{
		contLab:  prefix + "cont",
		valMap:   map[string]string{},
		labMap:   map[string]string{},
		paramMap: map[string]Value{},
	}
	for _, cb := range callee.Blocks {
		ctx.labMap[cb.Label] = prefix + cb.Label
	}
	for _, cb := range callee.Blocks {
		for _, ci := range cb.Insts {
			if ci.Dst != "" {
				ctx.valMap[ci.Dst] = prefix + ci.Dst
			}
		}
	}
	for i, p := range callee.Params {
		ctx.paramMap[p.Name] = call.Ops[i]
	}

	var cloned []*Block
	var retVals []Value
	var retLabs []string
	valueReturn := callee.Ret != nil && callee.Ret.Kind != KVoid
	for _, cb := range callee.Blocks {
		nb := &Block{Label: ctx.labMap[cb.Label]}
		for _, ci := range cb.Insts {
			if ci.Op == "ret" {
				if valueReturn {
					if len(ci.Ops) == 0 {
						return false
					}
					rv, ok := ctx.value(ci.Ops[0])
					if !ok {
						return false
					}
					retVals = append(retVals, rv)
				}
				retLabs = append(retLabs, nb.Label)
				nb.Insts = append(nb.Insts, &Inst{Op: "br", Labels: []string{ctx.contLab}})
				continue
			}
			ni, ok := ctx.inst(ci)
			if !ok {
				return false
			}
			nb.Insts = append(nb.Insts, ni)
		}
		cloned = append(cloned, nb)
	}
	if hot && len(retVals) == 0 {
		return false
	}

	pre := &Block{Label: blk.Label, Insts: append([]*Inst{}, blk.Insts[:ii]...)}
	pre.Insts = append(pre.Insts, &Inst{Op: "br", Labels: []string{ctx.labMap[callee.Blocks[0].Label]}})
	cont := &Block{Label: ctx.contLab, Insts: append([]*Inst{}, blk.Insts[ii+1:]...)}

	// A single return can be substituted directly into every use of the call
	// result.  More than one needs a phi in the continuation block.
	subst := false
	var repl Value
	if hot {
		if len(retVals) == 1 {
			repl = retVals[0]
			if repl.Kind != VLocal && hasIndirectUse(caller, call.Dst) {
				return false
			}
			subst = true
		} else {
			ty := call.Ty
			if ty == nil {
				ty = callee.Ret
			}
			phi := &Inst{Op: "phi", Dst: call.Dst, Ty: ty, Ops: retVals, Labels: retLabs}
			cont.Insts = append([]*Inst{phi}, cont.Insts...)
		}
	}

	newBlocks := make([]*Block, 0, len(caller.Blocks)+len(cloned)+1)
	newBlocks = append(newBlocks, caller.Blocks[:bi]...)
	newBlocks = append(newBlocks, pre)
	newBlocks = append(newBlocks, cloned...)
	newBlocks = append(newBlocks, cont)
	newBlocks = append(newBlocks, caller.Blocks[bi+1:]...)
	caller.Blocks = newBlocks
	if subst {
		substituteLocal(caller, call.Dst, repl)
	}
	return true
}

func hasIndirectUse(f *Func, name string) bool {
	for _, b := range f.Blocks {
		for _, i := range b.Insts {
			if i.Indirect && i.Callee == name {
				return true
			}
		}
	}
	return false
}

// substituteLocal replaces every use of a local in f with repl.  The defining
// instructions and the call result itself are not uses.
func substituteLocal(f *Func, name string, repl Value) {
	replace := func(v Value) Value {
		if v.Kind == VLocal && v.Name == name {
			return repl
		}
		return v
	}
	for _, b := range f.Blocks {
		for _, i := range b.Insts {
			for k := range i.Ops {
				i.Ops[k] = replace(i.Ops[k])
			}
			for k := range i.Cases {
				i.Cases[k].Val = replace(i.Cases[k].Val)
			}
			if i.Indirect && i.Callee == name && repl.Kind == VLocal {
				i.Callee = repl.Name
			}
		}
	}
}
