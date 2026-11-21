package installer_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestApprovedLatestRevisionCannotChange(t *testing.T) {
	t.Parallel()
	request := installer.ChartRequest{Revision: 8, Predecessor: 5, ManifestDigest: "reviewed manifest"}
	history := installer.History{
		Latest:   installer.Revision{Number: 8},
		Deployed: installer.Revision{Number: 5, ManifestDigest: "reviewed manifest"},
	}
	if err := request.CheckHistory(history); err != nil {
		t.Fatal("unchanged history refused", err)
	}
	history.Latest.Number++
	err := request.CheckHistory(history)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" || refusal.Detail != "Latest revision changed after review: expected 8; found 9." {
		t.Fatal("new revision bypassed approval because the predecessor was unchanged", err)
	}
}
