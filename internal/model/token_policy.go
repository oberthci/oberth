package model

import (
	"fmt"
	"time"
)

const DefaultUplinkTokenTTL = 90 * 24 * time.Hour
const MaximumUplinkTokenTTL = 365 * 24 * time.Hour

// UplinkTokenPolicy applies only when issuing a new credential. It never
// changes an existing credential's expiry, including legacy nil expiries.
type UplinkTokenPolicy struct {
	DefaultTTL time.Duration `json:"default_ttl"`
	MaxTTL     time.Duration `json:"max_ttl"`
}

func DefaultUplinkTokenPolicy() UplinkTokenPolicy {
	return UplinkTokenPolicy{DefaultTTL: DefaultUplinkTokenTTL, MaxTTL: MaximumUplinkTokenTTL}
}

func (policy UplinkTokenPolicy) Validate() error {
	if policy.DefaultTTL <= 0 || policy.MaxTTL <= 0 || policy.DefaultTTL > policy.MaxTTL || policy.MaxTTL > MaximumUplinkTokenTTL {
		return fmt.Errorf("uplink token TTLs must satisfy 0 < default <= maximum <= %s", MaximumUplinkTokenTTL)
	}
	return nil
}
