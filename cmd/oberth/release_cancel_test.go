package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
)

func TestReleaseRefusalCommandRequestsForcedCancellation(t *testing.T) {
	refused := &app.Refusal{Code: "release_old"}
	_, command, ok := strings.Cut(refused.Remedy(), ": oberth ")
	if !ok {
		t.Fatal("release refusal has no cancellation command", refused.Error())
	}
	command = strings.ReplaceAll(strings.TrimSuffix(command, "."), "<run>", "12345678")
	bindings := fake.Bindings()
	cli, out, diagnostic := fixture(t, bindings)
	if exit := cli.run(t.Context(), strings.Fields(command)); exit != 0 {
		t.Fatal("refusal command failed", exit, out, diagnostic)
	}
	inputs, callers, calls := bindings.RunCancel.Handler.(*fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt]).Observed()
	if len(inputs) != 1 || inputs[0].ID != "12345678" || !inputs[0].Force || calls != 1 || !callers[0].Actor.Admin {
		t.Fatal("refusal must request forced cancellation as the administrator", inputs, callers, calls)
	}
}
