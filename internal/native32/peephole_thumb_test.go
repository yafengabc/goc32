package native32

import (
	"reflect"
	"strings"
	"testing"
)

func TestArmSlotForward(t *testing.T) {
	in := []string{
		"f:",
		"\tstr r1, [sp, #8]",
		"\tldr r0, [sp, #8]",
		"\tstr r0, [sp, #16]",
		// A load of an unknown slot into r0 kills the cached value of slot 16.
		"\tldr r0, [sp, #24]",
		"\tldr r2, [sp, #16]",
		"\tstr r3, [sp, #8]",
		// Same register as the store: the load disappears.
		"\tldr r3, [sp, #8]",
		"\tldrb r1, [sp, #8]",
		"\tret",
	}
	want := []string{
		"f:",
		"\tstr r1, [sp, #8]",
		"\tmov r0, r1",
		"\tstr r0, [sp, #16]",
		"\tldr r0, [sp, #24]",
		"\tldr r2, [sp, #16]",
		"\tstr r3, [sp, #8]",
		"\tldrb r1, [sp, #8]",
		"\tret",
	}
	if got := strings.Split(armSlotForward(strings.Join(in, "\n")), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armSlotForward:\n got %q\nwant %q", got, want)
	}
}

func TestArmDeadStoreDelete(t *testing.T) {
	in := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		"\tstr r0, [sp, #4]",
		"\tstr r1, [sp, #8]",
		"\tstr r2, [sp, #12] @ goc32keep",
		"\tldr r3, [sp, #8]",
		"\tret",
		"\t.size f, .-f",
	}
	want := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		// slot 4 is written and never read; slot 8's only read is forwarded
		// to r1, so its store is dead too.
		"\tstr r2, [sp, #12] @ goc32keep",
		"\tmov r3, r1",
		"\tret",
		"\t.size f, .-f",
	}
	if got := strings.Split(armSlotForward(strings.Join(in, "\n")), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armSlotForward dead stores:\n got %q\nwant %q", got, want)
	}
}

func TestArmFoldMoves(t *testing.T) {
	in := []string{
		"f:",
		"\tmov r0, r4",
		"\tcmp r0, #0",
		"\tbne L",
		"\tmov r1, r5",
		"\tstr r1, [sp, #8]",
		"\tmov r2, r2",
		"\tret",
	}
	want := []string{
		"f:",
		"\tcmp r4, #0",
		"\tbne L",
		"\tstr r5, [sp, #8]",
		"\tret",
	}
	if got := strings.Split(armFoldMoves(strings.Join(in, "\n"), true), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armFoldMoves:\n got %q\nwant %q", got, want)
	}
	// A copy whose destination is still read after the store must stay.
	keep := []string{"\tmov r1, r5", "\tstr r1, [sp, #8]", "\tmov r0, r1", "\tret"}
	if got := strings.Split(armFoldMoves(strings.Join(keep, "\n"), true), "\n"); len(got) != 4 {
		t.Fatalf("live copy folded: %q", got)
	}
}

func TestArmSlotForwardR4(t *testing.T) {
	in := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		"\tadd r4, sp, #252",
		"\tstr r0, [r4]",
		"\tadd r4, sp, #252",
		"\tldr r1, [r4]",
		"\tstr r0, [r4]",
		"\tadd r4, sp, #252",
		"\tldr r2, [r4]",
		"\tadds r4, #4",
		"\tstr r3, [r4]",
		"\tret",
		"\t.size f, .-f",
	}
	want := []string{
		"\t.section .text.f,\"ax\",%progbits",
		"f:",
		"\tmov r1, r0",
		"\tmov r2, r0",
		"\tadds r4, #4",
		"\tstr r3, [r4]",
		"\tret",
		"\t.size f, .-f",
	}
	if got := strings.Split(armSlotForward(strings.Join(in, "\n")), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armSlotForward r4:\n got %q\nwant %q", got, want)
	}
}

func TestArmFoldImmediates(t *testing.T) {
	in := []string{
		"f:",
		"\tmovs r1, #7",
		"\tadds r0, r5, r1",
		"\tmovs r1, #0",
		"\tcmp r7, r1",
		"\tmovs r1, #9",
		"\tcmp r7, r1",
		"\tmovs r1, #300",
		"\tcmp r8, r1",
		"\tret",
	}
	want := []string{
		"f:",
		"\tadds r0, r5, #7",
		"\tcmp r7, #0",
		"\tcmp r7, #9",
		"\tmovs r1, #300",
		"\tcmp r8, r1",
		"\tret",
	}
	if got := strings.Split(armFoldMoves(strings.Join(in, "\n"), true), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("armFoldMoves immediates:\n got %q\nwant %q", got, want)
	}
}
