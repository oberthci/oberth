package gitcache

import (
	"context"
	"errors"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

type goFileTransport struct{}

func (goFileTransport) NewUploadPackSession(ep *transport.Endpoint, _ transport.AuthMethod) (transport.UploadPackSession, error) {
	r, closeStorage, err := openGoRepository(ep.Path)
	if err != nil {
		return nil, err
	}
	session, err := server.NewClient(fixedRepositoryLoader{store: r.Storer}).NewUploadPackSession(ep, nil)
	if err != nil {
		closeStorage()
		return nil, err
	}
	return &fileUploadSession{UploadPackSession: session, r: r, closeStorage: closeStorage}, nil
}
func (goFileTransport) NewReceivePackSession(ep *transport.Endpoint, _ transport.AuthMethod) (transport.ReceivePackSession, error) {
	r, closeStorage, err := openGoRepository(ep.Path)
	if err != nil {
		return nil, err
	}
	session, err := server.NewServer(fixedRepositoryLoader{store: r.Storer}).NewReceivePackSession(ep, nil)
	if err != nil {
		closeStorage()
		return nil, err
	}
	return &fileReceiveSession{ReceivePackSession: session, r: r, closeStorage: closeStorage}, nil
}

type fileUploadSession struct {
	transport.UploadPackSession
	r            *git.Repository
	closeStorage func()
}

func (s *fileUploadSession) Close() error { s.closeStorage(); return s.UploadPackSession.Close() }
func (s *fileUploadSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}
func (s *fileUploadSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	refs, err := s.UploadPackSession.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	_ = refs.Capabilities.Set(capability.AllowTipSHA1InWant)
	return refs, nil
}
func (s *fileUploadSession) UploadPack(ctx context.Context, request *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	// A client can have commits the remote has never seen. The embedded
	// go-git server assumes every have exists locally; omit unknown haves.
	copy := *request
	copy.Haves = nil
	for _, h := range request.Haves {
		if err := s.r.Storer.HasEncodedObject(h); err == nil {
			copy.Haves = append(copy.Haves, h)
		}
	}
	return s.UploadPackSession.UploadPack(ctx, &copy)
}

type fileReceiveSession struct {
	transport.ReceivePackSession
	r            *git.Repository
	closeStorage func()
}

func (s *fileReceiveSession) Close() error { s.closeStorage(); return s.ReceivePackSession.Close() }
func (s *fileReceiveSession) ReceivePack(ctx context.Context, request *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	report := packp.NewReportStatus()
	report.UnpackStatus = "ok"
	needsPack := false
	for _, cmd := range request.Commands {
		if !cmd.New.IsZero() {
			needsPack = true
		}
	}
	if request.Packfile != nil && needsPack {
		err := packfile.UpdateObjectStorage(s.r.Storer, contextReader{ctx, request.Packfile})
		closeErr := request.Packfile.Close()
		if err != nil || closeErr != nil {
			failure := errors.Join(err, closeErr)
			report.UnpackStatus = failure.Error()
			return report, failure
		}
	}
	var first error
	for _, cmd := range request.Commands {
		err := ctx.Err()
		if err == nil {
			err = applyReceiveRef(s.r, cmd)
		}
		status := "ok"
		if err != nil {
			status = err.Error()
			if first == nil {
				first = err
			}
		}
		report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{ReferenceName: cmd.Name, Status: status})
	}
	return report, first
}
