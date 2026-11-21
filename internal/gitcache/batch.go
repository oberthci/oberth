package gitcache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// One native batch process streams every regular blob; neither pipe holds a
// whole object or the inventory. All workers are joined before returning.
func (c *Cache) batch(ctx context.Context, store string, consume func(io.Writer, io.Reader) error) error {
	return c.run(ctx, command{dir: store, args: []string{"cat-file", "--batch"}, exchange: consume})
}

type batchObjects struct {
	input  io.Writer
	output *bufio.Reader
}

func (b batchObjects) copy(ctx context.Context, entry treeEntry, dest io.Writer) error {
	if _, err := fmt.Fprintln(b.input, entry.oid); err != nil {
		return err
	}
	header, err := b.output.ReadString('\n')
	if err != nil {
		return err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != entry.oid || fields[1] != "blob" {
		return refuse("batch object")
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size != entry.size {
		return refuse("batch size")
	}
	if _, err = io.CopyN(dest, &contextReader{ctx: ctx, reader: b.output}, size); err != nil {
		return err
	}
	end, err := b.output.ReadByte()
	if err != nil {
		return err
	}
	if end != '\n' {
		return refuse("batch framing")
	}
	return nil
}
