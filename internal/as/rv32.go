package as

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"goc32/internal/obj"
)

var rvRegs = map[string]uint32{
	"zero": 0, "ra": 1, "sp": 2, "gp": 3, "tp": 4,
	"t0": 5, "t1": 6, "t2": 7,
	"s0": 8, "fp": 8, "s1": 9,
	"a0": 10, "a1": 11, "a2": 12, "a3": 13, "a4": 14, "a5": 15, "a6": 16, "a7": 17,
	"s2": 18, "s3": 19, "s4": 20, "s5": 21, "s6": 22, "s7": 23,
	"s8": 24, "s9": 25, "s10": 26, "s11": 27,
	"t3": 28, "t4": 29, "t5": 30, "t6": 31,
}

var memRVRe = regexp.MustCompile(`^\s*(-?(?:0[xX][0-9a-fA-F]+|\d+))?\s*\((\w+)\)\s*$`)

func rvReg(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if r, ok := rvRegs[s]; ok {
		return r, nil
	}
	if strings.HasPrefix(s, "x") {
		n, err := strconv.Atoi(s[1:])
		if err == nil && n >= 0 && n < 32 {
			return uint32(n), nil
		}
	}
	return 0, fmt.Errorf("bad RV32 register %q", s)
}

func rvImm(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("bad immediate %q", s)
	}
	return v, nil
}

