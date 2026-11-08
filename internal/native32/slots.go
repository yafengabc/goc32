package native32

import "sort"

// Frame slot planning.  Every SSA value needs a slot, but most are short-lived
// temporaries inside one basic block.  Giving each a unique slot made frames
// huge (and on Thumb, large offsets cost an address calculation per access).
//
// Values that cross a block boundary (or are used by a phi, or are wider than
// one word) keep permanent slots.  A 4-byte value defined and used only in one
// block gets a slot from a per-block pool: the slot is returned as soon as the
// value's last use has been emitted, so the pool is only as large as the
// maximum number of simultaneously live block-local values.

func isAggregateAlloca(t *Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case KArray, KStruct:
		return true
	}
	return t.Size() > 8
}

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

func layoutSlots(f *Func, base, reserve int, skipSet map[string]string, thumb bool) *slotLayout {
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
	uses := map[string]int{}
	phiUses := map[string]bool{}
	addUse := func(v Value, bi int) {
		if v.Kind != VLocal {
			return
		}
		uses[v.Name]++
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
			if in.Dst == "" || in.Op == "alloca" || skipSet[in.Dst] != "" {
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

	// Mark the values that need dedicated slots.  In unique mode that is every
	// value; otherwise only values crossing blocks, used by a phi, or wider
	// than a word.
	permanent := map[string]bool{}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst == "" || in.Op == "alloca" || skipSet[in.Dst] != "" {
				continue
			}
			ty := dstTy(in)
			if uniqueMode || ty.Size() > 4 || ty.Align() > 4 || cross[in.Dst] || phiUses[in.Dst] || in.Op == "phi" {
				permanent[in.Dst] = true
			}
		}
	}

	// Simulate the per-block pool first, before placing anything else.  The
	// pool is the hot set, so it goes at the bottom of the frame where Thumb
	// and RVC have a short load/store offset.  Permanent slots move above it.
	poolIdx := map[string]int{}
	maxNext := 0
	if !uniqueMode {
		for _, blk := range f.Blocks {
			lastUse := blockLastUses(blk)
			free := []int{}
			next := 0
			pool := map[string]int{}
			freeName := func(name string) {
				if idx, ok := pool[name]; ok {
					free = append(free, idx)
					delete(pool, name)
				}
			}
			for i, in := range blk.Insts {
				if in.Dst != "" && !permanent[in.Dst] && in.Op != "alloca" && skipSet[in.Dst] == "" {
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
						poolIdx[in.Dst] = idx
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
	}
	putAlloca := func(in *Inst) {
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

	// Thumb needs short sp-relative offsets for hot scalar locals, so there
	// the scalar allocas sit at the bottom, the pool right above them, and
	// large aggregate allocas are pushed to the top of the frame (they are
	// reached through a rematerialised pointer anyway).  RV32 has a 12-bit
	// load/store offset, so it keeps the compact pool-first order.
	if thumb {
		for _, blk := range f.Blocks {
			for _, in := range blk.Insts {
				if in.Op == "alloca" && in.Dst != "" && !isAggregateAlloca(in.Ty) {
					putAlloca(in)
				}
			}
		}
	}

	// The pool is the hot set, so it goes near the bottom of the frame where
	// Thumb and RVC have a short load/store offset.
	poolBase := align(cur, 4)
	if uniqueMode {
		cur = poolBase
	} else {
		cur = poolBase + maxNext*4
	}

	if !thumb {
		for _, blk := range f.Blocks {
			for _, in := range blk.Insts {
				if in.Op == "alloca" && in.Dst != "" {
					putAlloca(in)
				}
			}
		}
	}

	// Parameters and permanent slots, hottest first.  This keeps the most
	// frequently read values at the short-offset end of the frame.
	type permItem struct {
		name string
		ty   *Type
		freq int
	}
	var items []permItem
	for _, p := range f.Params {
		ty := p.Ty
		if ty == nil {
			ty = &Type{Kind: KPtr}
		}
		items = append(items, permItem{p.Name, ty, uses[p.Name] + 1})
	}
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst == "" || in.Op == "alloca" || skipSet[in.Dst] != "" || !permanent[in.Dst] {
				continue
			}
			items = append(items, permItem{in.Dst, dstTy(in), uses[in.Dst] + 1})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].freq != items[j].freq {
			return items[i].freq > items[j].freq
		}
		return items[i].name < items[j].name
	})
	for _, it := range items {
		sz := it.ty.Size()
		if sz < 4 {
			sz = 4
		}
		cur = align(cur, max(4, min(8, it.ty.Align())))
		L.slots[it.name] = cur
		L.slotTy[it.name] = it.ty
		cur += sz
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

	// Large Thumb allocas go last: every scalar access above stays within a
	// short sp-relative offset.
	if thumb {
		for _, blk := range f.Blocks {
			for _, in := range blk.Insts {
				if in.Op == "alloca" && in.Dst != "" && isAggregateAlloca(in.Ty) {
					putAlloca(in)
				}
			}
		}
	}

	// Assign the pool slots at the bottom of the frame.  Hot block-local
	// values get the lowest indices: the pool can reuse an index over time,
	// so rank each index by the total number of accesses to its values.
	freq := map[int]int{}
	for name, idx := range poolIdx {
		freq[idx] += uses[name] + 1
	}
	order := make([]int, maxNext)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		if freq[order[a]] != freq[order[b]] {
			return freq[order[a]] > freq[order[b]]
		}
		return order[a] < order[b]
	})
	rank := make([]int, maxNext)
	for r, idx := range order {
		rank[idx] = r
	}
	for name, idx := range poolIdx {
		L.slots[name] = poolBase + rank[idx]*4
	}
	L.end = cur
	return L
}
