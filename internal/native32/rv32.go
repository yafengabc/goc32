package native32

// rv32.go emits RV32IM assembly (GNU syntax, assembler = clang/llvm-mc) from
// the parsed IR. It is deliberately a stack machine: every SSA value gets a
// frame slot, expressions are computed through t0/t1, and no register
// allocation is attempted. That is slow but easy to verify, and it is the
// first milestone before an internal assembler/linker exists.

import (
	"fmt"
	"strings"
)

type rv32 struct {
	m        *Module
	b        strings.Builder
	fn       *Func
	frame    int
	outBytes int
	slots    map[string]int
	slotTy   map[string]*Type
	allocas  map[string]int
	direct   map[string]bool
	regVar   map[string]string
	paramReg map[string]string
	sRegs    []string
	sRegOff  map[string]int
	regAlias map[string]string
	aliasVar map[string]string
	paramTgt map[string]string
	skipInst map[*Inst]bool
	cross    map[string]bool
	lastUse  map[string]int
	instIdx  int
	phiTmp   map[string]int
	saveOff  int
	epilogue string
	nFixed   int
	cur      string
	loops    []loopCtx
}

type loopCtx struct{ brk, cont string }

func EmitRV32(m *Module) (string, error) {
	e := &rv32{m: m}
	e.b.WriteString("\t.text\n")
	for _, g := range m.Globals {
		e.emitGlobal(g)
	}
	for _, f := range m.Funcs {
		if err := e.emitFunc(f); err != nil {
			return "", fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	e.b.WriteString("\t.section .note.GNU-stack,\"\",@progbits\n")
	return peepholeRV32(e.b.String()), nil
}

func (e *rv32) line(f string, a ...interface{}) {
	fmt.Fprintf(&e.b, "\t"+f+"\n", a...)
}

func (e *rv32) label(l string) { fmt.Fprintf(&e.b, "%s:\n", l) }

func (e *rv32) emitGlobal(g *Global) {
	if g.Ty == nil {
		return
	}
	if g.Name == "_fltused" || strings.HasPrefix(g.Name, "__goc_") {
		return
	}
	size := g.Ty.Size()
	zero := g.Init.Kind == VZero
	sec := ".data." + sanitize(g.Name)
	if g.Name == "G_tos_stack_note" || g.Name == "G_tos_heap_note" {
		// TOS_STACK/TOS_HEAP carry a section attribute the front end does
		// not model. The kernel reads these two notes out of PT_NOTE before
		// sizing the task window, so put them where user.ld expects them.
		sec = ".note.tos"
		e.b.WriteString("\t.section " + sec + ",\"a\",@note\n")
	} else if zero {
		sec = ".bss." + sanitize(g.Name)
		e.b.WriteString("\t.section " + sec + ",\"aw\",@nobits\n")
	} else if g.Const {
		sec = ".rodata." + sanitize(g.Name)
		e.b.WriteString("\t.section " + sec + ",\"a\",@progbits\n")
	} else {
		e.b.WriteString("\t.section " + sec + ",\"aw\",@progbits\n")
	}
	local := g.Private ||
		strings.HasPrefix(g.Name, ".const.") ||
		strings.HasPrefix(g.Name, ".str.") ||
		strings.HasPrefix(g.Name, "G_st_") ||
		strings.HasPrefix(g.Name, ".L")
	if !local {
		e.b.WriteString("\t.globl " + g.Name + "\n")
	}
	al := g.Align
	if al < g.Ty.Align() {
		al = g.Ty.Align()
	}
	e.b.WriteString(fmt.Sprintf("\t.balign %d\n%s:\n", al, g.Name))
	if zero {
		e.b.WriteString(fmt.Sprintf("\t.zero %d\n", size))
		return
	}
	e.emitInit(g.Ty, g.Init)
	if n := size - g.Init.Size(g.Ty); n > 0 {
		e.b.WriteString(fmt.Sprintf("\t.zero %d\n", n))
	}
}

func (v Value) Size(t *Type) int {
	switch v.Kind {
	case VZero, VUndef, VNull:
		return t.Size()
	case VBytes:
		return len(v.B)
	case VInt, VBool:
		return t.Size()
	case VGlobal, VGlobalOff:
		return t.Size()
	case VAgg:
		n := 0
		switch t.Kind {
		case KArray:
			for _, el := range v.Elems {
				n += el.Size(t.Elem)
			}
			if n < t.Size() {
				n = t.Size()
			}
		case KStruct:
			for i, el := range v.Elems {
				if i < len(t.Fields) {
					n = align(n, t.Fields[i].Align()) + el.Size(t.Fields[i])
				}
			}
			if n < t.Size() {
				n = t.Size()
			}
		default:
			n = t.Size()
		}
		return n
	}
	return t.Size()
}

func (e *rv32) emitInit(t *Type, v Value) {
	if t == nil {
		return
	}
	switch v.Kind {
	case VZero, VUndef, VNull:
		e.b.WriteString(fmt.Sprintf("\t.zero %d\n", t.Size()))
	case VBytes:
		for _, c := range v.B {
			e.b.WriteString(fmt.Sprintf("\t.byte %d\n", c))
		}
	case VInt:
		switch t.Size() {
		case 1:
			e.b.WriteString(fmt.Sprintf("\t.byte %d\n", byte(v.U)))
		case 2:
			e.b.WriteString(fmt.Sprintf("\t.half %d\n", uint16(v.U)))
		case 4:
			e.b.WriteString(fmt.Sprintf("\t.word %d\n", uint32(v.U)))
		case 8:
			e.b.WriteString(fmt.Sprintf("\t.dword %d\n", v.U))
		default:
			e.b.WriteString(fmt.Sprintf("\t.zero %d\n", t.Size()))
		}
	case VBool:
		e.b.WriteString(fmt.Sprintf("\t.byte %d\n", byte(v.U&1)))
	case VGlobal:
		e.b.WriteString(fmt.Sprintf("\t.word %s\n", v.Name))
	case VGlobalOff:
		if v.I == 0 {
			e.b.WriteString(fmt.Sprintf("\t.word %s\n", v.Name))
		} else if v.I > 0 {
			e.b.WriteString(fmt.Sprintf("\t.word %s+%d\n", v.Name, v.I))
		} else {
			e.b.WriteString(fmt.Sprintf("\t.word %s-%d\n", v.Name, -v.I))
		}
	case VAgg:
		e.emitAgg(t, v)
	default:
		e.b.WriteString(fmt.Sprintf("\t.zero %d\n", t.Size()))
	}
}

func (e *rv32) emitAgg(t *Type, v Value) {
	switch t.Kind {
	case KArray:
		for _, el := range v.Elems {
			e.emitInit(t.Elem, el)
		}
		if n := len(v.Elems); n < t.N {
			e.b.WriteString(fmt.Sprintf("\t.zero %d\n", (t.N-n)*t.Elem.Size()))
		}
	case KStruct:
		off := 0
		for i, f := range t.Fields {
			off = align(off, f.Align())
			if i < len(v.Elems) {
				e.emitInit(f, v.Elems[i])
			} else {
				e.b.WriteString(fmt.Sprintf("\t.zero %d\n", f.Size()))
			}
			off += f.Size()
		}
		if n := t.Size() - off; n > 0 {
			e.b.WriteString(fmt.Sprintf("\t.zero %d\n", n))
		}
	default:
		e.emitInit(t, v)
	}
}

// ---- frame layout -------------------------------------------------------

func (e *rv32) emitFunc(f *Func) error {
	e.fn = f
	e.slots = map[string]int{}
	e.slotTy = map[string]*Type{}
	e.allocas = map[string]int{}
	e.phiTmp = map[string]int{}
	e.nFixed = len(f.Params)
	e.saveOff = 0
	e.loops = nil
	e.outBytes = 0
	e.frame = 0

	// Pass 1: collect outgoing stack-argument bytes.
	maxStack := 0
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op == "call" {
				if n := callStackWords(in); n > maxStack {
					maxStack = n
				}
			}
		}
	}
	e.outBytes = align(maxStack*4, 8)

	// Pass 2: assign slots.  Permanent values (parameters, values live
	// across blocks, wide values) keep dedicated slots; short-lived words
	// share a per-block pool.  Alloca pointers are rematerialised as
	// sp+offset unless a phi copies them.
	e.direct = findDirectAllocas(f)
	regs := []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7"}
	e.regVar, e.sRegs = planRegVars(f, regs, 4)
	var usedParams []string
	e.paramReg, usedParams = planParamRegs(f, remainingRegs(regs, e.sRegs), 3)
	e.sRegs = append(e.sRegs, usedParams...)
	e.regAlias = map[string]string{}
	e.aliasVar = map[string]string{}
	L := layoutSlots(f, e.outBytes, 0, nil)
	e.slots = L.slots
	e.allocas = L.allocas
	e.phiTmp = L.phiTmp
	e.slotTy = L.slotTy
	e.cross = L.cross
	e.paramTgt, e.skipInst = findParamStores(f, e.direct, e.regVar)
	cur := L.end
	e.sRegOff = map[string]int{}
	for _, r := range e.sRegs {
		cur = align(cur, 4)
		e.sRegOff[r] = cur
		cur += 4
	}
	// ra save slot, below the vararg save area.
	cur = align(cur, 4)
	raOff := cur
	cur += 4
	if f.Variadic {
		cur = align(cur, 8)
		e.saveOff = cur
		cur += 8 * 4
	}
	e.frame = align(cur, 16)
	if e.frame < 16 {
		e.frame = 16
	}
	// ra lives below the vararg save area, so keep the save area at the top
	// and contiguous with the caller's stack arguments.
	e.b.WriteString("\t.section .text." + sanitize(f.Name) + ",\"ax\",@progbits\n")
	if !strings.HasPrefix(f.Name, ".") {
		e.b.WriteString("\t.globl " + f.Name + "\n")
	}
	e.b.WriteString("\t.type " + f.Name + ",@function\n")
	e.label(sym(f.Name))
	e.adjustSP(-e.frame)
	e.storeSlot(raOff, "ra")
	for _, r := range e.sRegs {
		e.storeSlot(e.sRegOff[r], r)
	}
	if f.Variadic {
		for i := 0; i < 8; i++ {
			e.storeSlotKeep(e.saveOff+i*4, fmt.Sprintf("a%d", i))
		}
	}
	// Spill parameters.  A hot word parameter goes straight into its
	// callee-saved register instead of a memory slot.
	ri := 0
	for _, p := range f.Params {
		sz := p.Ty.Size()
		words := (sz + 3) / 4
		if words < 1 {
			words = 1
		}
		off := e.slots[p.Name]
		for w := 0; w < words; w++ {
			if target := e.paramTgt[p.Name]; target != "" && words == 1 {
				src := ""
				if ri < 8 {
					src = fmt.Sprintf("a%d", ri)
				} else {
					e.loadSlot(e.frame+(ri-8)*4, "t0")
					src = "t0"
				}
				if reg := e.regVar[target]; reg != "" {
					e.line("mv %s, %s", reg, src)
				} else {
					e.storeSlot(e.allocas[target], src)
				}
				ri++
				continue
			}
			if reg := e.paramReg[p.Name]; reg != "" && words == 1 {
				if ri < 8 {
					e.line("mv %s, a%d", reg, ri)
				} else {
					e.loadSlot(e.frame+(ri-8)*4, "t0")
					e.line("mv %s, t0", reg)
				}
				ri++
				continue
			}
			if ri < 8 {
				e.storeSlot(off+w*4, fmt.Sprintf("a%d", ri))
			} else {
				e.loadSlot(e.frame+(ri-8)*4, "t0")
				e.storeSlot(off+w*4, "t0")
			}
			ri++
		}
	}
	e.epilogue = fmt.Sprintf(".Lret_%s", sanitize(f.Name))
	// Blocks.
	for i, blk := range f.Blocks {
		if i == 0 {
			// entry already has the prologue; its phis are unreachable
			// except through a branch, whose copies are emitted there.
		}
		e.label(blockLabel(f, blk.Label))
		e.cur = blk.Label
		e.regAlias = map[string]string{}
		e.aliasVar = map[string]string{}
		e.lastUse = blockLastUses(blk)
		for i, in := range blk.Insts {
			e.instIdx = i
			if err := e.emitInst(in); err != nil {
				return err
			}
		}
	}
	// Epilogue.
	e.label(e.epilogue)
	for _, r := range e.sRegs {
		e.loadSlot(e.sRegOff[r], r)
	}
	e.loadSlot(raOff, "ra")
	e.adjustSP(e.frame)
	e.line("ret")
	e.b.WriteString(fmt.Sprintf("\t.size %s, .-%s\n\n", f.Name, f.Name))
	return nil
}

