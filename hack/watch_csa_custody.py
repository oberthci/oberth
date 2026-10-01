"""Reviewed-source candidate; imports launch nothing.

Root supervisor with per-child unused host UID/GID and an exec-stop barrier.
exec-reset dumpability is explicit. No sysctl or user-account modification.
"""
from pathlib import Path
import ctypes
import grp
import hashlib
import json
import os
import pwd
import re
import resource
import secrets
import signal
import stat
import time

LIBC = ctypes.CDLL(None, use_errno=True)
LIBC.ptrace.restype = ctypes.c_long
PTRACE_TRACEME, PTRACE_DETACH, PTRACE_SETOPTIONS = 0, 17, 0x4200
PTRACE_O_EXITKILL = 0x00100000
REGISTRY = Path('/run/oberth-local-fixture-uid-reservations')


def refuse():
    raise RuntimeError('fixture custody verification failed')


def digest(path):
    with Path(path).open('rb') as file:
        return hashlib.file_digest(file, 'sha256').hexdigest()


def prctl(option, value):
    if LIBC.prctl(option, value, 0, 0, 0):
        refuse()


def ptrace(request, pid, data=0):
    if LIBC.ptrace(request, pid, ctypes.c_void_p(0), ctypes.c_void_p(data)) == -1:
        refuse()


def core_guard():
    value = Path('/proc/sys/kernel/core_pattern').read_text().strip()
    if not value or value.startswith('|'):
        refuse()
    return value


def guard():
    if os.getuid() != 0 or os.geteuid() != 0:
        refuse()
    if Path('/proc/self/uid_map').read_text().split() != ['0', '0', '4294967295']:
        refuse()
    if Path('/proc/self/gid_map').read_text().split() != ['0', '0', '4294967295']:
        refuse()
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    prctl(4, 0)   # Parent non-dumpable; never claim this survives child exec.
    prctl(36, 1)  # Adopt and positively join orphaned owned descendants.
    core_guard()
    for line in Path('/proc/swaps').read_text().splitlines()[1:]:
        name = line.split()[0]
        if not re.fullmatch(r'/dev/zram[0-9]+', name) or Path('/sys/block', name[5:], 'backing_dev').read_text().strip() != 'none':
            refuse()
    os.umask(0o077)


def reaper_guard():
    # The fallback kill is safe only while the direct child cannot have been
    # implicitly reaped. No thread or external SIGCHLD handler may race waitpid.
    if signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
        refuse()
    if len(list(Path('/proc/self/task').iterdir())) != 1:
        refuse()


def ram_guard(path, mode, uid):
    path = Path(path)
    if not path.is_absolute() or path.resolve(strict=True) != path:
        refuse()
    for item in [path, *path.parents]:
        if stat.S_ISLNK(item.lstat().st_mode):
            refuse()
    info = path.stat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != mode:
        refuse()
    # Linux statfs f_type is the first native long. Use a generously sized
    # aligned buffer and check tmpfs magic, not merely the /dev/shm name.
    fields = (ctypes.c_long * 32)()
    if LIBC.statfs(os.fsencode(path), ctypes.byref(fields)) or fields[0] != 0x01021994:
        refuse()
    if info.st_dev != Path('/dev/shm').stat().st_dev:
        refuse()


def status(pid):
    return dict(line.split(':', 1) for line in Path('/proc', str(pid), 'status').read_text().splitlines())


def ticks(pid):
    return int(Path('/proc', str(pid), 'stat').read_text().rsplit(')', 1)[1].split()[19])


def census():
    result = {}
    for item in Path('/proc').iterdir():
        if not item.name.isdigit():
            continue
        try:
            pid = int(item.name)
            row = status(pid)
            result[pid] = {'ticks': ticks(pid), 'parent': int(row['PPid']),
                           'uids': [int(v) for v in row['Uid'].split()],
                           'gids': [int(v) for v in row['Gid'].split()],
                           'groups': [int(v) for v in row['Groups'].split()]}
        except (FileNotFoundError, ProcessLookupError):
            continue
    return result


def delegated(number):
    for path in [Path('/etc/subuid'), Path('/etc/subgid')]:
        if not path.exists():
            continue
        for line in path.read_text().splitlines():
            if not line.strip() or line.startswith('#'):
                continue
            parts = line.split(':')
            if len(parts) != 3 or not parts[1].isdigit() or not parts[2].isdigit():
                refuse()
            start, length = int(parts[1]), int(parts[2])
            if start <= number < start + length:
                return True
    return False


