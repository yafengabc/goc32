package native32

import "testing"

func TestPlanBlockAlloc(t *testing.T) {
	i32 := &Type{Kind: KInt, Bits: 32}
	loc := func(n string) Value { return Value{Kind: VLocal, Name: n} }
	mk := func(op, dst string, ty *Type, args ...Value) *Inst {
		return &Inst{Op: op, Dst: dst, Ty: ty, Ops: args}
	}
	f := &Func{Name: "f", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			// v1 is used twice as a second operand, so q0 forwarding would
			// leave two copies; it should get a register.
			mk("add", "v1", i32, loc("a"), loc("b")),
			// v2 is only stored; the stack machine reads it from q0 for free.
			mk("add", "v2", i32, loc("c"), loc("d")),
			mk("add", "v3", i32, loc("e"), loc("v1")),
			mk("add", "v4", i32, loc("f"), loc("v1")),
			// v3 feeds a return, which needs a fixed register.
			mk("store", "", i32, loc("v2"), loc("p")),
			mk("store", "", i32, loc("v4"), loc("p")),
			mk("zext", "v6", i32, loc("v1")),
			{Op: "ret", Ops: []Value{loc("v3")}},
		},
	}}}
	alloc := planBlockAlloc(f, nil, map[string]string{}, map[string]string{})
	if alloc["v1"] != "a0" {
		t.Fatalf("v1 = %q, want a0 (%v)", alloc["v1"], alloc)
	}
	if alloc["v3"] != "a1" {
		t.Fatalf("v3 = %q, want a1 (%v)", alloc["v3"], alloc)
	}
	if _, ok := alloc["v2"]; ok {
		t.Fatalf("v2 allocated: %v", alloc)
	}
	if _, ok := alloc["v4"]; ok {
		t.Fatalf("v4 allocated: %v", alloc)
	}
	if _, ok := alloc["v6"]; ok {
		t.Fatalf("zext allocated: %v", alloc)
	}

	// A block containing a call is skipped: the call clobbers the pool, and
	// its result may be aliased to a0 for the next instruction.
	fc := &Func{Name: "g", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			mk("add", "v1", i32, loc("a"), loc("b")),
			{Op: "call", Callee: "h", Ops: []Value{loc("v1")}},
			mk("add", "v3", i32, loc("e"), loc("v1")),
			{Op: "ret", Ops: []Value{loc("v3")}},
		},
	}}}
	if got := planBlockAlloc(fc, nil, map[string]string{}, map[string]string{}); len(got) != 0 {
		t.Fatalf("call block allocated: %v", got)
	}
}

func TestPlanBlockAllocValueNotAllocated(t *testing.T) {
	i32 := &Type{Kind: KInt, Bits: 32}
	loc := func(n string) Value { return Value{Kind: VLocal, Name: n} }
	f := &Func{Name: "small", Blocks: []*Block{{
		Label: "entry",
		Insts: []*Inst{
			{Op: "add", Dst: "v1", Ty: i32, Ops: []Value{loc("a"), loc("b")}},
			{Op: "ret", Ops: []Value{loc("v1")}},
		},
	}}}
	// The single return use is a move, so v1 is worth allocating.
	alloc := planBlockAlloc(f, nil, map[string]string{}, map[string]string{})
	if alloc["v1"] == "" {
		t.Fatalf("v1 not allocated: %v", alloc)
	}
}
