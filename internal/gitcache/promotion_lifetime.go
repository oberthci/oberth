package gitcache

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitoid"
)

const PromotionLifetime = 24 * time.Hour

// Expiry is in the owner ref name, so restart needs no second promotion store.
func (c *Cache) preparePromotionOwner(ctx context.Context, p, receipt string) (string, error) {
	owners, err := c.promotionOwners(ctx, p, receipt)
	if err != nil {
		return "", err
	}
	for _, owner := range owners {
		if c.livePromotion(owner) {
			return owner, nil
		}
	}
	return promotionOwner(receipt) + "-" + strconv.FormatInt(c.config.Now().Add(PromotionLifetime).UnixNano(), 10), nil
}
func (c *Cache) promotionOwners(ctx context.Context, p, receipt string) ([]string, error) {
	prefix := promotionOwner(receipt) + "-"
	data,
		err := c.capture(ctx,
		command{dir: p,
			args: []string{"for-each-ref",
				"--format=%(refname)",
				"refs/oberth/keep/" + prefix + "*/*"}},
		2<<20)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var result []string
	for _, ref := range strings.Fields(string(data)) {
		owner := strings.Split(strings.TrimPrefix(ref, "refs/oberth/keep/"), "/")[0]
		if !seen[owner] {
			result = append(result, owner)
			seen[owner] = true
		}
	}
	return result, nil
}
func promotionExpiry(owner string) (time.Time, bool) {
	prefix := "promotion-"
	if !strings.HasPrefix(owner, prefix) {
		return time.Time{}, false
	}
	parts := strings.Split(strings.TrimPrefix(owner, prefix), "-")
	if len(parts) != 2 || len(parts[0]) != 64 || !gitoid.Valid(parts[0]) {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(parts[1], 10, 64)
	return time.Unix(0, seconds), err == nil
}
func (c *Cache) livePromotion(owner string) bool {
	expires, ok := promotionExpiry(owner)
	return ok && c.config.Now().Before(expires)
}
func (c *Cache) expiredPromotion(owner string) bool {
	expires, ok := promotionExpiry(owner)
	return ok && !c.config.Now().Before(expires)
}
func (c *Cache) promotionResult(ctx context.Context, p, receipt, oid string) error {
	owners, err := c.promotionOwners(ctx, p, receipt)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if c.livePromotion(owner) {
			if _, err = c.resolve(ctx, p, "", "refs/oberth/keep/"+owner+"/"+oid); err == nil {
				return nil
			}
			if !isAbsent(err) {
				return err
			}
		}
	}
	return refuse("promotion result")
}
