package service_test

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
)

type exportedFixture struct {
	Shortened []exportedShortening
	Files     []struct{ Name, SHA256 string }
	Issues    []exportedIssue
	Losses    []struct {
		ID    int64
		Field string
	} `json:"issue_losses"`
}
type exportedShortening struct {
	FullTextLocation       string `json:"full_text_location"`
	ID                     int64
	OriginalBytes          int `json:"original_bytes"`
	KeptBytes              int `json:"kept_bytes"`
	EntriesDropped         int `json:"entries_dropped"`
	OriginalHistoryEntries int `json:"original_history_entries"`
}

type exportedIssue struct {
	ID             int64
	Title          string
	State          model.IssueState
	Kind           model.IssueKind
	BodySHA256     string `json:"body_sha256"`
	KeptBodySHA256 string `json:"kept_body_sha256"`
	KeptBodyBytes  int    `json:"kept_body_bytes"`
	HistorySHA256  string `json:"history_sha256"`
	BodyBytes      int    `json:"body_bytes"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	ClosedAt       *int64 `json:"closed_at"`
}

// The fixture files are the external exporter's actual output, compressed for storage.
// The independent expectations come from source database rows, not issue conversion.
func TestIssueExporterFixtureRoundTrip(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) { checkExporterFixture(t, real) })
	}
}

func checkExporterFixture(t *testing.T, real bool) {
	t.Helper()
	root, fixture := loadExporterFixture(t)
	p := people(t)
	if !real {
		p.f.c.People.Issues = p.f.store.native
		p.f.restart(t)
	}
	files := readExportedFiles(t, root, fixture)
	total := 0
	for i, q := range files {
		result, err := importCall(t, p, q)
		if err != nil {
			t.Fatal(err)
		}
		total += result.Count
		if i+1 < len(files) {
			if native := createTask(t, p, "origin", "between import files"); native.ID != 1897+int64(i) {
				t.Fatal("file order stranded a later source number", native.ID)
			}
		}
	}
	if total != len(fixture.Issues) {
		t.Fatal("issues lost", total)
	}
	for _, want := range fixture.Issues {
		checkExportedIssue(t, p, want)
	}
	checkShortenedExports(t, p, fixture)
	checkExportFileReplay(t, p, files)
	if got := createTask(t, p, "origin", "native"); got.ID != 1896+int64(len(files)) {
		t.Fatal("number continuation lost", got.ID)
	}
	contracttest.ExportedIssueLifecycle(t, p.f.c.People.Issues, 1195)
	if !slices.ContainsFunc(fixture.Losses, func(v struct {
		ID    int64
		Field string
	}) bool {
		return v.ID == 201 && v.Field == "kind"
	}) {
		t.Fatal("CI kind loss is not listed")
	}
}

func checkExportFileReplay(t *testing.T, p *peopleFixture, files []apicontract.IssueImportArgs) {
	t.Helper()
	for _, q := range files {
		result, err := importCall(t, p, q)
		if err != nil || !result.AlreadyImported {
			t.Fatal("file replay changed", result, err)
		}
	}
}

func loadExporterFixture(t *testing.T) (string, exportedFixture) {
	t.Helper()
	root := filepath.Join("testdata", "issue-export")
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture exportedFixture
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Issues) != 1700 || len(fixture.Files) < 2 {
		t.Fatal("volume fixture missing")
	}
	checkExporterLossFile(t, root, fixture)
	return root, fixture
}

func readExportedFiles(t *testing.T, root string, fixture exportedFixture) []apicontract.IssueImportArgs {
	t.Helper()
	var files []apicontract.IssueImportArgs
	for _, entry := range fixture.Files {
		file, err := os.Open(filepath.Join(root, entry.Name))
		if err != nil {
			t.Fatal(err)
		}
		reader, err := gzip.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(io.LimitReader(reader, apicontract.RequestBytes+1))
		if err != nil {
			t.Fatal(err)
		}
		if err = reader.Close(); err != nil {
			t.Fatal(err)
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		if len(raw) > apicontract.RequestBytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != entry.SHA256 {
			t.Fatal("exported fixture bytes changed", entry.Name)
		}
		files = append(files, apicontract.IssueImportArgs{JSONL: string(raw), Upstream: "origin"})
	}
	return files
}

func checkExportedIssue(t *testing.T, p *peopleFixture, want exportedIssue) {
	t.Helper()
	first, history := readExportedHistory(t, p, want.ID)
	checkExportedMetadata(t, first, want)
	checkExportedAttribution(t, first)
	checkExportedBody(t, first, history, want)
	if (want.ID == 40 || want.ID == 201) && !strings.HasPrefix(first.Body, "This was a CI issue tracking repository") {
		t.Fatal("source CI provenance missing")
	}
}

func checkExportedMetadata(t *testing.T, first model.Issue, want exportedIssue) {
	t.Helper()
	if first.ID != want.ID || first.Title != want.Title || first.State != want.State || first.Kind != want.Kind || first.CreatedAt.UnixNano() != want.CreatedAt || first.UpdatedAt.UnixNano() != want.UpdatedAt {
		t.Fatal("source metadata changed", want.ID)
	}
	checkExportedCloseTime(t, first, want)
	if first.Imported == nil {
		t.Fatal("missing imported mark")
	}
}

func checkExportedCloseTime(t *testing.T, first model.Issue, want exportedIssue) {
	t.Helper()
	if want.ClosedAt == nil {
		if first.ClosedAt != nil || first.SourceClosedAt != nil {
			t.Fatal("open source acquired a closing time", want.ID)
		}
		return
	}
	if first.ClosedAt == nil || first.SourceClosedAt == nil {
		t.Fatal("source closing time missing", want.ID)
	}
	if first.ClosedAt.UnixNano() != *want.ClosedAt || first.SourceClosedAt.UnixNano() != *want.ClosedAt {
		t.Fatal("source closing time changed", want.ID)
	}
}

func checkExportedAttribution(t *testing.T, first model.Issue) {
	t.Helper()
	expected := map[int64][2]string{7: {"author text", "closer text"}, 40: {"CI author", ""}}
	if want, ok := expected[first.ID]; ok && (first.Author != want[0] || first.Closer != want[1]) {
		t.Fatal("source attribution changed", first.ID)
	}
}

func checkExportedBody(t *testing.T, first model.Issue, history []model.IssueHistory, want exportedIssue) {
	t.Helper()
	digest := want.BodySHA256
	bytes := want.BodyBytes
	if want.KeptBodySHA256 != "" {
		digest, bytes = want.KeptBodySHA256, want.KeptBodyBytes
		if !strings.HasSuffix(first.Body, exportShortenedNotice) {
			t.Fatal("shortened export lost its notice", want.ID)
		}
	}
	if len(first.Body) != bytes || fmt.Sprintf("%x", sha256.Sum256([]byte(first.Body))) != digest {
		t.Fatal("importer changed exported body bytes", want.ID)
	}
	raw, err := json.Marshal(history)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != want.HistorySHA256 {
		t.Fatal("importer changed exported history", want.ID, err)
	}
}

const exportShortenedNotice = "\n\nThis issue was shortened at export; the full text stays in the previous installation under the same number."

func readExportedHistory(t *testing.T, p *peopleFixture, id int64) (model.Issue, []model.IssueHistory) {
	t.Helper()
	call, err := p.f.s.PrepareIssueGet(t.Context(), adminCaller(), apicontract.IssueGetArgs{ID: id})
	issue, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(id, err)
	}
	raw, err := json.Marshal(issue)
	if err != nil || len(raw) > model.IssueBytes {
		t.Fatal("unreadable complete issue", id, err)
	}
	return issue, issue.History
}

func checkShortenedExports(t *testing.T, p *peopleFixture, fixture exportedFixture) {
	t.Helper()
	if len(fixture.Shortened) != 2 {
		t.Fatal("shortening losses missing")
	}
	for _, loss := range fixture.Shortened {
		issue, history := readExportedHistory(t, p, loss.ID)
		raw, err := json.Marshal(issue)
		if err != nil || len(raw) != loss.KeptBytes || loss.KeptBytes > model.IssueBytes || loss.OriginalBytes <= model.IssueBytes {
			t.Fatal("exporter's complete issue measurement differs from product", loss.ID, len(raw), loss, err)
		}
		if len(history)+loss.EntriesDropped != loss.OriginalHistoryEntries {
			t.Fatal("dropped history count differs", loss.ID)
		}
	}
}

func checkExporterLossFile(t *testing.T, root string, fixture exportedFixture) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "losses.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != len(fixture.Shortened)+len(fixture.Losses)+1 {
		t.Fatal("loss file must have one summary and one line per shortened issue")
	}
	for i, want := range fixture.Shortened {
		var got exportedShortening
		if err := json.Unmarshal([]byte(lines[i+len(fixture.Losses)+1]), &got); err != nil {
			t.Fatal(err)
		}
		if got.FullTextLocation != fmt.Sprintf("Previous installation, issue #%d.", want.ID) {
			t.Fatal("loss row omits whole-text location", got.ID)
		}
		if got.ID != want.ID || got.OriginalBytes != want.OriginalBytes || got.KeptBytes != want.KeptBytes || got.EntriesDropped != want.EntriesDropped {
			t.Fatal("shortening line differs from source expectations", got, want)
		}
	}
}
