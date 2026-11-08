/* TinyOS user-mode syscall entry for goc's RV32 output.
 * ABI: a7 = number, a0/a1/a2 = arguments, a0 = result. */
    .text
    .globl __tos_syscall
    .type __tos_syscall,@function
__tos_syscall:
    mv      a7, a0
    mv      a0, a1
    mv      a1, a2
    mv      a2, a3
    ecall
    ret
    .size __tos_syscall, .-__tos_syscall

/* GCC inline asm's TOS_ILLEGAL_INSN() in TinyOS's lib.h. */
    .globl __goc32_trap
    .type __goc32_trap,@function
__goc32_trap:
    .word 0
    ret
    .size __goc32_trap, .-__goc32_trap
