package installer

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const backupTimeFormat = "20060102T150405.000000000Z"

func (m Maintenance) upgradeBackupPath(r Record) string {
	return m.Engine.Credentials.pathFor("backup-" + m.Engine.Now().UTC().Format(backupTimeFormat) + "-" + r.ID() + "-" + r.TargetVersion + ".age")
}

// Keep the current recovery archive even if the wall clock moved backwards.
func (m Maintenance) pruneUpgradeBackups(current string) (int, error) {
	c := m.Engine.Credentials
	dir, err := c.root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() && upgradeBackupName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	slices.Reverse(names)
	names = slices.DeleteFunc(names, func(name string) bool { return name == filepath.Base(current) })
	removed := 0
	for _, name := range names[min(2, len(names)):] {
		if err := c.root.Remove(name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func upgradeBackupName(name string) bool {
	if !strings.HasPrefix(name, "backup-") || !strings.HasSuffix(name, ".age") {
		return false
	}
	parts := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(name, "backup-"), ".age"), "-", 3)
	if len(parts) != 3 || len(parts[1]) != 24 || strings.Trim(parts[1], "0123456789abcdef") != "" || !semver.IsValid(parts[2]) {
		return false
	}
	_, err := time.Parse(backupTimeFormat, parts[0])
	return err == nil
}

func (m Maintenance) finishUpgradeBackups(result *Result, r Record) {
	if r.BackupPath == "" {
		return
	}
	result.BackupPath = r.BackupPath
	count, err := m.pruneUpgradeBackups(r.BackupPath)
	if count > 0 {
		result.BackupReminder += fmt.Sprintf("\nRemoved older pre-upgrade backups: %d; kept the three newest.", count)
	}
	if err != nil {
		result.BackupReminder += "\nOlder pre-upgrade backups remain; check profile storage and remove older copies by hand."
	}
}
