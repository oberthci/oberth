//go:build !linux

package main

// The minimal server image is Linux. Other CLI platforms retain ordinary
// stdin/session lifetime handling and the caller's command cancellation.
func bindGitSSHParent() (func(), error) { return func() {}, nil }