func sym(name string) string {
	if name == "" {
		return name
	}
	if name[0] >= '0' && name[0] <= '9' {
		return "L" + name
	}
	return name
}

func sanitize(s string) string {
	r := strings.NewReplacer(".", "_", "-", "_", "@", "_", "$", "_")
	return r.Replace(s)
}

func blockLabel(f *Func, name string) string {
	return fmt.Sprintf(".L%s_%s", sanitize(f.Name), sanitize(name))
}

func (e *rv32) slot(name string) int { return e.slots[name] }

// loadSlot/storeSlot handle frame offsets beyond the 12-bit signed immediate
// of lw/sw. Deep or unoptimised functions easily exceed 2 KiB of slots.
// t5/t6 are not used by the instruction emitter itself, so they are safe
// temporaries even while t0..t4 hold a live 64-bit value.
func (e *rv32) loadSlot(off int, reg string) {
	if off >= -2048 && off <= 2047 {
		e.line("lw %s, %d(sp)", reg, off)
		return
	}
	scratch := "t5"
	if reg == "t5" {
		scratch = "t6"
	}
	e.line("li %s, %d", scratch, off)
	e.line("add %s, sp, %s", scratch, scratch)
	e.line("lw %s, 0(%s)", reg, scratch)
}

func (e *rv32) storeSlot(off int, reg string) {
	if off >= -2048 && off <= 2047 {
		e.line("sw %s, %d(sp)", reg, off)
		return
	}
	scratch := "t5"
	if reg == "t5" {
		scratch = "t6"
	}
	e.line("li %s, %d", scratch, off)
	e.line("add %s, sp, %s", scratch, scratch)
	e.line("sw %s, 0(%s)", reg, scratch)
}

