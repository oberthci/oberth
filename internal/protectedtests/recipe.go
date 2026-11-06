package protectedtests

import (
	"errors"
	"net/url"
	"regexp"
)

// Preserve the existing closed harness reference grammar.
var harnessImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[0-9a-f]{64}$`)

// Recipe is administrator-owned. Hashes are an independent allowlist for
// executable suite code; tests.yaml can select, but never create, this authority.
type Recipe struct {
	SuiteSHA256  string `json:"suite_sha256"`
	GuestSHA256  string `json:"guest_sha256"`
	HarnessImage string `json:"harness_image"`
	SuiteURL     string `json:"suite_url"`
	GuestURL     string `json:"guest_url"`
}

func (r Recipe) Validate() error {
	if len(r.HarnessImage) > 512 || !sha256HexPattern.MatchString(r.SuiteSHA256) || !sha256HexPattern.MatchString(r.GuestSHA256) || !harnessImagePattern.MatchString(r.HarnessImage) {
		return errors.New("trusted recipe requires complete suite, guest and harness pins")
	}
	for _, raw := range []string{r.SuiteURL, r.GuestURL} {
		u, err := url.Parse(raw)
		if len(raw) > 2048 || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("trusted recipe requires credential-free HTTPS bundle endpoints")
		}
	}
	return nil
}
