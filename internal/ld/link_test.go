package ld

import (
	"bytes"
	"debug/elf"
	"testing"

	"goc32/internal/obj"
)

// TestBSSIsInsideTheLoadSegment is the regression test for a link whose
// .bss started past the end of the writable PT_LOAD: the kernel only zeroes
// [p_vaddr+p_filesz, p_vaddr+p_memsz), so any byte of .bss outside that range
// kept whatever the window held before and every static variable started life
// with a random value.
func TestBSSIsInsideTheLoadSegment(t *testing.T) {
	o := obj.New("test")
	text := o.Sec(".text.entry", obj.SHF_ALLOC|obj.SHF_EXECINSTR, obj.SHT_PROGBITS, 4)
	text.Add(make([]byte, 8))
	o.Define("_start", text, 0, true, true)
	// Two absolute words keep the data and bss sections reachable.
	text.Relocs = append(text.Relocs,
		obj.Reloc{Off: 0, Kind: obj.Abs32, Sym: "d"},
		obj.Reloc{Off: 4, Kind: obj.Abs32, Sym: "b"},
	)
	data := o.Sec(".data.d", obj.SHF_ALLOC|obj.SHF_WRITE, obj.SHT_PROGBITS, 8)
	data.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	o.Define("d", data, 0, true, false)
	bss := o.Sec(".bss.b", obj.SHF_ALLOC|obj.SHF_WRITE, obj.SHT_NOBITS, 16)
	bss.AddZero(32)
	o.Define("b", bss, 0, true, false)

	image, err := Link([]*obj.Object{o}, Options{Machine: EM_RISCV, Entry: "_start"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		t.Fatalf("debug/elf cannot read the image: %v", err)
	}
	var writable *elf.Prog
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_W != 0 {
			writable = p
		}
	}
	if writable == nil {
		t.Fatal("no writable PT_LOAD")
	}
	end := writable.Vaddr + writable.Memsz
	bssStart, bssEnd := uint64(bss.OutAddr), uint64(bss.OutAddr)+uint64(bss.Size)
	if end < bssEnd {
		t.Fatalf("bss [%#x,%#x) is outside the load [%#x,%#x)",
			bssStart, bssEnd, writable.Vaddr, end)
	}
	if writable.Filesz > writable.Memsz {
		t.Fatalf("filesz %d > memsz %d", writable.Filesz, writable.Memsz)
	}
	if writable.Vaddr+writable.Filesz > bssStart {
		t.Fatalf("bss starts inside the file-backed part (%#x < %#x)",
			bssStart, writable.Vaddr+writable.Filesz)
	}
}
