#include "textflag.h"

// prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, program, 0, 0).
TEXT ·installGroupSignalFilter(SB),NOSPLIT,$0-16
    MOVQ $157, AX
    MOVQ $22, DI
    MOVQ $2, SI
    MOVQ program+0(FP), DX
    XORQ R10, R10
    XORQ R8, R8
    SYSCALL
    NEGQ AX
    MOVQ AX, ret+8(FP)
    RET
