package secretstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func joinRevoke(ctx context.Context, c *Client, token []byte, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(err, c.Revoke(cleanup, token))
}

type Delivery struct {
	Parent string
	Token  []byte
	Paths  []string
	Run    func(context.Context, string) error
}

// Deliver calls Run only with the complete private set and removes it on return.
func (c *Client) Deliver(ctx context.Context, cfg Config, d Delivery) error {
	return c.deliver(ctx, cfg, d, memoryDirectory)
}
func (c *Client) deliver(ctx context.Context, cfg Config, d Delivery, memory func(string) error) (err error) {
	return deliver(ctx, d, memory, func(ctx context.Context) (map[string][]byte, error) { return c.Fetch(ctx, cfg, d.Token, d.Paths) })
}

// DeliverBroker fetches with the requesting Pod's identity through the private
// server listener, then uses the same private-memory delivery contract.
func (c *Client) DeliverBroker(ctx context.Context, d Delivery) error {
	return deliver(ctx, d, memoryDirectory, func(ctx context.Context) (map[string][]byte, error) { return c.fetchBroker(ctx, d.Token, d.Paths) })
}

func deliver(ctx context.Context, d Delivery, memory func(string) error, fetch func(context.Context) (map[string][]byte, error)) (err error) {
	if d.Run == nil {
		return refuse("input", nil)
	}
	if err = memory(d.Parent); err != nil {
		return err
	}
	values, err := fetch(ctx)
	if err != nil {
		return err
	}
	defer func() {
		for _, value := range values {
			clear(value)
		}
	}()
	dir, err := os.MkdirTemp(d.Parent, ".oberth-secrets-")
	if err != nil {
		return refuse("delivery", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	for path, value := range values {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := filepath.Join(dir, path)
		if err = os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return refuse("delivery", err)
		}
		if err = os.WriteFile(name, value, 0600); err != nil {
			return refuse("delivery", err)
		}
	}
	return d.Run(ctx, dir)
}
