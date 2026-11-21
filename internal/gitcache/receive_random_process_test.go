//go:build linux

// These tests exercise the Linux server and Git receive path.

package gitcache_test

// Each stopped server has its own reaper. Orphaned hooks must finish and be
// reaped before the scenario advances; they never accumulate in the test
// process or compete with another command's Wait. Preserve the server's exit
// signal so the caller can distinguish an injected stop from a failed test.
const rsReaper = `import ctypes, os, subprocess, sys
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(36, 1, 0, 0, 0) != 0:
    raise OSError(ctypes.get_errno(), 'set child subreaper')
server = subprocess.Popen(sys.argv[1:])
result = server.wait()
while True:
    try:
        os.wait()
    except ChildProcessError:
        break
if result < 0:
    os.kill(os.getpid(), -result)
sys.exit(result)
`
