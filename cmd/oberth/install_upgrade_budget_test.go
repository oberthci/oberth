package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeBudgetStartsAfterApproval(t *testing.T) {
	f := pendingBudgetUpgrade(t)
	flags := installationFlags("upgrade")
	if err := flags.Parse([]string{"--version", "v0.2.0", "--release-dir", f.target, "--no-backup", "--yes"}); err != nil {
		t.Fatal(err)
	}
	f.session.request = installationRequest{name: "upgrade", flags: flags}
	f.session.app.config = client.Config{Now: f.session.engine.Now, Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("recorded repair contacted live metadata")
		return nil, errors.New("unexpected request")
	})}
	var out bytes.Buffer
	stop := errors.New("stop at approved revalidation")
	charts := &upgradeBudgetChart{ChartPort: f.charts, t: t, out: &out, stop: stop}
	f.session.engine.Charts = charts
	f.session.engine.Timeout = 30 * time.Minute
	applies := len(f.charts.Applies)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reference, stopReference := context.WithTimeout(ctx, 20*time.Minute)
	defer stopReference()
	charts.earliest, _ = reference.Deadline()
	err := f.session.changeInstallation(ctx, strings.NewReader(""), &out)
	if !errors.Is(err, stop) || !charts.reviewed || !charts.approved || len(f.charts.Applies) != applies {
		t.Fatal("upgrade did not reach both budget boundaries without applying", err, charts.reviewed, charts.approved)
	}
}

func pendingBudgetUpgrade(t *testing.T) signedMaintenance {
	t.Helper()
	f := signedMaintenanceFixture(t)
	plan := planSignedFixture(t, &f)
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		return installer.NewProblem("starting", nil, "")
	}
	f.session.engine.Pause = func(context.Context) error { return context.DeadlineExceeded }
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	if _, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("upgrade did not leave a pending record", err)
	}
	return f
}

type upgradeBudgetChart struct {
	installer.ChartPort
	earliest           time.Time
	t                  *testing.T
	out                *bytes.Buffer
	stop               error
	reviewed, approved bool
}

func (c *upgradeBudgetChart) Read(ctx context.Context, namespace string) (installer.History, error) {
	deadline, bounded := ctx.Deadline()
	if c.out.Len() > 0 {
		c.approved = true
		shift := deadline.Sub(c.earliest)
		if !bounded || shift < 0 || shift > time.Minute {
			c.t.Error("approved upgrade lacks its full twenty-minute budget", bounded, shift)
		}
		return installer.History{}, c.stop
	}
	if !c.reviewed {
		c.reviewed = true
		if bounded {
			c.t.Error("upgrade budget started before review", deadline)
		}
	}
	return c.ChartPort.Read(ctx, namespace)
}