// storeSlotKeep is storeSlot for values another function reads (outgoing stack
// arguments and the vararg register save area).  The peephole must not treat
// the absence of a local load as proof that the store is dead.
func (e *rv32) storeSlotKeep(off int, reg string) {
	if off >= -2048 && off <= 2047 {
		e.line("sw %s, %d(sp) # goc32keep", reg, off)
		return
	}
	scratch := "t5"
	if reg == "t5" {
		scratch = "t6"
	}
	e.line("li %s, %d", scratch, off)
	e.line("add %s, sp, %s", scratch, scratch)
	e.line("sw %s, 0(%s) # goc32keep", reg, scratch)
}

// loadSized reads 1, 2 or 4 bytes from a frame offset into reg.  The direct
// alloca path uses it so a char/short/int local costs one load and no pointer.
func (e *rv32) loadSized(off, size int, reg string) {
	op := "lw"
	switch size {
	case 1:
		op = "lbu"
	case 2:
		op = "lhu"
	}
	if off >= -2048 && off <= 2047 {
		e.line("%s %s, %d(sp)", op, reg, off)
		return
	}
	scratch := "t5"
	if reg == "t5" {
		scratch = "t6"
	}
	e.line("li %s, %d", scratch, off)
	e.line("add %s, sp, %s", scratch, scratch)
	e.line("%s %s, 0(%s)", op, reg, scratch)
}

func (e *rv32) storeSized(off, size int, reg string) {
	op := "sw"
	switch size {
	case 1:
		op = "sb"
	case 2:
		op = "sh"
	}
	if off >= -2048 && off <= 2047 {
		e.line("%s %s, %d(sp)", op, reg, off)
		return
	}
	scratch := "t5"
	if reg == "t5" {
		scratch = "t6"
	}
	e.line("li %s, %d", scratch, off)
	e.line("add %s, sp, %s", scratch, scratch)
	e.line("%s %s, 0(%s)", op, reg, scratch)
}

func (e *rv32) addrSp(off int, reg string) {
	if off >= -2048 && off <= 2047 {
		e.line("addi %s, sp, %d", reg, off)
		return
	}
	e.line("li %s, %d", reg, off)
	e.line("add %s, sp, %s", reg, reg)
}

