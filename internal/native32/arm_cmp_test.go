package native32

import "testing"

func TestPlanBranchConds(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	one := &Type{Kind: KInt, Bits: 1}
	f := &Func{Blocks: []*Block{{Label: "entry"}, {Label: "t"}, {Label: "f"}}}
	entry := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	c1 := &Inst{Op: "icmp", Dst: "c1", Ty: iw, Pred: "slt",
		Ops: []Value{{Kind: VLocal, Name: "a"}, {Kind: VLocal, Name: "b"}}}
	entry(c1)
	entry(&Inst{Op: "br", Ops: []Value{{Kind: VLocal, Name: "c1"}},
		Labels: []string{"t", "f"}})
	// c2 is also consumed by an arithmetic instruction, so it must be kept.
	entry(&Inst{Op: "icmp", Dst: "c2", Ty: iw, Pred: "eq",
		Ops: []Value{{Kind: VLocal, Name: "a"}, {Kind: VLocal, Name: "b"}}})
	entry(&Inst{Op: "add", Dst: "x", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "c2"}, {Kind: VInt, I: 1}}})
	entry(&Inst{Op: "br", Ops: []Value{{Kind: VLocal, Name: "c2"}},
		Labels: []string{"t", "f"}})
	// !(a < b): the xor is folded into an inverted branch.
	c3 := &Inst{Op: "icmp", Dst: "c3", Ty: iw, Pred: "slt",
		Ops: []Value{{Kind: VLocal, Name: "a"}, {Kind: VLocal, Name: "b"}}}
	entry(c3)
	x3 := &Inst{Op: "xor", Dst: "n3", Ty: one,
		Ops: []Value{{Kind: VLocal, Name: "c3"}, {Kind: VInt, I: 1}}}
	entry(x3)
	entry(&Inst{Op: "br", Ops: []Value{{Kind: VLocal, Name: "n3"}},
		Labels: []string{"t", "f"}})

	byName, skip := planBranchConds(f)
	if b, ok := byName["c1"]; !ok || b.neg || !skip[b.cmp] {
		t.Fatalf("single-use icmp not fused: %v", byName)
	}
	if _, ok := byName["c2"]; ok {
		t.Fatalf("multi-use icmp fused: %v", byName)
	}
	if b, ok := byName["n3"]; !ok || !b.neg || !skip[b.cmp] || !skip[x3] {
		t.Fatalf("negated icmp not fused: %v", byName)
	}
}
