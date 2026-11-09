package native32

// arm.go emits Thumb assembly (ARMv6-M baseline; the same instructions run on
// the M33) from the parsed IR. It mirrors rv32.go: every SSA value gets a
// frame slot, t0/t1 become r0/r1, and calls follow AAPCS (r0-r3 then stack,
// soft-float). The first target is TinyOS's Cortex-M3/M0+ userland, where
// __aeabi_* division helpers live in user/aeabi.c.

import (
	"fmt"
	"strconv"
	"strings"
)

type arm struct {
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
	sRegs    []string
	sRegOff  map[string]int
	regAlias map[string]string
	aliasVar map[string]string
	cross    map[string]bool
	lastUse  map[string]int
	instIdx  int
	paramTgt map[string]string
	skipInst map[*Inst]bool
	usedPool bool
	phiTmp   map[string]int
	saveOff  int
	epilogue string
	nFixed   int
	cur      string
	seq      int
	v6m      bool
	wide     bool
	brCond   map[string]armBranch
	skipCmp  map[*Inst]bool
}

type armBranch struct {
	cmp *Inst
	neg bool
}

func EmitARM(m *Module, cpu string) (string, error) {
	e := &arm{m: m}
	// ARMv6-M has no MOVW/MOVT and only a 1 KiB PC-relative literal-load
	// range, so a huge basic block can put the next pool out of reach. Take
	// the inline-literal path there; ARMv7-M/ARMv8-M keep the compact pools.
	e.v6m = strings.HasPrefix(cpu, "cortex-m0") || strings.HasPrefix(cpu, "cortex-m1")
	e.wide = !e.v6m
	// Keep the loads of the locals that planRegVars will hold in r5-r7
	// (ARMv6-M) or r5-r11 (Thumb-2) so forwarding does not undo that win.
	regVars := 3
	if e.wide {
		regVars = 7
	}
	forwardDirectLoads(m, regVars, 3)
	e.b.WriteString("\t.syntax unified\n\t.thumb\n\t.text\n")
	for _, g := range m.Globals {
		e.emitGlobal(g)
	}
	for _, f := range m.Funcs {
		if err := e.emitFunc(f); err != nil {
			return "", fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	e.b.WriteString("\t.section .note.GNU-stack,\"\",%progbits\n")
	return armPeephole(e.b.String(), e.wide), nil
}

func (e *arm) symAddr(reg, name string) {
	e.litLoad(reg, name)
}

// litLoad materialises a link-time constant (a symbol address or a large
// integer). On ARMv7-M/ARMv8-M `ldr rX, =expr` uses the pool at the end of the
// basic block. On ARMv6-M that pool can be more than ldr's 1 KiB reach away
// inside a large block, so the literal is emitted right here and skipped with
// a short branch. The .word also carries the same ABS32 relocation, which the
// kernel patches because TinyOS links these images with -z notext.
func (e *arm) litLoad(reg, expr string) {
	if !e.v6m {
		e.usedPool = true
		e.line("ldr %s, =%s", reg, expr)
		return
	}
	e.litInline(reg, expr)
}

// litInline emits a word immediately behind a short branch.  It works on every
// Thumb target and, unlike the block-end literal pool, stays in range no
// matter how large the basic block grows.
func (e *arm) litInline(reg, expr string) {
	n := e.seq
	e.seq++
	e.line("ldr %s, .Llit%d", reg, n)
	e.line("b .Llitend%d", n)
	e.line(".balign 4")
	e.label(fmt.Sprintf(".Llit%d", n))
	e.line(".word %s", expr)
	e.label(fmt.Sprintf(".Llitend%d", n))
}

func (e *arm) line(f string, a ...interface{}) { fmt.Fprintf(&e.b, "\t"+f+"\n", a...) }
func (e *arm) label(l string)                  { fmt.Fprintf(&e.b, "%s:\n", l) }

func (e *arm) emitGlobal(g *Global) {
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
		sec = ".note.tos"
		e.b.WriteString("\t.section " + sec + ",\"a\",%note\n")
	} else if zero {
		sec = ".bss." + sanitize(g.Name)
		e.b.WriteString("\t.section " + sec + ",\"aw\",%nobits\n")
	} else if g.Const {
		sec = ".rodata." + sanitize(g.Name)
		e.b.WriteString("\t.section " + sec + ",\"a\",%progbits\n")
	} else {
		e.b.WriteString("\t.section " + sec + ",\"aw\",%progbits\n")
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

func (e *arm) emitInit(t *Type, v Value) {
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
			e.b.WriteString(fmt.Sprintf("\t.hword %d\n", uint16(v.U)))
		case 4:
			e.b.WriteString(fmt.Sprintf("\t.word %d\n", uint32(v.U)))
		case 8:
			e.b.WriteString(fmt.Sprintf("\t.word %d\n", uint32(v.U)))
			e.b.WriteString(fmt.Sprintf("\t.word %d\n", uint32(v.U>>32)))
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
	default:
		e.b.WriteString(fmt.Sprintf("\t.zero %d\n", t.Size()))
	}
}

func (e *arm) emitFunc(f *Func) error {
	e.fn = f
	e.slots = map[string]int{}
	e.slotTy = map[string]*Type{}
	e.allocas = map[string]int{}
	e.phiTmp = map[string]int{}
	e.nFixed = len(f.Params)
	e.saveOff = 0
	e.outBytes = 0
	e.frame = 0
	e.usedPool = false
	e.brCond, e.skipCmp = planBranchConds(f)

	maxStack := 0
	for _, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op == "call" {
				if n := callStackWordsARM(in); n > maxStack {
					maxStack = n
				}
			}
		}
	}
	e.outBytes = align(maxStack*4, 8)

	cur := e.outBytes
	// r4 is the address scratch for slot offsets beyond the Thumb-1
	// sp-relative range. It is callee-saved and sits at a low offset so the
	// prologue can store it before it is used.
	r4Off := cur
	e.direct = findDirectAllocas(f)
	regs := []string{"r5", "r6", "r7"}
	if e.wide {
		// Thumb-2 encodes r8-r11 as ALU operands and in the 32-bit
		// load/store forms, so ARMv7-M/ARMv8-M can keep six more hot
		// locals in registers.  ARMv6-M must stay on low registers.
		regs = append(regs, "r8", "r9", "r10", "r11")
	}
	e.regVar, e.sRegs = planRegVars(f, regs, 3)
	e.regAlias = map[string]string{}
	e.aliasVar = map[string]string{}
	L := layoutSlots(f, e.outBytes, 4, nil, true)
	e.slots = L.slots
	e.allocas = L.allocas
	e.phiTmp = L.phiTmp
	e.slotTy = L.slotTy
	e.cross = L.cross
	e.paramTgt, e.skipInst = findParamStores(f, e.direct, e.regVar)
	cur = L.end
	e.sRegOff = map[string]int{}
	for _, r := range e.sRegs {
		cur = align(cur, 4)
		e.sRegOff[r] = cur
		cur += 4
	}
	// lr save slot below the vararg area; the area must end at sp+frame so
	// the caller's stack arguments continue it.
	cur = align(cur, 4)
	lrOff := cur
	cur += 4
	if f.Variadic {
		cur = align(cur, 8)
		e.saveOff = cur
		cur += 4 * 4
	}
	e.frame = align(cur, 8)
	if e.frame < 8 {
		e.frame = 8
	}

	e.b.WriteString("\t.section .text." + sanitize(f.Name) + ",\"ax\",%progbits\n")
	if !strings.HasPrefix(f.Name, ".") {
		e.b.WriteString("\t.globl " + f.Name + "\n")
	}
	e.b.WriteString("\t.type " + f.Name + ",%function\n")
	e.b.WriteString("\t.thumb_func\n")
	e.label(sanitize(f.Name))
	e.adjustSP(-e.frame)
	e.line("str r4, [sp, #%d]", r4Off)
	for _, r := range e.sRegs {
		e.storeSlotReg(e.sRegOff[r], r)
	}
	if f.Variadic {
		for i := 0; i < 4; i++ {
			e.storeSlotRegKeep(e.saveOff+i*4, fmt.Sprintf("r%d", i))
		}
	}
	ri := 0
	for _, p := range f.Params {
		sz := p.Ty.Size()
		w := (sz + 3) / 4
		if w < 1 {
			w = 1
		}
		off := e.slots[p.Name]
		for k := 0; k < w; k++ {
			if target := e.paramTgt[p.Name]; target != "" && w == 1 {
				src := ""
				if ri < 4 {
					src = fmt.Sprintf("r%d", ri)
				} else {
					e.loadSlot(e.frame+(ri-4)*4, "r0")
					src = "r0"
				}
				if reg := e.regVar[target]; reg != "" {
					e.line("mov %s, %s", reg, src)
				} else {
					e.storeSlotReg(e.allocas[target], src)
				}
				ri++
				continue
			}
			if ri < 4 {
				e.storeSlotReg(off+k*4, fmt.Sprintf("r%d", ri))
			} else {
				e.loadSlot(e.frame+(ri-4)*4, "r0")
				e.storeSlotReg(off+k*4, "r0")
			}
			ri++
		}
	}
	// Save lr only after the argument registers have been spilled: r3 is the
	// fourth AAPCS argument, so using it earlier silently replaced that
	// parameter with the return address (this broke every 4-argument call,
	// including tos_sys3's syscall number/arg bundle).
	e.line("mov r3, lr")
	e.storeSlotReg(lrOff, "r3")
	e.epilogue = fmt.Sprintf(".Lret_%s", sanitize(f.Name))
	for _, blk := range f.Blocks {
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
		if e.usedPool {
			e.b.WriteString("\t.ltorg\n")
			e.usedPool = false
		}
	}
	e.label(e.epilogue)
	e.loadSlot(lrOff, "r3")
	for _, r := range e.sRegs {
		e.loadSlot(e.sRegOff[r], r)
	}
	e.line("ldr r4, [sp, #%d]", r4Off)
	e.adjustSP(e.frame)
	e.line("bx r3")
	e.b.WriteString(fmt.Sprintf("\t.size %s, .-%s\n\n", f.Name, f.Name))
	return nil
}

