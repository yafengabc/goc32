package native32

import (
	"reflect"
	"testing"
)

func TestRV32FoldAddrsGeneralBase(t *testing.T) {
	in := []string{
		"f:",
		"\taddi q0, a1, 8",
		"\tlw q1, 0(q0)",
		"\tret",
	}
	want := []string{
		"f:",
		"\tlw q1, 8(a1)",
		"\tret",
	}
	if got := rv32FoldAllocaAddrs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("general base fold:\n got %q\nwant %q", got, want)
	}

	// A write to the base between the add and the access makes the fold
	// invalid; the address computation must stay.
	keep := []string{
		"f:",
		"\taddi q0, a1, 8",
		"\tli a1, 0",
		"\tlw q1, 0(q0)",
		"\tret",
	}
	if got := rv32FoldAllocaAddrs(append([]string{}, keep...)); len(got) != 5 {
		t.Fatalf("base written, add still folded: %q", got)
	}
}
