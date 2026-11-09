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
`-v`, `-pack` (wrap an already linked ELF in LZ4TOS without compiling), `-ll`
(treat the input as LLVM IR text and emit assembly). Extra includes/defines:
`-I`, `-D`.

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

The back end is mostly a stack machine: SSA values normally get a frame slot,
with a small block-local register allocator layered on top.  Measured over all
37 `user/*.c` programs (RV32IMC, `-O2` clang baseline from
`build/fs-stage/bin`):

| | goc32 | clang | ratio |
|---|---:|---:|---:|
| raw ELF total | 132,892 | 87,630 | 1.52x |
| LZ4TOS total | 95,894 | 66,850 | 1.43x |

Examples (raw ELF / LZ4TOS):

| program | goc32 | clang |
|---|---:|---:|
| `hello` | 1,560 / 1,112 | 998 / 724 |
| `hello_printf` | 2,644 / 1,905 | 1,606 / 1,202 |
| `reloc` | 2,040 / 1,440 | 1,142 / 856 |
| `wheap` | 3,400 / 2,492 | 1,974 / 1,550 |
| `sh` | 21,444 / 14,676 | 16,054 / 12,589 |

The first measurement was 4.94x raw / 3.94x compressed.  The biggest wins
after the peephole were:

- array subscripts and pointer arithmetic now emit a real `getelementptr`
  with an i32 index instead of `ptrtoint -> mul i64 -> add i64 -> inttoptr`;
  the old form dragged every subscript through a 64-bit pair and carry chain.
  The GEP also consumes a base or index already in a register directly and
  folds constant offsets into one `addi`;
- a non-escaping scalar local (`char`, `short`, `int`, pointer, double) is
  addressed directly as `sp+offset` for its loads and stores, so the alloca
  pointer is never materialised and the object needs no pointer slot;
- a non-escaping word local that is read or written at least four times is
  kept in a callee-saved register (`s0-s7` on RV32).  A load is a snapshot of
  that register, so its uses can read the register directly instead of a
  stack slot; a store updates it.  Hot parameters use the remaining registers;
- a parameter whose only use is the initial store to a direct local is written
  straight from its incoming argument register in the prologue, and the store
  instruction disappears;
- a value whose live range stays inside one basic block with no call is
  allocated to a caller-saved register (`a0-a3`, falling back to `a6`/`a7`),
  and the producer writes that register directly, so the value never gets a
  frame slot.  Caller-saved registers need no prologue save.  A value is only
  allocated when some use would otherwise need a copy out of `q0`; a value
  that every use can read from the scratch register is left in place.
  Values crossing a block, allocas, phi destinations and blocks containing a
  call keep their slots;
- a call result consumed by the next instruction stays in `a0` instead of
  taking the stack round trip, and memory stores use an already-live value or
  pointer register directly;
- dead pure SSA computations are removed before code generation, as are loads
  from non-escaping locals (a stack load cannot fault), and an unread local is
  dropped together with its stores;
- RV32 computes through `q0`/`q1` pseudo-temporaries that become `a4`/`a5`
  after the peephole.  Both are in `x8-x15`, so the assembler can use the
  compressed `c.*` forms; the peephole canonicalises the alias, so cached
  values are killed when the same physical register is written as a call
  argument;
- no-op casts (`ptrtoint`/`inttoptr`, same-width `bitcast`/`trunc`/`zext`/`sext`)
  are replaced by their operand, which removes the copy gocl emits for every
  pointer passed through TinyOS's i32 ABI;
- a load of a scalar local whose address never escapes is replaced by the SSA
  value the preceding store (or load) left in that local, both within a block
  and across a block edge when every predecessor agrees on the same value; the
  hottest locals are excluded because they already live in a callee-saved
  register;
- the RV32 peephole forwards loads, deletes dead stores, sinks small constants
  into ALU instructions, turns multiply/divide by a power of two into shifts,
  rewrites `li 0 + sw` as `sw zero`, drops jumps to the following label, and
  inverts a `bnez/beqz` + `j` pair when the branch target is the fallthrough;
- a comparison whose only consumer is a branch is fused into that branch
  (`slt` + `xori 1` + `beqz` becomes `blt`, `snez` + `beqz` becomes `beqz`,
  a bare `xori 1` flips the branch), and `xori x, y, 0` disappears; this
  removes most of the boolean materialisation the stack-machine emitter
  produces;
- `addi p, sp, off; load/store x, 0(p)` (and the same with one GEP offset
  folded in) becomes a direct `off(sp)` access when `p` is dead afterwards,
  removing the alloca pointer materialisation; one address computation can
  feed several accesses and the whole run is folded;