func (e *rv32) adjustSP(n int) {
	if n >= -2048 && n <= 2047 {
		e.line("addi sp, sp, %d", n)
		return
	}
	// li carries the sign, so a single add is enough. Subtracting the
	// immediate as well (as this did) turned a negative frame into an
	// upward move and every >2 KiB frame wrote above the task window.
	e.line("li t0, %d", n)
	e.line("add sp, sp, t0")
}

func (e *rv32) addImm(reg string, imm int) {
	if imm >= -2048 && imm <= 2047 {
		e.line("addi %s, %s, %d", reg, reg, imm)
		return
	}
	e.line("li t5, %d", imm)
	e.line("add %s, %s, t5", reg, reg)
}

func (e *rv32) loadVal(v Value, reg string) error {
	switch v.Kind {
	case VLocal:
		if src, ok := e.paramReg[v.Name]; ok {
			if reg != src {
				e.line("mv %s, %s", reg, src)
			}
			return nil
		}
		if src, ok := e.regAlias[v.Name]; ok {
			if reg != src {
				e.line("mv %s, %s", reg, src)
			}
			return nil
		}
		if off, ok := e.allocas[v.Name]; ok {
			e.addrSp(off, reg)
			return nil
		}
		off, ok := e.slots[v.Name]
		if !ok {
			return fmt.Errorf("unknown local %%%s", v.Name)
		}
		e.loadSlot(off, reg)
	case VGlobal:
		e.line("la %s, %s", reg, v.Name)
	case VInt:
		e.line("li %s, %d", reg, v.I)
	case VBool:
		e.line("li %s, %d", reg, v.U&1)
	case VNull, VUndef:
		e.line("li %s, 0", reg)
	default:
		return fmt.Errorf("cannot materialise value %s", v)
	}
	return nil
}

// loadValTo returns the register that holds v.  A value produced by a load
// from a register variable is already live in its callee-saved register, so
// the consumer can use it directly instead of making a copy in a scratch.
func (e *rv32) loadValTo(v Value, scratch string) (string, error) {
	if v.Kind == VLocal {
		if src, ok := e.paramReg[v.Name]; ok {
			return src, nil
		}
		if src, ok := e.regAlias[v.Name]; ok {
			return src, nil
		}
	}
	if err := e.loadVal(v, scratch); err != nil {
		return "", err
	}
	return scratch, nil
}

func (e *rv32) nextInst() *Inst {
	for _, blk := range e.fn.Blocks {
		if blk.Label != e.cur {
			continue
		}
		if e.instIdx+1 < len(blk.Insts) {
			return blk.Insts[e.instIdx+1]
		}
		return nil
	}
	return nil
}

func (e *rv32) storeDst(name string, reg string) error {
	off, ok := e.slots[name]
	if !ok {
		return fmt.Errorf("unknown destination %%%s", name)
	}
	e.storeSlot(off, reg)
	return nil
}

func (e *rv32) loadPair(v Value, lo, hi string) error {
	if v.Kind == VLocal {
		off, ok := e.slots[v.Name]
		if !ok {
			return fmt.Errorf("unknown local %%%s", v.Name)
		}
		e.loadSlot(off, lo)
		e.loadSlot(off+4, hi)
		return nil
	}
	if err := e.loadVal(v, lo); err != nil {
		return err
	}
	if v.Kind == VInt {
		e.line("li %s, %d", hi, int64(int32(v.I>>63)))
	} else {
		e.line("li %s, 0", hi)
	}
	return nil
}

