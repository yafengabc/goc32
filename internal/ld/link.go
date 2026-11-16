// Package ld links goc32 objects into the position-independent ELF TinyOS
// loads.  It implements the part of user/user.ld that the build depends on
// (.text, .rodata, .note.tos, .data, .dynamic, .rela.dyn, .bss), drops
// unreachable sections the way --gc-sections does, and writes an ET_DYN image
// linked at 0.
//
// The kernel relocates the image by adding the load bias to every entry in
// .rela.dyn, so absolute addresses (ARM literal words, pointer initialisers)
// are emitted as R_ARM_RELATIVE / R_RISCV_RELATIVE with the addend already
// pointing at the link-time address.
package ld

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"

	"goc32/internal/obj"
)

const (
	EM_ARM   = 40
	EM_RISCV = 243

	R_RISCV_RELATIVE = 3
	R_ARM_RELATIVE   = 23

	ptLoad    = 1
	ptDynamic = 2
	ptNote    = 4

	pfX = 1
	pfW = 2
	pfR = 4
)

// Options describes the image to produce.
type Options struct {
	Machine uint16
	Thumb   bool
	Entry   string
}

type symRef struct {
	obj *obj.Object
	sym *obj.Sym
}

type piece struct {
	o   *obj.Object
	sec *obj.Sec
	grp group
}

type group int

const (
	gText group = iota
	gRo
	gNote
	gData
	gBss
	gDiscard
)

func classify(name string) group {
	switch {
	case name == ".text" || strings.HasPrefix(name, ".text."):
		return gText
	case strings.HasPrefix(name, ".rodata"):
		return gRo
	case name == ".note.tos":
		return gNote
	case name == ".data" || strings.HasPrefix(name, ".data.") ||
		strings.HasPrefix(name, ".got"):
		return gData
	case name == ".bss" || strings.HasPrefix(name, ".bss."):
		return gBss
	}
	return gDiscard
}

