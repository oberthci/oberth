package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRetiredNoopFlagsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, []string, io.Writer) error
		args []string
	}{
		{"validate timeout", runValidate, []string{"--timeout=1s", "--help"}},
		{"validate static", runValidate, []string{"--static", "--help"}},
		{"init type", runInit, []string{"--type=go", "--help"}},
		{"install dev", func(ctx context.Context, a []string, w io.Writer) error {
			return runInstall(ctx, a, strings.NewReader(""), w)
		}, []string{"--dev", "--help"}},
		{"install production", func(ctx context.Context, a []string, w io.Writer) error {
			return runInstall(ctx, a, strings.NewReader(""), w)
		}, []string{"--production", "--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			err := tc.run(t.Context(), tc.args, &out)
			if !errors.Is(err, errUsage) {
				t.Fatalf("retired flag accepted: %v %s", err, &out)
			}
		})
	}
}
