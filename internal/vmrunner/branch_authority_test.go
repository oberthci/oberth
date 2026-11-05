package vmrunner

import (
	"errors"
	"strings"
	"testing"
)

func TestBranchAuthorityRequiresNativeKnownAbsence(t *testing.T) {
	repo, sha, baseline := "upstream/org/repo", strings.Repeat("a", 40), strings.Repeat("b", 40)
	proof, err := ResolveBranchAuthority(t.Context(), policyReader{baselineSHA: baseline}, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	b, d := proof.Identity()
	if b != baseline || !proof.Matches(repo, sha, b, d) || proof.Matches(repo+"-other", sha, b, d) || proof.Matches(repo, sha+"a", b, d) || proof.Matches(repo, sha, b, strings.Repeat("0", 64)) {
		t.Fatal("authority did not bind native identity")
	}
	contract := []byte("version: 1\nprofile: ebpf-offline-amd64-v1\nartifact: ebpf/secret_monitor.o\n")
	for name, reader := range map[string]policyReader{
		"unknown default": {}, "unreadable": {err: errors.New("unavailable")},
		"candidate declaration":     {baselineSHA: baseline, candidate: contract, candidateExists: true},
		"default declaration":       {baselineSHA: baseline, baseline: contract, baselineExists: true},
		"malformed candidate":       {baselineSHA: baseline, candidate: []byte("bad"), candidateExists: true},
		"content hidden as absence": {baselineSHA: baseline, baseline: contract},
	} {
		t.Run(name, func(t *testing.T) {
			if proof, err := ResolveBranchAuthority(t.Context(), reader, repo, sha); err == nil || proof != nil {
				t.Fatal("uncertain/protected policy produced branch authority")
			}
		})
	}
}
