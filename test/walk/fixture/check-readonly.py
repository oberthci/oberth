import os
from pathlib import Path

def verify(root):
    mount_readonly = bool(os.statvfs(root).f_flag & os.ST_RDONLY)
    for path, mode in (
        (root / ".walk-write", "x"),
        (root / ".git" / ".walk-write", "x"),
        (root / "source-marker", "a"),
    ):
        try:
            with path.open(mode) as output:
                output.write("unexpected write")
        except OSError as error:
            if error.errno not in (13, 30):
                raise AssertionError(
                    "write failed for a reason other than denied source access: " + str(path)
                ) from error
        else:
            raise AssertionError("source accepted a write: " + str(path))
    try:
        (root / "source-marker").chmod(0o600)
    except OSError as error:
        if error.errno not in (1, 13, 30):
            raise AssertionError("chmod failed for an unrelated reason: " + str(root / "source-marker")) from error
    else:
        raise AssertionError("consumer can change source permissions")
    return mount_readonly


if __name__ == "__main__":
    if os.geteuid() != 0:
        raise AssertionError("probe requires the image default root user")
    mount_readonly = verify(Path("/work/src"))
    print("walk: source read-only")
    print("walk: source mount_readonly=" + str(mount_readonly).lower())
