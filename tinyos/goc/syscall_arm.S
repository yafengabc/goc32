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
