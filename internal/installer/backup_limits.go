package installer

import (
	"fmt"

	"github.com/oberthci/oberth/internal/store"
)

// DER recipient inspection has an absolute bound; operation-specific limits
// are checked before decryption/extraction.
const maxBackupCipherBytes = store.HardBackupMaxBytes + (144 << 20)

// Keep database, archive framing and non-database budgets separate: WAL is not
// part of the logical snapshot size; JSON and old log members keep their cap.
type backupSizeLimits struct {
	database, plaintext, ciphertext int64
}

func backupLimits(databaseBytes int64) (backupSizeLimits, error) {
	limit, err := store.BackupMaxBytes(databaseBytes)
	if err != nil {
		return backupSizeLimits{}, err
	}
	return backupSizeLimits{database: limit + (64 << 10), plaintext: limit + (128 << 20), ciphertext: limit + (144 << 20)}, nil
}

func (b backupSizeLimits) member(name string) int64 {
	if name == backupMemberDatabase {
		return b.database
	}
	return maxBackupMemberBytes
}

func backupLimitError(size, limit int64) error {
	return fmt.Errorf("backup size %d bytes exceeds limit %d bytes (raise --max-database-bytes within the 4 GiB maximum)", size, limit)
}
