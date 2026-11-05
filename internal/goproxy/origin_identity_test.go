package goproxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestPinnedOriginPublicIdentity(t *testing.T) {
	const route = "upstream2/oberth/oberth-fixture"
	const remote = "ssh://git@codeberg.org/oberth/oberth-fixture.git"
	const public = "https://codeberg.org/oberth/oberth-fixture.git"
	for _, tc := range []struct {
		name, route, remote, want string
		refused                   bool
	}{
		{"standard", route, remote, public, false},
		{"default port", route, "ssh://git@codeberg.org:22/oberth/oberth-fixture.git", public, false},
		{"other repository", "forge/other/another", "ssh://git@codeberg.org/other/another.git", "https://codeberg.org/other/another.git", false},
		{"unknown host", route, "ssh://git@forge.example.test/oberth/oberth-fixture.git", "", false},
		{"host suffix", route, "ssh://git@codeberg.org.example.test/oberth/oberth-fixture.git", "", false},
		{"host case", route, "ssh://git@CODEBERG.ORG/oberth/oberth-fixture.git", "", false},
		{"other user", route, "ssh://deploy@codeberg.org/oberth/oberth-fixture.git", "", false},
		{"no user", route, "ssh://codeberg.org/oberth/oberth-fixture.git", "", false},
		{"other port", route, "ssh://git@codeberg.org:2222/oberth/oberth-fixture.git", "", false},
		{"empty port", route, "ssh://git@codeberg.org:/oberth/oberth-fixture.git", "", false},
		{"padded port", route, "ssh://git@codeberg.org:022/oberth/oberth-fixture.git", "", false},
		{"wrong owner", route, "ssh://git@codeberg.org/other/oberth-fixture.git", "", false},
		{"wrong repo", route, "ssh://git@codeberg.org/oberth/other.git", "", false},
		{"escaped owner", route, "ssh://git@codeberg.org/%6fberth/oberth-fixture.git", "", false},
		{"escaped separator", route, "ssh://git@codeberg.org/oberth%2foberth-fixture.git", "", false},
		{"escaped user", route, "ssh://%67it@codeberg.org/oberth/oberth-fixture.git", "", false},
		{"no git suffix", route, "ssh://git@codeberg.org/oberth/oberth-fixture", "", false},
		{"trailing slash", route, remote + "/", "", false},
		{"extra slash", route, "ssh://git@codeberg.org//oberth/oberth-fixture.git", "", false},
		{"extra segment", route, "ssh://git@codeberg.org/oberth/oberth-fixture/nested.git", "", false},
		{"bare route", "oberth-fixture", remote, "", false},
		{"owner route", "oberth/oberth-fixture", remote, "", false},
		{"leading slash route", "/" + route, remote, "", false},
		{"git suffix route", route + ".git", remote, "", false},
		{"invalid route", "forge/../oberth-fixture", remote, "", false},
		{"local fixture", route, "/tmp/oberth-fixture.git", "", false},
		{"password", route, "ssh://git:secret@codeberg.org/oberth/oberth-fixture.git", "", true},
		{"empty password", route, "ssh://git:@codeberg.org/oberth/oberth-fixture.git", "", true},
		{"https transport", route, public, "", true},
		{"query", route, remote + "?other=1", "", true},
		{"fragment", route, remote + "#other", "", true},
		{"traversal", route, "ssh://git@codeberg.org/oberth/../oberth-fixture.git", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newArtifactGit()
			h := NewHandler(g, "go.example.test", map[string]string{"issue764-fixture": tc.route})
			snapshot := gitcache.ModuleSnapshot{Commit: artifactOID, Time: time.Date(2026, 10, 1, 7, 8, 7, 0, time.UTC), Ref: "refs/tags/v1.0.0", URL: tc.remote}
			source, objects, err := h.buildPinnedVersion(t.Context(), tc.route, artifactModule, "v1.0.0", snapshot)
			if tc.refused {
				if err == nil || objects != nil || g.calls.Load() != 0 {
					t.Fatalf("unsafe transport reached metadata: %v %s", err, objects[".info"])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var info moduleInfo
			if err := json.Unmarshal(objects[".info"], &info); err != nil {
				t.Fatal(err)
			}
			wantURL := tc.want
			if wantURL == "" {
				wantURL = tc.remote
			}
			want := moduleOrigin{VCS: "git", URL: wantURL, Hash: artifactOID, Ref: snapshot.Ref}
			if source != artifactOID || info.Origin != want || !info.Time.Equal(snapshot.Time) || info.Version != "v1.0.0" || string(objects[".mod"]) != artifactMod {
				t.Fatalf("pinned identity/bytes: %+v; want %+v", info, want)
			}
		})
	}
}
