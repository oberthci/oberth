package releaseimage

import (
	"archive/tar"
	"fmt"
	"strings"
)

// The substrate is data plus two static executables and one exact alias. An
// allowlist also rejects unexpected interpreters, package metadata and tools
// under names not known to a denylist.
func validateRuntimeEntry(header *tar.Header, name string) error {
	if (header.Typeflag != tar.TypeSymlink && header.Mode&0o7022 != 0) || header.Uid != 0 || header.Gid != 0 {
		return fmt.Errorf("unsafe runtime ownership or mode at %s", name)
	}
	switch name {
	case "usr/local/bin/oberth-git-ssh":
		if header.Typeflag != tar.TypeSymlink || header.Linkname != "oberth" || header.Size != 0 {
			return fmt.Errorf("invalid native SSH alias")
		}
	case "usr/local/bin/oberth", "usr/bin/git":
		if !regularTarType(header.Typeflag) || header.Size <= 0 || header.Mode&0o111 == 0 {
			return fmt.Errorf("invalid runtime executable %s", name)
		}
	case "etc/ssl/certs/ca-certificates.crt":
		if !regularTarType(header.Typeflag) || header.Size <= 0 || header.Mode&0o111 != 0 {
			return fmt.Errorf("invalid runtime CA bundle")
		}
	default:
		allowedDirectory := name == "data" || name == "tmp"
		for _, file := range []string{"usr/local/bin/oberth", "usr/bin/git", "etc/ssl/certs/ca-certificates.crt"} {
			allowedDirectory = allowedDirectory || strings.HasPrefix(file, name+"/")
		}
		if !allowedDirectory || header.Typeflag != tar.TypeDir {
			return fmt.Errorf("unexpected runtime payload %s", name)
		}
	}
	return nil
}
