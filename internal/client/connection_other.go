//go:build !unix

package client

// trustedConfigFile has no ownership model to check outside Unix.
func trustedConfigFile(string) error { return nil }
