package protectedtests

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func descriptorFixture() (Contract, Recipe) {
	c := Contract{Version: 2, Profile: KVMSuiteAMD64Profile, SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), Artifacts: []string{"build/program"}}
	r := Recipe{SuiteSHA256: c.SuiteSHA256, GuestSHA256: c.GuestSHA256, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
	return c, r
}

func TestDescriptorRequiresExactContractAndCurrentAuthority(t *testing.T) {
	c, r := descriptorFixture()
	d, err := SelectDescriptor(c, []Recipe{r})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.MatchContract(&c); err != nil {
		t.Fatal(err)
	}
	if err = d.MatchContract(nil); err == nil {
		t.Fatal("absent policy accepted")
	}
	for name, mutate := range map[string]func(*Contract){
		"suite":    func(c *Contract) { c.SuiteSHA256 = strings.Repeat("d", 64) },
		"guest":    func(c *Contract) { c.GuestSHA256 = strings.Repeat("d", 64) },
		"artifact": func(c *Contract) { c.Artifacts = []string{"build/other"} },
		"v1":       func(c *Contract) { *c = Contract{Version: 1, Profile: OfflineEBPFProfile, Artifact: BPFArtifact} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := d.Contract()
			mutate(&changed)
			if err := d.MatchContract(&changed); err == nil {
				t.Fatal("changed contract accepted")
			}
			if _, err := RequiredContract(&changed, &c, nil); err == nil {
				t.Fatal("unauthorized baseline change accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*Recipe){
		"suite":          func(r *Recipe) { r.SuiteSHA256 = strings.Repeat("d", 64) },
		"guest":          func(r *Recipe) { r.GuestSHA256 = strings.Repeat("d", 64) },
		"harness":        func(r *Recipe) { r.HarnessImage = "registry.example/harness@sha256:" + strings.Repeat("d", 64) },
		"suite endpoint": func(r *Recipe) { r.SuiteURL = "https://example.org/new-suite" },
		"guest endpoint": func(r *Recipe) { r.GuestURL = "https://example.org/new-guest" },
	} {
		t.Run(name+" authority", func(t *testing.T) {
			changed := r
			mutate(&changed)
			if err := RecipeAllowed(d.Recipe(), []Recipe{changed}); err == nil {
				t.Fatal("recipe change authorized journaled execution")
			}
		})
	}
	for _, approved := range [][]Recipe{nil, {}, {r, r}} {
		if _, err := SelectDescriptor(c, approved); err == nil {
			t.Fatal("missing or ambiguous approval accepted")
		}
		if err := RecipeAllowed(r, approved); err == nil {
			t.Fatal("missing or ambiguous authority accepted")
		}
	}
	var zero Descriptor
	if err := zero.MatchContract(&c); err == nil {
		t.Fatal("zero descriptor accepted")
	}
	if _, err := zero.VerifyBundle([]byte("candidate")); err == nil {
		t.Fatal("zero descriptor verified input")
	}
	if _, err := RequiredContract(nil, &c, nil); err == nil {
		t.Fatal("removed baseline accepted")
	}
	// Structurally binding a candidate's new pair does not approve it.
	changed := c
	changed.SuiteSHA256 = strings.Repeat("d", 64)
	changed.GuestSHA256 = strings.Repeat("e", 64)
	changedRecipe := r
	changedRecipe.SuiteSHA256 = changed.SuiteSHA256
	changedRecipe.GuestSHA256 = changed.GuestSHA256
	if _, err := BindContract(changed, changedRecipe); err != nil {
		t.Fatal(err)
	}
	authorize := func(c Contract) error { _, err := SelectDescriptor(c, []Recipe{r}); return err }
	if _, err := RequiredContract(&changed, &c, authorize); err == nil {
		t.Fatal("unapproved paired rotation accepted")
	}
	authorize = func(c Contract) error { _, err := SelectDescriptor(c, []Recipe{r, changedRecipe}); return err }
	if _, err := RequiredContract(&changed, &c, authorize); err != nil {
		t.Fatalf("explicitly approved paired rotation refused: %v", err)
	}
}

func TestDescriptorDoesNotShareCandidateOrCallerStorage(t *testing.T) {
	c, r := descriptorFixture()
	approved := []Recipe{r}
	d, err := SelectDescriptor(c, approved)
	if err != nil {
		t.Fatal(err)
	}
	c.Artifacts[0] = "candidate/override"
	exposed := d.Contract()
	exposed.Artifacts[0] = "caller/override"
	approved[0].HarnessImage = "candidate/harness"
	if d.Contract().Artifacts[0] != "build/program" || d.Recipe() != r {
		t.Fatal("mutable caller storage changed descriptor")
	}
	if err := RecipeAllowed(d.Recipe(), approved); err == nil {
		t.Fatal("changed current authority accepted")
	}
}

func descriptorArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(data)), Mode: 0444, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDescriptorBindsAllBundleInputsAndCompleteVerdict(t *testing.T) {
	c, r := descriptorFixture()
	files := map[string]string{
		"manifest.yaml": fmt.Sprintf("version: 1\nprofile: %s\nguest_sha256: %s\nartifacts: [build/program]\ncases: [deny, allow]\n", c.Profile, c.GuestSHA256),
		"suite-driver":  "closed driver bytes", "Makefile": "test: scripts/check.sh\n", "scripts/check.sh": "test expected result\n", "testdata/expected": "denied\n",
	}
	bundle := descriptorArchive(t, files)
	sum := sha256.Sum256(bundle)
	c.SuiteSHA256 = hex.EncodeToString(sum[:])
	r.SuiteSHA256 = c.SuiteSHA256
	d, err := SelectDescriptor(c, []Recipe{r})
	if err != nil {
		t.Fatal(err)
	}
	suite, err := d.VerifyBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		t.Run(name, func(t *testing.T) {
			altered := make(map[string]string, len(files))
			for path, data := range files {
				altered[path] = data
			}
			altered[name] += "candidate override"
			if _, err := d.VerifyBundle(descriptorArchive(t, altered)); err == nil {
				t.Fatal("changed transitive bundle input accepted under unchanged pin")
			}
		})
	}
	valid := []CaseResult{{ID: "deny", Outcome: "passed"}, {ID: "allow", Outcome: "passed"}}
	if err := suite.VerifyInventory(c, valid); err != nil {
		t.Fatal(err)
	}
	for _, results := range [][]CaseResult{
		valid[:1], append(slices.Clone(valid), CaseResult{ID: "extra", Outcome: "passed"}),
		{valid[0], valid[0]}, {valid[0], {ID: "allow", Outcome: "skipped"}}, {valid[0], {ID: "allow", Outcome: "failed"}},
	} {
		if err := suite.VerifyInventory(c, results); err == nil {
			t.Fatal("incomplete or non-passing verdict accepted")
		}
	}
	ids := suite.CaseIDs()
	ids[0] = "forged"
	if err := suite.VerifyInventory(c, valid); err != nil {
		t.Fatal("caller changed private inventory")
	}
}
