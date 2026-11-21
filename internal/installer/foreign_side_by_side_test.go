package installer_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
)

func TestForeignHistoryStillRequiresProfileKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"absent", "foreign", "missing"} {
		for _, recheck := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recheck=%t", key, recheck), func(t *testing.T) {
				f, store, kc := foreignInstallFixture(t, false)
				appendForeignRevisions(t, store)
				var profile installer.Profile
				if err := json.Unmarshal([]byte(installationRecord(t, f)), &profile); err != nil {
					t.Fatal(err)
				}
				var plan *installer.Plan
				if recheck {
					plan = prepare(t, f)
				}
				dir := replaceOwnerCredentials(t, f, profile, key)
				unchanged := unchangedInstallation(t, f, store, kc)
				var err error
				if recheck {
					_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
				} else {
					_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
				}
				requireOwnerRefusal(t, err)
				unchanged()
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatal("refusal created a profile directory", err)
				}
			})
		}
	}
}

func TestForeignHistoryRefusesKeyLossAtFinalWriteBoundary(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	var profile installer.Profile
	if err := json.Unmarshal([]byte(installationRecord(t, f)), &profile); err != nil {
		t.Fatal(err)
	}
	plan := prepare(t, f)
	var unchanged func()
	_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.ReleaseChecked {
			if err := os.Remove(profile.AdminKey); err != nil {
				t.Fatal(err)
			}
			unchanged = unchangedInstallation(t, f, store, kc)
		}
		return nil
	})
	if unchanged == nil {
		t.Fatal("final write boundary was not reached")
	}
	requireOwnerRefusal(t, err)
	unchanged()
}

func TestForeignHistoryKeepsOtherChartNamespaceHolds(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"oberth", "oberth-runs"} {
		for _, recheck := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recheck=%t", target, recheck), func(t *testing.T) {
				f, store, kc := foreignInstallFixture(t, false)
				appendForeignRevisions(t, store)
				var plan *installer.Plan
				if recheck {
					plan = prepare(t, f)
				}
				record := foreignNamespaceRelease()
				record.Manifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + target + "\n"
				saveForeignRelease(t, f, record)
				unchanged := unchangedInstallation(t, f, store, kc)
				var err error
				if recheck {
					_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
				} else {
					_, err = f.engine.Prepare(t.Context(), f.release, f.choices)
				}
				if err == nil || !strings.Contains(err.Error(), "control/fixture-owner") || !strings.Contains(err.Error(), target) {
					t.Fatal("foreign history hid a namespace holder", err)
				}
				unchanged()
			})
		}
	}
}

func TestForeignHistoryRestoresAppliedEndpointAnswers(t *testing.T) {
	t.Parallel()
	for _, status := range []release.Status{release.StatusDeployed, release.StatusUninstalled} {
		t.Run(string(status), func(t *testing.T) {
			f := setup(t)
			f.release = loadFixture(t, releaseFixture(t))
			f.choices = installer.Choices{Context: "test", URL: "https://ci.oberth.example:41443", SSHPort: 41022, PipelineNamespace: "custom-jobs", HTTPSNodePort: 31443, SSHNodePort: 31022}
			apply(t, f, prepare(t, f))
			store, kc := delta2Native(t, f, false, false)
			allowForeignDeletion(kc)
			appendForeignRevisions(t, store)
			changeRevision(t, store, 3, func(record *release.Release) { record.Info.Status = status })
			f.choices = installer.Choices{Context: "test"}
			unchanged := unchangedInstallation(t, f, store, kc)
			plan := prepare(t, f)
			review := strings.Join(plan.Review(), "\n")
			for _, line := range []string{"Namespaces: oberth / custom-jobs", "HTTPS: https://ci.oberth.example:41443 -> node port 31443", "SSH: ci.oberth.example:41022 -> node port 31022"} {
				if !strings.Contains(review, line) {
					t.Fatal("review lost applied answers", review)
				}
			}
			unchanged()
			result := apply(t, f, plan)
			if result.Profile.ServerURL != "https://ci.oberth.example:41443" || result.Profile.SSHURL != "ssh://git@ci.oberth.example:41022" || result.Profile.PipelineNamespace != "custom-jobs" || result.Profile.HTTPSNodePort != 31443 || result.Profile.SSHNodePort != 31022 {
				t.Fatal("apply lost saved endpoint answers")
			}
			assertForeignApply(t, f, store, 4)
		})
	}
}
