package native32

import (
	"reflect"
	"strings"
	"testing"
)

func TestArmThreadBlocks(t *testing.T) {
	in := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		"\tcmp r0, #0",
		"\tbeq .Lx",
		"\tb .Lend",
		".Lx:",
		"\tb .Ly",
		".Ly:",
		"\tmovs r0, #1",
		".Lend:",
		"\tret",
		"\t.size f, .-f",
	}
	want := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		"\tcmp r0, #0",
		"\tbeq .Ly",
		"\tb .Lend",
		".Ly:",
		"\tmovs r0, #1",
		".Lend:",
		"\tret",
		"\t.size f, .-f",
	}
	if got := strings.Split(armThreadBlocks(strings.Join(in, "\n"), true), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armThreadBlocks:\n got %q\nwant %q", got, want)
	}

	// A trampoline reachable by fallthrough must stay.
	keep := []string{
		"f:",
		"\tbeq .Lx",
		".Lx:",
		"\tb .Ly",
		".Ly:",
		"\tret",
	}
	if got := strings.Split(armThreadBlocks(strings.Join(keep, "\n"), true), "\n"); len(got) != 6 {
		t.Fatalf("fallthrough trampoline removed: %q", got)
	}
}
