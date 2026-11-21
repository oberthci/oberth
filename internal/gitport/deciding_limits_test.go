package gitport

import (
	"errors"
	"testing"
)

func TestSelectedFileAcceptsBothRegularModesAtItsByteLimit(t *testing.T) {
	revision := request().Source
	for _, mode := range []uint32{0100644, 0100755} {
		for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
			file := File{Path: ".oberth/build.yaml", Blob: revision.Commit, Mode: mode, Bytes: make([]byte, size)}
			err := file.Validate(revision)
			if size <= 1<<20 {
				if err != nil {
					t.Fatal("bounded regular file refused", mode, size, err)
				}
			} else {
				var refusal *AuthorRefusalError
				if !errors.As(err, &refusal) || refusal.Path != "file bytes" || refusal.Code != "too_large" {
					t.Fatal("oversize file not refused at its byte bound", mode, err)
				}
			}
		}
	}
}