func (e *rv32) emitInst(in *Inst) error {
	switch in.Op {
	case "alloca":
		if e.direct[in.Dst] {
			return nil
		}
		if _, ok := e.slots[in.Dst]; !ok {
			// Rematerialised as sp+offset at each use.
			return nil
		}
		off := e.allocas[in.Dst]
		e.addrSp(off, "t0")
		return e.storeDst(in.Dst, "t0")
	case "load":
		if v := in.Ops[0]; v.Kind == VLocal && e.direct[v.Name] {
			off := e.allocas[v.Name]
			if reg := e.regVar[v.Name]; reg != "" {
				if !e.cross[in.Dst] {
					e.regAlias[in.Dst] = reg
					e.aliasVar[in.Dst] = v.Name
					return nil
				}
				e.line("mv t1, %s", reg)
			} else if in.Ty.Size() == 8 {
				e.loadSlot(off, "t1")
				e.loadSlot(off+4, "t2")
				dst := e.slots[in.Dst]
				e.storeSlot(dst, "t1")
				e.storeSlot(dst+4, "t2")
				return nil
			} else {
				e.loadSized(off, in.Ty.Size(), "t1")
			}
			return e.storeDst(in.Dst, "t1")
		}
		if err := e.loadVal(in.Ops[0], "t0"); err != nil {
			return err
		}
		sz := in.Ty.Size()
		switch sz {
		case 1:
			e.line("lbu t1, 0(t0)")
		case 2:
			e.line("lhu t1, 0(t0)")
		case 4:
			e.line("lw t1, 0(t0)")
		case 8:
			e.line("lw t1, 0(t0)")
			e.line("lw t2, 4(t0)")
			off := e.slots[in.Dst]
			e.storeSlot(off, "t1")
			e.storeSlot(off+4, "t2")
			return nil
		default:
			// Aggregate load: copy bytes into the destination slot.
			off := e.slots[in.Dst]
			for i := 0; i < sz; i += 4 {
				e.line("lw t1, %d(t0)", i)
				e.storeSlot(off+i, "t1")
			}
			return nil
		}
		return e.storeDst(in.Dst, "t1")
	case "store":
		if e.skipInst[in] {
			return nil
		}
		if v := in.Ops[1]; v.Kind == VLocal && e.direct[v.Name] {
			sz := in.Ty.Size()
			if reg := e.regVar[v.Name]; reg != "" {
				// A pending load of this variable is a snapshot of the old
				// value.  If the snapshot is still used after this store,
				// write it back to its slot before overwriting the register.
				for name, av := range e.aliasVar {
					if av != v.Name {
						continue
					}
					if e.lastUse[name] > e.instIdx {
						e.line("mv t1, %s", reg)
						if err := e.storeDst(name, "t1"); err != nil {
							return err
						}
					}
					delete(e.regAlias, name)
					delete(e.aliasVar, name)
				}
				src, err := e.loadValTo(in.Ops[0], "t0")
				if err != nil {
					return err
				}
				if src != reg {
					e.line("mv %s, %s", reg, src)
				}
				return nil
			}
			if sz == 8 {
				if err := e.loadPair(in.Ops[0], "t0", "t1"); err != nil {
					return err
				}
				off := e.allocas[v.Name]
				e.storeSlot(off, "t0")
				e.storeSlot(off+4, "t1")
				return nil
			}
			src, err := e.loadValTo(in.Ops[0], "t0")
			if err != nil {
				return err
			}
			e.storeSized(e.allocas[v.Name], sz, src)
			return nil
		}
		sz := in.Ty.Size()
		if sz == 1 || sz == 2 || sz == 4 {
			val, err := e.loadValTo(in.Ops[0], "t0")
			if err != nil {
				return err
			}
			ptr, err := e.loadValTo(in.Ops[1], "t1")
			if err != nil {
				return err
			}
			op := map[int]string{1: "sb", 2: "sh", 4: "sw"}[sz]
			e.line("%s %s, 0(%s)", op, val, ptr)
			return nil
		}
		if err := e.loadVal(in.Ops[0], "t0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "t1"); err != nil {
			return err
		}
		switch sz {
		case 8:
			e.loadSlot(e.slots[in.Ops[0].Name]+4, "t2")
			e.line("sw t0, 0(t1)")
			e.line("sw t2, 4(t1)")
		default:
			off := 0
			if in.Ops[0].Kind == VLocal {
				off = e.slots[in.Ops[0].Name]
			}
			for i := 0; i < sz; i += 4 {
				e.loadSlot(off+i, "t2")
				e.line("sw t2, %d(t1)", i)
			}
		}
		return nil
	case "getelementptr":
		return e.emitGEP(in)
	case "add", "sub", "mul", "sdiv", "udiv", "srem", "urem",
		"shl", "lshr", "ashr", "and", "or", "xor":
		return e.emitBin(in)
	case "icmp":
		return e.emitICmp(in)
	case "br":
		return e.emitBr(in)
	case "switch":
		return e.emitSwitch(in)
	case "ret":
		if in.Ty != nil && in.Ty.Kind != KVoid && len(in.Ops) > 0 {
			if in.Ty.Size() > 4 {
				if err := e.loadPair(in.Ops[0], "a0", "a1"); err != nil {
					return err
				}
			} else if err := e.loadVal(in.Ops[0], "a0"); err != nil {
				return err
			}
		}
		e.line("j %s", e.epilogue)
		return nil
	case "trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr":
		return e.emitCast(in)
	case "select":
		return e.emitSelect(in)
	case "phi":
		// Values are copied on the incoming edges.
		return nil
	case "call":
		return e.emitCall(in)
	case "unreachable":
		e.line("j %s", e.epilogue)
		return nil
	}
	return fmt.Errorf("unsupported instruction %s", in.Op)
}

func (e *rv32) emitGEP(in *Inst) error {
	base, err := e.loadValTo(in.Ops[0], "t0")
	if err != nil {
		return err
	}
	curReg := base
	cur := in.Ty
	addConst := func(off int) {
		if off == 0 {
			return
		}
		if curReg == "t0" {
			e.addImm("t0", off)
			return
		}
		if off >= -2048 && off <= 2047 {
			e.line("addi t0, %s, %d", curReg, off)
		} else {
			e.line("li t5, %d", off)
			e.line("add t0, %s, t5", curReg)
		}
		curReg = "t0"
	}
	for i := 1; i < len(in.Ops); i++ {
		idx := in.Ops[i]
		var scale int
		if i == 1 {
			scale = cur.Size()
		} else if cur != nil && cur.Kind == KArray {
			cur = cur.Elem
			scale = cur.Size()
		} else if cur != nil && cur.Kind == KStruct {
			// constant field index
			if idx.Kind != VInt {
				return fmt.Errorf("non-constant struct GEP index")
			}
			off := 0
			for j := 0; j < int(idx.I) && j < len(cur.Fields); j++ {
				off = align(off, cur.Fields[j].Align()) + cur.Fields[j].Size()
			}
			cur = cur.Fields[int(idx.I)]
			addConst(off)
			continue
		} else {
			scale = 1
		}
		if idx.Kind == VInt {
			addConst(int(idx.I) * scale)
			continue
		}
		idxReg, err := e.loadValTo(idx, "t1")
		if err != nil {
			return err
		}
		if scale == 1 {
			e.line("add t0, %s, %s", curReg, idxReg)
		} else {
			if scale&(scale-1) == 0 {
				k := 0
				for s := scale; s > 1; s >>= 1 {
					k++
				}
				e.line("slli t1, %s, %d", idxReg, k)
			} else {
				e.line("li t2, %d", scale)
				e.line("mul t1, %s, t2", idxReg)
			}
			e.line("add t0, %s, t1", curReg)
		}
		curReg = "t0"
	}
	return e.storeDst(in.Dst, curReg)
}

