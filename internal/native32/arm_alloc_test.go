package native32

import "testing"

func TestPlanARMBlockAlloc(t *testing.T) {
	i32 := &Type{Kind: KInt, Bits: 32}
	loc := func(n string) Value { return Value{Kind: VLocal, Name: n} }
	mk := func(op, dst string, ty *Type, args ...Value) *Inst {
		return &Inst{Op: op, Dst: dst, Ty: ty, Ops: args}
	}
	f := &Func{Name: "f", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			// v1 is used as a second operand twice.
			mk("add", "v1", i32, loc("a"), loc("b")),
			mk("add", "v2", i32, loc("c"), loc("v1")),
			mk("add", "v3", i32, loc("d"), loc("v1")),
			// v4 is only ever the first operand, which r0 already holds.
			mk("add", "v4", i32, loc("e"), loc("f")),
			mk("add", "v5", i32, loc("v4"), loc("g")),
			{Op: "ret", Ops: []Value{loc("v2")}},
		},
	}}}
	alloc := planARMBlockAlloc(f, nil, map[string]string{})
	if alloc["v1"] != "r2" {
		t.Fatalf("v1 = %q, want r2 (%v)", alloc["v1"], alloc)
	}
	if _, ok := alloc["v4"]; ok {
		t.Fatalf("v4 allocated: %v", alloc)
	}

	// Division calls a helper that clobbers r2, so the block is skipped.
	fd := &Func{Name: "g", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			mk("add", "v1", i32, loc("a"), loc("b")),
			mk("sdiv", "v2", i32, loc("v1"), loc("c")),
			mk("add", "v3", i32, loc("d"), loc("v1")),
			{Op: "ret", Ops: []Value{loc("v3")}},
		},
	}}}
	if got := planARMBlockAlloc(fd, nil, map[string]string{}); len(got) != 0 {
		t.Fatalf("division block allocated: %v", got)
	}

	// A 64-bit operation uses r2 as its second scratch register.
	fw := &Func{Name: "h", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			mk("add", "v1", i32, loc("a"), loc("b")),
			mk("add", "w1", &Type{Kind: KInt, Bits: 64}, loc("x"), loc("y")),
			mk("add", "v3", i32, loc("d"), loc("v1")),
			{Op: "ret", Ops: []Value{loc("v3")}},
		},
	}}}
	if got := planARMBlockAlloc(fw, nil, map[string]string{}); len(got) != 0 {
		t.Fatalf("wide block allocated: %v", got)
	}
}
