package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/installer"
)

func runPreload(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("preload", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	as := flags.String("as", "",
		"also tag the pulled image under this name inside the node, for a suite that names an "+
			"image with no build for this architecture (e.g. --as postgis/postgis:16-3.4)")
	cluster := flags.String("cluster", installer.KindClusterName, "kind cluster name")

	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: preload takes exactly one image reference", errUsage)
	}

	return installer.Preload(ctx, installer.InstallDeps{Output: output}, *cluster, flags.Arg(0), *as)
}
