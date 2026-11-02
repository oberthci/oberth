package sshserver

type IdentityResolverFunc func(string) (string, bool, error)

func (f IdentityResolverFunc) ResolveFingerprint(fingerprint string) (string, bool, error) {
	return f(fingerprint)
}
