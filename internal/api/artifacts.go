package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// ArtifactSHA256Header carries the lowercase hex SHA-256 of the exact bytes a
// raw artifact download sends. Clients hash what they receive and refuse the
// download on any difference (#1011).
const ArtifactSHA256Header = "X-Oberth-Artifact-SHA256"

// artifactWriteIdle bounds how long one chunk of a raw download may take to
// reach the client. The deadline is renewed after every chunk, so a large
// artifact on a slow link is not cut off by the server-wide WriteTimeout while
// a stalled client still releases its descriptor.
const artifactWriteIdle = time.Minute

const artifactCopyChunk = 1 << 20

// ArtifactObject is one retained artifact opened for a raw download. Content
// is positioned at its first byte; Size and SHA256 describe exactly the bytes
// it yields. The handler owns Content and closes it.
type ArtifactObject struct {
	Name    string
	Size    int64
	SHA256  string
	Content io.ReadCloser
}

// ArtifactDownloads opens retained artifacts as raw byte streams. It is kept
// apart from the filtered text view (RunArtifact) so the download route never
// inherits a text-view ceiling: the old route served the filtered view and
// silently stopped at its 4 MiB bound (#1011).
type ArtifactDownloads interface {
	OpenRunArtifact(ctx context.Context, actor Actor, id, name string) (ArtifactObject, error)
}

func (server *Server) handleRunArtifacts(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run")
	if !validRunSelector(id) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "a run ID is required"})
		return
	}
	value, err := server.views.RunArtifacts(request.Context(), actorFrom(request.Context()), id)
	server.writeView(writer, value, err)
}

func (server *Server) handleRunArtifactDownload(writer http.ResponseWriter, request *http.Request) {
	if _, present := request.URL.Query()["byte_offset"]; present {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "byte_offset is only supported for run logs, not artifact downloads"})
		return
	}
	id := request.PathValue("run")
	name := request.PathValue("name")
	if !validRunSelector(id) || strings.TrimSpace(name) == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "a run ID and artifact name are required"})
		return
	}
	object, err := server.downloads.OpenRunArtifact(request.Context(), actorFrom(request.Context()), id, name)
	if err != nil {
		server.writeView(writer, nil, err)
		return
	}
	defer func() { _ = object.Content.Close() }()
	writeArtifactHeaders(writer, name)
	writer.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	writer.Header().Set(ArtifactSHA256Header, object.SHA256)
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	written, err := copyArtifact(writer, object.Content, object.Size)
	if err != nil || written != object.Size {
		// The status line is already sent. Abort the connection instead of
		// ending the response cleanly, so the client sees a body shorter
		// than Content-Length rather than a short success.
		if !errors.Is(err, context.Canceled) {
			log.Printf("api: artifact download aborted after %d of %d bytes: %v", written, object.Size, err)
		}
		panic(http.ErrAbortHandler)
	}
}

// copyArtifact streams exactly size bytes, renewing the write deadline per
// chunk. A source that ends early returns io.ErrUnexpectedEOF.
func copyArtifact(writer http.ResponseWriter, source io.Reader, size int64) (int64, error) {
	controller := http.NewResponseController(writer)
	buffer := make([]byte, artifactCopyChunk)
	var written int64
	for written < size {
		want := min(int64(len(buffer)), size-written)
		count, readErr := io.ReadFull(source, buffer[:want])
		if count > 0 {
			_ = controller.SetWriteDeadline(time.Now().Add(artifactWriteIdle))
			// #nosec G705 -- served as application/octet-stream attachment with
			// X-Content-Type-Options: nosniff and CSP default-src 'none'; sandbox.
			// The response is a forced download, never rendered.
			sent, writeErr := writer.Write(buffer[:count])
			written += int64(sent)
			if writeErr != nil {
				return written, writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				readErr = io.ErrUnexpectedEOF
			}
			return written, readErr
		}
	}
	return written, nil
}

func writeArtifactHeaders(writer http.ResponseWriter, name string) {
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(name)+"\"")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	writer.Header().Set("Cache-Control", "no-store")
}
