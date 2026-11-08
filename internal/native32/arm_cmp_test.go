package native32

import "testing"

func TestPlanBranchConds(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	f := &Func{Blocks: []*Block{{Label: "entry"}, {Label: "t"}, {Label: "f"}}}
	entry := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	entry(&Inst{Op: "icmp", Dst: "c1", Ty: iw, Pred: "slt",
		Ops: []Value{{Kind: VLocal, Name: "a"}, {Kind: VLocal, Name: "b"}}})
	entry(&Inst{Op: "br", Ops: []Value{{Kind: VLocal, Name: "c1"}},
		Labels: []string{"t", "f"}})
	// c2 is also consumed by an arithmetic instruction, so it must be kept.
	entry(&Inst{Op: "icmp", Dst: "c2", Ty: iw, Pred: "eq",
		Ops: []Value{{Kind: VLocal, Name: "a"}, {Kind: VLocal, Name: "b"}}})
	entry(&Inst{Op: "add", Dst: "x", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "c2"}, {Kind: VInt, I: 1}}})
	entry(&Inst{Op: "br", Ops: []Value{{Kind: VLocal, Name: "c2"}},
		Labels: []string{"t", "f"}})

	byName, skip := planBranchConds(f)
	if byName["c1"] == nil || !skip[byName["c1"]] {
		t.Fatalf("single-use icmp not fused: %v", byName)
	}
	if byName["c2"] != nil {
		t.Fatalf("multi-use icmp fused: %v", byName)
	}
}
