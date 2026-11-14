package model

import "regexp"

var releaseVersionTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+` + runRefCharacterClass + `*$`)

// IsReleaseVersionTag reserves version tags, including suffixes in the run-fact alphabet, for
// release admission. Ordinary tag CI cannot choose this trust domain.
func IsReleaseVersionTag(name string) bool { return releaseVersionTag.MatchString(name) }