// Link performs the whole link in one call.  It returns the ELF file bytes.
func Link(objs []*obj.Object, opt Options) ([]byte, error) {
	globals := map[string]*obj.Sym{}
	for _, o := range objs {
		for _, s := range o.Syms {
			if s.Global && s.Sec != nil {
				globals[s.Name] = s
			}
		}
	}
	resolve := func(o *obj.Object, name string) (*obj.Sym, error) {
		if s, ok := o.Syms[name]; ok && s.Sec != nil {
			return s, nil
		}
		if s, ok := globals[name]; ok {
			return s, nil
		}
		return nil, fmt.Errorf("%s: undefined symbol %q", o.Path, name)
	}

	// ---- reachability, the --gc-sections equivalent -----------------------
	live := map[*obj.Sec]bool{}
	var mark func(p *piece)
	var pieces []piece
	for _, o := range objs {
		for _, s := range o.Secs {
			g := classify(s.Name)
			if g == gDiscard {
				continue
			}
			pieces = append(pieces, piece{o: o, sec: s, grp: g})
		}
	}
	owner := map[*obj.Sec]*obj.Object{}
	for _, p := range pieces {
		owner[p.sec] = p.o
	}
	mark = func(p *piece) {
		if p == nil || p.grp == gDiscard || live[p.sec] {
			return
		}
		live[p.sec] = true
		for _, r := range p.sec.Relocs {
			s, err := resolve(p.o, r.Sym)
			if err != nil || s.Sec == nil {
				continue
			}
			mark(&piece{o: owner[s.Sec], sec: s.Sec, grp: classify(s.Sec.Name)})
		}
	}
	if es, ok := globals[opt.Entry]; ok {
		mark(&piece{o: owner[es.Sec], sec: es.Sec, grp: classify(es.Sec.Name)})
	} else {
		return nil, fmt.Errorf("entry symbol %q is not defined", opt.Entry)
	}
	for _, p := range pieces {
		if p.grp == gNote {
			mark(&p)
		}
	}
	if !live[globals[opt.Entry].Sec] {
		return nil, fmt.Errorf("entry section was collected")
	}

	// ---- order the pieces the way user.ld does ----------------------------
	var text, ro, note, data, bss []piece
	for _, p := range pieces {
		if !live[p.sec] {
			continue
		}
		switch p.grp {
		case gText:
			text = append(text, p)
		case gRo:
			ro = append(ro, p)
		case gNote:
			note = append(note, p)
		case gData:
			data = append(data, p)
		case gBss:
			bss = append(bss, p)
		}
	}
	// *(.text.entry) comes first; the rest keep their input order.
	sort.SliceStable(text, func(i, j int) bool {
		return text[i].sec.Name == ".text.entry" && text[j].sec.Name != ".text.entry"
	})
	// *(.data.rel.ro*) before *(.data*) before *(.got*).
	rank := func(name string) int {
		if strings.HasPrefix(name, ".data.rel.ro") {
			return 0
		}
		if strings.HasPrefix(name, ".got") {
			return 2
		}
		return 1
	}
	sort.SliceStable(data, func(i, j int) bool { return rank(data[i].sec.Name) < rank(data[j].sec.Name) })

	align := func(v, a uint32) uint32 {
		if a <= 1 {
			return v
		}
		return (v + a - 1) / a * a
	}

	addr := uint32(0)
	for i := range text {
		addr = align(addr, 4)
		text[i].sec.OutAddr = addr
		addr += uint32(text[i].sec.Size)
	}
	textEnd := addr

	addr = align(addr, 16)
	roStart := addr
	for i := range ro {
		addr = align(addr, uint32(max1(ro[i].sec.Align)))
		ro[i].sec.OutAddr = addr
		addr += uint32(ro[i].sec.Size)
	}
	for i := range note {
		addr = align(addr, 4)
		note[i].sec.OutAddr = addr
		addr += uint32(note[i].sec.Size)
	}
	roEnd := addr

	addr = align(addr, 32)
	dataStart := addr
	for i := range data {
		addr = align(addr, uint32(max1(data[i].sec.Align)))
		data[i].sec.OutAddr = addr
		addr += uint32(data[i].sec.Size)
	}
	dataEnd := addr

	// Count the absolute words first: their number decides the size of
	// .rela.dyn, and .bss starts after it.
	ndyn := 0
	for _, p := range pieces {
		if !live[p.sec] {
			continue
		}
		for _, r := range p.sec.Relocs {
			if r.Kind == obj.Abs32 {
				ndyn++
			}
		}
	}
	dynSize := uint32(0)
	relaSize := uint32(0)
	if ndyn > 0 {
		dynSize = 4 * 8
		relaSize = uint32(ndyn) * 12
	}
	addr = align(addr, 4)
	dynAddr := addr
	addr += dynSize
	addr = align(addr, 4)
	relaAddr := addr
	addr += relaSize
	addr = align(addr, 16)
	bssAddr := addr
	for i := range bss {
		addr = align(addr, uint32(max1(bss[i].sec.Align)))
		bss[i].sec.OutAddr = addr
		addr += uint32(bss[i].sec.Size)
	}
	bssEnd := addr

	// ---- symbol addresses -------------------------------------------------
	if os.Getenv("GOC32_MAP") != "" {
		for _, p := range pieces {
			if !live[p.sec] {
				continue
			}
			fmt.Printf("map %-28s %06x %5d  %s\n", p.sec.Name, p.sec.OutAddr, p.sec.Size, p.o.Path)
		}
	}
	for _, o := range objs {
		for _, s := range o.Syms {
			if s.Sec == nil {
				continue
			}
			s.Addr = s.Sec.OutAddr + uint32(s.Value)
			if opt.Thumb && s.Func && s.Sec.Flags&obj.SHF_EXECINSTR != 0 {
				s.Addr |= 1
			}
		}
	}

	// ---- relocations ------------------------------------------------------
	var dyn []struct {
		off    uint32
		typ    uint32
		addend uint32
	}
	rel := uint32(R_RISCV_RELATIVE)
	if opt.Machine == EM_ARM {
		rel = R_ARM_RELATIVE
	}
	for _, p := range pieces {
		if !live[p.sec] || p.sec.Type == obj.SHT_NOBITS {
			continue
		}
		for _, r := range p.sec.Relocs {
			s, err := resolve(p.o, r.Sym)
			if err != nil {
				return nil, err
			}
			S := int64(s.Addr)
			P := int64(p.sec.OutAddr) + int64(r.Off)
			switch r.Kind {
			case obj.Abs32:
				val := uint32(S + r.Addend)
				if err := put32(p.sec, r.Off, val); err != nil {
					return nil, fmt.Errorf("%s: %v", p.o.Path, err)
				}
				dyn = append(dyn, struct {
					off    uint32
					typ    uint32
					addend uint32
				}{uint32(P), rel, val})
			case obj.RVPCRel, obj.RVCall:
				delta := S + r.Addend - P
				hi := (delta + 0x800) >> 12
				lo := delta - hi<<12
				i0, err := get32(p.sec, r.Off)
				if err != nil {
					return nil, err
				}
				i1, err := get32(p.sec, r.Off+4)
				if err != nil {
					return nil, err
				}
				i0 = i0&0x00000fff | uint32(hi&0xfffff)<<12
				i1 = i1&0x000fffff | uint32(lo&0xfff)<<20
				if err := put32(p.sec, r.Off, i0); err != nil {
					return nil, err
				}
				if err := put32(p.sec, r.Off+4, i1); err != nil {
					return nil, err
				}
			case obj.RVJal:
				i0, err := get32(p.sec, r.Off)
				if err != nil {
					return nil, err
				}
				rd := (i0 >> 7) & 0x1f
				off := S + r.Addend - P
				if err := put32(p.sec, r.Off, encJ(off, rd)); err != nil {
					return nil, err
				}
			case obj.RVBranch:
				i0, err := get32(p.sec, r.Off)
				if err != nil {
					return nil, err
				}
				rs1 := (i0 >> 15) & 0x1f
				rs2 := (i0 >> 20) & 0x1f
				f3 := (i0 >> 12) & 7
				off := S + r.Addend - P
				if err := put32(p.sec, r.Off, encB(off, rs2, rs1, f3)); err != nil {
					return nil, err
				}
			case obj.ARMBl:
				off := (S &^ 1) + r.Addend - (P + 4)
				a, b, err := encThumbBL(off)
				if err != nil {
					return nil, fmt.Errorf("%s: %v", p.o.Path, err)
				}
				if err := put16(p.sec, r.Off, a); err != nil {
					return nil, err
				}
				if err := put16(p.sec, r.Off+2, b); err != nil {
					return nil, err
				}
			}
		}
	}

	// ---- .dynamic and .rela.dyn ------------------------------------------
	var dynBytes, relaBytes []byte
	if ndyn > 0 {
		for _, e := range []struct {
			tag, val uint32
		}{
			{7, relaAddr}, // DT_RELA
			{8, relaSize}, // DT_RELASZ
			{9, 12},       // DT_RELAENT
			{0, 0},        // DT_NULL
		} {
			dynBytes = append(dynBytes, u32(e.tag)...)
			dynBytes = append(dynBytes, u32(e.val)...)
		}
		if len(dyn) != ndyn {
			return nil, fmt.Errorf("internal: %d relocations, expected %d", len(dyn), ndyn)
		}
		for _, e := range dyn {
			relaBytes = append(relaBytes, u32(e.off)...)
			relaBytes = append(relaBytes, u32(e.typ)...)
			relaBytes = append(relaBytes, u32(e.addend)...)
		}
	}

	// ---- build section blobs ---------------------------------------------
	roBytes := make([]byte, roEnd-roStart)
	for _, p := range ro {
		off := p.sec.OutAddr - roStart
		copy(roBytes[off:], p.sec.Data)
	}
	for _, p := range note {
		off := p.sec.OutAddr - roStart
		copy(roBytes[off:], p.sec.Data)
	}
	dataLast := relaAddr + relaSize
	if dataLast < dynAddr+dynSize {
		dataLast = dynAddr + dynSize
	}
	if dataLast < dataEnd {
		dataLast = dataEnd
	}
	dataBytes := make([]byte, dataLast-dataStart)
	for _, p := range data {
		off := p.sec.OutAddr - dataStart
		copy(dataBytes[off:], p.sec.Data)
	}
	if dynSize > 0 {
		copy(dataBytes[dynAddr-dataStart:], dynBytes)
		copy(dataBytes[relaAddr-dataStart:], relaBytes)
	}
	bssSize := bssEnd - bssAddr

	// ---- ELF writing ------------------------------------------------------
	machine := opt.Machine
	var flags uint32
	if machine == EM_ARM {
		flags = 0x05000200 // EABI version 5, soft-float ABI
	}
	entry := globals[opt.Entry].Addr

	phnum := 1
	hasRo := roEnd > roStart
	hasData := dataLast > dataStart || bssSize > 0
	hasNote := len(note) > 0
	hasDyn := ndyn > 0
	if hasRo {
		phnum++
	}
	if hasData {
		phnum++
	}
	if hasNote {
		phnum++
	}
	if hasDyn {
		phnum++
	}

	hdrsize := uint32(52 + phnum*32)
	textSize := textEnd
	offText := align(hdrsize, 16)
	offRo := align(max32(offText+textSize, roStart), 16)
	offData := align(max32(offRo+uint32(len(roBytes)), dataStart), 16)

	// Place .shstrtab and the section headers after everything else.
	fileEnd := offData + uint32(len(dataBytes))

	// The note blob is whatever .note.tos sections the link kept; they are
	// contiguous at the end of the read-only band.
	noteStart, noteSize := uint32(0), uint32(0)
	if len(note) > 0 {
		noteStart = note[0].sec.OutAddr
		noteSize = roEnd - noteStart
	}
	noteFileOff := offRo + (noteStart - roStart)
	roDataEnd := roStart
	if len(ro) > 0 {
		last := ro[len(ro)-1].sec
		roDataEnd = last.OutAddr + uint32(last.Size)
	}

	// Section header indices for the sections that exist.
	type shdr struct {
		name            string
		typ, flags      uint32
		addr, off, size uint32
		entsize         uint32
	}
	var shdrs []shdr
	add := func(s shdr) {
		if s.typ == 0 {
			s.typ = 1
		}
		shdrs = append(shdrs, s)
	}
	if textSize > 0 {
		add(shdr{name: ".text", typ: 1, flags: 2 | 4, addr: 0, off: offText, size: textSize})
	}
	if len(ro) > 0 {
		add(shdr{name: ".rodata", typ: 1, flags: 2, addr: roStart, off: offRo, size: roDataEnd - roStart})
	}
	if hasNote {
		// The note lives inside the read-only blob; its own header is only
		// for tools.
		add(shdr{name: ".note.tos", typ: 7, flags: 2, addr: noteStart, off: noteFileOff, size: noteSize})
	}
	if dataEnd > dataStart {
		add(shdr{name: ".data", typ: 1, flags: 3, addr: dataStart, off: offData, size: dataEnd - dataStart})
	}
	if hasDyn {
		add(shdr{name: ".dynamic", typ: 6, flags: 3, addr: dynAddr, off: offData + (dynAddr - dataStart), size: dynSize, entsize: 8})
		add(shdr{name: ".rela.dyn", typ: 4, flags: 2, addr: relaAddr, off: offData + (relaAddr - dataStart), size: relaSize, entsize: 12})
	}
	if bssSize > 0 {
		add(shdr{name: ".bss", typ: 8, flags: 3, addr: bssAddr, size: bssSize})
	}

	shnames := []string{"", ".text", ".rodata", ".note.tos", ".data", ".dynamic", ".rela.dyn", ".bss", ".shstrtab"}
	var shstr []byte
	shoff := make([]uint32, len(shnames))
	for i, n := range shnames {
		shoff[i] = uint32(len(shstr))
		shstr = append(shstr, []byte(n)...)
		shstr = append(shstr, 0)
	}
	shTableOff := align(fileEnd, 4)
	shstrIdx := len(shdrs) + 1
	shnum := shstrIdx + 1
	shstrFileOff := shTableOff + uint32(shnum*40)
	total := shstrFileOff + uint32(len(shstr))

	// The section header table order: null, then the sections we added, then
	// .shstrtab.  index(shdrs[i]) = i+1.
	nameOff := func(n string) uint32 {
		for i, s := range shnames {
			if s == n {
				return shoff[i]
			}
		}
		return 0
	}

	buf := make([]byte, total)

	// ELF header.
	copy(buf[0:], []byte{0x7f, 'E', 'L', 'F', 1, 1, 1})
	binary.LittleEndian.PutUint16(buf[16:], 3) // ET_DYN
	binary.LittleEndian.PutUint16(buf[18:], machine)
	binary.LittleEndian.PutUint32(buf[20:], 1)
	binary.LittleEndian.PutUint32(buf[24:], entry)
	binary.LittleEndian.PutUint32(buf[28:], 52)
	binary.LittleEndian.PutUint32(buf[32:], shTableOff)
	binary.LittleEndian.PutUint32(buf[36:], flags)
	binary.LittleEndian.PutUint16(buf[40:], 52)
	binary.LittleEndian.PutUint16(buf[42:], 32)
	binary.LittleEndian.PutUint16(buf[44:], uint16(phnum))
	binary.LittleEndian.PutUint16(buf[46:], 40)
	binary.LittleEndian.PutUint16(buf[48:], uint16(shnum))
	binary.LittleEndian.PutUint16(buf[50:], uint16(shstrIdx))

	ph := func(i int, typ, off, vaddr, filesz, memsz, fl, al uint32) {
		b := buf[52+i*32:]
		binary.LittleEndian.PutUint32(b[0:], typ)
		binary.LittleEndian.PutUint32(b[4:], off)
		binary.LittleEndian.PutUint32(b[8:], vaddr)
		binary.LittleEndian.PutUint32(b[12:], vaddr)
		binary.LittleEndian.PutUint32(b[16:], filesz)
		binary.LittleEndian.PutUint32(b[20:], memsz)
		binary.LittleEndian.PutUint32(b[24:], fl)
		binary.LittleEndian.PutUint32(b[28:], al)
	}
	n := 0
	ph(n, ptLoad, offText, 0, textSize, textSize, pfR|pfX, 16)
	n++
	if hasRo {
		ph(n, ptLoad, offRo, roStart, roEnd-roStart, roEnd-roStart, pfR, 16)
		n++
	}
	if hasData {
		// memsz runs from the first data byte to the end of .bss, including
		// the alignment gap between them: the kernel zeroes [vaddr+filesz,
		// vaddr+memsz) and that includes the gap and every .bss byte.
		dataMem := bssEnd - dataStart
		ph(n, ptLoad, offData, dataStart, dataLast-dataStart, dataMem, pfR|pfW, 16)
		n++
	}
	if hasNote {
		ph(n, ptNote, noteFileOff, noteStart, noteSize, noteSize, pfR, 4)
		n++
	}
	if hasDyn {
		ph(n, ptDynamic, offData+(dynAddr-dataStart), dynAddr, dynSize, dynSize, pfR|pfW, 4)
		n++
	}

	// Segment contents.
	for _, p := range text {
		copy(buf[offText+p.sec.OutAddr:], p.sec.Data)
	}
	copy(buf[offRo:], roBytes)
	copy(buf[offData:], dataBytes)

	// Section headers.
	sh := func(i int, nameOff, typ, flags, addr, off, size, link, info, align, entsize uint32) {
		b := buf[shTableOff+uint32(i)*40:]
		binary.LittleEndian.PutUint32(b[0:], nameOff)
		binary.LittleEndian.PutUint32(b[4:], typ)
		binary.LittleEndian.PutUint32(b[8:], flags)
		binary.LittleEndian.PutUint32(b[12:], addr)
		binary.LittleEndian.PutUint32(b[16:], off)
		binary.LittleEndian.PutUint32(b[20:], size)
		binary.LittleEndian.PutUint32(b[24:], link)
		binary.LittleEndian.PutUint32(b[28:], info)
		binary.LittleEndian.PutUint32(b[32:], align)
		binary.LittleEndian.PutUint32(b[36:], entsize)
	}
	for i, s := range shdrs {
		sh(i+1, nameOff(s.name), s.typ, s.flags, s.addr, s.off, s.size, 0, 0, 4, s.entsize)
	}
	sh(shstrIdx, nameOff(".shstrtab"), 3, 0, 0, shstrFileOff, uint32(len(shstr)), 0, 0, 1, 0)
	copy(buf[shstrFileOff:], shstr)
	return buf, nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func u32(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
}

func max32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

func get32(s *obj.Sec, off int) (uint32, error) {
	if off < 0 || off+4 > len(s.Data) {
		return 0, fmt.Errorf("relocation at %d outside %s", off, s.Name)
	}
	return binary.LittleEndian.Uint32(s.Data[off:]), nil
}

func put32(s *obj.Sec, off int, v uint32) error {
	if off < 0 || off+4 > len(s.Data) {
		return fmt.Errorf("relocation at %d outside %s", off, s.Name)
	}
	binary.LittleEndian.PutUint32(s.Data[off:], v)
	return nil
}

func put16(s *obj.Sec, off int, v uint16) error {
	if off < 0 || off+2 > len(s.Data) {
		return fmt.Errorf("relocation at %d outside %s", off, s.Name)
	}
	binary.LittleEndian.PutUint16(s.Data[off:], v)
	return nil
}

func encJ(off int64, rd uint32) uint32 {
	u := uint32(off)
	return (u>>20&1)<<31 | (u>>1&0x3ff)<<21 | (u>>11&1)<<20 | (u>>12&0xff)<<12 | rd<<7 | 0x6f
}

func encB(off int64, rs2, rs1, f3 uint32) uint32 {
	u := uint32(off)
	return (u>>12&1)<<31 | (u>>5&0x3f)<<25 | rs2<<20 | rs1<<15 | f3<<12 |
		(u>>1&0xf)<<8 | (u>>11&1)<<7 | 0x63
}

func encThumbBL(off int64) (uint16, uint16, error) {
	if off < -(1<<24) || off >= 1<<24 || off%2 != 0 {
		return 0, 0, fmt.Errorf("bl target out of range")
	}
	u := uint32(off)
	s := (u >> 24) & 1
	i1 := (u >> 23) & 1
	i2 := (u >> 22) & 1
	j1 := (^(i1 ^ s)) & 1
	j2 := (^(i2 ^ s)) & 1
	hw1 := uint16(0xF000) | uint16(s)<<10 | uint16(u>>12&0x3ff)
	hw2 := uint16(0xD000) | uint16(j1)<<13 | uint16(j2)<<11 | uint16(u>>1&0x7ff)
	return hw1, hw2, nil
}
