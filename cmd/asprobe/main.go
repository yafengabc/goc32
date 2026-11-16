// asprobe assembles one small file and prints the allocated section bytes as
// hex.  It exists so the assembler can be diffed against clang
// instruction-by-instruction from a script; it is not part of the build.
package main

import (
	"flag"
	"fmt"
	"os"

	"goc32/internal/as"
	"goc32/internal/obj"
)

func main() {
	archName := flag.String("arch", "thumb2", "rv32|thumb1|thumb2")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: asprobe -arch ... file.s")
		os.Exit(2)
	}
	arch := as.Thumb2
	switch *archName {
	case "rv32":
		arch = as.RV32
	case "thumb1":
		arch = as.Thumb1
	}
	src, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ob, err := as.Assemble(src, as.Opt{Arch: arch, File: flag.Arg(0)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var out []byte
	for _, s := range ob.Secs {
		if s.Flags&obj.SHF_ALLOC != 0 {
			out = append(out, s.Data...)
		}
	}
	fmt.Printf("%x\n", out)
}
