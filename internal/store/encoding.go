package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

func formatID(id int64) string { return strconv.FormatInt(id, 10) }
func validDigest(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == 32 && hex.EncodeToString(body) == value
}
func hashBytes(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }
func encode(value any) string       { body, _ := json.Marshal(value); return string(body) }