def same_identity_used(number, rows):
    return any(number in row['uids'] + row['gids'] + row['groups'] for row in rows.values())


def account_identity_used(number):
    for lookup in [pwd.getpwuid, grp.getgrgid]:
        try:
            lookup(number)
        except KeyError:
            pass
        else:
            return True
    return delegated(number)


def ram_identity_used(number):
    # An earlier failed fixture may have no remaining process but still own
    # private RAM. Inspect ownership only, without reading any file content.
    pending, seen = [Path('/dev/shm')], 0
    while pending:
        path = pending.pop()
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        seen += 1
        if seen > 16384:
            refuse()
        if number in [info.st_uid, info.st_gid]:
            return True
        if stat.S_ISDIR(info.st_mode):
            pending.extend(path.iterdir())  # Never follow a symlink.
    return False


class Owner:
    def __init__(self, ram_root):
        guard()
        reaper_guard()
        self.ram = Path(ram_root)
        # Traversable root contains separately owned, private0700 child homes.
        ram_guard(self.ram, 0o711, 0)
        try:
            REGISTRY.mkdir(mode=0o700)
        except FileExistsError:
            pass
        if REGISTRY.resolve(strict=True) != REGISTRY:
            refuse()
        info = REGISTRY.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o700:
            refuse()
        self.reservations, self.children = [], []
        self.failed = False
        self.cap_last = int(Path('/proc/sys/kernel/cap_last_cap').read_text())
        self.initial_core = core_guard()
        self.pid, self.start = os.getpid(), ticks(os.getpid())

    def reserve(self, label):
        if not re.fullmatch(r'[a-z][a-z0-9-]{0,40}', label):
            refuse()
        ram_guard(self.ram, 0o711, 0)
        # Shared atomic reservation domain outlives failed/restarted launchers.
        # Never remove/reclaim a marker here. Stale cleanup requires an
        # independent operation that proves all prior processes and RAM gone.
        for _ in range(128):
            number = 1000000000 + secrets.randbelow(1000000000)
            if account_identity_used(number) or same_identity_used(number, census()) or ram_identity_used(number):
                continue
            marker = REGISTRY / str(number)
            try:
                marker.mkdir(mode=0o700)
            except FileExistsError:
                continue
            home = self.ram / label
            reservation = {'uid': number, 'label': label, 'home': home, 'marker': marker, 'consumed': False}
            # A marker is never reclaimed after a partially completed reserve.
            # Register it before any fallible metadata/home preparation.
            self.reservations.append(reservation)
            with (marker / 'owner.json').open('x') as file:
                json.dump({'owner_pid': self.pid, 'owner_start_ticks': self.start, 'label': label, 'ram_root': str(self.ram)}, file)
                file.write('\n'); file.flush(); os.fsync(file.fileno())
            home.mkdir(mode=0o700, exist_ok=False)
            os.chown(home, number, number)
            ram_guard(home, 0o700, number)
            return reservation
        refuse()

    def launch(self, reservation, executable, expected_sha, argv, stdin_fd=None):
        if reservation not in self.reservations or reservation['consumed']:
            refuse()
        reaper_guard()  # Python post-fork setup is only valid without threads.
        uid, home = reservation['uid'], reservation['home']
        ram_guard(home, 0o700, uid)
        if account_identity_used(uid) or same_identity_used(uid, census()):
            refuse()
        if digest(executable) != expected_sha or core_guard() != self.initial_core:
            refuse()
        reservation['consumed'] = True
        output_r, output_w = os.pipe2(os.O_CLOEXEC)
        null = None
        try:
            null = os.open('/dev/null', os.O_RDWR | os.O_CLOEXEC)
            os.set_blocking(output_r, False)
        except BaseException:
            os.close(output_r); os.close(output_w)
            if null is not None:
                os.close(null)
            raise RuntimeError('fixture launch preparation failed') from None
        environment = {'PATH': '/usr/bin:/bin', 'HOME': str(home), 'TMPDIR': str(home),
                       'XDG_CONFIG_HOME': str(home), 'XDG_CACHE_HOME': str(home),
                       'LANG': 'C', 'LC_ALL': 'C', 'GOMAXPROCS': '2'}
        child = {'pid': None, 'pidfd': None, 'uid': uid, 'home': home,
                 'label': reservation['label'], 'output_fd': output_r,
                 'joined': False, 'leader_reaped': False, 'traced': True,
                 'released': False, 'exit': None, 'captured_bytes': 0, 'owned': {}}
        # Install ownership before fork; every parent-side action after a
        # successful fork belongs to the guarded cleanup path below.
        self.children.append(child)
        try:
            pid = os.fork()
        except BaseException:
            os.close(output_r); os.close(output_w); os.close(null)
            child['joined'] = True
            raise RuntimeError('fixture fork failed') from None
        if pid == 0:
            try:
                os.setsid()
                resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
                for capability in range(self.cap_last + 1):
                    prctl(24, capability)
                os.setgroups([])
                os.setresgid(uid, uid, uid)
                os.setresuid(uid, uid, uid)
                prctl(38, 1)  # no_new_privs
                prctl(1, signal.SIGKILL)  # Set after credentials; close race.
                if os.getppid() != self.pid:
                    os._exit(121)
                os.chdir(home)
                os.dup2(null if stdin_fd is None else stdin_fd, 0)
                os.dup2(output_w, 1); os.dup2(output_w, 2)
                os.closerange(3, int(os.sysconf('SC_OPEN_MAX')))
                ptrace(PTRACE_TRACEME, 0)
                os.execve(str(executable), [str(executable), *argv], environment)
            except BaseException:
                os._exit(121)
        child['pid'] = pid
        try:
            os.close(output_w); os.close(null)
            child['pidfd'] = os.pidfd_open(pid)
            child['start_ticks'] = ticks(pid)
            child['owned'][pid] = child['start_ticks']
            deadline = time.monotonic() + 10
            while True:
                found, state = os.waitpid(pid, os.WNOHANG | os.WUNTRACED)
                if found:
                    break
                if time.monotonic() >= deadline:
                    refuse()
                time.sleep(0.01)
            if not os.WIFSTOPPED(state) or os.WSTOPSIG(state) != signal.SIGTRAP:
                if os.WIFEXITED(state) or os.WIFSIGNALED(state):
                    child['exit'], child['leader_reaped'] = os.waitstatus_to_exitcode(state), True
                refuse()
            ptrace(PTRACE_SETOPTIONS, pid, PTRACE_O_EXITKILL)
            actual = status(pid)
            if int(actual['TracerPid']) != self.pid or int(actual['PPid']) != self.pid or ticks(pid) != child['start_ticks']:
                refuse()
            if actual['Uid'].split() != [str(uid)] * 4 or actual['Gid'].split() != [str(uid)] * 4 or actual['Groups'].split():
                refuse()
            if account_identity_used(uid):
                refuse()
            if any(int(actual[field], 16) != 0 for field in ['CapInh', 'CapPrm', 'CapEff', 'CapBnd', 'CapAmb']):
                refuse()
            if int(actual['NoNewPrivs']) != 1 or os.getpgid(pid) != pid:
                refuse()
            core_line = next(line for line in Path('/proc', str(pid), 'limits').read_text().splitlines() if line.startswith('Max core file size'))
            if core_line.split()[4:6] != ['0', '0']:
                refuse()
            if digest(Path('/proc', str(pid), 'exe')) != expected_sha or core_guard() != self.initial_core:
                refuse()
            child['post_exec'] = {'uids': [uid] * 4, 'gids': [uid] * 4, 'groups': [],
                                  'capabilities': 'all zero including bounding', 'no_new_privs': True,
                                  'core_soft_hard': [0, 0], 'core_collector_piped': False,
                                  'executable_sha256': expected_sha,
                                  'dumpability': 'exec reset; containment is reserved distinct host UID, not inherited dumpable=0',
                                  'proc_directory_uid': Path('/proc', str(pid)).stat().st_uid}
            ptrace(PTRACE_DETACH, pid)
            child['traced'], child['released'] = False, True
            return child
        except BaseException:
            self.finish(child, force=True)
            raise RuntimeError('fixture exec-stop custody failed') from None

    def discover(self, child):
        rows = census()
        # Track the unreaped direct child even when it failed between gid/uid
        # setup or before pidfd/start acquisition. A direct child with default
        # SIGCHLD cannot have its PID reused until this supervisor waits it.
        pid = child['pid']
        if not child['leader_reaped'] and pid in rows:
            row = rows[pid]
            if row['parent'] != self.pid or ('start_ticks' in child and row['ticks'] != child['start_ticks']):
                self.failed = True
                refuse()
            child['owned'][pid] = row['ticks']
        # Link by a live PID/start parent, or by actual adoption by this
        # subreaper. UID alone is never enough to gain signal authority.
        changed = True
        while changed:
            changed = False
            for pid, row in rows.items():
                if child['uid'] not in row['uids'] or pid in child['owned']:
                    continue
                parent = row['parent']
                linked = parent in child['owned'] and parent in rows and rows[parent]['ticks'] == child['owned'][parent]
                adopted = parent == self.pid and ticks(self.pid) == self.start
                if linked or adopted:
                    child['owned'][pid] = row['ticks']
                    changed = True
        # Unexpected peer ownership is a failure, not a target to kill.
        for pid, row in rows.items():
            if child['uid'] in row['uids'] + row['gids'] + row['groups'] and (pid not in child['owned'] or row['ticks'] != child['owned'][pid]):
                # Continue joining known children without gaining signal
                # authority over this unexpected peer. Retain RAM/reservation
                # and fail qualification if the collision does not disappear.
                self.failed = True
        return rows

    def poll(self, child):
        reaper_guard()
        if child['leader_reaped']:
            return child['exit']
        found, state = os.waitpid(child['pid'], os.WNOHANG)
        if found:
            if not (os.WIFEXITED(state) or os.WIFSIGNALED(state)):
                refuse()
            child['exit'], child['leader_reaped'] = os.waitstatus_to_exitcode(state), True
        return child['exit']

    def read_available(self, child, budget=1048576):
        data = self.drain(child)
        if child['captured_bytes'] > budget:
            self.failed = True
            data[:] = b'\0' * len(data)
            refuse()
        return data

    def drain(self, child):
        # Every call is bounded even if a daemon continuously writes. Cleanup
        # uses the same discard path without recursively invoking finish.
        data = bytearray()
        for _ in range(64):
            try:
                chunk = os.read(child['output_fd'], 4096)
            except BlockingIOError:
                break
            if not chunk:
                break
            child['captured_bytes'] += len(chunk)
            if child['captured_bytes'] > 1048576:
                self.failed = True
            data.extend(chunk)
        return data

    def finish(self, child, force=False):
        if child['joined']:
            return
        reaper_guard()
        # Unreaped direct-child PID cannot be reused. This fallback exists for
        # failure before pidfd/start acquisition, with no unowned UID signaling.
        def signal_leader(sig):
            if child['leader_reaped']:
                return
            try:
                if child['pidfd'] is not None:
                    signal.pidfd_send_signal(child['pidfd'], sig)
                else:
                    os.kill(child['pid'], sig)
            except ProcessLookupError:
                pass
        def signal_owned(sig):
            self.discover(child)
            for pid, observed_ticks in list(child['owned'].items()):
                try:
                    fd = os.pidfd_open(pid)
                    try:
                        if ticks(pid) == observed_ticks:
                            signal.pidfd_send_signal(fd, sig)
                    finally:
                        os.close(fd)
                except (ProcessLookupError, FileNotFoundError):
                    continue
        forced = force or child['traced']
        first = signal.SIGKILL if forced else signal.SIGTERM
        signal_leader(first)
        signal_owned(first)
        deadline = time.monotonic() + (5 if forced else 15)
        while True:
            data = self.drain(child)
            data[:] = b'\0' * len(data)
            rows = self.discover(child)
            owned = set(child['owned'])
            if not child['leader_reaped']:
                owned.add(child['pid'])
            for pid in owned:
                try:
                    found, state = os.waitpid(pid, os.WNOHANG)
                    if found == child['pid'] and (os.WIFEXITED(state) or os.WIFSIGNALED(state)):
                        child['exit'], child['leader_reaped'] = os.waitstatus_to_exitcode(state), True
                except ChildProcessError:
                    pass
            if child['leader_reaped'] and not same_identity_used(child['uid'], census()):
                break
            if time.monotonic() >= deadline:
                if forced:
                    refuse()
                forced = True
                signal_leader(signal.SIGKILL); signal_owned(signal.SIGKILL)
                deadline = time.monotonic() + 5
            time.sleep(0.01)
        child['joined'], child['forced_cleanup'] = True, forced
        os.close(child['output_fd'])
        if child['pidfd'] is not None:
            os.close(child['pidfd'])

    def close(self):
        failed = False
        for child in reversed(self.children):
            try:
                self.finish(child)
            except BaseException:
                failed = True
        if failed or self.failed or any(child.get('forced_cleanup') or not child['joined'] for child in self.children):
            refuse()  # Forced teardown is preserved as failed qualification.

    def receipt(self):
        return [{key: value for key, value in child.items()
                 if key in {'pid', 'start_ticks', 'uid', 'label', 'joined', 'exit', 'released', 'post_exec', 'forced_cleanup', 'captured_bytes'}}
                for child in self.children]
