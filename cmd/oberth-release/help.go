package main

import (
	"flag"
	"fmt"
	"io"
)

// Help and execution share the same flag definitions and defaults.
func releaseFlags(o *options) *flag.FlagSet {
	flags := flag.NewFlagSet("oberth-release", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.publicKey, "public-key-file", "", "owner public key PEM")
	flags.BoolVar(&o.replaceKey, "replace", false, "replace a different release public key")
	flags.StringVar(&o.dir, "dir", "dist", "release directory")
	flags.StringVar(&o.base, "base", "base-image", "local OCI base layout")
	flags.StringVar(&o.pin, "pin", ".oberth/pins/base-image.digest", "reviewed base digest")
	flags.StringVar(&o.chart, "chart", "charts/oberth", "chart source")
	flags.StringVar(&o.version, "version", "", "exact version")
	flags.StringVar(&o.commit, "commit", "", "source commit")
	flags.StringVar(&o.origin, "origin", "", "release origin")
	flags.StringVar(&o.token, "token-file", "", "delegated token file")
	flags.StringVar(&o.receipt, "receipt", "", "clean verification receipt")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: oberth-release <operation> [flags]")
		fmt.Fprintln(flags.Output(), usage.Remedy())
		fmt.Fprintln(flags.Output())
		flags.PrintDefaults()
	}
	return flags
}
