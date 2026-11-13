"""Exec a regression binary under a kernel policy refusing group signals.

Only negative-PID kill is refused. This reproduces confinement independently of
the host runtime's default profile and does not require any capabilities.
"""
import ctypes
import errno
import os
import platform
import sys


class Filter(ctypes.Structure):
    _fields_ = [("code", ctypes.c_ushort), ("jt", ctypes.c_ubyte),
                ("jf", ctypes.c_ubyte), ("k", ctypes.c_uint)]


class Program(ctypes.Structure):
    _fields_ = [("length", ctypes.c_ushort), ("filter", ctypes.POINTER(Filter))]


number = {"x86_64": 62, "aarch64": 129}.get(platform.machine())
if number is None:
    raise SystemExit("group-signal fixture supports Linux amd64 and arm64")
# seccomp_data.nr then args[0]'s low word; negative PID means group signal.
refused = int(sys.argv[1])
if refused not in (errno.EACCES, errno.EPERM):
    raise SystemExit("expected a group-signal permission error")
filters = (Filter * 6)(
    Filter(0x20, 0, 0, 0),
    Filter(0x15, 0, 3, number),
    Filter(0x20, 0, 0, 16),
    Filter(0x45, 0, 1, 0x80000000),
    Filter(0x06, 0, 0, 0x00050000 | refused),
    Filter(0x06, 0, 0, 0x7fff0000),
)
program = Program(len(filters), filters)
libc = ctypes.CDLL(None, use_errno=True)
libc.prctl.restype = ctypes.c_int
if libc.prctl(38, 1, 0, 0, 0) != 0:  # PR_SET_NO_NEW_PRIVS
    raise OSError(ctypes.get_errno(), "no_new_privs")
if libc.prctl(22, 2, ctypes.byref(program), 0, 0) != 0:  # PR_SET_SECCOMP, FILTER
    raise OSError(ctypes.get_errno(), "seccomp group-signal fixture")
os.execv(sys.argv[2], sys.argv[2:])
