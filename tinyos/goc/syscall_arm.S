/* TinyOS user-mode syscall entry for goc's Thumb output.
 * ABI: r0 = number, r1/r2/r3 = arguments, r0 = result. */
    .syntax unified
    .thumb
    .text
    .globl __tos_syscall
    .thumb_func
    .type __tos_syscall,%function
__tos_syscall:
    svc     #0
    bx      lr
    .size __tos_syscall, .-__tos_syscall

/* GCC inline asm's TOS_ILLEGAL_INSN() in TinyOS's lib.h.  0xDE00 is the
 * 16-bit Thumb encoding of UDF #0, available on ARMv6-M and ARMv8-M. */
    .globl __goc32_trap
    .thumb_func
    .type __goc32_trap,%function
__goc32_trap:
    .hword  0xDE00
    bx      lr
    .size __goc32_trap, .-__goc32_trap