func (e *rv32) emitBin(in *Inst) error {
	wide := in.Ty.Size() > 4
	if wide {
		return e.emitBin64(in)
	}
	r0, err := e.loadValTo(in.Ops[0], "t0")
	if err != nil {
		return err
	}
	r1, err := e.loadValTo(in.Ops[1], "t1")
	if err != nil {
		return err
	}
	op := map[string]string{
		"add": "add", "sub": "sub", "mul": "mul",
		"sdiv": "div", "udiv": "divu", "srem": "rem", "urem": "remu",
		"shl": "sll", "lshr": "srl", "ashr": "sra",
		"and": "and", "or": "or", "xor": "xor",
	}[in.Op]
	if op == "" {
		return fmt.Errorf("unsupported binop %s", in.Op)
	}
	e.line("%s t0, %s, %s", op, r0, r1)
	return e.storeDst(in.Dst, "t0")
}

func (e *rv32) emitBin64(in *Inst) error {
	offa, offb, offd := e.slots[in.Ops[0].Name], e.slots[in.Ops[1].Name], e.slots[in.Dst]
	if in.Ops[0].Kind != VLocal || in.Ops[1].Kind != VLocal {
		// Constants/globals in 64-bit arithmetic: materialise low words.
		if err := e.loadVal(in.Ops[0], "t0"); err != nil {
			return err
		}
		e.storeSlot(offd, "t0")
		e.storeSlot(offd+4, "zero")
		if err := e.loadVal(in.Ops[1], "t1"); err != nil {
			return err
		}
		switch in.Op {
		case "add":
			e.line("add t0, t0, t1")
			e.storeSlot(offd, "t0")
		case "mul":
			e.line("mul t0, t0, t1")
			e.storeSlot(offd, "t0")
		case "and":
			e.line("and t0, t0, t1")
			e.storeSlot(offd, "t0")
		case "or":
			e.line("or t0, t0, t1")
			e.storeSlot(offd, "t0")
		case "xor":
			e.line("xor t0, t0, t1")
			e.storeSlot(offd, "t0")
		case "shl", "lshr", "ashr":
			if err := e.loadVal(in.Ops[0], "t0"); err != nil {
				return err
			}
			switch in.Op {
			case "shl":
				e.line("sll t0, t0, t1")
			case "lshr":
				e.line("srl t0, t0, t1")
			case "ashr":
				e.line("sra t0, t0, t1")
			}
			e.storeSlot(offd, "t0")
		}
		return nil
	}
	e.loadSlot(offa, "t0")
	e.loadSlot(offa+4, "t2")
	e.loadSlot(offb, "t1")
	e.loadSlot(offb+4, "t3")
	switch in.Op {
	case "add":
		e.line("add t0, t0, t1")
		e.line("sltu t4, t0, t1")
		e.line("add t2, t2, t3")
		e.line("add t2, t2, t4")
	case "sub":
		e.line("sub t0, t0, t1")
		e.line("sltu t4, t0, t1")
		e.line("sub t2, t2, t3")
		e.line("sub t2, t2, t4")
	case "mul":
		e.line("mul t0, t0, t1")
		e.line("li t2, 0")
	case "and":
		e.line("and t0, t0, t1")
		e.line("and t2, t2, t3")
	case "or":
		e.line("or t0, t0, t1")
		e.line("or t2, t2, t3")
	case "xor":
		e.line("xor t0, t0, t1")
		e.line("xor t2, t2, t3")
	case "shl":
		// Only the low 32 bits matter for our address arithmetic.
		e.line("sll t0, t0, t1")
		e.line("li t2, 0")
	case "lshr", "ashr":
		e.line("srl t0, t0, t1")
		e.line("li t2, 0")
	default:
		return fmt.Errorf("unsupported 64-bit op %s", in.Op)
	}
	e.storeSlot(offd, "t0")
	e.storeSlot(offd+4, "t2")
	return nil
}

func (e *rv32) emitICmp(in *Inst) error {
	wide := in.Ty.Size() > 4
	if wide {
		return e.emitICmp64(in)
	}
	r0, err := e.loadValTo(in.Ops[0], "t0")
	if err != nil {
		return err
	}
	r1, err := e.loadValTo(in.Ops[1], "t1")
	if err != nil {
		return err
	}
	switch in.Pred {
	case "eq":
		e.line("xor t0, %s, %s", r0, r1)
		e.line("seqz t0, t0")
	case "ne":
		e.line("xor t0, %s, %s", r0, r1)
		e.line("snez t0, t0")
	case "slt":
		e.line("slt t0, %s, %s", r0, r1)
	case "sgt":
		e.line("slt t0, %s, %s", r1, r0)
	case "sle":
		e.line("slt t0, %s, %s", r1, r0)
		e.line("xori t0, t0, 1")
	case "sge":
		e.line("slt t0, %s, %s", r0, r1)
		e.line("xori t0, t0, 1")
	case "ult":
		e.line("sltu t0, %s, %s", r0, r1)
	case "ugt":
		e.line("sltu t0, %s, %s", r1, r0)
	case "ule":
		e.line("sltu t0, %s, %s", r1, r0)
		e.line("xori t0, t0, 1")
	case "uge":
		e.line("sltu t0, %s, %s", r0, r1)
		e.line("xori t0, t0, 1")
	default:
		return fmt.Errorf("unsupported icmp predicate %s", in.Pred)
	}
	return e.storeDst(in.Dst, "t0")
}

