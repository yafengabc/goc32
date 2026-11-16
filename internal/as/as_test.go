package as

import (
	"strings"
	"testing"

	"goc32/internal/obj"
)

func assemble(t *testing.T, arch Arch, body string) []byte {
	t.Helper()
	src := body
	if arch != RV32 {
		src = ".syntax unified\n.thumb\n" + body
	}
	o, err := Assemble([]byte(src), Opt{Arch: arch, File: "test.s"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	var out []byte
	for _, s := range o.Secs {
		if s.Flags&obj.SHF_ALLOC != 0 && s.Type != obj.SHT_NOBITS {
			// Only the first section carries the instructions under test.
			if len(out) == 0 {
				out = append(out, s.Data...)
			}
		}
	}
	return out
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(digits[c>>4])
		sb.WriteByte(digits[c&15])
	}
	return sb.String()
}

func TestRV32Encodings(t *testing.T) {
	cases := []struct{ insn, want string }{
		{"addi sp, sp, -16", "130101ff"},
		{"sw ra, 12(sp)", "23261100"},
		{"lw a0, 0(a1)", "03a50500"},
		{"lbu a0, 3(a1)", "03c53500"},
		{"sw ra, 12(sp)", "23261100"},
		{"ret", "67800000"},
		{"mul a0, a1, a2", "3385c502"},
		{"seqz a0, a1", "13b51500"},
		{"ecall", "73000000"},
		// li falls back to lui+addi when the immediate does not fit in 12 bits.
		{"li a0, 0xf800", "3705010013050580"},
	}
	for _, c := range cases {
		got := hex(assemble(t, RV32, "\t.text\n\t"+c.insn+"\n"))
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.insn, got, c.want)
		}
	}
}

func TestThumb1Encodings(t *testing.T) {
	cases := []struct{ insn, want string }{
		{"push {r4, lr}", "10b5"},
		{"pop {r4, pc}", "10bd"},
		{"movs r0, #0", "0020"},
		{"adds r0, r5, #1", "681c"},
		{"ldr r0, [sp, #4]", "0198"},
		{"str r0, [sp, #4]", "0190"},
		{"cmp r0, #32", "2028"},
		{"uxtb r0, r0", "c0b2"},
		{"lsls r1, r0, #2", "8100"},
		{"add r3, sp, #80", "14ab"},
		{"add sp, #112", "1cb0"},
		{"sub sp, #16", "84b0"},
	}
	for _, c := range cases {
		got := hex(assemble(t, Thumb1, "\t.text\n\t"+c.insn+"\n"))
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.insn, got, c.want)
		}
	}
}

func TestThumb2Encodings(t *testing.T) {
	cases := []struct{ insn, want string }{
		// 32-bit forms that native32 emits once the wide allocator picks r8+.
		{"adds r0, r8, #1", "18f10100"},
		{"ldr r8, [sp, #176]", "ddf8b080"},
		{"str r10, [sp, #132]", "cdf884a0"},
		{"cmp r10, #1", "baf1010f"},
		{"movw r1, #4660", "41f23421"},
		{"movt r1, #43981", "caf6cd31"},
	}
	for _, c := range cases {
		got := hex(assemble(t, Thumb2, "\t.text\n\t"+c.insn+"\n"))
		if got != c.want {
			t.Errorf("%s: got %s want %s", c.insn, got, c.want)
		}
	}
}

func TestArmLiteralIsAlignedDown(t *testing.T) {
	// The ldr literal base is Align(PC,4), which rounds down.  A literal after
	// a one-instruction prefix therefore needs imm8=1, not 0; getting this
	// wrong loads the branch instruction instead of the address.
	got := assemble(t, Thumb2, "\t.text\n\tnop\n\tldr r0, =0x12345678\n\t.ltorg\n")
	if len(got) < 8 {
		t.Fatalf("short output: %s", hex(got))
	}
	// ldr r0,[pc,#4] = 0x4801 follows nop (0xbf00).
	if hex(got[:4]) != "00bf0148" {
		t.Fatalf("literal load: %s", hex(got[:4]))
	}
}
