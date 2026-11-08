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
