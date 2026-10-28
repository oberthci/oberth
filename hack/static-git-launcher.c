#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/landlock.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/prctl.h>
#include <sys/statvfs.h>
#include <sys/syscall.h>
#include <unistd.h>

/* CI-only, single-threaded launcher. The verified Oberth nonroot profile owns
 * UID/capabilities/read-only mounts. Landlock permits execution only of the
 * static candidate and test payload, including all their future children.
 * No namespace/mount/chroot capability or privileged container is needed. */
static int refuse(const char *reason) {
    fprintf(stderr, "static Git qualification refused: %s\n", reason);
    return 1;
}

static int allow_path(int rules, const char *path, unsigned long long access) {
    int file = open(path, O_PATH | O_CLOEXEC);
    if (file < 0) return -1;
    struct landlock_path_beneath_attr rule = {.allowed_access = access, .parent_fd = file};
    int result = syscall(SYS_landlock_add_rule, rules, LANDLOCK_RULE_PATH_BENEATH, &rule, 0);
    close(file);
    return result;
}

static int readonly_path(const char *path) {
    struct statvfs filesystem;
    return statvfs(path, &filesystem) == 0 && (filesystem.f_flag & ST_RDONLY);
}

int main(int argc, char **argv) {
    if (argc != 1) return refuse("launcher takes no arguments");
    (void)argv;
    if (getuid() != 65534 || geteuid() != 65534 || getgid() != 65534 || getegid() != 65534)
        return refuse("requires real UID/GID 65534");
    if (!readonly_path("/") || !readonly_path("/qualification") ||
        !readonly_path("/usr/bin/git") || !readonly_path("/usr/local/bin"))
        return refuse("root and candidate inputs must be read-only");
    if (setenv("PATH", "/usr/local/bin:/qualification/rootfs/usr/bin", 1) ||
        setenv("GIT_SSH", "/usr/local/bin/oberth-git-ssh", 1) ||
        setenv("GIT_SSH_VARIANT", "simple", 1) || unsetenv("GIT_SSH_COMMAND"))
        return refuse("cannot close the execution environment");

    struct landlock_ruleset_attr ruleset = {
        .handled_access_fs = LANDLOCK_ACCESS_FS_EXECUTE | LANDLOCK_ACCESS_FS_REFER |
                             LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_READ_DIR
    };
    int rules = syscall(SYS_landlock_create_ruleset, &ruleset, sizeof(ruleset), 0);
    if (rules < 0) return refuse("Landlock execution restriction unavailable");
    const char *allowed[] = {"/qualification/gitcache.test", "/usr/bin/git",
                            "/qualification/rootfs/usr/bin/git", "/usr/local/bin/oberth"};
    for (unsigned int i = 0; i < sizeof(allowed) / sizeof(allowed[0]); i++) {
        if (allow_path(rules, allowed[i], LANDLOCK_ACCESS_FS_EXECUTE | LANDLOCK_ACCESS_FS_READ_FILE)) {
            close(rules); return refuse("cannot bind candidate executable");
        }
    }
    /* The only readable trees are the scratch-like payload, fixtures, private
     * temporary files and the same proc/dev interfaces as a scratch container.
     * In particular the runner's Git templates and interpreter files are hidden.
     * ABI2 REFER allows Git's quarantine/object moves inside /tmp, without
     * allowing execution there. Unsupported kernels refuse, never fall back. */
    const char *readable[] = {"/qualification", "/usr/local/bin", "/proc", "/dev"};
    for (unsigned int i = 0; i < sizeof(readable) / sizeof(readable[0]); i++) {
        if (allow_path(rules, readable[i], LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_READ_DIR)) {
            close(rules); return refuse("cannot bind readable payload");
        }
    }
    if (allow_path(rules, "/tmp", LANDLOCK_ACCESS_FS_READ_FILE | LANDLOCK_ACCESS_FS_READ_DIR | LANDLOCK_ACCESS_FS_REFER) ||
        allow_path(rules, "/etc/ssl/certs/ca-certificates.crt", LANDLOCK_ACCESS_FS_READ_FILE)) {
        close(rules); return refuse("cannot bind temporary files or CA data");
    }
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) || syscall(SYS_landlock_restrict_self, rules, 0)) {
        close(rules);
        return refuse("cannot enforce execution restriction");
    }
    close(rules);
    char *const arguments[] = {"/qualification/gitcache.test", "-test.v", "-test.timeout=15m",
                               "-test.parallel=2", NULL};
    execv(arguments[0], arguments);
    return refuse("cannot execute the complete qualification suite");
}
