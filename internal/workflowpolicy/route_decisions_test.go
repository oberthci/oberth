package workflowpolicy

import (
	"fmt"
	"strings"
	"testing"
)

func TestReleaseRoutePatternByteLimitIsInclusive(t *testing.T) {
	for _, size := range []int{255, 256, 257} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			pattern := strings.Repeat("v", size)
			route := ReleaseRoute{Pattern: pattern, File: ReleaseFile}
			routes := ReleaseRoutes{"forge/org/repo": {route}}
			err := routes.Validate()
			if size == 257 {
				want := fmt.Sprintf("pattern: requires a bounded bare-tag glob. Configure bounded, distinct tag patterns that select one release file. Repository %q pattern %q file %q.", "forge/org/repo", pattern, ReleaseFile)
				if err == nil || err.Error() != want {
					t.Fatalf("Validate() = %v, want %s", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := routes.Resolve("forge/org/repo", pattern); err != nil || got != route {
				t.Fatalf("Resolve() = %+v, %v; want %+v", got, err, route)
			}
		})
	}
}

func TestReleaseRouteRefusalsFollowRepositoryOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routes ReleaseRoutes
		want   string
	}{
		{
			name:   "repository names",
			routes: ReleaseRoutes{"z": {{Pattern: "v*", File: ReleaseFile}}, "a": {{Pattern: "v*", File: ReleaseFile}}},
			want:   `repo: Repository name is not canonical. Use the registered upstream/org/repository name. Repository: "a".`,
		},
		{
			name:   "repository routes",
			routes: ReleaseRoutes{"forge/org/z": {}, "forge/org/a": {}},
			want:   `routes: Repository requires 1..32 routes. Configure bounded, distinct tag patterns that select one release file. Repository: "forge/org/a".`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.routes.Validate(); err == nil || err.Error() != tc.want {
				t.Fatalf("Validate() = %v, want %s", err, tc.want)
			}
		})
	}
}
