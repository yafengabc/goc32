package native32

import (
	"reflect"
	"testing"
)

func TestFuseSetcc(t *testing.T) {
	in := []string{
		"f:",
		"\txori a4, s2, 0",
		"\tsnez a4, a4",
		"\tbeqz a4, L",
		"\tslt a4, a0, a1",
		"\txori a4, a4, 1",
		"\tbeqz a4, L",
		"\tslt a4, a0, a1",
		"\tbeqz a4, L",
		"\txori a4, a0, 1",
		"\tbnez a4, L",
		"\tret",
	}
	want := []string{
		"f:",
		"\tbeqz s2, L",
		"\tblt a0, a1, L",
		"\tbge a0, a1, L",
		"\tbeqz a0, L",
		"\tret",
	}
	if got := rv32FuseSetcc(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("rv32FuseSetcc:\n got %q\nwant %q", got, want)
	}
}
