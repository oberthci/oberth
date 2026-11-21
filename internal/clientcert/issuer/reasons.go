package issuer

import "github.com/oberthci/oberth/internal/publicerr"

type problem int

const (
	problemDependencies problem = iota
	problemKeyPair
	problemAuthority
	problemIdentity
	problemLifetime
	problemCoverage
	problemSigning
)

type diagnostic struct {
	code           publicerr.Code
	reason, remedy string
}

func diagnostics() []diagnostic {
	return []diagnostic{
		{publicerr.InvalidInput, "Authority dependencies are missing.", "Supply entropy and a clock."},
		{publicerr.InvalidInput, "The authority key pair is invalid.", "Supply a matching authority certificate and key."},
		{publicerr.InvalidInput, "The authority certificate is invalid.", "Supply a self-signed client authority certificate."},
		{publicerr.InvalidInput, "The client identity is invalid.", "Supply a nonempty identity of at most 256 bytes without control characters."},
		{publicerr.InvalidInput, "The certificate lifetime is invalid.", "Use a lifetime between one hour and 365 days."},
		{publicerr.InvalidInput, "The authority does not cover the requested lifetime.", "Renew the authority or shorten the client certificate lifetime."},
		{publicerr.Unavailable, "Certificate signing is unavailable.", "Check the authority entropy source and retry."},
	}
}
func refusal(p problem, cause error) error {
	d := diagnostics()[p]
	return publicerr.New(d.code, d.reason, d.remedy, cause)
}
