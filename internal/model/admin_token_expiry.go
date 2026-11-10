package model

import "time"

// AdminTokenExpiry summarizes when the installation's active unscoped admin
// bearer tokens expire, so an approaching expiry is visible before every
// admin call starts failing. Tokens without an expiry are not counted here.
type AdminTokenExpiry struct {
	// EarliestExpiresAt is the soonest expiry among active admin tokens.
	EarliestExpiresAt time.Time `json:"earliest_expires_at"`
	// ExpiringWithin30Days counts active admin tokens expiring in 30 days.
	ExpiringWithin30Days int `json:"expiring_within_30d"`
	// Warning is set inside AdminTokenWarningWindow of the earliest expiry.
	Warning string `json:"warning,omitempty"`
}

// AdminTokenWarningWindow is how long before an admin token expires status
// and the server log start warning.
const AdminTokenWarningWindow = 14 * 24 * time.Hour
