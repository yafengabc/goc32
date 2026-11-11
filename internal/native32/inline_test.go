package native32

import (
	"testing"
)

const inlineSingleRet = `
define internal i32 @add1(i32 %p0) {
entry:
  %v = add i32 %p0, 1
  ret i32 %v
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %r = call i32 @add1(i32 5)
  ret i32 %r
}
`

func TestInlineSingleReturn(t *testing.T) {
	m, err := ParseModule(inlineSingleRet)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 1 || m.Funcs[0].Name != "main" {
		t.Fatalf("functions after inline: %v", funcNames(m))
	}
	if n := countCalls(m, "add1"); n != 0 {
		t.Fatalf("%d calls to add1 remain", n)
	}
	found := false
	for _, b := range m.Funcs[0].Blocks {
		for _, in := range b.Insts {
			if in.Op == "add" && len(in.Ops) == 2 && in.Ops[0].Kind == VInt && in.Ops[0].I == 5 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("inlined body missing")
	}
}

const inlineMultiRet = `
define internal i32 @pick(i32 %p0) {
entry:
  %c = icmp ne i32 %p0, 0
  br i1 %c, label %yes, label %no
yes:
  ret i32 10
no:
  ret i32 20
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %r = call i32 @pick(i32 1)
  ret i32 %r
}
`

func TestInlineMultiReturn(t *testing.T) {
	m, err := ParseModule(inlineMultiRet)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 1 || m.Funcs[0].Name != "main" {
		t.Fatalf("functions after inline: %v", funcNames(m))
	}
	var phi *Inst
	for _, b := range m.Funcs[0].Blocks {
		for _, in := range b.Insts {
			if in.Op == "phi" && in.Dst == "r" {
				phi = in
			}
		}
	}
	if phi == nil {
		t.Fatalf("no return phi: %#v", m.Funcs[0].Blocks)
	}
	if len(phi.Ops) != 2 || len(phi.Labels) != 2 {
		t.Fatalf("phi operands = %d labels = %d", len(phi.Ops), len(phi.Labels))
	}
}

const inlineRecursive = `
define internal i32 @loop(i32 %p0) {
entry:
  %c = icmp ne i32 %p0, 0
  br i1 %c, label %again, label %done
again:
  %r = call i32 @loop(i32 0)
  ret i32 %r
done:
  ret i32 0
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %r = call i32 @loop(i32 1)
  ret i32 %r
}
`

func TestInlineSkipsRecursion(t *testing.T) {
	m, err := ParseModule(inlineRecursive)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 2 {
		t.Fatalf("recursive function was inlined: %v", funcNames(m))
	}
}

const inlineTwoCalls = `
define internal i32 @f(i32 %p0) {
entry:
  %v = add i32 %p0, 1
  ret i32 %v
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %a = call i32 @f(i32 1)
  %b = call i32 @f(i32 2)
  %s = add i32 %a, %b
  ret i32 %s
}
`

func TestInlineSkipsTwoCallSites(t *testing.T) {
	m, err := ParseModule(inlineTwoCalls)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 2 {
		t.Fatalf("two-call-site function was inlined: %v", funcNames(m))
	}
}

const inlineAddressTaken = `
define internal i32 @f(i32 %p0) {
entry:
  %v = add i32 %p0, 1
  ret i32 %v
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %addr = ptrtoint ptr @f to i32
  %r = call i32 @f(i32 5)
  %s = add i32 %r, %addr
  ret i32 %s
}
`

func TestInlineKeepsAddressTaken(t *testing.T) {
	m, err := ParseModule(inlineAddressTaken)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 2 {
		t.Fatalf("address-taken function removed: %v", funcNames(m))
	}
	if n := countCalls(m, "f"); n != 0 {
		t.Fatalf("call not inlined: %d", n)
	}
}

const inlineProtectedIR = `
define internal i32 @memcpy(ptr %p0, ptr %p1, i32 %p2) {
entry:
  ret i32 0
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %r = call i32 @memcpy(ptr %p1, ptr %p1, i32 4)
  ret i32 %r
}
`

func TestInlineKeepsBackendHelpers(t *testing.T) {
	m, err := ParseModule(inlineProtectedIR)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	found := false
	for _, f := range m.Funcs {
		if f.Name == "memcpy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("memcpy removed: %v", funcNames(m))
	}
}

const inlineExternal = `
define i32 @helper(i32 %p0) {
entry:
  %v = add i32 %p0, 1
  ret i32 %v
}
define i32 @main(i32 %p0, ptr %p1) {
entry:
  %r = call i32 @helper(i32 5)
  ret i32 %r
}
`

func TestInlineSkipsExternal(t *testing.T) {
	m, err := ParseModule(inlineExternal)
	if err != nil {
		t.Fatal(err)
	}
	InlineSmall(m, 1000)
	if len(m.Funcs) != 2 {
		t.Fatalf("external function was inlined or removed: %v", funcNames(m))
	}
	if n := countCalls(m, "helper"); n != 1 {
		t.Fatalf("external call changed: %d", n)
	}
}

func funcNames(m *Module) []string {
	var out []string
	for _, f := range m.Funcs {
		out = append(out, f.Name)
	}
	return out
}

func countCalls(m *Module, callee string) int {
	n := 0
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, in := range b.Insts {
				if in.Op == "call" && !in.Indirect && in.Callee == callee {
					n++
				}
			}
		}
	}
	return n
}
