package native32

import "testing"

func TestForwardSkipsRegisterCandidate(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	f := &Func{Blocks: []*Block{{Label: "entry"}}}
	add := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	add(&Inst{Op: "alloca", Dst: "hot", Ty: iw})
	add(&Inst{Op: "alloca", Dst: "cold", Ty: iw})
	for i := 0; i < 2; i++ {
		add(&Inst{Op: "store", Ty: iw,
			Ops: []Value{{Kind: VLocal, Name: "v"}, {Kind: VLocal, Name: "hot"}}})
		add(&Inst{Op: "load", Dst: "h" + string(rune('a'+i)), Ty: iw,
			Ops: []Value{{Kind: VLocal, Name: "hot"}}})
	}
	add(&Inst{Op: "store", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "v"}, {Kind: VLocal, Name: "cold"}}})
	add(&Inst{Op: "load", Dst: "c", Ty: iw, Ops: []Value{{Kind: VLocal, Name: "cold"}}})

	forwardLoadsFunc(f, 1, 4)

	loads := 0
	for _, in := range f.Blocks[0].Insts {
		if in.Op == "load" {
			loads++
		}
	}
	if loads != 2 {
		t.Fatalf("loads left = %d, want the two hot loads", loads)
	}
}

func TestForwardDirectLoads(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	f := &Func{Blocks: []*Block{{Label: "entry"}, {Label: "next"}}}
	entry := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	entry(&Inst{Op: "alloca", Dst: "a", Ty: iw})
	entry(&Inst{Op: "alloca", Dst: "b", Ty: iw})
	entry(&Inst{Op: "store", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "x"}, {Kind: VLocal, Name: "a"}}})
	entry(&Inst{Op: "load", Dst: "l1", Ty: iw, Ops: []Value{{Kind: VLocal, Name: "a"}}})
	entry(&Inst{Op: "load", Dst: "l2", Ty: iw, Ops: []Value{{Kind: VLocal, Name: "a"}}})
	entry(&Inst{Op: "add", Dst: "sum", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "l1"}, {Kind: VLocal, Name: "l2"}}})
	// b escapes through a GEP, so its loads and stores must stay.
	entry(&Inst{Op: "getelementptr", Dst: "g", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "b"}}})
	entry(&Inst{Op: "store", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "y"}, {Kind: VLocal, Name: "b"}}})
	entry(&Inst{Op: "load", Dst: "lb", Ty: iw, Ops: []Value{{Kind: VLocal, Name: "b"}}})
	// Availability does not cross a block boundary.
	f.Blocks[1].Insts = append(f.Blocks[1].Insts,
		&Inst{Op: "load", Dst: "l3", Ty: iw, Ops: []Value{{Kind: VLocal, Name: "a"}}})

	forwardLoadsFunc(f, 0, 3)

	var add *Inst
	for _, in := range f.Blocks[0].Insts {
		if in.Op == "load" && in.Dst != "lb" {
			t.Fatalf("direct load %%%s not forwarded", in.Dst)
		}
		if in.Op == "add" {
			add = in
		}
	}
	if add == nil {
		t.Fatal("add removed")
	}
	if add.Ops[0].Name != "x" || add.Ops[1].Name != "x" {
		t.Fatalf("add operands = %v, want x, x", add.Ops)
	}
	if len(f.Blocks[1].Insts) != 1 || f.Blocks[1].Insts[0].Dst != "l3" {
		t.Fatalf("cross-block load changed: %+v", f.Blocks[1].Insts)
	}
}
