package workflowpolicy

import (
	_ "embed"
	"regexp"
	"strings"
)

//go:embed release-version.pattern
var releaseVersionPattern string

var releaseVersion = regexp.MustCompile(strings.TrimSpace(releaseVersionPattern))

// IsReleaseVersion accepts the stable version syntax shared with publication scripts.
func IsReleaseVersion(version string) bool { return releaseVersion.MatchString(version) }
