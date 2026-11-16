package as

import (
	"fmt"
	"strconv"
	"strings"

	"goc32/internal/obj"
)

var thRegs = map[string]uint32{
	"sp": 13, "lr": 14, "pc": 15, "ip": 12, "sl": 10, "fp": 11,
}

func thReg(s string) (uint32, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if r, ok := thRegs[s]; ok {
		return r, nil
	}
	if strings.HasPrefix(s, "r") {
		n, err := strconv.Atoi(s[1:])
		if err == nil && n >= 0 && n < 16 {
			return uint32(n), nil
		}
	}
	return 0, fmt.Errorf("bad Thumb register %q", s)
}

func thImm(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("bad immediate %q", s)
	}
	return v, nil
}

var condCodes = map[string]uint16{
	"eq": 0, "ne": 1, "cs": 2, "hs": 2, "cc": 3, "lo": 3,
	"mi": 4, "pl": 5, "vs": 6, "vc": 7, "hi": 8, "ls": 9,
	"ge": 10, "lt": 11, "gt": 12, "le": 13, "al": 14,
}

func t16(v uint16) []byte    { return u16(v) }
func t32(a, b uint16) []byte { return []byte{byte(a), byte(a >> 8), byte(b), byte(b >> 8)} }

func alignTo(n, a int) int {
	if a <= 1 {
		return n
	}
	return (n + a - 1) / a * a
}

// memThumb parses "[rn, #imm]" and "[rn]".
func memThumb(s string) (base uint32, off int64, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return 0, 0, fmt.Errorf("bad memory operand %q", s)
	}
	inner := s[1 : len(s)-1]
	if i := strings.IndexByte(inner, ','); i >= 0 {
		base, err = thReg(inner[:i])
		if err != nil {
			return 0, 0, err
		}
		off, err = thImm(inner[i+1:])
		return base, off, err
	}
	base, err = thReg(inner)
	return base, 0, err
}

func low(r uint32) bool { return r < 8 }

