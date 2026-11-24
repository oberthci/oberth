package secretstore

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/runtimeconfig"
)

type Config runtimeconfig.SecretStoreAccess

type Change struct {
	Path  string
	Value map[string]any
}
type response struct {
	Data map[string]any `json:"data"`
}

func validName(s string) bool { return runtimeconfig.ValidStoreName(s) }
func (c Config) Validate() error {
	if !runtimeconfig.SecretStoreAccess(c).Valid() || c.WriterRole == BackupRole || c.StepRole == BackupRole || c.ServerAccount == BackupRole {
		return refuse("input", nil)
	}
	return nil
}
func (c *Client) entries(ctx context.Context, token []byte, path string) (map[string]any, error) {
	var r response
	_, err := c.JSON(ctx, Request{Method: "GET", Path: path, Token: token}, &r)
	return r.Data, err
}

// Setup only creates missing resources; differing owned settings are never overwritten.
func (c *Client) Setup(ctx context.Context, token []byte, cfg Config) error {
	plan, accessor, err := c.configuration(ctx, token, cfg)
	if err != nil {
		return err
	}
	if accessor == "" {
		auth, err := c.enableAuth(ctx, token, cfg)
		if err != nil {
			return err
		}
		accessor, err = authAccessor(auth[cfg.AuthMount+"/"])
		if err != nil || accessor == "" {
			return refuse("response", err)
		}
	}
	resources, err := c.Plan(ctx, token, cfg.resources(accessor))
	if err != nil {
		return err
	}
	if err = c.apply(ctx, token, append(plan, resources...)); err != nil {
		return err
	}
	return c.Check(ctx, token, cfg)
}

// configuration reads the shared mount/auth facts once per verification pass.
func (c *Client) configuration(ctx context.Context, token []byte, cfg Config) ([]Change, string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	mounts, err := c.entries(ctx, token, "/sys/mounts")
	if err != nil {
		return nil, "", err
	}
	want := map[string]any{"type": "kv", "options": map[string]any{"version": "1"}}
	missing, err := matches(mounts[cfg.Mount+"/"], want)
	if err != nil {
		return nil, "", err
	}
	var plan []Change
	if missing {
		plan = append(plan, Change{Path: "/sys/mounts/" + cfg.Mount, Value: want})
	}
	auth, err := c.entries(ctx, token, "/sys/auth")
	if err != nil {
		return nil, "", err
	}
	accessor, err := authAccessor(auth[cfg.AuthMount+"/"])
	return plan, accessor, err
}
func authAccessor(info any) (string, error) {
	missing, err := matches(info, map[string]any{"type": "kubernetes"})
	if err != nil || missing {
		return "", err
	}
	accessor, _ := info.(map[string]any)["accessor"].(string)
	if !validName(accessor) {
		return "", refuse("response", nil)
	}
	return accessor, nil
}

func matches(got any, want map[string]any) (bool, error) {
	if got == nil {
		return true, nil
	}
	data, ok := got.(map[string]any)
	if !ok {
		return false, refuse("response", nil)
	}
	for key, value := range want {
		a, _ := json.Marshal(data[key])
		b, _ := json.Marshal(value)
		if string(a) != string(b) {
			return false, refuse("drift", fmt.Errorf("setting %s differs", key))
		}
	}
	return false, nil
}

// Plan is read-only and exposes public configuration changes only.
func (c *Client) Plan(ctx context.Context, token []byte, want []Change) ([]Change, error) {
	var plan []Change
	for _, change := range want {
		var r response
		status, err := c.JSON(ctx, Request{Method: "GET", Path: change.Path, Token: token, Accept: 404}, &r)
		if err != nil {
			return nil, err
		}
		if status == 404 {
			plan = append(plan, change)
			continue
		}
		if r.Data == nil {
			return nil, refuse("response", nil)
		}
		if _, err = matches(r.Data, change.Value); err != nil {
			return nil, fmt.Errorf("%s: %w", change.Path, err)
		}
	}
	return plan, nil
}
func (c *Client) Check(ctx context.Context, token []byte, cfg Config) error {
	plan, accessor, err := c.configuration(ctx, token, cfg)
	if err != nil {
		return err
	}
	if len(plan) != 0 || accessor == "" {
		return refuse("drift", nil)
	}
	plan, err = c.Plan(ctx, token, cfg.resources(accessor))
	if err == nil && len(plan) != 0 {
		return refuse("drift", nil)
	}
	return err
}

func (c *Client) enableAuth(ctx context.Context, token []byte, cfg Config) (map[string]any, error) {
	for _, name := range []string{cfg.WriterRole, cfg.StepRole} {
		var old response
		status, err := c.JSON(ctx, Request{Method: "GET", Path: "/sys/policies/acl/" + name, Token: token, Accept: 404}, &old)
		if err != nil {
			return nil, err
		}
		if status != 404 {
			return nil, refuse("drift", fmt.Errorf("policy %s exists without its auth mount", name))
		}
	}
	_, err := c.JSON(ctx, Request{Method: "POST", Path: "/sys/auth/" + cfg.AuthMount, Token: token, Body: map[string]any{"type": "kubernetes"}}, nil)
	if err != nil {
		return nil, err
	}
	return c.entries(ctx, token, "/sys/auth")
}

func (c *Client) apply(ctx context.Context, token []byte, plan []Change) error {
	for _, change := range plan {
		if _, err := c.JSON(ctx, Request{Method: "POST", Path: change.Path, Token: token, Body: change.Value}, nil); err != nil {
			return err
		}
	}
	return nil
}
