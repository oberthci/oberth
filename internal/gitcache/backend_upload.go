package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/revlist"
)

// serveGoUpload implements protocol v0's single-ACK negotiation. Decoding only
// UploadRequest misses the subsequent have/flush/done exchange: real clients
// wait for an ACK/NAK after each batch before sending more history.
func serveGoUpload(ctx context.Context, r *git.Repository, input io.Reader, out io.Writer, refs *packp.AdvRefs) error {
	scan := pktline.NewScanner(contextReader{ctx, input})
	var header strings.Builder
	for count := 0; ; count++ {
		if !scan.Scan() {
			return uploadScanError(scan)
		}
		line := scan.Bytes()
		if len(line) == 0 {
			if count == 0 {
				return nil
			} // ls-remote or an already up-to-date client.
			header.WriteString("0000")
			break
		}
		if count >= 4096 || header.Len()+len(line) > 1<<20 {
			return errors.New("upload request exceeds want limit")
		}
		_, _ = fmt.Fprintf(&header, "%04x%s", len(line)+4, line)
	}
	request := packp.NewUploadPackRequest()
	if err := request.Decode(strings.NewReader(header.String())); err != nil {
		return err
	}
	if len(request.Shallows) != 0 || !request.Depth.IsZero() {
		return errors.New("shallow upload is not supported")
	}
	for _, name := range request.Capabilities.All() {
		if !refs.Capabilities.Supports(name) {
			return fmt.Errorf("unsupported upload capability %s", name)
		}
	}
	// A raw SHA must select an advertised tip, never a server-private pin.
	public := map[plumbing.Hash]bool{}
	for _, hash := range refs.References {
		public[hash] = true
	}
	for _, want := range request.Wants {
		if !public[want] {
			return errors.New("upload-pack requested an unadvertised object")
		}
	}
	encoder := pktline.NewEncoder(out)
	acknowledged := false
	haveCount := 0
	haveSet := map[plumbing.Hash]bool{}
	for {
		if !scan.Scan() {
			return uploadScanError(scan)
		}
		line := strings.TrimSuffix(string(scan.Bytes()), "\n")
		if line == "done" {
			break
		}
		if line == "" {
			if !acknowledged {
				if err := encoder.EncodeString("NAK\n"); err != nil {
					return err
				}
			}
			continue
		}
		haveCount++
		if haveCount > 262144 {
			return errors.New("upload request exceeds have limit")
		}
		if !strings.HasPrefix(line, "have ") || !plumbing.IsHash(line[5:]) {
			return errors.New("invalid upload negotiation")
		}
		hash := plumbing.NewHash(line[5:])
		_, err := r.Storer.EncodedObject(plumbing.CommitObject, hash)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !haveSet[hash] {
			request.Haves = append(request.Haves, hash)
			haveSet[hash] = true
		}
		if !acknowledged {
			if err := encoder.Encodef("ACK %s\n", hash); err != nil {
				return err
			}
			acknowledged = true
		}
	}
	if !acknowledged {
		if err := encoder.EncodeString("NAK\n"); err != nil {
			return err
		}
	}
	store, ok := r.Storer.(*atomicReferenceStore)
	if !ok {
		return errors.New("atomic object store required")
	}
	objects := &contextObjectStore{atomicReferenceStore: store, ctx: ctx}
	hashes, err := revlist.Objects(objects, request.Wants, request.Haves)
	if err != nil {
		return err
	}
	// Lazy objects and the same bounded delta window as maintenance prevent a
	// large mirror from retaining all blob contents during pack construction.
	_, err = packfile.NewEncoder(out, objects, false).Encode(hashes, maintenanceDeltaWindow)
	return err
}

func uploadScanError(scan *pktline.Scanner) error {
	if err := scan.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}
