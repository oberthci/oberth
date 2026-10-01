package argojob

import (
	"strings"
	"testing"
)

// These run the actual release.sh entrypoints against stateful external-tool
// fixtures. Any second chart/signature write is refused like an immutable
// registry; matching retries must instead verify and reuse the first object.
func TestReleaseRegistrySignaturesAreReusedOnRetry(t *testing.T) {
	for _, action := range []string{"publish-images", "publish-chart"} {
		t.Run(action, func(t *testing.T) {
			f := newReleaseRuntimeFixture(t)
			f.tool("cosign", `printf 'cosign:%s\n' "$1" >>"$FIXTURE_TRACE"
case "$1" in
 public-key) cat "$FIXTURE_ROOT/.oberth/pins/release-cosign.pub";;
 verify) test -f "$FIXTURE_ROOT/registry-signed";;
 sign) if test -f "$FIXTURE_ROOT/registry-signed"; then echo IMMUTABLE_SIGNATURE_REFUSAL >&2; exit 91; fi; touch "$FIXTURE_ROOT/registry-signed";;
 sign-blob) while [ "$#" -gt 0 ]; do if [ "$1" = --bundle ]; then shift; printf "valid\n" >"$1"; fi; shift; done;;
 verify-blob) while [ "$#" -gt 0 ]; do if [ "$1" = --bundle ]; then shift; grep -qx valid "$1" || exit 92; fi; shift; done;;
 *) exit 90;;
esac`)
			f.tool("oberth-release-support", `case "$1" in
 registry-token) printf fixture-restricted-access-token >"$4";;
 registry-digest) if test -f "$FIXTURE_ROOT/chart-published"; then printf 'sha256:%064d\n' 0 | tr 0 b; else echo missing; fi;;
 *) exit 90;;
esac`)
			f.tool("helm", immutableChartFixture)
			for attempt := 1; attempt <= 2; attempt++ {
				out, err := f.run(action, true)
				if err != nil {
					t.Fatalf("attempt %d: %v\n%s\n%s", attempt, err, out, f.log())
				}
			}
			if got := strings.Count(f.log(), "cosign:sign\n"); got != 1 {
				t.Fatalf("signature writes = %d, want exactly one\n%s", got, f.log())
			}
			if action == "publish-chart" && strings.Count(f.log(), "helm:push\n") != 1 {
				t.Fatalf("chart retry did not reuse the existing immutable object\n%s", f.log())
			}
		})
	}
}

func TestReleaseChartCollisionRefusesSigningAndPublication(t *testing.T) {
	f := newReleaseRuntimeFixture(t)
	f.write("chart-published", "already exists")
	f.write("objects/oberth/oberth-1.2.3.tgz", "foreign chart bytes")
	f.tool("helm", immutableChartFixture)
	out, err := f.run("publish-chart", true)
	if err == nil || !strings.Contains(out, "GAR chart version already exists with different bytes") {
		t.Fatalf("foreign chart accepted: %v\n%s", err, out)
	}
	for _, mutation := range []string{"helm:push", "cosign:sign", "curl:PUT"} {
		if strings.Contains(f.log(), mutation) {
			t.Fatalf("collision performed %s\n%s", mutation, f.log())
		}
	}
}

const immutableChartFixture = `printf 'helm:%s\n' "$1" >>"$FIXTURE_TRACE"
case "$1" in
 registry) cat >/dev/null;;
 push) if test -f "$FIXTURE_ROOT/chart-published"; then echo IMMUTABLE_CHART_REFUSAL >&2; exit 91; fi; touch "$FIXTURE_ROOT/chart-published";;
 pull) while [ "$#" -gt 0 ]; do if [ "$1" = --destination ]; then shift; cp "$FIXTURE_ROOT/objects/oberth/oberth-1.2.3.tgz" "$1/oberth-1.2.3.tgz"; fi; shift; done;;
 *) exit 90;;
esac`
