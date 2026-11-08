#ifndef TINYOS_ARCH_USYS_GOC_H
#define TINYOS_ARCH_USYS_GOC_H

/* goc-friendly replacement for arch/<arch>/usys.h.
 *
 * The real header uses GCC extended inline asm with register variables
 * (`register int a7 __asm__("a7")`), which goc does not parse.  The compiler
 * links tinyos/goc/syscall_<arch>.S instead; that file implements the same
 * ABI (RV32: a7=number, a0/a1/a2=args; ARM: r0=number, r1/r2/r3=args). */

int __tos_syscall(int n, int a, int b, int c);

static inline int tos_sys0(int n)
{
    return __tos_syscall(n, 0, 0, 0);
}

static inline int tos_sys1(int n, int a)
{
    return __tos_syscall(n, a, 0, 0);
}

static inline int tos_sys2(int n, int a, int b)
{
    return __tos_syscall(n, a, b, 0);
}

static inline int tos_sys3(int n, int a, int b, int c)
{
    return __tos_syscall(n, a, b, c);
}

#endif
