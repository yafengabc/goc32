[English](README.md) | [中文](README.zh.md)

# goc32 — TinyOS 用户态 C 编译器（RV32 + Thumb）

`goc32` 是 TinyOS 用户程序用的精简 C 编译器，**不依赖 libLLVM**。它是
[TinyOS](https://github.com/yafengabc/TinyOS) 的子项目，前端直接继承自
[goc](https://github.com/yafengabc/goc)。

一条命令就能把 C 源码变成 TinyOS 放进 littlefs 的那种 **LZ4TOS** 信封：

```
goc32 user/hello.c
```

流水线：

```
C -> goc 前端 -> 纯 Go 的 IR 文本 -> native32 -> RV32IM / Thumb 汇编
  -> clang（汇编）-> ld.lld（链接）-> LZ4TOS 信封
```

没有 cgo，也没有 `libLLVM.dll`。这一阶段 clang/ld.lld 只当汇编器和链接器用；
内置汇编器/链接器是下一步。

## 构建

```powershell
.\build.ps1            # -> bin/goc32.exe
```

或者 `bash build.sh`。需要 Go 1.25+。

## 用法

```powershell
# 在 TinyOS 检出目录里运行（或在任意地方用 -tos DIR）。默认目标 rv32。
goc32 user/hello.c
# -> hello.tos

goc32 -target esp32c3 -o build/hello.tos user/hello.c
goc32 -target rp2040  -o build/hello.tos user/hello.c
goc32 -target rp2350  -o build/hello.tos user/hello.c
goc32 -target arm     -o build/hello.tos user/hello.c
```

目标：

| `-target` | ISA | clang triple / CPU | 链接 emulation |
|---|---|---|---|
| `rv32`（默认）、`esp32c3` | RV32IMC | `riscv32-unknown-elf`、`-march=rv32imc -mabi=ilp32` | `elf32lriscv` |
| `rv32nc` | RV32IM | 同上，`-march=rv32im` | `elf32lriscv` |
| `arm` | Thumb / ARMv7-M | `thumbv7m`、Cortex-M3 | `armelf` |
| `rp2040` | Thumb-1 / ARMv6-M | `thumbv6m`、Cortex-M0+ | `armelf` |
| `rp2350` | Thumb / ARMv8-M | `thumbv8m.main`、Cortex-M33 | `armelf` |

诊断选项：`-S`（汇编）、`-ir`（gocl IR）、`-E`（预处理后的 C）、`-keep`、
`-v`、`-pack`（不编译，只把已链接的 ELF 包进 LZ4TOS）、`-ll`（把输入当作 LLVM
IR 文本并输出汇编）。额外 include/宏：`-I`、`-D`。

编译器从输入源文件向上查找 TinyOS 检出目录（找 `user/lib.c`、`user/user.ld`、
`include/tos.h`）。源码不在检出目录里时用 `-tos DIR` 或 `TOS_ROOT`。

## 输出

产物不是裸 ELF，而是带提示的 `LZ4TOS` 信封：

```
0  "LZ4TOS" (6)
6  u16 flags = 1
8  u32 raw ELF size
12 u32 compressed size
16 u32 span        （按页取整的 PT_LOAD 范围）
20 u32 stack_ask   （.note.tos TOS_STACK，0 = 板级默认）
24 u32 heap_ask    （.note.tos TOS_HEAP，0 = 不申请）
28 LZ4 block
```

span/stack/heap 的算法与 `tools/mklfs.c` 完全一致，所以内核能在解压**之前**
按这三个量分配任务窗口。`mklfs` 对非 ELF 文件是原样拷贝，因此把 `.tos` 放进
`build/fs-stage/bin/` 再重建 ramdisk 就能安装，不需要二次压缩。

端到端验证过的组合：

- RV32 / ESP32-C3：`hello`、`hello_printf`、`reloc`、`wheap`、`ls`、`sh`，
  在 `rv32emu-c` 里跑；
- ARM / QEMU mps2 Cortex-M3：同一组程序，在 `qemu-system-arm` 里跑。

`user/` 下的 37 个程序在 RV32 上全部能编译链接；ARM 上是 36/37，例外是
`smp.c`——它把 GCC 寄存器变量绑到 `r0`/`r1`/`r2` 并直接发 `svc #0`。

## 代码体积

后端基本是栈式机器：SSA 值通常各占一个栈帧槽位，上面再叠一层块内的小寄存器
分配器。全部 37 个 `user/*.c` 的实测（RV32IMC，`-O2` clang 基线取自
`build/fs-stage/bin`）：

| | goc32 | clang | 比值 |
|---|---:|---:|---:|
| 裸 ELF 合计 | 132,476 | 87,630 | 1.51x |
| LZ4TOS 合计 | 95,488 | 66,850 | 1.43x |

单个程序示例（裸 ELF / LZ4TOS）：

| 程序 | goc32 | clang |
|---|---:|---:|
| `hello` | 1,560 / 1,112 | 998 / 724 |
| `hello_printf` | 2,644 / 1,905 | 1,606 / 1,202 |
| `reloc` | 2,040 / 1,440 | 1,142 / 856 |
| `wheap` | 3,400 / 2,477 | 1,974 / 1,550 |
| `sh` | 21,380 / 14,571 | 16,054 / 12,589 |

第一版实测是裸 4.94x / 压缩后 3.94x。窥孔优化之后收益最大的几项：

- 数组下标和指针运算现在生成真正的 `getelementptr` + i32 下标，不再是
  `ptrtoint -> mul i64 -> add i64 -> inttoptr`；旧写法让每个下标都拖着一对
  64 位寄存器和进位链。GEP 还会直接复用已在寄存器里的基址或下标，并把常量
  偏移折进一条 `addi`；
- 不逃逸的标量局部变量（`char`、`short`、`int`、指针、double）直接按
  `sp+偏移` 读写，不再为 alloca 指针分配寄存器/槽位；
- 读写至少五次、且不逃逸的字长局部变量放进被调用者保存寄存器（RV32 的
  `s0-s7`）。读就是该寄存器的一次快照，后续使用直接读寄存器；写则更新它。
  热点参数使用剩下的寄存器；
- 参数的唯一用途是初始化一个直接局部变量时，前导代码直接从入参寄存器写入，
  那条 store 消失；
- 活范围在同一基本块内、且块内没有函数调用的值，分配到调用者保存寄存器
  （`a0-a3`，不够时退到 `a6`/`a7`），生产者直接写该寄存器，值不再占栈帧槽位。
  调用者保存寄存器不需要前导代码保存。只有当某个使用点否则必须从 `q0` 拷出
  时才分配；所有使用点都能直接读 scratch 寄存器的值就留在原地。
  跨块的值、alloca、phi 目标、含调用的基本块保持槽位。跨块值也评估过用空闲的
  `s0-s7`，但前端把 C 局部变量都放在 alloca 里，剩下的临时值太少（而且经过
  `q0` 转发后太便宜），抵不过前导保存/恢复的成本；
- 紧接下一条指令使用的调用结果留在 `a0`，不再往返栈；内存写直接复用已活的
  值或指针寄存器；
- 生成代码前删除死掉的纯 SSA 计算；不逃逸局部变量的 load（栈读取不会 fault）
  以及没人读的局部变量连同它的 store 一起删掉；
- RV32 用 `q0`/`q1` 两个伪临时寄存器，窥孔后变成 `a4`/`a5`。两者都在
  `x8-x15`，汇编器可以用压缩的 `c.*` 形式；窥孔会规范化别名，所以同一个物理
  寄存器作为调用参数被写时，缓存值会失效；
- 空操作转换（`ptrtoint`/`inttoptr`、同宽 `bitcast`/`trunc`/`zext`/`sext`）
  直接用操作数替代，去掉了 gocl 为每个经过 TinyOS i32 ABI 的指针生成的拷贝；
- 地址不逃逸的标量局部变量的 load，替换为前一条 store（或 load）留下的 SSA
  值；同一块内成立，跨块时要求所有前驱一致。最热的局部变量排除在外，因为它们
  已经在被调用者保存寄存器里；
- RV32 窥孔会转发 load、删除死 store、把小区间常量下沉进 ALU 指令、把乘/除以
  2 的幂改成移位、把 `li 0 + sw` 改写成 `sw zero`、删掉跳到下一条标签的 jump，
  以及在分支目标是 fallthrough 时反转 `bnez/beqz` + `j`；
- 只被分支使用的比较直接融进该分支（`slt` + `xori 1` + `beqz` 变 `blt`，
  `snez` + `beqz` 变 `beqz`，单独的 `xori 1` 反转分支），`xori x, y, 0`
  直接消失；这去掉了栈式发射器产生的绝大部分布尔值物化；
- `addi p, base, off; load/store x, 0(p)`（任意基址寄存器，并且可把
  `addi p, p, add` 的 GEP 步进折进来）在 `p` 之后死亡时变成直接的 `off(base)`
  访问，去掉指针物化；基址被写则停止；一次地址计算可以喂多个访问；
- 只含一条 `j` 的空 RV32 基本块被串掉，和 Thumb 一侧相同；
- 只被调用一次、且不超过 40 条 IR 指令的函数在该调用点内联，地址未被取用时
  丢弃函数体外的那份。gocl 把 C 的 `static` 函数发成 `internal` 链接，只有
  这些是候选：别的编译单元不可能引用被删的符号，所以多文件程序也是安全的
  （以前只支持单输入）。更大的函数保持不内联：被调用者的值并进调用者会让
  栈式帧变大，到 80 条指令左右就已经超过省下的调用开销。ARMv6-M 出于同样
  原因排除；
- 小的 RV32 函数每个值保留一个槽位，窥孔才能按值证明 store 已死；帧超过
  2 KiB 时改用按块的生命周期池。Thumb 更在意帧大小而不是逐值 store 证明，
  所以始终用池，并把最热的槽位放在短偏移处。池索引按访问次数排序，热点临时
  值拿到 Thumb 和 RVC 能紧凑编码的短偏移；`deep` 的递归帧从 2,224 B 降到
  2,112 B。Thumb 还会把热标量 alloca 放在池下面、大的聚合 alloca 放在帧顶，
  让常见访问落在短的 `sp` 偏移内。

同样的寄存器变量与参数 store 融合也用在 Thumb 上，使用 `r5-r7`（ARMv6-M 的
ALU 指令只能编码低寄存器），参数直接从 `r0-r3` 写入。ARMv7-M/ARMv8-M 把可用
集合扩到 `r8-r11`——Thumb-2 接受它们作为 ALU 操作数，也有 32 位 load/store
形式（ARMv6-M 必须留在低寄存器）；只有少见的、带高寄存器操作数的 `muls`
需要先拷到低寄存器。Thumb 的 GEP 直接复用已加载的基址/下标，并把小偏移折进
`adds`；当分支与 fallthrough 目标之间没有字面量池时反转条件分支，`.ltorg`
只为真正用到池的基本块发出。比较结果只被分支使用时，直接发成 `cmp` + 条件
分支，而不是物化 0/1、存栈、再为分支读回；比较外的 `xor 1` 变成反转分支。
文本阶段还会把槽位缓存留下的寄存器拷贝折进消费它的 `cmp`/store，并在 scratch
寄存器之后死亡时，把 `movs rX, #imm; cmp rD, rX` / `adds rD, rN, rX` 改成
立即数形式。拷贝也会折进 load/store 基址、符号/零扩展或第二条 move。只含一条
`b` 的空块被串掉；ARMv7-M/ARMv8-M 可以跳函数内任意位置，ARMv6-M 只在短条件/
无条件跳转范围内改目标。缓存在块内保留字长的 `str/ldr [sp,#off]` 对，存过又读
的值变成寄存器 move（或直接消失）；没有被后续 load 读取的字 store 被删除，
但传出栈参数区和 vararg 寄存器保存区（`@ goc32keep`）除外。ARMv6-M 上
`add r4, sp, #off` 后接 `[r4]` 访问的是同一个槽位，所以超过 124 字节 sp 范围的
帧也走同样处理；任何其他写 r4 都会让缓存的偏移失效。

一个块内值如果否则需要在某个使用点从 `r0` 拷出（第二 ALU 操作数或 store
地址），可以放在 `r2`：生产者直接写 `r2`，定义时的 store 和每个使用点的拷贝
都消失。`r2` 是调用者保存、标量代码用不到，但含 64 位运算、聚合/8 字节内存
访问、除法辅助函数或 intrinsic 的块保留槽位，因为这些路径把 `r2` 当 scratch。
`r3` 不给分配器：与范围无关的 Thumb-1 跳转会把目标放进去。一个"字节左移到位"
的常量用 `movs` + `lsls` 构造，而不是字面量；在 ARMv6-M 上是 4 字节，而内联
字面量块是 12 字节。

旧 Thumb 体积的很大一部分来自保守的长跳转蹦床：每条无条件跳转都是
`ldr r3,.LbrN; mov pc,r3; .balign 4; .word target`，`sh` 里有 1,665 条
（6.6 KiB 的字加上指令）。后处理现在在目标可达时把蹦床替换成直接跳转
（ARMv7-M/v8-M 上总是可行，ARMv6-M 上按行距保守判断）。剩下的部分是 Thumb-1
本身的限制：`sh` 的 4,850 个 sp 相对访问里有 3,929 个因为帧偏移超过 124 而
必须用 32 位 `ldr/str` 形式，两操作数 ALU 运算需要先 `movs`，ARMv6-M 的寄存器
变量只能用 `r5-r7`，常量和符号地址走字面量池。ARM 的 `sh` 现在是 Cortex-M3
上 25,832 B / 压缩 16,751，RP2040 上 30,460 / 17,563——蹦床重写前是 72 KB /
84 KB，最初是 199 KB。ARMv7-M/ARMv8-M 上数值常量用 `movw`/`movt` 而不是
字面量（16 位以内 4 字节，超过 8 字节，也不占池）；ARMv6-M 的块末前向池也
实测过，虽然能省约 2 KB 裸文本，但压缩后比内联 `ldr`/字对更差，所以 ARMv6-M
保留内联形式。

栈帧是另一项可见成本。`deep.c` 故意溢出 8 KiB 默认栈，依赖内核把栈翻倍一次。
槽位布局调整后 `recurse` 帧是 2,112 B，镜像仍在一个 16 KiB 窗口粒度内，
`deep 6` 会增长栈并带着完整 journal 结束（故意写坏的 `deep 6 0 8` 变体仍按
文档所述死在堆尾部）。

## 目录

```
main.go                 一键驱动：编译、链接、打包
internal/frontend/      C 词法/语法/检查器（来自 goc）
internal/common/        预处理器与共享程序类型（来自 goc）
internal/ir/            gocl 的纯 Go IR 转换器，去掉了 LLVM DLL 加载器和目标文件发射
internal/goa/           汇编器查询的两函数桩（TinyOS 不会走那条路径）
internal/native32/      IR 解析器 + RV32IM 与 Thumb 发射器
internal/lz4tos/        LZ4 块压缩、ELF 窗口提示、信封
tinyos/goc/             usys.h 兼容层 + ecall/svc 用的 __tos_syscall
```

## 当前覆盖与限制

后端覆盖 `user/lib.c` 和小型用户程序用到的整数/指针/数组/结构体/函数/可变参数
子集。浮点、`long double`、原子操作、TLS 和内联汇编还没实现。值通常住在栈帧
槽位里（栈式机器）；上面的局部窥孔、槽位复用和 RV32 块内寄存器分配器能压小
输出，但没有函数级寄存器分配器，所以**正确性和体积仍优先于速度**。

`rp2040` 发 ARMv6-M 子集；`arm`（Cortex-M3）和 `rp2350`（Cortex-M33）使用更宽的
Thumb-2 偏移形式，所以二进制按板子分别构建。ARM 链接目前需要 `-z notext`，
因为 Thumb-1 的超范围跳转和符号字面量是 `.text` 里的绝对字；TinyOS 的加载器
已经会处理 `R_ARM_ABS32`。