func (e *arm) storeSlotReg(off int, reg string) {
	e.storeSlotOp(off, reg, false)
}

// storeSlotRegKeep writes a slot that is read by the callee or through a
// pointer rather than by a local ldr.  The slot pass must not delete it.
func (e *arm) storeSlotRegKeep(off int, reg string) {
	e.storeSlotOp(off, reg, true)
}

func (e *arm) storeSlotOp(off int, reg string, keep bool) {
	// ARMv6-M can encode only a 5-bit scaled offset (#124); ARMv7-M and
	// ARMv8-M also have the 12-bit form (#4095), which keeps big frames
	// from paying for an address calculation on every access.
	limit := 124
	if e.wide {
		limit = 4095
	}
	suffix := ""
	if keep {
		suffix = " @ goc32keep"
	}
	if off >= 0 && off <= limit && off%4 == 0 {
		e.line("str %s, [sp, #%d]%s", reg, off, suffix)
		return
	}
	addr := "r4"
	e.addrOf(off, addr)
	e.line("str %s, [%s]%s", reg, addr, suffix)
}

func (e *arm) loadSlot(off int, reg string) {
	limit := 124
	if e.wide {
		limit = 4095
	}
	if off >= 0 && off <= limit && off%4 == 0 {
		if e.v6m && highReg(reg) {
			// Thumb-1 ldr takes only low registers for Rt.  (The wide
			// targets can encode a high Rt in the 32-bit form.)
			e.addrOf(off, "r4")
			e.line("ldr r4, [r4]")
			e.line("mov %s, r4", reg)
			return
		}
		e.line("ldr %s, [sp, #%d]", reg, off)
		return
	}
	// Form the address in the callee-saved scratch r4, never in the
	// destination: a large local can be loaded into r12 (indirect call
	// target) or r3 (high word of a 64-bit pair), and Thumb-1's adds
	// immediate cannot address r8-r15 anyway.
	e.addrOf(off, "r4")
	if highReg(reg) {
		// Thumb-1 ldr only takes low registers for Rt, so land in r4
		// (already our scratch) and move across. r0-r3 may be holding
		// call arguments at this point.
		e.line("ldr r4, [r4]")
		e.line("mov %s, r4", reg)
		return
	}
	e.line("ldr %s, [r4]", reg)
}

