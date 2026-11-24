package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestSystemStatusUsesPartReadiness(t *testing.T) {
	for _, scenario := range []string{"starting", "ready", "watch", "database", "failed", "stopping"} {
		t.Run(scenario, func(t *testing.T) {
			c := testComposition(t)
			c.storeWriter.Client, _ = secretstore.New(secretstore.Connection{URL: "https://store.invalid", Transport: &storefake.Store{Initialized: true}})
			c.lifecycle = newLifecycle(t.Context(), []Part{{Name: "store"}, {Name: "cluster"}, {Name: "source"}, {Name: "schedules"}, {Name: "logs"}, {Name: "secret store"}})
			defer c.lifecycle.cancel()
			c.health.success = c.clock.Now()
			c.health.watch = func() bool { return scenario != "watch" }
			if scenario != "starting" {
				if err := c.lifecycle.start(); err != nil {
					t.Fatal(err)
				}
				c.health.setStarted(true)
			}
			switch scenario {
			case "database":
				c.health.success = c.clock.Now().Add(-91 * time.Second)
			case "failed":
				c.lifecycle.parts[3].err = errors.New("private native detail")
				c.lifecycle.parts[3].ready = false
			case "stopping":
				if err := c.lifecycle.stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			status, err := (serverAdministration{&serverOwners{c: c}}).Status(t.Context(), "system_status")
			if err != nil || status.Ready != (scenario == "ready") || status.Detail != "" {
				t.Fatal("status invented readiness or exposed detail", status, err)
			}
			assertPartStatus(t, status.Parts, scenario)
		})
	}
}

func assertPartStatus(t *testing.T, rows []apicontract.PartStatus, scenario string) {
	t.Helper()
	if len(rows) != 6 {
		t.Fatal("missing server parts", rows)
	}
	for _, row := range rows {
		if row.DisplayReason == "" || len([]rune(row.DisplayReason)) > 40 {
			t.Fatal("part reason is not a complete short phrase", row)
		}
		want := scenario == "ready" || scenario == "watch" && row.Name != "cluster" || scenario == "database" && row.Name != "store" || scenario == "failed" && row.Name != "schedules"
		if row.Ready != want {
			t.Fatal("wrong per-part readiness", scenario, row)
		}
	}
}