func (e *rv32) emitICmp64(in *Inst) error {
	if err := e.loadPair(in.Ops[0], "t0", "t2"); err != nil {
		return err
	}
	if err := e.loadPair(in.Ops[1], "t1", "t3"); err != nil {
		return err
	}
	switch in.Pred {
	case "eq", "ne":
		e.line("xor t0, t0, t1")
		e.line("xor t2, t2, t3")
		e.line("or t0, t0, t2")
		if in.Pred == "eq" {
			e.line("seqz t0, t0")
		} else {
			e.line("snez t0, t0")
		}
	case "slt", "sgt", "sle", "sge", "ult", "ugt", "ule", "uge":
		// Compare high words, then low words on a tie.
		signed := !strings.HasPrefix(in.Pred, "u")
		cmp := "slt"
		if !signed {
			cmp = "sltu"
		}
		e.line("%s t4, t2, t3", cmp)
		e.line("bne t2, t3, 1f")
		e.line("sltu t4, t0, t1")
		e.label("1")
		switch in.Pred {
		case "slt", "ult":
			e.line("mv t0, t4")
		case "sgt", "ugt":
			e.line("xori t0, t4, 1")
		case "sle", "ule":
			e.line("sltu t0, t1, t0")
			e.line("xori t0, t0, 1")
		case "sge", "uge":
			e.line("sltu t0, t0, t1")
			e.line("xori t0, t0, 1")
		}
	default:
		return fmt.Errorf("unsupported 64-bit icmp %s", in.Pred)
	}
	return e.storeDst(in.Dst, "t0")
}

func (e *rv32) emitCast(in *Inst) error {
	src := in.Ops[0]
	dstTy := in.Ty
	srcSz := 4
	if in.SrcTy != nil {
		srcSz = in.SrcTy.Size()
	}
	dstSz := dstTy.Size()
	if in.Op == "trunc" {
		if srcSz > 4 {
			// take the low word
			if src.Kind == VLocal {
				e.loadSlot(e.slots[src.Name], "t0")
			} else if err := e.loadVal(src, "t0"); err != nil {
				return err
			}
		} else if err := e.loadVal(src, "t0"); err != nil {
			return err
		}
		switch dstSz {
		case 1:
			e.line("andi t0, t0, 255")
		case 2:
			e.line("li t1, 65535")
			e.line("and t0, t0, t1")
		case 4:
		}
		return e.storeDst(in.Dst, "t0")
	}
	// zext / sext / bitcast / ptrtoint / inttoptr
	if err := e.loadVal(src, "t0"); err != nil {
		return err
	}
	if dstSz > 4 {
		off := e.slots[in.Dst]
		e.storeSlot(off, "t0")
		if in.Op == "sext" && srcSz < 4 {
			sh := uint(32 - srcSz*8)
			e.line("slli t0, t0, %d", sh)
			e.line("srai t0, t0, %d", sh)
			e.storeSlot(off, "t0")
			e.line("srai t1, t0, 31")
			e.storeSlot(off+4, "t1")
		} else {
			e.storeSlot(off+4, "zero")
		}
		return nil
	}
	return e.storeDst(in.Dst, "t0")
}

func (e *rv32) emitSelect(in *Inst) error {
	if err := e.loadVal(in.Ops[0], "t0"); err != nil {
		return err
	}
	fl, tl := e.newLabel(), e.newLabel()
	done := e.newLabel()
	e.line("bnez t0, %s", tl)
	e.label(fl)
	if err := e.loadVal(in.Ops[2], "t0"); err != nil {
		return err
	}
	e.line("j %s", done)
	e.label(tl)
	if err := e.loadVal(in.Ops[1], "t0"); err != nil {
		return err
	}
	e.label(done)
	return e.storeDst(in.Dst, "t0")
}

var labelSeq int

func (e *rv32) newLabel() string {
	labelSeq++
	return fmt.Sprintf(".Ltmp%d", labelSeq)
}

func (e *rv32) emitBr(in *Inst) error {
	if len(in.Labels) == 1 {
		if err := e.emitPhiCopies(in.Labels[0], e.curBlockName()); err != nil {
			return err
		}
		e.line("j %s", blockLabel(e.fn, in.Labels[0]))
		return nil
	}
	cond, err := e.loadValTo(in.Ops[0], "t0")
	if err != nil {
		return err
	}
	tr := e.newLabel()
	e.line("bnez %s, %s", cond, tr)
	// false edge
	if err := e.emitPhiCopies(in.Labels[1], e.curBlockName()); err != nil {
		return err
	}
	e.line("j %s", blockLabel(e.fn, in.Labels[1]))
	e.label(tr)
	// true edge
	if err := e.emitPhiCopies(in.Labels[0], e.curBlockName()); err != nil {
		return err
	}
	e.line("j %s", blockLabel(e.fn, in.Labels[0]))
	return nil
}

func (e *rv32) emitSwitch(in *Inst) error {
	cond, err := e.loadValTo(in.Ops[0], "t0")
	if err != nil {
		return err
	}
	for _, c := range in.Cases {
		e.line("li t1, %d", c.Val.I)
		e.line("beq %s, t1, %s", cond, blockLabel(e.fn, c.Dest))
	}
	if err := e.emitPhiCopies(in.Labels[0], e.curBlockName()); err != nil {
		return err
	}
	e.line("j %s", blockLabel(e.fn, in.Labels[0]))
	return nil
}

func (e *rv32) curBlockName() string {
	if e.cur == "" {
		return ""
	}
	return e.cur
}