func highReg(reg string) bool {
	switch reg {
	case "r8", "r9", "r10", "r11", "r12":
		return true
	}
	return false
}

// loadSized reads 1, 2 or 4 bytes from a frame offset.  Direct allocas use
// it so a char/short/int local needs no pointer materialisation.
func (e *arm) loadSized(off, size int, reg string) {
	op := "ldr"
	switch size {
	case 1:
		op = "ldrb"
	case 2:
		op = "ldrh"
	}
	if e.smallOffset(off, size) {
		e.line("%s %s, [sp, #%d]", op, reg, off)
		return
	}
	e.addrOf(off, "r4")
	e.line("%s %s, [r4]", op, reg)
}

func (e *arm) storeSized(off, size int, reg string) {
	op := "str"
	switch size {
	case 1:
		op = "strb"
	case 2:
		op = "strh"
	}
	if e.smallOffset(off, size) {
		e.line("%s %s, [sp, #%d]", op, reg, off)
		return
	}
	e.addrOf(off, "r4")
	e.line("%s %s, [r4]", op, reg)
}

// smallOffset reports whether [sp,#off] fits the sized load/store encoding.
func (e *arm) smallOffset(off, size int) bool {
	if off < 0 {
		return false
	}
	if e.wide {
		return off <= 4095
	}
	switch size {
	case 1:
		// Thumb-1 LDRB/STRB take a low-register base, not SP.
		return false
	case 2:
		return false
	default:
		return off%4 == 0 && off <= 124
	}
}

// addImm adds a 32-bit constant to a low register.  Thumb-1 has an 8-bit
// immediate add/sub, which covers almost every field offset and element step;
// larger or negative values go through r1.
func (e *arm) addImm(reg string, imm int) {
	if imm == 0 {
		return
	}
	if imm > 0 && imm <= 255 {
		e.line("adds %s, #%d", reg, imm)
		return
	}
	if imm < 0 && imm >= -255 {
		e.line("subs %s, #%d", reg, -imm)
		return
	}
	if reg == "r1" {
		// The literal load would overwrite the value in r1.
		e.li("r2", uint64(uint32(int32(imm))))
		e.line("adds %s, r2", reg)
		return
	}
	e.li("r1", uint64(uint32(int32(imm))))
	e.line("adds %s, r1", reg)
}

func (e *arm) addrOf(off int, reg string) {
	// Thumb-1 ADD (SP plus immediate) reaches #1020 in one instruction.
	// For larger frames start with that and add the rest in 8-bit chunks;
	// the old mov+adds loop spent five instructions on a 1 KiB offset.
	if off >= 0 && off%4 == 0 {
		// Past a few KiB, a literal word plus `add reg, sp` is shorter (and
		// always in range) than a chain of 255-byte adds.
		if off > 2500 {
			e.litInline(reg, strconv.Itoa(off))
			e.line("add %s, sp", reg)
			return
		}
		first := off
		if first > 1020 {
			first = 1020
		}
		e.line("add %s, sp, #%d", reg, first)
		off -= first
		for off > 0 {
			n := off
			if n > 255 {
				n = 255
			}
			e.line("adds %s, #%d", reg, n)
			off -= n
		}
		return
	}
	e.line("mov %s, sp", reg)
	for off > 0 {
		n := off
		if n > 255 {
			n = 255
		}
		e.line("adds %s, #%d", reg, n)
		off -= n
	}
}

// adjustSP moves sp by n bytes. Thumb-1's ADD/SUB SP immediate is only seven
// bits scaled by four (max 508 bytes), so large frames are adjusted in chunks.
func (e *arm) adjustSP(n int) {
	if n == 0 {
		return
	}
	op := "add"
	if n < 0 {
		op = "sub"
		n = -n
	}
	for n > 0 {
		c := n
		if c > 508 {
			c = 508
		}
		c &^= 3
		if c == 0 {
			c = 4
		}
		e.line("%s sp, #%d", op, c)
		n -= c
	}
}