func rvMem(s string) (off int64, base uint32, err error) {
	m := memRVRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("bad memory operand %q", s)
	}
	if m[1] != "" {
		off, err = strconv.ParseInt(m[1], 0, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	base, err = rvReg(m[2])
	return off, base, err
}

func encR(funct7, rs2, rs1, funct3, rd, opcode uint32) uint32 {
	return funct7<<25 | rs2<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

func encI(imm int64, rs1, funct3, rd, opcode uint32) uint32 {
	return uint32(imm&0xfff)<<20 | rs1<<15 | funct3<<12 | rd<<7 | opcode
}

func encS(imm int64, rs2, rs1, funct3 uint32) uint32 {
	u := uint32(imm) & 0xfff
	return (u>>5)<<25 | rs2<<20 | rs1<<15 | funct3<<12 | (u&0x1f)<<7 | 0x23
}

func encB(off int64, rs2, rs1, funct3 uint32) uint32 {
	u := uint32(off)
	return (u>>12&1)<<31 | (u>>5&0x3f)<<25 | rs2<<20 | rs1<<15 | funct3<<12 |
		(u>>1&0xf)<<8 | (u>>11&1)<<7 | 0x63
}

func encJ(off int64, rd uint32) uint32 {
	u := uint32(off)
	return (u>>20&1)<<31 | (u>>1&0x3ff)<<21 | (u>>11&1)<<20 | (u>>12&0xff)<<12 |
		rd<<7 | 0x6f
}

var rvBranchF3 = map[string]uint32{
	"beq": 0, "bne": 1, "blt": 4, "bge": 5, "bltu": 6, "bgeu": 7,
}

var rvRType = map[string][2]uint32{
	"add": {0, 0}, "sub": {0x20, 0}, "sll": {0, 1}, "slt": {0, 2},
	"sltu": {0, 3}, "xor": {0, 4}, "srl": {0, 5}, "sra": {0x20, 5},
	"or": {0, 6}, "and": {0, 7},
	"mul": {1, 0}, "div": {1, 4}, "divu": {1, 5}, "rem": {1, 6}, "remu": {1, 7},
}

var rvIType = map[string]uint32{
	"addi": 0, "slti": 2, "sltiu": 3, "xori": 4, "ori": 6, "andi": 7,
}

var rvLoadF3 = map[string]uint32{"lb": 0, "lh": 1, "lw": 2, "lbu": 4, "lhu": 5}
var rvStoreF3 = map[string]uint32{"sb": 0, "sh": 1, "sw": 2}

// rv32Insn encodes one RV32 instruction.  sizeOnly is the sizing pass: it runs
// the same encoder with placeholder targets so the instruction length can
// never disagree between passes.
func (a *asm) rv32Insn(text string, off int, sizeOnly bool) ([]byte, bool, error) {
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
		return rvReg(s)
	}
	imm := func(i int) (int64, error) {
		s, err := get(i)
		if err != nil {
			return 0, err
		}
		return rvImm(s)
	}
	out := func(insns ...uint32) ([]byte, bool, error) {
		var b []byte
		for _, v := range insns {
			b = append(b, u32(v)...)
		}
		return b, false, nil
	}

	switch {
	case mn == "ret":
		return out(encI(0, 1, 0, 0, 0x67))
	case mn == "ecall":
		return out(0x00000073)
	case mn == "nop":
		return out(encI(0, 0, 0, 0, 0x13))
	case mn == "mv":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		return out(encI(0, rs, 0, rd, 0x13))
	case mn == "seqz" || mn == "snez":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		if mn == "seqz" {
			return out(encI(1, rs, 3, rd, 0x13))
		}
		return out(encR(0, rs, 0, 3, rd, 0x33))
	case mn == "li":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		v, err := imm(1)
		if err != nil {
			return nil, false, err
		}
		sv := int32(v)
		if sv >= -2048 && sv <= 2047 {
			return out(encI(int64(sv), 0, 0, rd, 0x13))
		}
		hi := (sv + 0x800) >> 12
		lo := sv - hi<<12
		if lo == 0 {
			return out(uint32(hi&0xfffff)<<12 | rd<<7 | 0x37)
		}
		return out(uint32(hi&0xfffff)<<12|rd<<7|0x37, encI(int64(lo), rd, 0, rd, 0x13))
	case mn == "la":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		e, err := parseExpr(ops[1])
		if err != nil || !e.hasSym {
			return nil, false, a.errf("la needs a symbol")
		}
		a.addReloc(obj.RVPCRel, off, e.sym, e.add)
		return out(uint32(rd)<<7|0x17, encI(0, rd, 0, rd, 0x13))
	case mn == "call":
		e, err := parseExpr(ops[0])
		if err != nil || !e.hasSym {
			return nil, false, a.errf("call needs a symbol")
		}
		a.addReloc(obj.RVCall, off, e.sym, e.add)
		return out(1<<7|0x17, encI(0, 1, 0, 1, 0x67))
	case mn == "j":
		if len(ops) == 2 {
			// old-style `j rd, label`
			rd, err := reg(0)
			if err != nil {
				return nil, false, err
			}
			target := ops[1]
			return a.rvJump(target, rd, off, sizeOnly)
		}
		return a.rvJump(ops[0], 0, off, sizeOnly)
	case mn == "jal":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		return a.rvJump(ops[1], rd, off, sizeOnly)
	case mn == "jalr":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		v := int64(0)
		if len(ops) > 2 {
			v, err = rvImm(ops[2])
			if err != nil {
				return nil, false, err
			}
		}
		return out(encI(v, rs, 0, rd, 0x67))
	case rvBranchF3[mn] != 0 || mn == "beq":
		f3 := rvBranchF3[mn]
		rs1, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs2, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		return a.rvBranch(ops[2], rs1, rs2, f3, off, sizeOnly)
	case mn == "beqz" || mn == "bnez":
		rs1, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		f3 := uint32(0)
		if mn == "bnez" {
			f3 = 1
		}
		return a.rvBranch(ops[1], rs1, 0, f3, off, sizeOnly)
	}

	if spec, ok := rvRType[mn]; ok {
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs1, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		rs2, err := reg(2)
		if err != nil {
			return nil, false, err
		}
		return out(encR(spec[0], rs2, rs1, spec[1], rd, 0x33))
	}
	if f3, ok := rvIType[mn]; ok {
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs1, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		v, err := imm(2)
		if err != nil {
			return nil, false, err
		}
		return out(encI(v, rs1, f3, rd, 0x13))
	}
	switch mn {
	case "slli", "srli", "srai":
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		rs1, err := reg(1)
		if err != nil {
			return nil, false, err
		}
		v, err := imm(2)
		if err != nil {
			return nil, false, err
		}
		f3 := uint32(1)
		f7 := uint32(0)
		if mn == "srli" {
			f3 = 5
		} else if mn == "srai" {
			f3, f7 = 5, 0x20
		}
		return out(f7<<25 | uint32(v&0x1f)<<20 | rs1<<15 | f3<<12 | rd<<7 | 0x13)
	}
	if f3, ok := rvLoadF3[mn]; ok {
		rd, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		moff, base, err := rvMem(ops[1])
		if err != nil {
			return nil, false, a.errf("%v", err)
		}
		return out(encI(moff, base, f3, rd, 0x03))
	}
	if f3, ok := rvStoreF3[mn]; ok {
		rs2, err := reg(0)
		if err != nil {
			return nil, false, err
		}
		moff, base, err := rvMem(ops[1])
		if err != nil {
			return nil, false, a.errf("%v", err)
		}
		return out(encS(moff, rs2, base, f3))
	}
	return nil, false, a.errf("unsupported RV32 instruction %q", mn)
}

func (a *asm) rvJump(name string, rd uint32, off int, sizeOnly bool) ([]byte, bool, error) {
	s, ok := a.obj.Syms[name]
	if !ok || s.Sec == nil {
		if sizeOnly {
			return u32(encJ(0, rd)), false, nil
		}
		return nil, false, a.errf("undefined symbol %q", name)
	}
	if s.Sec != a.cur {
		if sizeOnly {
			return u32(0), false, nil
		}
		a.addReloc(obj.RVJal, off, name, 0)
		return u32(encJ(0, rd)), false, nil
	}
	rel := int64(s.Value - off)
	if !sizeOnly && (rel < -(1<<20) || rel >= 1<<20 || rel%2 != 0) {
		return nil, false, a.errf("jump to %s out of range", name)
	}
	return u32(encJ(rel, rd)), false, nil
}

func (a *asm) rvBranch(name string, rs1, rs2, f3 uint32, off int, sizeOnly bool) ([]byte, bool, error) {
	s, ok := a.obj.Syms[name]
	if !ok || s.Sec == nil {
		if sizeOnly {
			return u32(encB(0, rs2, rs1, f3)), false, nil
		}
		return nil, false, a.errf("undefined symbol %q", name)
	}
	if s.Sec != a.cur {
		if sizeOnly {
			return u32(0), false, nil
		}
		a.addReloc(obj.RVBranch, off, name, 0)
		return u32(encB(0, rs2, rs1, f3)), false, nil
	}
	rel := int64(s.Value - off)
	if !sizeOnly && (rel < -4096 || rel > 4094 || rel%2 != 0) {
		return nil, false, a.errf("branch to %s out of range", name)
	}
	return u32(encB(rel, rs2, rs1, f3)), false, nil
}
