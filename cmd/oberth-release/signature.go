package main

import (
	"context"
	"fmt"
	"os"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func verifySignature(ctx context.Context, o options) error {
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := releaseverify.VerifySignature(ctx, root.FS()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "Release signature verified.")
	return err
}