func (e *arm) loadVal(v Value, reg string) error {
	switch v.Kind {
	case VLocal:
		if src, ok := e.regAlias[v.Name]; ok {
			e.line("mov %s, %s", reg, src)
			return nil
		}
		if off, ok := e.allocas[v.Name]; ok {
			e.addrOf(off, reg)
			return nil
		}
		off, ok := e.slots[v.Name]
		if !ok {
			return fmt.Errorf("unknown local %%%s", v.Name)
		}
		e.loadSlot(off, reg)
	case VGlobal:
		e.symAddr(reg, v.Name)
	case VInt:
		e.li(reg, v.U)
	case VBool:
		e.li(reg, v.U&1)
	case VNull, VUndef:
		e.li(reg, 0)
	default:
		return fmt.Errorf("cannot materialise value %s", v)
	}
	return nil
}

func (e *arm) li(reg string, v uint64) {
	if v <= 255 {
		e.line("movs %s, #%d", reg, v)
		return
	}
	e.litLoad(reg, fmt.Sprintf("%d", v))
}

func (e *arm) loadValTo(v Value, scratch string) (string, error) {
	if v.Kind == VLocal {
		if src, ok := e.regAlias[v.Name]; ok {
			return src, nil
		}
	}
	if err := e.loadVal(v, scratch); err != nil {
		return "", err
	}
	return scratch, nil
}

func (e *arm) storeDst(name, reg string) error {
	off, ok := e.slots[name]
	if !ok {
		return fmt.Errorf("unknown destination %%%s", name)
	}
	e.storeSlotReg(off, reg)
	return nil
}

func (e *arm) loadPair(v Value, lo, hi string) error {
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
	e.li(hi, 0)
	return nil
}

func (e *arm) emitInst(in *Inst) error {
	// A comparison (and the xor that negates it) consumed directly by a
	// branch is emitted at the branch; do not materialise a boolean slot.
	if e.skipCmp[in] {
		return nil
	}
	switch in.Op {
	case "alloca":
		if e.direct[in.Dst] {
			return nil
		}
		if _, ok := e.slots[in.Dst]; !ok {
			return nil
		}
		e.addrOf(e.allocas[in.Dst], "r0")
		return e.storeDst(in.Dst, "r0")
	case "load":
		if v := in.Ops[0]; v.Kind == VLocal && e.direct[v.Name] {
			off := e.allocas[v.Name]
			if reg := e.regVar[v.Name]; reg != "" {
				if !e.cross[in.Dst] {
					e.regAlias[in.Dst] = reg
					e.aliasVar[in.Dst] = v.Name
					return nil
				}
				e.line("mov r1, %s", reg)
			} else if in.Ty.Size() == 8 {
				e.loadSlot(off, "r1")
				e.loadSlot(off+4, "r2")
				dst := e.slots[in.Dst]
				e.storeSlotReg(dst, "r1")
				e.storeSlotReg(dst+4, "r2")
				return nil
			} else {
				e.loadSized(off, in.Ty.Size(), "r1")
			}
			return e.storeDst(in.Dst, "r1")
		}
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		sz := in.Ty.Size()
		switch sz {
		case 1:
			e.line("ldrb r1, [r0]")
		case 2:
			e.line("ldrh r1, [r0]")
		case 4:
			e.line("ldr r1, [r0]")
		case 8:
			off := e.slots[in.Dst]
			e.line("ldr r1, [r0]")
			e.line("ldr r2, [r0, #4]")
			e.storeSlotReg(off, "r1")
			e.storeSlotReg(off+4, "r2")
			return nil
		default:
			off := e.slots[in.Dst]
			for i := 0; i < sz; i += 4 {
				e.line("ldr r1, [r0, #%d]", i)
				e.storeSlotReg(off+i, "r1")
			}
			return nil
		}
		return e.storeDst(in.Dst, "r1")
	case "store":
		if e.skipInst[in] {
			return nil
		}
		if v := in.Ops[1]; v.Kind == VLocal && e.direct[v.Name] {
			sz := in.Ty.Size()
			if reg := e.regVar[v.Name]; reg != "" {
				for name, av := range e.aliasVar {
					if av != v.Name {
						continue
					}
					if e.lastUse[name] > e.instIdx {
						e.line("mov r1, %s", reg)
						if err := e.storeDst(name, "r1"); err != nil {
							return err
						}
					}
					delete(e.regAlias, name)
					delete(e.aliasVar, name)
				}
				src, err := e.loadValTo(in.Ops[0], "r0")
				if err != nil {
					return err
				}
				if src != reg {
					e.line("mov %s, %s", reg, src)
				}
				return nil
			}
			if sz == 8 {
				if err := e.loadPair(in.Ops[0], "r0", "r1"); err != nil {
					return err
				}
				off := e.allocas[v.Name]
				e.storeSlotReg(off, "r0")
				e.storeSlotReg(off+4, "r1")
				return nil
			}
			if err := e.loadVal(in.Ops[0], "r0"); err != nil {
				return err
			}
			e.storeSized(e.allocas[v.Name], sz, "r0")
			return nil
		}
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		sz := in.Ty.Size()
		switch sz {
		case 1:
			e.line("strb r0, [r1]")
		case 2:
			e.line("strh r0, [r1]")
		case 4:
			e.line("str r0, [r1]")
		case 8:
			off := e.slots[in.Ops[0].Name]
			e.loadSlot(off+4, "r2")
			e.line("str r0, [r1]")
			e.line("str r2, [r1, #4]")
		default:
			off := 0
			if in.Ops[0].Kind == VLocal {
				off = e.slots[in.Ops[0].Name]
			}
			for i := 0; i < sz; i += 4 {
				e.loadSlot(off+i, "r2")
				e.line("str r2, [r1, #%d]", i)
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
				if err := e.loadPair(in.Ops[0], "r0", "r1"); err != nil {
					return err
				}
			} else if err := e.loadVal(in.Ops[0], "r0"); err != nil {
				return err
			}
		}
		e.jump(e.epilogue)
		return nil
	case "trunc", "zext", "sext", "bitcast", "ptrtoint", "inttoptr":
		return e.emitCast(in)
	case "select":
		return e.emitSelect(in)
	case "phi":
		return nil
	case "call":
		return e.emitCall(in)
	case "unreachable":
		e.jump(e.epilogue)
		return nil
	}
	return fmt.Errorf("unsupported instruction %s", in.Op)
}