- empty RV32 blocks that contain only a `j` are threaded away, the same way as
  the Thumb ones;
- small RV32 functions keep one slot per value so the peephole can prove stores
  dead per value; frames over 2 KiB use a per-block liveness pool instead.
  Thumb cares more about frame size than about per-value store proof, so it
  always uses the pool and keeps the hottest slots at short offsets.
  The pool indices are ranked by access count, so hot temporaries get the
  short offsets Thumb and RVC can encode compactly; `deep`'s recursive frame
  falls from 2,224 to 2,112 bytes.  Thumb additionally puts the hot scalar
  allocas below the pool and large aggregate allocas at the top of the frame,
  keeping the common accesses inside a short `sp` offset.

The same register-variable and parameter-store fusion runs on Thumb, using
`r5-r7` (ARMv6-M ALU instructions only encode low registers) and writing
parameters straight from `r0-r3`.  ARMv7-M/ARMv8-M extend that set with
`r8-r11`, which Thumb-2 accepts as ALU operands and in the 32-bit
load/store forms (ARMv6-M must stay on low registers); only the rare `muls`
with a high operand needs a low-register copy.  Thumb GEPs consume an
already-loaded base or index directly and fold small offsets into `adds`; a
conditional branch is inverted when no literal pool sits between it and its
fallthrough target, and `.ltorg` is emitted only for blocks that actually use
a pool.  A comparison whose result is used only by a branch is emitted as a
direct `cmp` + conditional branch instead of materialising a 0/1 value,
storing it to the stack frame and reloading it for the branch; a `xor 1`
around that comparison becomes the inverted branch.  The text pass also folds
the register copies left by the slot cache into the consuming `cmp`/store, and
turns `movs rX, #imm; cmp rD, rX` / `adds rD, rN, rX` into the immediate forms
when the scratch register is dead afterwards.  A copy is also folded into a
load/store base, a sign/zero extend, or a second move.  Empty blocks that contain only
a `b` are threaded away; ARMv7-M/ARMv8-M can branch anywhere in the function,
while ARMv6-M retargets only within the short conditional/unconditional range.
The cache keeps word-sized `str/ldr [sp,#off]` pairs within a block, so a
value stored and read again becomes a register move (or disappears); word
stores that no surviving load reads are deleted, except the outgoing stack
argument area and the vararg register save area (`@ goc32keep`).  On
ARMv6-M, `add r4, sp, #off` followed by `[r4]` addresses the same slot, so
frames past the 124-byte sp range get the same treatment; the cached offset
is invalidated by any other write to r4.

A large part of the old Thumb size was the conservative long-branch
trampoline: every unconditional jump used `ldr r3,.LbrN; mov pc,r3;
.balign 4; .word target`, and `sh` had 1,665 of them (6.6 KiB of words plus
instructions).  The post-pass now replaces a trampoline with a direct branch
when its target is in range (always on ARMv7-M/v8-M, line-distance guarded on
ARMv6-M).  What remains is inherent to Thumb-1: 3,929 of `sh`'s 4,850
sp-relative accesses need the 32-bit `ldr/str` form because the frame offset
is past 124, two-operand ALU operations need a setting `movs`, ARMv6-M can
hold register variables only in `r5-r7`, and constants/symbol addresses use
literal pools.  ARM `sh` is now 26,024 bytes / 16,859 compressed on the
Cortex-M3 target and 30,956 / 17,722 for the RP2040, down from 72 KB / 84 KB
before the trampoline rewrite and 199 KB originally.

The frame is the other visible cost. `deep.c` deliberately overflows the 8 KiB
default stack and relies on the kernel doubling it once.  After the slot
layout work the `recurse` frame is 2,112 bytes, the image stays inside one
16 KiB window grain, and `deep 6` now grows the stack and completes with its
journal intact (the deliberate `deep 6 0 8` variant still dies in the heap
tail, as its documentation says it should).

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
atomics, TLS and inline asm are not implemented yet. Values normally live in
frame slots (a stack machine); the local peephole, slot reuse and the RV32
block-local register allocator above shrink the output, but there is no
function-wide register allocator, so correctness and size still come before
speed.

`rp2040` emits the ARMv6-M subset; `arm` (Cortex-M3) and `rp2350` (Cortex-M33)
use the wider Thumb-2 offset forms, so binaries are built per board. ARM links
currently need `-z notext` because Thumb-1's out-of-range branches and symbol
literals are absolute words in `.text`; TinyOS's loader already applies
`R_ARM_ABS32`.
