package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

func (c *Cache) runBoundedReceive(ctx context.Context, r ReceiveRequest, transport command) error {
	detail := &nativeDetail{}
	output := &receiveOutput{next: transport.output}
	transport.diagnostics = detail
	transport.output = output
	err := c.run(ctx, transport)
	if len(output.frame) != 0 {
		err = errors.Join(err, unavailable("git", io.ErrUnexpectedEOF))
	}
	native := detail.String() + output.detail.String()
	if native != "" && c.config.Log != nil {
		c.config.Log("receive-pack", native)
	}
	if strings.Contains(native, "pack exceeds maximum allowed size") {
		refusal := &RefusalError{reason: fmt.Sprintf("pack exceeds %d bytes", c.config.MaxInputBytes), remedy: "Push a smaller pack or ask the operator to raise the receive limit."}
		return &reportedError{errors.Join(refusal, writeFeedback(r.Diagnostics, []string{refusal.Error()}))}
	}
	return err
}
