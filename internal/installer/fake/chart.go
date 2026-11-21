// Package fake supplies an offline chart port with native rendering and release history.
//
// Spec: docs/plans/c9-install-and-upgrade.md#owned-data-and-public-contract
package fake

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/installer"
)

// Chart records public requests and can inject a lost apply reply.
type Chart struct {
	History           installer.History
	Previews, Applies []installer.ChartRequest
	Fail              error
}

func (c *Chart) Read(ctx context.Context, _ string) (installer.History, error) {
	h := c.History
	h.Verified = h.Verified || h.Latest.Kind == "verified"
	if h.Deployed.Installer() {
		h.Choices = h.Deployed
	}
	return h, ctx.Err()
}
func (c *Chart) Preview(ctx context.Context, r installer.ChartRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := installer.Render(r.Chart, r.Values, r.Namespace); err != nil {
		return err
	}
	c.Previews = append(c.Previews, r)
	return nil
}
func (c *Chart) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := r.CheckHistory(c.History); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := installer.Render(r.Chart, r.Values, r.Namespace); err != nil {
		return err
	}
	if r.RollbackID != "" && (c.History.Latest.Number != r.Revision || !c.History.Latest.Installer()) {
		return errors.New("release changed")
	}
	c.Applies = append(c.Applies, r)
	if r.Recovery != nil {
		r.Revision = r.Recovery.Pending
	}

	c.History = installer.History{Verified: c.History.Verified || !r.Local, Deployed: installer.Revision{
		Number:     r.Revision + 1,
		Kind:       r.Kind(),
		RollbackID: r.RollbackID,
		Status:     "deployed",
		Version:    r.Chart.Metadata.AppVersion,
		Values:     r.Values,
	}}
	c.History.Latest = c.History.Deployed
	return c.Fail
}
