package service

import (
	"strings"

	"github.com/oberthci/oberth/internal/gitcache"
)

// upstreamConfigurationFailure reports a failure that the forge's own
// configuration causes; retrying unchanged repeats it.
func upstreamConfigurationFailure(message string) bool {
	return strings.Contains(message, gitcache.ErrUpstreamDefaultBranch.Error())
}
