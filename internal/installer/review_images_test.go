package installer_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestImageReferencesAppearInJSONAndDetailOnly(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"local", "signed"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			want := map[string]string{"server": "oberthci/oberth:dev", "source": "oberthci/oberth-source-plugin:dev"}
			if kind == "signed" {
				f.release = loadFixture(t, releaseFixture(t))
				want["server"] = "oberthci/oberth@sha256:" + strings.Repeat("b", 64)
				want["source"] = "oberthci/oberth-source-plugin@sha256:" + strings.Repeat("c", 64)
			}
			plan := prepare(t, f)
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{string(raw), strings.Join(plan.Detail(), "\n")} {
				assertReviewImages(t, output, want)
			}
			compact := strings.Join(plan.Review(), "\n")
			for _, image := range want {
				if strings.Contains(compact, image) {
					t.Fatal("image reference entered compact review", compact)
				}
			}
			assertNoInstallWrites(t, f)
		})
	}
}

func assertReviewImages(t *testing.T, output string, want map[string]string) {
	t.Helper()
	var review struct {
		Images map[string]string `json:"images"`
	}
	if err := json.Unmarshal([]byte(output), &review); err != nil || !reflect.DeepEqual(review.Images, want) {
		t.Fatal("image references missing from public review", err, output)
	}
}