func (e *arm) emitGEP(in *Inst) error {
	base, err := e.loadValTo(in.Ops[0], "r0")
	if err != nil {
		return err
	}
	curReg := base
	cur := in.Ty
	addConst := func(off int) {
		if off == 0 {
			return
		}
		if curReg != "r0" {
			e.line("movs r0, %s", curReg)
			curReg = "r0"
		}
		e.addImm("r0", off)
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
		if scale <= 0 {
			scale = 1
		}
		if idx.Kind == VInt {
			addConst(int(idx.I) * scale)
			continue
		}
		idxReg, err := e.loadValTo(idx, "r1")
		if err != nil {
			return err
		}
		if scale == 1 {
			e.line("adds r0, %s, %s", curReg, idxReg)
		} else {
			if scale&(scale-1) == 0 {
				k := 0
				for s := scale; s > 1; s >>= 1 {
					k++
				}
				e.line("lsls r1, %s, #%d", idxReg, k)
			} else {
				if idxReg != "r1" {
					e.line("movs r1, %s", idxReg)
				}
				e.li("r2", uint64(scale))
				e.line("muls r1, r2")
			}
			e.line("adds r0, %s, r1", curReg)
		}
		curReg = "r0"
	}
	return e.storeDst(in.Dst, curReg)
}

func (e *arm) emitBin(in *Inst) error {
	wide := in.Ty.Size() > 4
	if wide {
		return e.emitBin64(in)
	}
	rA, err := e.loadValTo(in.Ops[0], "r0")
	if err != nil {
		return err
	}
	rB, err := e.loadValTo(in.Ops[1], "r1")
	if err != nil {
		return err
	}
	toR0 := func() {
		if rA != "r0" {
			e.line("movs r0, %s", rA)
		}
	}
	switch in.Op {
	case "add":
		e.line("adds r0, %s, %s", rA, rB)
	case "sub":
		e.line("subs r0, %s, %s", rA, rB)
	case "mul":
		toR0()
		if highReg(rB) {
			// MUL only encodes low registers; the high register is a
			// Thumb-2 register-variable candidate.  A square can reuse r0.
			if rA == rB {
				rB = "r0"
			} else {
				e.line("movs r1, %s", rB)
				rB = "r1"
			}
		}
		e.line("muls r0, %s", rB)
	case "sdiv", "udiv", "srem", "urem":
		if rA != "r0" {
			e.line("mov r0, %s", rA)
		}
		if rB != "r1" {
			e.line("mov r1, %s", rB)
		}
		switch in.Op {
		case "sdiv":
			e.line("bl __aeabi_idiv")
		case "udiv":
			e.line("bl __aeabi_uidiv")
		case "srem":
			e.line("bl __aeabi_idivmod")
			e.line("movs r0, r1")
		case "urem":
			e.line("bl __aeabi_uidivmod")
			e.line("movs r0, r1")
		}
	case "shl":
		toR0()
		e.line("lsls r0, %s", rB)
	case "lshr":
		toR0()
		e.line("lsrs r0, %s", rB)
	case "ashr":
		toR0()
		e.line("asrs r0, %s", rB)
	case "and":
		toR0()
		e.line("ands r0, %s", rB)
	case "or":
		toR0()
		e.line("orrs r0, %s", rB)
	case "xor":
		toR0()
		e.line("eors r0, %s", rB)
	default:
		return fmt.Errorf("unsupported binop %s", in.Op)
	}
	return e.storeDst(in.Dst, "r0")
}

func (e *arm) emitBin64(in *Inst) error {
	if in.Op == "mul" || in.Op == "shl" || in.Op == "lshr" || in.Op == "ashr" {
		// Address arithmetic only needs the low word; gocl uses i64 here
		// solely because LLVM's GEP indices are i64.
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		switch in.Op {
		case "mul":
			e.line("movs r2, r0")
			e.line("muls r2, r1")
			e.line("movs r0, r2")
		case "shl":
			e.line("lsls r0, r1")
		case "lshr":
			e.line("lsrs r0, r1")
		case "ashr":
			e.line("asrs r0, r1")
		}
		off := e.slots[in.Dst]
		e.storeSlotReg(off, "r0")
		e.storeSlotReg(off+4, "r0")
		return nil
	}
	offa, offb, offd := e.slots[in.Ops[0].Name], e.slots[in.Ops[1].Name], e.slots[in.Dst]
	if in.Ops[0].Kind != VLocal || in.Ops[1].Kind != VLocal {
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		switch in.Op {
		case "add":
			e.line("adds r0, r0, r1")
		case "sub":
			e.line("subs r0, r0, r1")
		case "mul":
			e.line("movs r2, r0")
			e.line("muls r2, r1")
			e.line("movs r0, r2")
		case "and":
			e.line("ands r0, r1")
		case "or":
			e.line("orrs r0, r1")
		case "xor":
			e.line("eors r0, r1")
		default:
			return fmt.Errorf("unsupported 64-bit op %s", in.Op)
		}
		e.storeSlotReg(offd, "r0")
		e.storeSlotReg(offd+4, "r0")
		return nil
	}
	e.loadSlot(offa, "r0")
	e.loadSlot(offa+4, "r1")
	e.loadSlot(offb, "r2")
	e.loadSlot(offb+4, "r3")
	switch in.Op {
	case "add":
		e.line("adds r0, r0, r2")
		e.line("adcs r1, r1, r3")
	case "sub":
		e.line("subs r0, r0, r2")
		e.line("sbcs r1, r1, r3")
	case "and":
		e.line("ands r0, r2")
		e.line("ands r1, r3")
	case "or":
		e.line("orrs r0, r2")
		e.line("orrs r1, r3")
	case "xor":
		e.line("eors r0, r2")
		e.line("eors r1, r3")
	default:
		return fmt.Errorf("unsupported 64-bit op %s", in.Op)
	}
	e.storeSlotReg(offd, "r0")
	e.storeSlotReg(offd+4, "r1")
	return nil
}

