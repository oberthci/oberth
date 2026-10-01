package gitcache

import "testing"

func TestValidateUpstreamNameRejectsReservedNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"release", "data", "upstream", "sys", "Release", "DATA",
		"metadata", "Metadata", "receive-outbox", "Receive-Outbox",
	} {
		if err := ValidateUpstreamName(name); err == nil {
			t.Fatalf("ValidateUpstreamName(%q) should reject reserved name", name)
		}
	}
}

func TestValidateUpstreamNameRejectsGitSuffix(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"repo.git", "upstream.GIT", "my-forge.Git"} {
		if err := ValidateUpstreamName(name); err == nil {
			t.Fatalf("ValidateUpstreamName(%q) should reject .git suffix", name)
		}
	}
}

func TestValidateUpstreamNameAcceptsValidNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"codeberg", "github", "gitlab", "my-forge", "forge1"} {
		if err := ValidateUpstreamName(name); err != nil {
			t.Fatalf("ValidateUpstreamName(%q) = %v", name, err)
		}
	}
}

func TestValidateUpstreamNameRejectsInvalidCharset(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"-bad", "..", "", "a/b", "bad name"} {
		if err := ValidateUpstreamName(name); err == nil {
			t.Fatalf("ValidateUpstreamName(%q) should reject invalid charset", name)
		}
	}
}

func TestParseRepoPathThreeSegments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input        string
		wantUpstream string
		wantOrg      string
		wantRepo     string
	}{
		{"codeberg/acme/terraform", "codeberg", "acme", "terraform"},
		{"github/oberthci/oberth", "github", "oberthci", "oberth"},
		{"/forge/org/repo.git", "forge", "org", "repo"},
	}
	for _, test := range tests {
		upstream, org, repo, err := ParseRepoPath(test.input)
		if err != nil {
			t.Fatalf("ParseRepoPath(%q): %v", test.input, err)
		}
		if upstream != test.wantUpstream || org != test.wantOrg || repo != test.wantRepo {
			t.Fatalf("ParseRepoPath(%q) = (%q, %q, %q), want (%q, %q, %q)",
				test.input, upstream, org, repo, test.wantUpstream, test.wantOrg, test.wantRepo)
		}
	}
}

func TestParseRepoPathRejectsFourSegments(t *testing.T) {
	t.Parallel()
	if _, _, _, err := ParseRepoPath("a/b/c/d"); err == nil {
		t.Fatal("ParseRepoPath with 4 segments should fail")
	}
}

func TestParseRepoPathRejectsInvalidSegments(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"-bad/org/repo",    // invalid upstream
		"up/-bad/repo",     // invalid org
		"up/org/-bad",      // invalid repo
		"up/../secret",     // traversal in 2-segment (would be caught)
		"../org/repo",      // traversal as upstream
		"up/org/../../etc", // too many segments + traversal
	} {
		if _, _, _, err := ParseRepoPath(value); err == nil {
			t.Fatalf("ParseRepoPath(%q) should fail", value)
		}
	}
}
