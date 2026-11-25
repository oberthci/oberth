package installer

import (
	"context"
	"strings"
)

const scheduleImagePlaceholder = "REPLACE_WITH_PINNED_OBERTH_IMAGE"

// InstalledBackupSchedule fills the schedule's image with the deployed
// release's server image by digest. It only reads the release history; a
// release without a digest keeps the placeholder for the operator to edit.
func (e *Engine) InstalledBackupSchedule(ctx context.Context, namespace string) (string, error) {
	history, err := e.Charts.Read(ctx, namespace)
	if err != nil {
		return "", err
	}
	repository, _ := history.Deployed.Values["imageRepository"].(string)
	digest, _ := history.Deployed.Values["imageDigest"].(string)
	if history.Deployed.Number == 0 || repository == "" || !strings.HasPrefix(digest, "sha256:") {
		return BackupSchedule, nil
	}
	return strings.Replace(BackupSchedule, scheduleImagePlaceholder, repository+"@"+digest, 1), nil
}
