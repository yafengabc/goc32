package native32

import "testing"

func TestSimplifyNoopCasts(t *testing.T) {
	iw := &Type{Kind: KInt, Bits: 32}
	f := &Func{Blocks: []*Block{{Label: "entry"}}}
	add := func(in *Inst) { f.Blocks[0].Insts = append(f.Blocks[0].Insts, in) }
	add(&Inst{Op: "ptrtoint", Dst: "a", Ty: iw, SrcTy: &Type{Kind: KPtr},
		Ops: []Value{{Kind: VLocal, Name: "p"}}})
	add(&Inst{Op: "inttoptr", Dst: "b", Ty: &Type{Kind: KPtr}, SrcTy: iw,
		Ops: []Value{{Kind: VLocal, Name: "a"}}})
	add(&Inst{Op: "bitcast", Dst: "f", Ty: &Type{Kind: KFloat}, SrcTy: iw,
		Ops: []Value{{Kind: VLocal, Name: "b"}}})
	add(&Inst{Op: "add", Dst: "sum", Ty: iw,
		Ops: []Value{{Kind: VLocal, Name: "f"}, {Kind: VInt, I: 1}}})
	// An aggregate bitcast has the same byte size but is not a scalar copy.
	add(&Inst{Op: "bitcast", Dst: "agg", Ty: &Type{Kind: KArray,
		Elem: &Type{Kind: KInt, Bits: 8}, N: 4}, SrcTy: iw,
		Ops: []Value{{Kind: VLocal, Name: "p"}}})

	simplifyFunc(f)

	var addInst *Inst
	for _, in := range f.Blocks[0].Insts {
		switch in.Op {
		case "ptrtoint", "inttoptr":
			t.Fatalf("no-op cast %s left", in.Op)
		case "bitcast":
			if in.Dst != "agg" {
				t.Fatalf("scalar bitcast left: %s", in.Dst)
			}
		case "add":
			addInst = in
		}
	}
	if addInst == nil || addInst.Ops[0].Name != "p" {
		t.Fatalf("add operand = %+v, want p", addInst)
	}
}
