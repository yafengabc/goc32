package gocl

// The trimmed driver seam. The LLVM object file is gone; only the pure-Go IR
// text producer and the helpers translateProgram calls remain.

import (
	"fmt"

	"goc/common"
	"goc/frontend"
)

// TranslateIR returns the textual IR for prog without loading libLLVM.
// native32 lowers this text to RV32IM or Thumb assembly.
func TranslateIR(prog *frontend.Program, arch string) (string, error) {
	common.DisablePrintfSpec = true
	ir, _, _, err := translateProgram(prog, nil, false, 1, arch)
	if err != nil {
		return "", fmt.Errorf("generating IR: %w", err)
	}
	return ir, nil
}

// UsesNativeLink is always false here: TinyOS supplies crt0 and the syscall
// entry, so the IR module must not inject an entry stub or syscall wrappers.
func UsesNativeLink(linux bool, arch string, prog *frontend.Program, lib *common.Program) bool {
	return false
}

// setStubExit rewrites the injected entry stub's terminator. TinyOS never
// injects one, so this is a no-op, but translateProgram still calls it.
func setStubExit(prog *frontend.Program, name string) {
	for _, f := range prog.Funcs {
		if f.Name != "_start" || f.Body == nil {
			continue
		}
		frontend.WalkStmts(f.Body, func(s frontend.Stmt) {
			for _, e := range stmtExprs(s) {
				walkExpr(e, func(n frontend.Expr) {
					if c, ok := n.(*frontend.Call); ok && c.Name == "__goclib_exit" {
						c.Name = name
					}
				})
			}
		})
	}
}
