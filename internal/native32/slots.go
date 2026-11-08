package native32

// Frame slot planning.  Every SSA value needs a slot, but most are short-lived
// temporaries inside one basic block.  Giving each a unique slot made frames
// huge (and on Thumb, large offsets cost an address calculation per access).
//
// Values that cross a block boundary (or are used by a phi, or are wider than
// one word) keep permanent slots.  A 4-byte value defined and used only in one
// block gets a slot from a per-block pool: the slot is returned as soon as the
// value's last use has been emitted, so the pool is only as large as the
// maximum number of simultaneously live block-local values.

func blockLastUses(blk *Block) map[string]int {
	m := map[string]int{}
	for i, in := range blk.Insts {
		for _, op := range in.Ops {
			if op.Kind == VLocal {
				m[op.Name] = i
			}
		}
		for _, c := range in.Cases {
			if c.Val.Kind == VLocal {
				m[c.Val.Name] = i
			}
		}
		if in.Indirect && in.Callee != "" {
			m[in.Callee] = i
		}
	}
	return m
}

type slotLayout struct {
	slots   map[string]int
	allocas map[string]int
	phiTmp  map[string]int
	slotTy  map[string]*Type
	cross   map[string]bool
	end     int
}

func layoutSlots(f *Func, base, reserve int) *slotLayout {
	L := &slotLayout{
		slots:   map[string]int{},
		allocas: map[string]int{},
		phiTmp:  map[string]int{},
		slotTy:  map[string]*Type{},
	}

	dstTy := func(in *Inst) *Type {
		ty := in.Ty
		if ty == nil {
			ty = &Type{Kind: KPtr}
		}
		return ty
	}

	// Collect definitions and uses per block.  A value used in another block
	// (or by a phi, whose copies run in the predecessor) must not live in the
	// per-block pool.
	defBlock := map[string]int{}
	cross := map[string]bool{}
	useBlocks := map[string]map[int]bool{}
	phiUses := map[string]bool{}
	addUse := func(v Value, bi int) {
		if v.Kind != VLocal {
			return
		}
		if useBlocks[v.Name] == nil {
			useBlocks[v.Name] = map[int]bool{}
		}
		useBlocks[v.Name][bi] = true
	}
	for bi, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst != "" {
				defBlock[in.Dst] = bi
			}
			for _, op := range in.Ops {
				addUse(op, bi)
			}
			for _, c := range in.Cases {
				addUse(c.Val, bi)
			}
			if in.Indirect && in.Callee != "" {
				addUse(Value{Kind: VLocal, Name: in.Callee}, bi)
			}
			if in.Op == "phi" {
				for _, op := range in.Ops {
					if op.Kind == VLocal {
						phiUses[op.Name] = true
					}
				}
			}
		}
	}
	for name, bs := range useBlocks {
		db := defBlock[name]
		for b := range bs {
			if b != db {
				cross[name] = true
			}
		}
	}
	L.cross = cross

	cur := base + reserve

	// Estimate the unique-slot frame.  Small functions keep a slot per value:
	// the assembly peephole can then prove stores dead per value, which is
	// worth more than a slightly smaller frame.  Large frames (where offsets
	// need address calculations on Thumb or the t5/t6 helper on RV32) switch
	// to the pool below.
	est := cur
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "alloca" || in.Dst == "" {
				continue
			}
			est = align(est, in.Ty.Align())
			est += in.Ty.Size()
			if est < 4 {
				est = 4
			}
			if phiUses[in.Dst] || cross[in.Dst] {
				est = align(est, 4)
				est += 4
			}
		}
	}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst == "" || in.Op == "alloca" {
				continue
			}
			ty := dstTy(in)
			sz := ty.Size()
			if sz < 4 {
				sz = 4
			}
			est = align(est, max(4, min(8, ty.Align())))
			est += sz
		}
	}
	for _, p := range f.Params {
		sz := p.Ty.Size()
		if sz < 4 {
			sz = 4
		}
		est = align(est, 4)
		est += sz
	}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "phi" {
				continue
			}
			sz := dstTy(in).Size()
			if sz < 4 {
				sz = 4
			}
			est = align(est, 4)
			est += sz
		}
	}
	uniqueMode := est-cur <= 2048

	// Alloca objects.  The pointer slot is only needed when the pointer is
	// copied by a phi; otherwise the back end rematerialises sp+offset.
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "alloca" || in.Dst == "" {
				continue
			}
			cur = align(cur, in.Ty.Align())
			L.allocas[in.Dst] = cur
			cur += in.Ty.Size()
			if cur < 4 {
				cur = 4
			}
			if phiUses[in.Dst] || cross[in.Dst] {
				cur = align(cur, 4)
				L.slots[in.Dst] = cur
				L.slotTy[in.Dst] = dstTy(in)
				cur += 4
			}
		}
	}

	// Permanent slots: parameters, values live across blocks, values used by
	// a phi, and values wider than a word.
	for _, p := range f.Params {
		sz := p.Ty.Size()
		if sz < 4 {
			sz = 4
		}
		cur = align(cur, 4)
		L.slots[p.Name] = cur
		L.slotTy[p.Name] = p.Ty
		cur += sz
	}
	permanent := map[string]bool{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst == "" || in.Op == "alloca" {
				continue
			}
			ty := dstTy(in)
			if uniqueMode || ty.Size() > 4 || ty.Align() > 4 || cross[in.Dst] || phiUses[in.Dst] || in.Op == "phi" {
				sz := ty.Size()
				if sz < 4 {
					sz = 4
				}
				cur = align(cur, max(4, min(8, ty.Align())))
				L.slots[in.Dst] = cur
				L.slotTy[in.Dst] = ty
				cur += sz
				permanent[in.Dst] = true
			}
		}
	}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "phi" {
				continue
			}
			sz := dstTy(in).Size()
			if sz < 4 {
				sz = 4
			}
			cur = align(cur, 4)
			L.phiTmp[in.Dst] = cur
			cur += sz
		}
	}

	// Pool for block-local words.  Slots are indices into a pool based at
	// poolBase; the pool is reset for each block because a block-local value
	// is dead by the end of its block.
	if uniqueMode {
		L.end = cur
		return L
	}
	poolBase := cur
	pool := map[string]int{} // value -> pool index
	maxNext := 0
	for _, blk := range f.Blocks {
		lastUse := map[string]int{}
		for i, in := range blk.Insts {
			for _, op := range in.Ops {
				if op.Kind == VLocal {
					lastUse[op.Name] = i
				}
			}
			for _, c := range in.Cases {
				if c.Val.Kind == VLocal {
					lastUse[c.Val.Name] = i
				}
			}
			if in.Indirect && in.Callee != "" {
				lastUse[in.Callee] = i
			}
		}
		free := []int{}
		next := 0
		pool = map[string]int{}
		freeName := func(name string) {
			if idx, ok := pool[name]; ok {
				free = append(free, idx)
				delete(pool, name)
			}
		}
		for i, in := range blk.Insts {
			if in.Dst != "" && !permanent[in.Dst] && in.Op != "alloca" {
				ty := dstTy(in)
				if ty.Size() <= 4 && ty.Align() <= 4 {
					var idx int
					if len(free) > 0 {
						idx = free[len(free)-1]
						free = free[:len(free)-1]
					} else {
						idx = next
						next++
					}
					pool[in.Dst] = idx
					L.slots[in.Dst] = poolBase + idx*4
					L.slotTy[in.Dst] = ty
				}
			}
			for _, op := range in.Ops {
				if op.Kind == VLocal && lastUse[op.Name] == i {
					freeName(op.Name)
				}
			}
			for _, c := range in.Cases {
				if c.Val.Kind == VLocal && lastUse[c.Val.Name] == i {
					freeName(c.Val.Name)
				}
			}
			if in.Indirect && in.Callee != "" && lastUse[in.Callee] == i {
				freeName(in.Callee)
			}
			if in.Dst != "" && lastUse[in.Dst] == i {
				freeName(in.Dst)
			}
		}
		if next > maxNext {
			maxNext = next
		}
	}
	cur = poolBase + maxNext*4
	L.end = cur
	return L
}
