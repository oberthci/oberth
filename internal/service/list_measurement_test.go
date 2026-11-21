package service_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestListCountsMeasurement(t *testing.T) {
	if os.Getenv("OBERTH_LIST_MEASURE") != "1" {
		t.Skip("opt-in 20 repository / 20000 run measurement")
	}
	p := people(t)
	seedListMeasurement(t, p)
	var gitCalls atomic.Int64
	p.f.c.Git = contracttest.GitFuncs{ListRefsFunc: func(context.Context, string) ([]gitport.Ref, error) {
		gitCalls.Add(1)
		return []gitport.Ref{{Name: "refs/heads/main"}}, nil
	}}
	p.f.c.Runs = p.state
	p.f.c.Catalog = p.state
	p.f.restart(t)
	for _, name := range []string{"repos", "counts"} {
		gitCalls.Store(0)
		bench := measureListQuery(t, p, name)
		t.Logf("%s: %s; %s; Git calls %d", name, bench.String(), bench.MemString(), gitCalls.Load())
	}
}

func seedListMeasurement(t *testing.T, p *peopleFixture) {
	t.Helper()
	for n := 1; n < 20; n++ {
		_, err := p.state.RegisterRepository(t.Context(), stateport.Action{Actor: "owner"}, fmt.Sprintf("origin/team/repo%02d", n))
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<20000)
 INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,step_count,steps_planned_exact,steps_pending)
 SELECT printf('%032x',i),1+(i-1)%20,'refs/heads/main','running',0,'{}',
 json_object('id',printf('%032x',i),'repo',(SELECT canonical FROM repositories WHERE id=1+(i-1)%20),'status','running','ref','refs/heads/main'),0,1,0 FROM n`)
	if err != nil {
		t.Fatal(err)
	}
}

func measureListQuery(t *testing.T, p *peopleFixture, name string) testing.BenchmarkResult {
	return testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if name == "repos" {
				call, err := p.f.s.PrepareRepoList(t.Context(), adminCaller(), apicontract.RepoListArgs{NamesOnly: true})
				rows, err := execute(t.Context(), call, err)
				if err != nil || len(rows.Repositories) != 20 {
					b.Fatal(rows, err)
				}
			} else {
				call, err := p.f.s.Views().Counts.Handler.Prepare(t.Context(), adminCaller(), apicontract.Empty{})
				counts, err := execute(t.Context(), call, err)
				if err != nil || counts.Running != 20000 {
					b.Fatal(counts, err)
				}
			}
		}
	})
}
