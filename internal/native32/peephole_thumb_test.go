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
