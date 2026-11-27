package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

// recordLifecycle audits a completed upgrade or rollback. The caller's client
// uses the profile's files as they are after the change; nil means none could be built.
func (s installationSession) recordLifecycle(ctx context.Context, remote *client.Client, done installer.UpgradeCompletion, rollback bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	details := map[string]string{"from": done.From, "to": done.To, "revision": strconv.Itoa(done.Revision)}
	operation, action := "Upgrade", "upgrade.completed"
	if rollback {
		operation, action = "Rollback", "rollback.completed"
	}
	if remote != nil {
		args := apicontract.AdminAuditRecordArgs{Action: action, Details: details}
		if receipt, err := remote.RecordInstallation(ctx, args); err == nil && receipt.State == "recorded" {
			return
		}
	}
	profile := strings.ReplaceAll(s.name, "'", "'\"'\"'")
	_, _ = fmt.Fprintf(s.app.diagnostic, "%s succeeded; audit unconfirmed. Check: OBERTH_PROFILE='%s' oberth audit\n", operation, profile)
}
