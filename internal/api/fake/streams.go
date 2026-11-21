package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"strings"
)

type Streams struct {
	Open    func(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error)
	Check   func(context.Context, apicontract.Caller, string) error
	Archive func(context.Context, apicontract.Caller, apicontract.Empty) (io.ReadCloser, error)
}

func (s *Streams) OpenLog(ctx context.Context, c apicontract.Caller, a apicontract.ContainerArgs) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Open != nil {
		return s.Open(ctx, c, a)
	}
	return io.NopCloser(strings.NewReader("native bytes\n")), nil
}
func (s *Streams) CheckLog(ctx context.Context, c apicontract.Caller, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Check != nil {
		return s.Check(ctx, c, id)
	}
	return nil
}
func (s *Streams) Backup(ctx context.Context, c apicontract.Caller, a apicontract.Empty) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Archive != nil {
		return s.Archive(ctx, c, a)
	}
	return io.NopCloser(strings.NewReader("archive bytes")), nil
}
