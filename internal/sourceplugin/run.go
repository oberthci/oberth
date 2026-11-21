package sourceplugin

import (
	"context"
	"fmt"
	"io"
)

func Run(ctx context.Context, args []string, cfg Config, output io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(output, failure(reasonSocket))
		return 1
	}
	if cfg.Log == nil {
		cfg.Log = func(message string) { _, _ = fmt.Fprintln(output, message) }
	}
	client, err := New(cfg)
	if err == nil {
		defer client.Close()
		err = Serve(ctx, args[0], client)
	}
	if err != nil {
		_, _ = fmt.Fprintln(output, err)
		return 1
	}
	return 0
}