func (e *rv32) emitPhiCopies(dest, pred string) error {
	var blk *Block
	for _, b := range e.fn.Blocks {
		if b.Label == dest {
			blk = b
			break
		}
	}
	if blk == nil {
		return nil
	}
	var phis []*Inst
	for _, in := range blk.Insts {
		if in.Op == "phi" {
			phis = append(phis, in)
		}
	}
	if len(phis) == 0 {
		return nil
	}
	// Load every incoming value into its temporary first; the sources may
	// include the phi destinations themselves.
	for _, in := range phis {
		idx := -1
		for i, l := range in.Labels {
			if l == pred {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		if err := e.loadVal(in.Ops[idx], "t0"); err != nil {
			return err
		}
		e.storeSlot(e.phiTmp[in.Dst], "t0")
	}
	for _, in := range phis {
		e.loadSlot(e.phiTmp[in.Dst], "t0")
		e.storeSlot(e.slots[in.Dst], "t0")
	}
	return nil
}

func (e *rv32) emitCall(in *Inst) error {
	name := in.Callee
	if strings.HasPrefix(name, "llvm.") {
		return e.emitIntrinsic(in)
	}
	// Place stack arguments first (at sp).
	// Compute register/stack placement.
	type place struct {
		reg  int
		word int
	}
	places := make([]place, len(in.Ops))
	ri := 0
	sw := 0
	for i, ty := range in.OpTys {
		w := words(ty)
		if ri+w <= 8 {
			places[i] = place{reg: ri, word: -1}
			ri += w
		} else {
			if ri&1 == 1 && w == 2 {
				ri++
			}
			places[i] = place{reg: -1, word: sw}
			sw += w
		}
	}
	// Store stack arguments.
	for i, v := range in.Ops {
		p := places[i]
		if p.reg >= 0 {
			continue
		}
		ty := in.OpTys[i]
		if ty.Size() > 4 {
			if err := e.loadPair(v, "t0", "t1"); err != nil {
				return err
			}
			e.storeSlotKeep(p.word*4, "t0")
			e.storeSlotKeep(p.word*4+4, "t1")
		} else {
			if err := e.loadVal(v, "t0"); err != nil {
				return err
			}
			e.storeSlotKeep(p.word*4, "t0")
		}
	}
	// Load register arguments.
	for i, v := range in.Ops {
		p := places[i]
		if p.reg < 0 {
			continue
		}
		ty := in.OpTys[i]
		if ty.Size() > 4 {
			if err := e.loadPair(v, fmt.Sprintf("a%d", p.reg), fmt.Sprintf("a%d", p.reg+1)); err != nil {
				return err
			}
		} else if err := e.loadVal(v, fmt.Sprintf("a%d", p.reg)); err != nil {
			return err
		}
	}
	if in.Indirect {
		if err := e.loadVal(Value{Kind: VLocal, Name: in.Callee}, "t0"); err != nil {
			return err
		}
		e.line("jalr ra, t0, 0")
	} else {
		e.line("call %s", name)
	}
	if in.Dst != "" && in.Ty != nil && in.Ty.Kind != KVoid {
		// A result consumed by the very next instruction can stay in a0 (or
		// a0/a1) instead of taking the store+load round trip.  A following
		// call is excluded: its other argument setup may overwrite a0 before
		// this value is moved to its argument register.
		if in.Ty.Size() <= 4 && !e.cross[in.Dst] && e.lastUse[in.Dst] == e.instIdx+1 {
			if n := e.nextInst(); n != nil && n.Op != "call" {
				e.regAlias[in.Dst] = "a0"
				return nil
			}
		}
		if in.Ty.Size() > 4 {
			off := e.slots[in.Dst]
			e.storeSlot(off, "a0")
			e.storeSlot(off+4, "a1")
		} else {
			e.storeSlot(e.slots[in.Dst], "a0")
		}
	}
	return nil
}

func words(t *Type) int {
	if t == nil {
		return 1
	}
	w := (t.Size() + 3) / 4
	if w < 1 {
		w = 1
	}
	return w
}

func callStackWords(in *Inst) int {
	ri := 0
	sw := 0
	for _, ty := range in.OpTys {
		w := words(ty)
		if ri+w <= 8 && ri < 8 {
			ri += w
		} else {
			if ri&1 == 1 && w == 2 {
				ri++
			}
			sw += w
		}
	}
	return sw
}

func (e *rv32) emitIntrinsic(in *Inst) error {
	switch {
	case strings.HasPrefix(in.Callee, "llvm.va_start"):
		// The argument is the address of the va_list object. It points at
		// the first variadic slot: the register save area at the top of the
		// frame, or the caller's stack arguments immediately above it.
		if err := e.loadVal(in.Ops[0], "t1"); err != nil {
			return err
		}
		if e.nFixed < 8 {
			e.addrSp(e.saveOff+e.nFixed*4, "t0")
		} else {
			e.addrSp(e.frame+(e.nFixed-8)*4, "t0")
		}
		e.line("sw t0, 0(t1)")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.va_end"):
		return nil
	case strings.HasPrefix(in.Callee, "llvm.va_copy"):
		if err := e.loadVal(in.Ops[0], "t0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "t1"); err != nil {
			return err
		}
		e.line("lw t2, 0(t1)")
		e.line("sw t2, 0(t0)")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.memcpy"), strings.HasPrefix(in.Callee, "llvm.memmove"):
		if len(in.Ops) < 3 {
			return fmt.Errorf("memcpy with %d args", len(in.Ops))
		}
		if err := e.loadVal(in.Ops[0], "a0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "a1"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[2], "a2"); err != nil {
			return err
		}
		e.line("call memcpy")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.memset"):
		if len(in.Ops) < 3 {
			return fmt.Errorf("memset with %d args", len(in.Ops))
		}
		if err := e.loadVal(in.Ops[0], "a0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "a1"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[2], "a2"); err != nil {
			return err
		}
		e.line("call memset")
		return nil
	}
	// Unknown intrinsic: return zero.
	if in.Dst != "" {
		e.line("li t0, 0")
		return e.storeDst(in.Dst, "t0")
	}
	return nil
}
