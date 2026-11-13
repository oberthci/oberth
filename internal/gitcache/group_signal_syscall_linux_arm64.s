#include "textflag.h"

// prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, program, 0, 0).
TEXT ·installGroupSignalFilter(SB),NOSPLIT,$0-16
    MOVD $167, R8
    MOVD $22, R0
    MOVD $2, R1
    MOVD program+0(FP), R2
    MOVD ZR, R3
    MOVD ZR, R4
    SVC $0
    NEG R0, R0
    MOVD R0, ret+8(FP)
    RET
