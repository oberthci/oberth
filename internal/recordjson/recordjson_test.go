package recordjson

import (
	"testing"
	"time"
)

type inner struct {
	ExitCode int
	Tagged   string `json:"tagged_name"`
}

type embedded struct {
	QueuePosition int
}

type record struct {
	embedded
	ID        string
	RepoID    int64
	SHA       string
	StartedAt *time.Time
	Steps     []inner
	ByRepo    map[int64]inner
	Labels    map[string]string
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{"ID": "id", "RepoID": "repo_id", "SHA": "sha", "QueueSequence": "queue_sequence", "CIWorkID": "ci_work_id", "already_snake": "already_snake"} {
		if got := SnakeCase(in); got != want {
			t.Errorf("SnakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnmarshalReadsSnakeAndLegacyNames(t *testing.T) {
	for name, body := range map[string]string{
		"snake":  `{"id":"r","repo_id":9007199254740993,"sha":"s","queue_position":2,"started_at":"2026-11-10T00:00:00Z","steps":[{"exit_code":3,"tagged_name":"t"}],"by_repo":{"7":{"exit_code":4}},"labels":{"BuildMain":"kept"}}`,
		"legacy": `{"ID":"r","RepoID":9007199254740993,"SHA":"s","QueuePosition":2,"StartedAt":"2026-11-10T00:00:00Z","Steps":[{"ExitCode":3,"tagged_name":"t"}],"ByRepo":{"7":{"ExitCode":4}},"Labels":{"BuildMain":"kept"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got record
			if err := Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != "r" || got.RepoID != 9007199254740993 || got.SHA != "s" || got.QueuePosition != 2 || got.StartedAt == nil ||
				len(got.Steps) != 1 || got.Steps[0].ExitCode != 3 || got.Steps[0].Tagged != "t" || got.ByRepo[7].ExitCode != 4 || got.Labels["BuildMain"] != "kept" {
				t.Fatalf("decoded %+v", got)
			}
		})
	}
	var rows []record
	if err := Unmarshal([]byte(`[{"repo_id":1},{"RepoID":2}]`), &rows); err != nil || rows[0].RepoID != 1 || rows[1].RepoID != 2 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	if err := Unmarshal([]byte(`{"id":`), &rows); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}
