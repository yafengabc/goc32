package native32

import (
	"reflect"
	"testing"
)

func TestRV32ThreadBlocks(t *testing.T) {
	in := []string{
		".text",
		"f:",
		"\tbeqz a0, .Lx",
		"\tj .Lend",
		".Lx:",
		"\tj .Ly",
		".Ly:",
		"\tli a0, 1",
		".Lend:",
		"\tret",
	}
	want := []string{
		".text",
		"f:",
		"\tbeqz a0, .Ly",
		"\tj .Lend",
		".Ly:",
		"\tli a0, 1",
		".Lend:",
		"\tret",
	}
	if got := rv32ThreadBlocksFunc(append([]string{}, in...)); !reflect.DeepEqual(got, want) {
		t.Fatalf("rv32ThreadBlocksFunc:\n got %q\nwant %q", got, want)
	}

	// Fallthrough into a trampoline keeps it.
	keep := []string{
		"f:",
		"\tbeqz a0, .Lx",
		".Lx:",
		"\tj .Ly",
		".Ly:",
		"\tret",
	}
	if got := rv32ThreadBlocksFunc(append([]string{}, keep...)); len(got) != 6 {
		t.Fatalf("fallthrough trampoline removed: %q", got)
	}
}

func TestRV32FoldAllocaAddrsMulti(t *testing.T) {
	in := []string{
		"f:",
		"\taddi q0, sp, 12",
		"\tlw q1, 0(q0)",
		"\taddi q0, q0, 4",
		"\tsw q1, 0(q0)",
		"\tli q2, 3",
		"\tlw q3, 0(q0)",
		"\tret",
	}
	want := []string{
		"f:",
		"\tlw q1, 12(sp)",
		"\tsw q1, 16(sp)",
		"\tli q2, 3",
		"\tlw q3, 16(sp)",
		"\tret",
	}
	if got := rv32FoldAllocaAddrs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("rv32FoldAllocaAddrs multi:\n got %q\nwant %q", got, want)
	}
}
