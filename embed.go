// Package oberth exposes repository-level assets that ship inside the Oberth
// binaries. Embedding keeps exactly one canonical copy of each asset: the
// repository file is the source, and the digest-pinned, signed container image
// carries the same bytes, so an administrator can extract them over a channel
// whose integrity is already proven by image signature verification.
package oberth

import _ "embed"

// SetupSecretStoreScript is the canonical scripts/setup-secretstore.sh: the
// one command an administrator runs NEXT TO their OpenBao (or Vault) to make
// the store trust Oberth's ServiceAccount. It is embedded so
// `oberth secretstore setup --print-script` can hand the administrator the
// exact reviewed bytes from the signed image, instead of asking them to trust
// a separate download channel.
//
// The Oberth server binary deliberately has no code path that accepts a
// secret-store admin token: store-side configuration always runs where the
// administrator's own CLI session already holds that authority.
//
//go:embed scripts/setup-secretstore.sh
var SetupSecretStoreScript []byte
