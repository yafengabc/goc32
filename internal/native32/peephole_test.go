package native32

import (
	"strings"
	"testing"
)

func funcText(body string) string {
	return "\t.type f,@function\nf:\n" + body + "\t.size f, .-f\n"
}

func TestPeepholeDropsRedundantLoad(t *testing.T) {
	out := peepholeRV32(funcText("\tsw t0, 4(sp)\n\tlw t0, 4(sp)\n\tsw t0, 8(sp)\n\tlw a0, 8(sp)\n\tret\n"))
	if strings.Contains(out, "lw") {
		t.Fatalf("loads not forwarded:\n%s", out)
	}
}

func TestPeepholeKillsEverySlotOfARegister(t *testing.T) {
	// t0 is cached for both slots, then redefined. The later load must not be
	// deleted just because slot 4 still names t0.
	out := peepholeRV32(funcText(
		"\tsw t0, 4(sp)\n\tsw t0, 8(sp)\n\taddi t0, t0, 1\n\tlw t0, 4(sp)\n\tret\n"))
	if !strings.Contains(out, "lw t0, 4(sp)") {
		t.Fatalf("stale slot cache deleted a real load:\n%s", out)
	}
}

func TestPeepholeKeepsMarkedStores(t *testing.T) {
	out := peepholeRV32(funcText("\tsw a0, 4(sp) # goc32keep\n\tret\n"))
	if !strings.Contains(out, "sw a0, 4(sp)") {
		t.Fatalf("marked store removed:\n%s", out)
	}
}

func TestPeepholeDropsZeroOffset(t *testing.T) {
	out := peepholeRV32(funcText("\tla t0, sym\n\taddi t0, t0, 0\n\tret\n"))
	if strings.Contains(out, "addi t0, t0, 0") {
		t.Fatalf("zero addi kept:\n%s", out)
	}
}

func TestPeepholeFoldsMoveIntoTemporary(t *testing.T) {
	out := peepholeRV32(funcText("\tla t0, sym\n\tmv a0, t0\n\tcall puts\n\tret\n"))
	if strings.Contains(out, "mv a0, t0") {
		t.Fatalf("move not folded:\n%s", out)
	}
	if !strings.Contains(out, "la a0, sym") {
		t.Fatalf("producer not retargeted:\n%s", out)
	}
}

func TestPeepholeDoesNotFoldLiveTemporary(t *testing.T) {
	// t0 is read again after the move, so it must stay in t0.
	out := peepholeRV32(funcText("\tla t0, sym\n\tmv a0, t0\n\tlw a1, 0(t0)\n\tret\n"))
	if !strings.Contains(out, "la t0, sym") || !strings.Contains(out, "mv a0, t0") {
		t.Fatalf("live temporary folded:\n%s", out)
	}
}

func TestPeepholeKeepsZeroReadByLaterInstruction(t *testing.T) {
	// The second mul reads t0 before overwriting it, so the zero definition
	// cannot be folded into the store.
	out := peepholeRV32(funcText(
		"\tli t0, 0\n\tsw t0, 4(sp)\n\tli t1, 4\n\tmul t0, t0, t1\n\tsw t0, 8(sp)\n\tret\n"))
	if !strings.Contains(out, "li t0, 0") {
		t.Fatalf("zero killed while still live:\n%s", out)
	}
}

func TestPeepholeFoldsImmediateIntoAdd(t *testing.T) {
	out := peepholeRV32(funcText("\tli t1, 4\n\tadd t0, t0, t1\n\tret\n"))
	if !strings.Contains(out, "addi t0, t0, 4") || strings.Contains(out, "li t1, 4") {
		t.Fatalf("immediate not folded:\n%s", out)
	}
}

func TestPeepholeTurnsMultiplyByPowerOfTwoIntoShift(t *testing.T) {
	out := peepholeRV32(funcText("\tli t1, 4\n\tmul t0, t0, t1\n\tret\n"))
	if !strings.Contains(out, "slli t0, t0, 2") {
		t.Fatalf("multiply not turned into a shift:\n%s", out)
	}
}

func TestPeepholeKeepsImmediateUsedTwice(t *testing.T) {
	out := peepholeRV32(funcText("\tli t1, 4\n\tadd t0, t0, t1\n\tadd t2, t2, t1\n\tret\n"))
	if !strings.Contains(out, "li t1, 4") {
		t.Fatalf("constant folded while still live:\n%s", out)
	}
}

func TestPeepholeDoesNotFoldDestinationRead(t *testing.T) {
	// add t0, t0, t1 reads the old t0, so folding la into a0 while deleting
	// the move would lose the value the add needs.
	out := peepholeRV32(funcText(
		"\tla t0, sym\n\tmv a0, t0\n\tadd t0, t0, t1\n\tret\n"))
	if !strings.Contains(out, "la t0, sym") || !strings.Contains(out, "mv a0, t0") {
		t.Fatalf("destination read missed:\n%s", out)
	}
}
