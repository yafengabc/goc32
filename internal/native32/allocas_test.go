package native32

import "testing"

func TestPlanRegVarsOnlyDirectHot(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	f := &Func{Blocks: []*Block{{Label: "entry"}}}
	add := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	add(&Inst{Op: "alloca", Dst: "a", Ty: iw})
	add(&Inst{Op: "alloca", Dst: "b", Ty: iw})
	// a is read/written six times and never escapes.
	for i := 0; i < 6; i++ {
		add(&Inst{Op: "load", Dst: "l" + string(rune('a'+i)), Ty: iw,
			Ops: []Value{{Kind: VLocal, Name: "a"}}})
	}
	// b's address escapes through a GEP, so it must stay in memory.
	add(&Inst{Op: "getelementptr", Dst: "g", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "b"}}})
	vars, regs := planRegVars(f, []string{"s0"}, 6)
	if vars["a"] != "s0" {
		t.Fatalf("hot direct alloca not assigned: %v", vars)
	}
	if _, ok := vars["b"]; ok {
		t.Fatalf("escaping alloca assigned a register: %v", vars)
	}
	if len(regs) != 1 || regs[0] != "s0" {
		t.Fatalf("unexpected register list: %v", regs)
	}
}
