package native32

import (
	"reflect"
	"testing"
)

func TestFoldAllocaAddrs(t *testing.T) {
	in := []string{
		"f:",
		"\taddi q0, sp, 12",
		"\tlw q1, 0(q0)",
		"\taddi q0, sp, 20",
		"\taddi q0, q0, 4",
		"\tsw q1, 0(q0)",
		"\taddi q0, sp, 28",
		"\tlw q1, 0(q0)",
		"\tmv a0, q0",
		"\tret",
	}
	want := []string{
		"f:",
		"\tlw q1, 12(sp)",
		"\tsw q1, 24(sp)",
		"\taddi q0, sp, 28",
		"\tlw q1, 0(q0)",
		"\tmv a0, q0",
		"\tret",
	}
	if got := rv32FoldAllocaAddrs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("rv32FoldAllocaAddrs:\n got %q\nwant %q", got, want)
	}
}
