# goc32 — TinyOS user-space C compiler (RV32 + Thumb)

`goc32` is a trimmed, libLLVM-free C compiler for TinyOS user programs. It is a
subproject of [TinyOS](https://github.com/yafengabc/TinyOS) and a direct
descendant of the [goc](https://github.com/yafengabc/goc) front end.

One command takes a C source and produces the exact **LZ4TOS** envelope TinyOS
places in littlefs:

```
goc32 user/hello.c
```

Pipeline:

```
C -> goc front end -> pure-Go IR text -> native32 -> RV32IM / Thumb asm
  -> clang (assemble) -> ld.lld (link) -> LZ4TOS envelope
```

No cgo and no `libLLVM.dll`. clang/ld.lld are used only as the assembler and
linker in this milestone; an internal assembler/linker is next.

## Build

```powershell
.\build.ps1            # -> bin/goc32.exe
```

or `bash build32.sh`. Go 1.25+ is required.

## Usage

```powershell
# Run from a TinyOS checkout (or pass -tos DIR). The target defaults to rv32.
goc32 user/hello.c
# -> hello.tos

goc32 -target esp32c3 -o build/hello.tos user/hello.c
goc32 -target rp2040  -o build/hello.tos user/hello.c
goc32 -target rp2350  -o build/hello.tos user/hello.c
goc32 -target arm     -o build/hello.tos user/hello.c
```

Targets:

| `-target` | ISA | clang triple / CPU | link emulation |
|---|---|---|---|
| `rv32` (default), `esp32c3` | RV32IMC | `riscv32-unknown-elf`, `-march=rv32imc -mabi=ilp32` | `elf32lriscv` |
| `rv32nc` | RV32IM | same, `-march=rv32im` | `elf32lriscv` |
| `arm` | Thumb / ARMv7-M | `thumbv7m`, Cortex-M3 | `armelf` |
| `rp2040` | Thumb-1 / ARMv6-M | `thumbv6m`, Cortex-M0+ | `armelf` |
| `rp2350` | Thumb / ARMv8-M | `thumbv8m.main`, Cortex-M33 | `armelf` |

Diagnostics: `-S` (assembly), `-ir` (gocl IR), `-E` (preprocessed C), `-keep`,
`-v`, `-pack` (wrap an already linked ELF in LZ4TOS without compiling). Extra
includes/defines: `-I`, `-D`.

The compiler finds the TinyOS checkout by walking up from the input source (it
looks for `user/lib.c`, `user/user.ld`, `include/tos.h`). Use `-tos DIR` or
`TOS_ROOT` when the source is outside a checkout.

## Output

The output is not a raw ELF. It is the hinted `LZ4TOS` envelope:

```
0  "LZ4TOS" (6)
6  u16 flags = 1
8  u32 raw ELF size
12 u32 compressed size
16 u32 span        (page-rounded PT_LOAD range)
20 u32 stack_ask   (.note.tos TOS_STACK, 0 = board default)
24 u32 heap_ask    (.note.tos TOS_HEAP, 0 = none)
28 LZ4 block
```

The span/stack/heap values are derived exactly like `tools/mklfs.c`, so the
kernel can size the task window before decompressing. `mklfs` copies a file
that is not an ELF verbatim, so dropping the `.tos` into `build/fs-stage/bin/`
and rebuilding the ramdisk installs it without recompression.

Tested end to end:

- RV32 / ESP32-C3, `hello`, `hello_printf`, `reloc`, `wheap`, `ls` and `sh`
  in `rv32emu-c`;
- ARM / QEMU mps2 Cortex-M3, the same set in `qemu-system-arm`.

All 37 programs under `user/` compile and link for RV32. 36/37 do so for ARM;
`smp.c` is the exception because it binds GCC register variables to `r0`/`r1`/
`r2` and issues a raw `svc #0`.

## Code size

The back end is a stack machine: every SSA value gets a frame slot and no
optimisation runs, so images are larger than clang's. Measured over all 37
`user/*.c` programs (RV32IMC, `-O2` clang baseline from `build/fs-stage/bin`):

| | goc32 | clang | ratio |
|---|---:|---:|---:|
| raw ELF total | 289,660 | 87,630 | 3.31x |
| LZ4TOS total | 149,725 | 66,850 | 2.24x |

Examples (raw ELF / LZ4TOS):

| program | goc32 | clang |
|---|---:|---:|
| `hello` | 2,616 / 1,662 | 998 / 724 |
| `hello_printf` | 5,716 / 3,074 | 1,606 / 1,202 |
| `reloc` | 3,224 / 2,017 | 1,142 / 856 |
| `wheap` | 7,176 / 3,885 | 1,974 / 1,550 |
| `sh` | 56,516 / 23,745 | 16,054 / 12,589 |

The first measurement was 4.94x raw / 3.94x compressed.  The reduction comes
from several passes:

- a per-basic-block RV32 peephole forwards a load from the register that
  already holds the value, deletes a store whose slot is never read, drops
  zero-offset GEPs, sinks a small constant into the consuming ALU instruction
  (`li t1,4; add` becomes `addi`), turns multiply-by-power-of-two into a
  shift, and removes a jump to the following label;
- dead pure SSA computations are removed before code generation;
- alloca pointers are rematerialised as `sp+offset` instead of taking a slot;
- stores another function reads (outgoing stack arguments and the vararg save
  area) are marked `# goc32keep` so dead-store elimination cannot touch them.

Small functions keep a unique slot per value, which lets the peephole prove
stores dead per value.  Functions whose unique frame would exceed 2 KiB use a
per-block slot pool instead: values live only inside one block share slots by
liveness, keeping frame offsets small.  That is what takes `deep`'s recursive
frame from 4,400 to 2,224 bytes and shrinks the big programs.

Thumb output is roughly another 1.7-2x larger than RV32 because Thumb-1 cannot
encode most immediates or long branches directly; those become literal-pool
words and branch islands.  The ARM emitter uses the 12-bit `[sp,#off]` form on
ARMv7-M/ARMv8-M, one `add r4, sp, #imm` for ARMv6-M up to 1 KiB, and an inline
literal for larger offsets.  ARM `sh` is 93 KiB on the Cortex-M3 target and
123 KiB for the RP2040, down from 199 KiB.

The frame is the other visible cost. `deep.c` deliberately overflows the 8 KiB
default stack and relies on the kernel doubling it once; goc32's unoptimised
frame for `recurse` is 4,400 bytes instead of roughly 2 KiB, which pushes the
image into the next 16 KiB window grain and leaves no heap tail to grow into.
`deep 1` runs, `deep` (six levels) still faults. Shrinking frames needs slot
reuse or a register allocator.

## Layout

```
main.go                 one-click driver: compile, link, pack
internal/frontend/      C lexer/parser/checker (from goc)
internal/common/        preprocessor and shared program types (from goc)
internal/ir/            gocl's pure-Go IR translator, with the LLVM DLL
                        loader and object emitter removed
internal/goa/           two-function stub for the assembler queries the IR
                        translator makes (TinyOS never takes that path)
internal/native32/      IR parser + RV32IM and Thumb emitters
internal/lz4tos/        LZ4 block compressor, ELF window hint, envelope
tinyos/goc/             usys.h shim + __tos_syscall for ecall/svc
```

## Current coverage and limits

The back ends cover the integer/pointer/array/struct/function/variadic subset
used by `user/lib.c` and the small user programs. Floating point, `long double`,
atomics, TLS and inline asm are not implemented yet. Values live in frame slots
(a stack machine); the local peephole and slot reuse above shrink the output,
but there is no register allocator, so correctness and size still come before
speed.

`rp2040` emits the ARMv6-M subset; `arm` (Cortex-M3) and `rp2350` (Cortex-M33)
use the wider Thumb-2 offset forms, so binaries are built per board. ARM links
currently need `-z notext` because Thumb-1's out-of-range branches and symbol
literals are absolute words in `.text`; TinyOS's loader already applies
`R_ARM_ABS32`.
