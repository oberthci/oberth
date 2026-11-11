package model

import "regexp"

var releaseVersionTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`)

// IsReleaseVersionTag reserves version tags, including every suffix, for
// release admission. Ordinary tag CI cannot choose this trust domain.
func IsReleaseVersionTag(name string) bool { return releaseVersionTag.MatchString(name) }