func (e *arm) emitICmp(in *Inst) error {
	if in.Ty.Size() > 4 {
		return e.emitICmp64(in)
	}
	rA, err := e.loadValTo(in.Ops[0], "r0")
	if err != nil {
		return err
	}
	rB, err := e.loadValTo(in.Ops[1], "r1")
	if err != nil {
		return err
	}
	// cmp sets flags; branch before the first movs, which would clobber them.
	cond := ""
	switch in.Pred {
	case "eq":
		cond = "eq"
	case "ne":
		cond = "ne"
	case "slt":
		cond = "lt"
	case "sgt":
		cond = "gt"
	case "sle":
		cond = "le"
	case "sge":
		cond = "ge"
	case "ult":
		cond = "lo"
	case "ugt":
		cond = "hi"
	case "ule":
		cond = "ls"
	case "uge":
		cond = "hs"
	default:
		return fmt.Errorf("unsupported icmp predicate %s", in.Pred)
	}
	t, done := e.newLabel(), e.newLabel()
	e.line("cmp %s, %s", rA, rB)
	e.condBranch(cond, t)
	e.line("movs r0, #0")
	e.jump(done)
	e.label(t)
	e.line("movs r0, #1")
	e.label(done)
	return e.storeDst(in.Dst, "r0")
}

func (e *arm) emitICmp64(in *Inst) error {
	if err := e.loadPair(in.Ops[0], "r0", "r1"); err != nil {
		return err
	}
	if err := e.loadPair(in.Ops[1], "r2", "r3"); err != nil {
		return err
	}
	switch in.Pred {
	case "eq", "ne":
		e.line("eors r0, r2")
		e.line("eors r1, r3")
		e.line("orrs r0, r1")
		e.line("cmp r0, #0")
		eqL, doneL := e.newLabel(), e.newLabel()
		if in.Pred == "eq" {
			e.condBranch("eq", eqL)
		} else {
			e.condBranch("ne", eqL)
		}
		e.line("movs r0, #0")
		e.jump(doneL)
		e.label(eqL)
		e.line("movs r0, #1")
		e.label(doneL)
		return e.storeDst(in.Dst, "r0")
	}
	// Compare the high words first, then the low words when equal.
	hiCond := ""
	switch in.Pred {
	case "slt", "sle", "sgt", "sge":
		hiCond = "lt"
	case "ult", "ule", "ugt", "uge":
		hiCond = "lo"
	}
	t, done := e.newLabel(), e.newLabel()
	e.line("cmp r1, r3")
	e.condBranch(hiCond, t)
	e.condBranch("ne", done)
	e.line("cmp r0, r2")
	e.condBranch(hiCond, t)
	e.label(done)
	e.line("movs r0, #0")
	d2 := e.newLabel()
	e.jump(d2)
	e.label(t)
	e.line("movs r0, #1")
	e.label(d2)
	return e.storeDst(in.Dst, "r0")
}

func (e *arm) emitCast(in *Inst) error {
	src := in.Ops[0]
	dstSz := in.Ty.Size()
	srcSz := 4
	if in.SrcTy != nil {
		srcSz = in.SrcTy.Size()
	}
	if err := e.loadVal(src, "r0"); err != nil {
		return err
	}
	if in.Op == "trunc" {
		switch dstSz {
		case 1:
			e.line("uxtb r0, r0")
		case 2:
			e.line("uxth r0, r0")
		}
		return e.storeDst(in.Dst, "r0")
	}
	if in.Op == "sext" && srcSz < 4 {
		if srcSz == 1 {
			e.line("sxtb r0, r0")
		} else if srcSz == 2 {
			e.line("sxth r0, r0")
		}
	}
	if dstSz > 4 {
		off := e.slots[in.Dst]
		e.storeSlotReg(off, "r0")
		if in.Op == "sext" {
			e.line("asrs r1, r0, #31")
		} else {
			e.li("r1", 0)
		}
		e.storeSlotReg(off+4, "r1")
		return nil
	}
	return e.storeDst(in.Dst, "r0")
}

func (e *arm) emitSelect(in *Inst) error {
	if err := e.loadVal(in.Ops[0], "r0"); err != nil {
		return err
	}
	t := e.newLabel()
	done := e.newLabel()
	e.line("cmp r0, #0")
	e.condBranch("ne", t)
	if err := e.loadVal(in.Ops[2], "r0"); err != nil {
		return err
	}
	e.jump(done)
	e.label(t)
	if err := e.loadVal(in.Ops[1], "r0"); err != nil {
		return err
	}
	e.label(done)
	return e.storeDst(in.Dst, "r0")
}

func (e *arm) newLabel() string {
	e.seq++
	return fmt.Sprintf(".Ltmp%d", e.seq)
}