// thumbInsn encodes one Thumb instruction.
func (a *asm) thumbInsn(text string, off int, sizeOnly bool) ([]byte, bool, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil, false, nil
	}
	mn := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(text, mn))
	ops := splitOps(rest)

	get := func(i int) (string, error) {
		if i >= len(ops) {
			return "", a.errf("%s: missing operand", mn)
		}
		return ops[i], nil
	}
	reg := func(i int) (uint32, error) {
		s, err := get(i)
		if err != nil {
			return 0, err
		}
		return thReg(s)
	}
	imm := func(i int) (int64, error) {
		s, err := get(i)
		if err != nil {
			return 0, err
		}
		return thImm(s)
	}
	one := func(v uint16) ([]byte, bool, error) { return t16(v), false, nil }
	two := func(a, b uint16) ([]byte, bool, error) { return t32(a, b), false, nil }

	// Unconditional and conditional branches, and BL.
	if mn == "b" {
		return a.thumbB(ops[0], off, sizeOnly)
	}
	if len(mn) > 1 && mn[0] == 'b' && mn != "bl" && mn != "blx" && mn != "bx" {
		cond, ok := condCodes[mn[1:]]
		if !ok {
			return nil, false, a.errf("unknown branch %q", mn)
		}
		name := ops[0]
		s, found := a.obj.Syms[name]
		if !found || s.Sec == nil {
			if sizeOnly {
				if a.widen[a.line] {
					return t32(0xF000, 0x8000), false, nil
				}
				return one(0xD000 | cond<<8)
			}
			return nil, false, a.errf("undefined symbol %q", name)
		}
		if s.Sec != a.cur {
			return nil, false, a.errf("conditional branch to another section (%s)", name)
		}
		if sizeOnly {
			if a.widen[a.line] {
				return t32(0xF000, 0x8000), false, nil
			}
			return one(0xD000 | cond<<8)
		}
		rel := s.Value - (off + 4)
		if rel >= -256 && rel <= 254 && rel%2 == 0 {
			return one(0xD000 | cond<<8 | uint16(rel>>1)&0xff)
		}
		if a.arch != Thumb2 {
			return t16(0xD000 | cond<<8), true, nil
		}
		h1, h2 := thumbCondWide(cond, rel)
		return t32(h1, h2), true, nil
	}

	switch mn {
	case "nop":
		return one(0xBF00)
	case "bx":
		r, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		return one(0x4700 | uint16(r&15)<<3)
	case "blx":
		r, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		return one(0x4780 | uint16(r&15)<<3)
	case "bl":
		e, err := parseExpr(ops[0])
		if err != nil || !e.hasSym {
			return nil, false, a.errf("bl needs a symbol")
		}
		a.addReloc(obj.ARMBl, off, e.sym, e.add)
		return t32(0xF000, 0xF800), false, nil
	case "push", "pop":
		list, err := thRegList(ops[0])
		if err != nil {
			return nil, false, err
		}
		mask := uint16(0)
		for _, r := range list {
			if r < 8 {
				mask |= 1 << r
			} else if r == 14 && mn == "push" {
				mask |= 0x100
			} else if r == 15 && mn == "pop" {
				mask |= 0x100
			} else {
				return nil, false, a.errf("%s cannot move %s", mn, regName(r))
			}
		}
		if mn == "push" {
			return one(0xB400 | mask)
		}
		return one(0xBC00 | mask)
	case "svc":
		v, err := imm(0)
		if err != nil {
			return nil, false, err
		}
		return one(0xDF00 | uint16(v&0xff))
	case "movw", "movt":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		v, err := imm(1)
		if err != nil {
			return nil, false, err
		}
		hw := uint16(0xF240)
		if mn == "movt" {
			hw = 0xF2C0
		}
		u := uint16(v)
		i := (u >> 11) & 1
		imm4 := (u >> 12) & 0xf
		imm3 := (u >> 8) & 7
		imm8 := u & 0xff
		return two(hw|i<<10|imm4, imm3<<12|uint16(rd)<<8|imm8)
	case "mov":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		if len(ops) > 1 && strings.HasPrefix(strings.TrimSpace(ops[1]), "#") {
			v, err := imm(1)
			if err != nil {
				return nil, false, err
			}
			if !low(rd) || v < 0 || v > 255 {
				return nil, false, a.errf("mov #imm needs a low register and 0..255")
			}
			return one(0x2000 | uint16(rd)<<8 | uint16(v))
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		return one(0x4600 | uint16((rd&8)<<4) | uint16(rm&15)<<3 | uint16(rd&7))
	case "movs":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		if strings.HasPrefix(strings.TrimSpace(ops[1]), "#") {
			v, err := imm(1)
			if err != nil {
				return nil, false, err
			}
			if !low(rd) || v < 0 || v > 255 {
				return nil, false, a.errf("movs #imm needs a low register and 0..255")
			}
			return one(0x2000 | uint16(rd)<<8 | uint16(v))
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		if low(rd) && low(rm) {
			return one(uint16(rm)<<3 | uint16(rd))
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("movs with a high register needs Thumb-2")
		}
		return two(0xEA5F, uint16(rd)<<8|uint16(rm))
	case "muls":
		if len(ops) != 2 {
			return nil, false, a.errf("muls takes two registers")
		}
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		if !low(rd) || !low(rm) {
			return nil, false, a.errf("muls needs low registers")
		}
		return one(0x4340 | uint16(rm)<<3 | uint16(rd))
	case "cmp":
		rn, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		if strings.HasPrefix(strings.TrimSpace(ops[1]), "#") {
			v, err := imm(1)
			if err != nil {
				return nil, false, err
			}
			if low(rn) && v >= 0 && v <= 255 {
				return one(0x2800 | uint16(rn)<<8 | uint16(v))
			}
			if a.arch != Thumb2 {
				return nil, false, a.errf("cmp with a high register or big immediate needs Thumb-2")
			}
			return a.thumbImm12(0xF1B0, 0, rn, v, 15, 0)
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		if low(rn) && low(rm) {
			return one(0x4280 | uint16(rm)<<3 | uint16(rn))
		}
		return one(0x4500 | uint16((rn&8)<<4) | uint16(rm&15)<<3 | uint16(rn&7))
	case "and", "ands", "orr", "orrs", "eor", "eors":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		base := uint16(0x4000)
		wide := uint16(0xEA00)
		switch {
		case strings.HasPrefix(mn, "orr"):
			base, wide = 0x4300, 0xEA40
		case strings.HasPrefix(mn, "eor"):
			base, wide = 0x4040, 0xEA80
		}
		if low(rd) && low(rm) && mn[len(mn)-1] == 's' {
			return one(base | uint16(rm)<<3 | uint16(rd))
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("%s needs low registers on ARMv6-M", mn)
		}
		s := uint16(0x10)
		if mn[len(mn)-1] != 's' {
			s = 0
		}
		return two(wide|s|uint16(rd&15), uint16(rd&15)<<8|uint16(rm&15))
	case "adcs", "sbcs":
		if len(ops) != 3 {
			return nil, false, a.errf("%s takes three registers", mn)
		}
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rn, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		rm, err := reg(2)
		if err != nil {
			return nil, false, err
		}
		if low(rd) && low(rn) && low(rm) && rd == rn {
			hw := uint16(0x4140)
			if mn == "sbcs" {
				hw = 0x4180
			}
			return one(hw | uint16(rm)<<3 | uint16(rd))
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("%s with three registers needs Thumb-2", mn)
		}
		hw := uint16(0xEB40)
		if mn == "sbcs" {
			hw = 0xEB60
		}
		return two(hw|0x10|uint16(rn&15), uint16(rd&15)<<8|uint16(rm&15))
	case "adds", "subs", "add", "sub":
		return a.thumbAddSub(mn, ops, off)
	case "lsls", "lsrs", "asrs", "lsl", "lsr", "asr":
		return a.thumbShift(mn, ops)
	case "sxtb", "sxth", "uxtb", "uxth":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rm, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		small := map[string]uint16{"sxtb": 0xB240, "sxth": 0xB200, "uxtb": 0xB2C0, "uxth": 0xB280}
		wide := map[string]uint16{"sxtb": 0xFA4F, "sxth": 0xFA0F, "uxtb": 0xFA5F, "uxth": 0xFA1F}
		if low(rd) && low(rm) {
			return one(small[mn] | uint16(rm)<<3 | uint16(rd))
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("%s needs low registers on ARMv6-M", mn)
		}
		return two(wide[mn], 0xF000|uint16(rd)<<8|uint16(rm))
	case "ldr", "str", "ldrb", "strb", "ldrh", "strh":
		return a.thumbLoadStore(mn, ops, off, sizeOnly)
	}
	return nil, false, a.errf("unsupported Thumb instruction %q", mn)
}

func regName(r uint32) string {
	switch r {
	case 13:
		return "sp"
	case 14:
		return "lr"
	case 15:
		return "pc"
	}
	return fmt.Sprintf("r%d", r)
}

func thRegList(s string) ([]uint32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("bad register list %q", s)
	}
	var out []uint32
	for _, part := range strings.Split(s[1:len(s)-1], ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		r, err := thReg(part)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// thumbCondWide encodes B<c>.W (T3), the +/-1 MiB form of a conditional
// branch.  The 16-bit form reaches only +/-256 bytes, and a good-sized
// function exceeds that easily.
func thumbCondWide(cond uint16, rel int) (uint16, uint16) {
	u := uint32(rel)
	s := (u >> 24) & 1
	h1 := uint16(0xF000) | uint16(s)<<10 | cond<<6 | uint16(u>>12&0x3f)
	h2 := uint16(0x8000) | uint16(u>>23&1)<<13 | uint16(u>>22&1)<<11 | uint16(u>>1&0x7ff)
	return h1, h2
}

func (a *asm) thumbB(name string, off int, sizeOnly bool) ([]byte, bool, error) {
	// The sizing pass runs before a forward label exists, so it must decide the
	// width from the relaxation set alone.
	if sizeOnly {
		if a.widen[a.line] {
			return t32(0xF000, 0x9000), false, nil
		}
		return t16(0xE000), false, nil
	}
	s, ok := a.obj.Syms[name]
	if !ok || s.Sec == nil {
		return nil, false, a.errf("undefined symbol %q", name)
	}
	if s.Sec != a.cur {
		return nil, false, a.errf("unconditional branch to another section (%s)", name)
	}
	rel := s.Value - (off + 4)
	if rel >= -2048 && rel <= 2046 && rel%2 == 0 {
		return t16(0xE000 | uint16(rel>>1)&0x7ff), false, nil
	}
	if a.arch != Thumb2 {
		return t16(0xE000), true, nil
	}
	u := uint32(rel)
	hw1 := uint16(0xF000) | uint16(u>>24&1)<<10 | uint16(u>>12&0x3ff)
	i1 := (u >> 23) & 1
	i2 := (u >> 22) & 1
	sbit := (u >> 24) & 1
	j1 := (^(i1 ^ sbit)) & 1
	j2 := (^(i2 ^ sbit)) & 1
	hw2 := uint16(0x9000) | uint16(j1)<<13 | uint16(j2)<<11 | uint16(u>>1&0x7ff)
	return t32(hw1, hw2), true, nil
}

// thumbImm12 encodes ADD/SUB/CMP (immediate) T3/T2: the 12-bit immediate is
// split as i:imm3:imm8, with the opcode base in base.
func (a *asm) thumbImm12(base uint16, s uint16, rn uint32, v int64, rd uint32, extra uint16) ([]byte, bool, error) {
	if v < 0 || v > 4095 {
		return nil, false, a.errf("immediate %d out of range", v)
	}
	u := uint16(v)
	i := (u >> 11) & 1
	imm3 := (u >> 8) & 7
	imm8 := u & 0xff
	hw1 := base | i<<10 | s | uint16(rn&15)
	hw2 := imm3<<12 | uint16(rd&15)<<8 | imm8 | extra
	return t32(hw1, hw2), false, nil
}

func (a *asm) thumbAddSub(mn string, ops []string, off int) ([]byte, bool, error) {
	isSub := strings.HasPrefix(mn, "sub")
	withS := strings.HasSuffix(mn, "s")
	rd, err := thReg(ops[0])
	if err != nil {
		return nil, false, err
	}
	if len(ops) == 2 {
		// adds rd, #imm / sub sp, #imm
		v, err := thImm(ops[1])
		if err != nil {
			return nil, false, err
		}
		if rd == 13 && !isSub && v >= 0 && v%4 == 0 && v <= 1020 {
			return t16(0xB000 | uint16(v/4)), false, nil
		}
		if rd == 13 && isSub && v >= 0 && v%4 == 0 && v <= 508 {
			return t16(0xB080 | uint16(v/4)), false, nil
		}
		if !withS {
			return nil, false, a.errf("%s needs two operands", mn)
		}
		if low(rd) && v >= 0 && v <= 255 {
			base := uint16(0x3000)
			if isSub {
				base = 0x3800
			}
			return t16(base | uint16(rd)<<8 | uint16(v)), false, nil
		}
		return a.thumbImm13(mn, rd, rd, v, withS)
	}
	rn, err := thReg(ops[1])
	if err != nil {
		return nil, false, err
	}
	if strings.HasPrefix(strings.TrimSpace(ops[2]), "#") {
		v, err := thImm(ops[2])
		if err != nil {
			return nil, false, err
		}
		if !isSub && rn == 13 && v >= 0 && v%4 == 0 && v <= 1020 {
			return t16(0xA800 | uint16(rd)<<8 | uint16(v/4)), false, nil
		}
		if rd == 13 && !isSub && v >= 0 && v%4 == 0 && v <= 508 {
			return t16(0xB000 | uint16(v/4)), false, nil
		}
		if rd == 13 && isSub && v >= 0 && v%4 == 0 && v <= 508 {
			return t16(0xB080 | uint16(v/4)), false, nil
		}
		if low(rd) && low(rn) && v >= 0 && v <= 7 {
			base := uint16(0x1C00)
			if isSub {
				base = 0x1E00
			}
			return t16(base | uint16(v)<<6 | uint16(rn)<<3 | uint16(rd)), false, nil
		}
		if withS && rd == rn && low(rd) && v >= 0 && v <= 255 {
			base := uint16(0x3000)
			if isSub {
				base = 0x3800
			}
			return t16(base | uint16(rd)<<8 | uint16(v)), false, nil
		}
		return a.thumbImm13(mn, rd, rn, v, withS)
	}
	rm, err := thReg(ops[2])
	if err != nil {
		return nil, false, err
	}
	if low(rd) && low(rn) && low(rm) && withS {
		base := uint16(0x1800)
		if isSub {
			base = 0x1A00
		}
		return t16(base | uint16(rm)<<6 | uint16(rn)<<3 | uint16(rd)), false, nil
	}
	if a.arch != Thumb2 {
		return nil, false, a.errf("%s with a high register needs Thumb-2", mn)
	}
	sbit := uint16(0)
	if withS {
		sbit = 0x10
	}
	hw := uint16(0xEB00)
	if isSub {
		hw = 0xEBA0
	}
	return t32(hw|sbit|uint16(rn&15), uint16(rd&15)<<8|uint16(rm&15)), false, nil
}

func (a *asm) thumbImm13(mn string, rd, rn uint32, v int64, withS bool) ([]byte, bool, error) {
	if a.arch != Thumb2 {
		return nil, false, a.errf("%s with this immediate needs Thumb-2", mn)
	}
	base := uint16(0xF100)
	if strings.HasPrefix(mn, "sub") {
		base = 0xF1A0
	}
	s := uint16(0)
	if withS {
		s = 0x10
	}
	return a.thumbImm12(base, s, rn, v, rd, 0)
}

func (a *asm) thumbShift(mn string, ops []string) ([]byte, bool, error) {
	kind := mn[:3]
	isSet := strings.HasSuffix(mn, "s")
	rd, err := thReg(ops[0])
	if err != nil {
		return nil, false, err
	}
	small := map[string]uint16{"lsl": 0x0000, "lsr": 0x0800, "asr": 0x1000}[kind]
	wbase := map[string]uint16{"lsl": 0xEA4F, "lsr": 0xEA4F, "asr": 0xEA4F}[kind]
	typ := map[string]uint16{"lsl": 0, "lsr": 1, "asr": 2}[kind]
	if len(ops) == 2 {
		rm, err := thReg(ops[1])
		if err != nil {
			return nil, false, err
		}
		if low(rd) && low(rm) && isSet {
			rb := map[string]uint16{"lsl": 0x4080, "lsr": 0x40C0, "asr": 0x4100}[kind]
			return t16(rb | uint16(rm)<<3 | uint16(rd)), false, nil
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("%s needs low registers on ARMv6-M", mn)
		}
		// Register form: LSL/LSR/ASR (register) T2.
		rb := map[string]uint16{"lsl": 0xFA00, "lsr": 0xFA20, "asr": 0xFA40}[kind]
		s := uint16(0)
		if isSet {
			s = 0x10
		}
		return t32(rb|s|uint16(rd&15), 0xF000|uint16(rd&15)<<8|uint16(rm&15)), false, nil
	}
	rn, err := thReg(ops[1])
	if err != nil {
		return nil, false, err
	}
	if strings.HasPrefix(strings.TrimSpace(ops[2]), "#") {
		v, err := thImm(ops[2])
		if err != nil {
			return nil, false, err
		}
		if low(rd) && low(rn) && v >= 0 && v <= 31 && isSet {
			return t16(small | uint16(v)<<6 | uint16(rn)<<3 | uint16(rd)), false, nil
		}
		if a.arch != Thumb2 {
			return nil, false, a.errf("%s needs low registers on ARMv6-M", mn)
		}
		if v < 0 || v > 31 {
			return nil, false, a.errf("shift %d out of range", v)
		}
		s := uint16(0)
		if isSet {
			s = 0x10
		}
		u := uint16(v)
		imm3 := (u >> 2) & 7
		imm2 := u & 3
		hw1 := wbase | s | 0xF
		hw2 := imm3<<12 | uint16(rd&15)<<8 | imm2<<6 | typ<<4 | uint16(rn&15)
		return t32(hw1, hw2), false, nil
	}
	rm, err := thReg(ops[2])
	if err != nil {
		return nil, false, err
	}
	if low(rd) && low(rn) && low(rm) && isSet && rd == rn {
		rb := map[string]uint16{"lsl": 0x4080, "lsr": 0x40C0, "asr": 0x4100}[kind]
		return t16(rb | uint16(rm)<<3 | uint16(rd)), false, nil
	}
	if a.arch != Thumb2 {
		return nil, false, a.errf("%s needs low registers on ARMv6-M", mn)
	}
	hb := map[string]uint16{"lsl": 0xFA00, "lsr": 0xFA20, "asr": 0xFA40}[kind]
	s := uint16(0)
	if isSet {
		s = 0x10
	}
	// Shift by register: LSL/LSR/ASR (register) T2, Rm in the low nibble.
	return t32(hb|s|uint16(rn&15), 0xF000|uint16(rd&15)<<8|uint16(rm&15)), false, nil
}

func (a *asm) thumbLoadStore(mn string, ops []string, off int, sizeOnly bool) ([]byte, bool, error) {
	rt, err := thReg(ops[0])
	if err != nil {
		return nil, false, err
	}
	operand := strings.TrimSpace(ops[1])
	if mn == "ldr" && strings.HasPrefix(operand, "=") {
		return a.thumbLitEq(rt, strings.TrimPrefix(operand, "="), off, sizeOnly)
	}
	if mn == "ldr" && !strings.HasPrefix(operand, "[") {
		// PC-relative literal load: ldr rX, .LlitN
		s, ok := a.obj.Syms[operand]
		if !ok || s.Sec == nil {
			if sizeOnly {
				return t16(0x4800 | uint16(rt)<<8), false, nil
			}
			return nil, false, a.errf("undefined symbol %q", operand)
		}
		if s.Sec != a.cur {
			return nil, false, a.errf("literal load from another section (%s)", operand)
		}
		if sizeOnly {
			return t16(0x4800 | uint16(rt)<<8), false, nil
		}
		base := (off + 4) &^ 3
		if s.Value%4 != 0 {
			return nil, false, a.errf("literal %s is not word aligned", operand)
		}
		imm := (s.Value - base) / 4
		if imm < 0 || imm > 255 {
			return nil, false, a.errf("literal %s is too far from its ldr", operand)
		}
		return t16(0x4800 | uint16(rt)<<8 | uint16(imm)), false, nil
	}
	base, v, err := memThumb(operand)
	if err != nil {
		return nil, false, err
	}
	// 16-bit forms first.
	switch mn {
	case "ldr", "str":
		if low(rt) {
			if base == 13 && v >= 0 && v%4 == 0 && v <= 1020 {
				w := uint16(0x9800)
				if mn == "str" {
					w = 0x9000
				}
				return t16(w | uint16(rt)<<8 | uint16(v/4)), false, nil
			}
			if low(base) && v >= 0 && v%4 == 0 && v <= 124 {
				w := uint16(0x6800)
				if mn == "str" {
					w = 0x6000
				}
				return t16(w | uint16(v/4)<<6 | uint16(base)<<3 | uint16(rt)), false, nil
			}
		}
	case "ldrb", "strb":
		if low(rt) && low(base) && v >= 0 && v <= 31 {
			w := uint16(0x7800)
			if mn == "strb" {
				w = 0x7000
			}
			return t16(w | uint16(v)<<6 | uint16(base)<<3 | uint16(rt)), false, nil
		}
	case "ldrh", "strh":
		if low(rt) && low(base) && v >= 0 && v%2 == 0 && v <= 62 {
			w := uint16(0x8800)
			if mn == "strh" {
				w = 0x8000
			}
			return t16(w | uint16(v/2)<<6 | uint16(base)<<3 | uint16(rt)), false, nil
		}
	}
	if a.arch != Thumb2 {
		return nil, false, a.errf("%s with these operands needs Thumb-2", mn)
	}
	if v < 0 || v > 4095 {
		return nil, false, a.errf("%s offset %d out of range", mn, v)
	}
	w := map[string]uint16{
		"ldr": 0xF8D0, "str": 0xF8C0,
		"ldrb": 0xF890, "strb": 0xF880,
		"ldrh": 0xF8B0, "strh": 0xF8A0,
	}[mn]
	return t32(w|uint16(base&15), uint16(rt&15)<<12|uint16(v)), false, nil
}

// thumbLitEq expands `ldr rX, =expr` into the same inline literal used on
// ARMv6-M: a short branch over an aligned word.  Doing it on every target
// removes the block-end literal pool from the picture entirely, which also
// removes its range problem.
func (a *asm) thumbLitEq(rt uint32, exprText string, off int, sizeOnly bool) ([]byte, bool, error) {
	base := (off + 4) &^ 3
	wordOff := alignTo(off+4, 4)
	imm := (wordOff - base) / 4
	bOff := wordOff + 4 - (off + 6)
	b := uint16(0xE000) | uint16(bOff>>1)&0x7ff

	var word []byte
	e, err := parseExpr(exprText)
	if err != nil {
		return nil, false, a.errf("ldr =%s: %v", exprText, err)
	}
	if e.hasSym {
		a.addReloc(obj.Abs32, wordOff, e.sym, e.add)
		word = u32(uint32(e.add))
	} else {
		word = u32(uint32(e.val))
	}
	out := make([]byte, 0, 8+wordOff-(off+4))
	out = append(out, t16(0x4800|uint16(rt)<<8|uint16(imm))...)
	out = append(out, t16(b)...)
	out = append(out, make([]byte, wordOff-(off+4))...)
	out = append(out, word...)
	return out, false, nil
}
