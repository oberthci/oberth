package nativebinding

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	cronPrefix      = "oberth-cron-"
	cronDigestBytes = 12
)

// ValidChildRunID checks the complete scheduled-child name's bounded shape.
// Shape alone never grants run visibility or establishes parent custody.
func ValidChildRunID(id string) bool {
	if !strings.HasPrefix(id, cronPrefix) {
		return false
	}
	rest := strings.TrimPrefix(id, cronPrefix)
	repository, rest, ok := strings.Cut(rest, "-")
	value, err := strconv.ParseInt(repository, 36, 64)
	if !ok || err != nil || value <= 0 || strconv.FormatInt(value, 36) != repository {
		return false
	}
	hash, stamp, ok := strings.Cut(rest, "-")
	if !ok || len(hash) != hex.EncodedLen(cronDigestBytes) || strings.ToLower(hash) != hash {
		return false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return false
	}
	return ChildName(id, strings.TrimSuffix(id, "-"+stamp))
}

func cronName(repositoryID int64, entry string) string {
	digest := sha256.Sum256([]byte(entry))
	return cronPrefix + strconv.FormatInt(repositoryID, 36) + "-" + hex.EncodeToString(digest[:cronDigestBytes])
}

// CronRepositoryID verifies the complete parent name against its entry. The
// name alone never grants ownership; the native controller UID must match too.
func CronRepositoryID(name, entry string) (int64, error) {
	rest, ok := strings.CutPrefix(name, cronPrefix)
	id, _, _ := strings.Cut(rest, "-")
	value, err := strconv.ParseInt(id, 36, 64)
	if !ok || err != nil || value <= 0 || entry == "" || cronName(value, entry) != name {
		return 0, &Refusal{reason: "schedule parent name does not match its admitted entry"}
	}
	return value, nil
}

// ChildName matches the native parent prefix and canonical positive timestamp suffix.
func ChildName(child, parent string) bool {
	suffix, ok := strings.CutPrefix(child, parent+"-")
	stamp, err := strconv.ParseInt(suffix, 10, 64)
	return ok && err == nil && stamp > 0 && strconv.FormatInt(stamp, 10) == suffix
}