// jump is a range-independent unconditional branch for Thumb-1. The short B
// is only +/-2 KiB and lib.c's tos_vsnprintf is ~18 KiB, so a plain b across
// it does not assemble on the M0+. ldr/mov pc reaches the literal pool, and
// the absolute target word is a text relocation the TinyOS loader applies.
func (e *arm) jump(target string) {
	n := e.seq
	e.seq++
	e.line("ldr r3, .Lbr%d", n)
	e.line("mov pc, r3")
	e.b.WriteString("\t.balign 4\n")
	e.label(fmt.Sprintf(".Lbr%d", n))
	e.b.WriteString("\t.word " + target + "\n")
}

// condBranch emits a conditional branch with an inverted skip, because the
// Thumb-1 conditional branch range is only +/-256 bytes and generated
// functions can exceed it.
func (e *arm) condBranch(cond, target string) {
	skip := e.newLabel()
	e.line("b%s %s", invCond(cond), skip)
	e.jump(target)
	e.label(skip)
}

func invCond(c string) string {
	switch c {
	case "eq":
		return "ne"
	case "ne":
		return "eq"
	case "lt":
		return "ge"
	case "ge":
		return "lt"
	case "gt":
		return "le"
	case "le":
		return "gt"
	case "lo":
		return "hs"
	case "hs":
		return "lo"
	case "hi":
		return "ls"
	case "ls":
		return "hi"
	}
	return "eq"
}

// planBranchConds finds comparison results whose only consumer is a branch in
// the same block.  Such an icmp never needs to materialise a 0/1 value: the
// branch can compare its operands directly, which also removes the store and
// reload of the boolean slot.  A `xor icmp, 1` (the front end's boolean NOT)
// is folded into an inverted branch as well.
func planBranchConds(f *Func) (map[string]armBranch, map[*Inst]bool) {
	def := map[string]*Inst{}
	blockOf := map[string]int{}
	uses := map[string]int{}
	for bi, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Dst != "" {
				def[in.Dst] = in
				blockOf[in.Dst] = bi
			}
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
			if in.Indirect && in.Callee != "" {
				uses[in.Callee]++
			}
		}
	}
	byName := map[string]armBranch{}
	skip := map[*Inst]bool{}
	usable := func(name string, bi int) *Inst {
		d := def[name]
		if d == nil || d.Op != "icmp" || d.Ty == nil || d.Ty.Size() > 4 ||
			uses[name] != 1 || blockOf[name] != bi {
			return nil
		}
		return d
	}
	for bi, blk := range f.Blocks {
		for _, in := range blk.Insts {
			if in.Op != "br" || len(in.Labels) != 2 || len(in.Ops) == 0 || in.Ops[0].Kind != VLocal {
				continue
			}
			name := in.Ops[0].Name
			if d := usable(name, bi); d != nil {
				byName[name] = armBranch{cmp: d}
				skip[d] = true
				continue
			}
			d := def[name]
			if d == nil || d.Op != "xor" || len(d.Ops) != 2 || uses[name] != 1 || blockOf[name] != bi {
				continue
			}
			var cand *Inst
			for k := 0; k < 2; k++ {
				v := d.Ops[k]
				isOne := (v.Kind == VInt || v.Kind == VBool) && (v.I == 1 || v.U&1 == 1)
				if !isOne {
					continue
				}
				other := d.Ops[1-k]
				if other.Kind != VLocal {
					continue
				}
				if c := usable(other.Name, bi); c != nil {
					cand = c
				}
			}
			if cand == nil {
				continue
			}
			byName[name] = armBranch{cmp: cand, neg: true}
			skip[d] = true
			skip[cand] = true
		}
	}
	return byName, skip
}

func armCond(pred string) (string, bool) {
	switch pred {
	case "eq":
		return "eq", true
	case "ne":
		return "ne", true
	case "slt":
		return "lt", true
	case "sgt":
		return "gt", true
	case "sle":
		return "le", true
	case "sge":
		return "ge", true
	case "ult":
		return "lo", true
	case "ugt":
		return "hi", true
	case "ule":
		return "ls", true
	case "uge":
		return "hs", true
	}
	return "", false
}

func (e *arm) emitBrCmp(in, cmp *Inst, neg bool) error {
	if len(cmp.Ops) != 2 {
		return fmt.Errorf("icmp with %d operands", len(cmp.Ops))
	}
	rA, err := e.loadValTo(cmp.Ops[0], "r0")
	if err != nil {
		return err
	}
	rB, err := e.loadValTo(cmp.Ops[1], "r1")
	if err != nil {
		return err
	}
	cond, ok := armCond(cmp.Pred)
	if !ok {
		return fmt.Errorf("unsupported icmp predicate %s", cmp.Pred)
	}
	if neg {
		cond = invCond(cond)
	}
	tr := e.newLabel()
	e.line("cmp %s, %s", rA, rB)
	e.condBranch(cond, tr)
	if err := e.emitPhiCopies(in.Labels[1], e.cur); err != nil {
		return err
	}
	e.jump(blockLabel(e.fn, in.Labels[1]))
	e.label(tr)
	if err := e.emitPhiCopies(in.Labels[0], e.cur); err != nil {
		return err
	}
	e.jump(blockLabel(e.fn, in.Labels[0]))
	return nil
}

func (e *arm) emitBr(in *Inst) error {
	if len(in.Labels) == 1 {
		if err := e.emitPhiCopies(in.Labels[0], e.cur); err != nil {
			return err
		}
		e.jump(blockLabel(e.fn, in.Labels[0]))
		return nil
	}
	if len(in.Ops) > 0 && in.Ops[0].Kind == VLocal {
		if b, ok := e.brCond[in.Ops[0].Name]; ok {
			return e.emitBrCmp(in, b.cmp, b.neg)
		}
	}
	cond, err := e.loadValTo(in.Ops[0], "r0")
	if err != nil {
		return err
	}
	tr := e.newLabel()
	e.line("cmp %s, #0", cond)
	e.condBranch("ne", tr)
	if err := e.emitPhiCopies(in.Labels[1], e.cur); err != nil {
		return err
	}
	e.jump(blockLabel(e.fn, in.Labels[1]))
	e.label(tr)
	if err := e.emitPhiCopies(in.Labels[0], e.cur); err != nil {
		return err
	}
	e.line("b %s", blockLabel(e.fn, in.Labels[0]))
	return nil
}

func (e *arm) emitSwitch(in *Inst) error {
	cond, err := e.loadValTo(in.Ops[0], "r0")
	if err != nil {
		return err
	}
	for _, c := range in.Cases {
		e.li("r1", uint64(c.Val.I))
		e.line("cmp %s, r1", cond)
		e.condBranch("eq", blockLabel(e.fn, c.Dest))
	}
	if err := e.emitPhiCopies(in.Labels[0], e.cur); err != nil {
		return err
	}
	e.line("b %s", blockLabel(e.fn, in.Labels[0]))
	return nil
}

func (e *arm) emitPhiCopies(dest, pred string) error {
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
		if err := e.loadVal(in.Ops[idx], "r0"); err != nil {
			return err
		}
		e.storeSlotReg(e.phiTmp[in.Dst], "r0")
	}
	for _, in := range phis {
		e.loadSlot(e.phiTmp[in.Dst], "r0")
		e.storeSlotReg(e.slots[in.Dst], "r0")
	}
	return nil
}

func (e *arm) emitCall(in *Inst) error {
	name := in.Callee
	if strings.HasPrefix(name, "llvm.") {
		return e.emitIntrinsic(in)
	}
	type place struct {
		reg  int
		word int
	}
	places := make([]place, len(in.Ops))
	ri, sw := 0, 0
	for i, ty := range in.OpTys {
		w := words(ty)
		if ri+w <= 4 && ri < 4 {
			places[i] = place{reg: ri, word: -1}
			ri += w
		} else {
			places[i] = place{reg: -1, word: sw}
			sw += w
		}
	}
	for i, v := range in.Ops {
		p := places[i]
		if p.reg >= 0 {
			continue
		}
		ty := in.OpTys[i]
		if ty.Size() > 4 {
			if err := e.loadPair(v, "r0", "r1"); err != nil {
				return err
			}
			e.storeSlotRegKeep(p.word*4, "r0")
			e.storeSlotRegKeep(p.word*4+4, "r1")
		} else {
			if err := e.loadVal(v, "r0"); err != nil {
				return err
			}
			e.storeSlotRegKeep(p.word*4, "r0")
		}
	}
	for i, v := range in.Ops {
		p := places[i]
		if p.reg < 0 {
			continue
		}
		ty := in.OpTys[i]
		if ty.Size() > 4 {
			if err := e.loadPair(v, fmt.Sprintf("r%d", p.reg), fmt.Sprintf("r%d", p.reg+1)); err != nil {
				return err
			}
		} else if err := e.loadVal(v, fmt.Sprintf("r%d", p.reg)); err != nil {
			return err
		}
	}
	if in.Indirect {
		if err := e.loadVal(Value{Kind: VLocal, Name: in.Callee}, "r12"); err != nil {
			return err
		}
		e.line("blx r12")
	} else {
		e.line("bl %s", name)
	}
	if in.Dst != "" && in.Ty != nil && in.Ty.Kind != KVoid {
		if in.Ty.Size() > 4 {
			off := e.slots[in.Dst]
			e.storeSlotReg(off, "r0")
			e.storeSlotReg(off+4, "r1")
		} else {
			e.storeSlotReg(e.slots[in.Dst], "r0")
		}
	}
	return nil
}

func callStackWordsARM(in *Inst) int {
	ri, sw := 0, 0
	for _, ty := range in.OpTys {
		w := words(ty)
		if ri+w <= 4 && ri < 4 {
			ri += w
		} else {
			sw += w
		}
	}
	return sw
}

func (e *arm) emitIntrinsic(in *Inst) error {
	switch {
	case strings.HasPrefix(in.Callee, "llvm.va_start"):
		if err := e.loadVal(in.Ops[0], "r1"); err != nil {
			return err
		}
		if e.nFixed < 4 {
			e.addrOf(e.saveOff+e.nFixed*4, "r0")
		} else {
			e.addrOf(e.frame+(e.nFixed-4)*4, "r0")
		}
		e.line("str r0, [r1]")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.va_end"):
		return nil
	case strings.HasPrefix(in.Callee, "llvm.va_copy"):
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		e.line("ldr r2, [r1]")
		e.line("str r2, [r0]")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.memcpy"), strings.HasPrefix(in.Callee, "llvm.memmove"):
		if len(in.Ops) < 3 {
			return fmt.Errorf("memcpy with %d args", len(in.Ops))
		}
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[2], "r2"); err != nil {
			return err
		}
		e.line("bl memcpy")
		return nil
	case strings.HasPrefix(in.Callee, "llvm.memset"):
		if len(in.Ops) < 3 {
			return fmt.Errorf("memset with %d args", len(in.Ops))
		}
		if err := e.loadVal(in.Ops[0], "r0"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[1], "r1"); err != nil {
			return err
		}
		if err := e.loadVal(in.Ops[2], "r2"); err != nil {
			return err
		}
		e.line("bl memset")
		return nil
	}
	if in.Dst != "" {
		e.li("r0", 0)
		return e.storeDst(in.Dst, "r0")
	}
	return nil
}
