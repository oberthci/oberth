package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

const installationBackupFormat = "oberth-installation-v1"

type bundleMember struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type bundleManifest struct {
	Members []bundleMember         `json:"members"`
	Report  stateport.BackupReport `json:"report"`
}

type snapshotStream func(context.Context, io.Writer, int64) error

type installationBundle struct {
	info     installationBackupInfo
	secrets  []byte
	database SnapshotPort
	store    snapshotStream
}

func (b installationBundle) write(ctx context.Context, out io.Writer, options stateport.Options) (stateport.BackupReport, error) {
	info, secrets, database, store := b.info, b.secrets, b.database, b.store
	w := multipart.NewWriter(out)
	if _, err := fmt.Fprintln(out, installationBackupFormat+" "+w.Boundary()); err != nil {
		return stateport.BackupReport{}, err
	}
	var manifest bundleManifest
	member := func(name string, write func(io.Writer) error) error {
		item, err := writeBundleMember(w, name, write)
		manifest.Members = append(manifest.Members, item)
		return err
	}
	if err := member("installation.json", func(out io.Writer) error { return json.NewEncoder(out).Encode(info) }); err != nil {
		return manifest.Report, err
	}
	if err := member("database.tar", func(out io.Writer) (err error) {
		manifest.Report, err = database.Snapshot(ctx, options, out)
		return err
	}); err != nil {
		return manifest.Report, err
	}
	if err := member("installation-secrets.json", func(out io.Writer) error { _, err := out.Write(secrets); return err }); err != nil {
		return manifest.Report, err
	}
	if info.StoreIncluded {
		limit, _ := options.Limit()
		if err := member("store.snapshot", func(out io.Writer) error { return store(ctx, out, limit) }); err != nil {
			return manifest.Report, err
		}
	}
	if _, err := writeBundleMember(w, "manifest.json", func(out io.Writer) error { return json.NewEncoder(out).Encode(manifest) }); err != nil {
		return manifest.Report, err
	}
	return manifest.Report, w.Close()
}

type countingWriter struct {
	out io.Writer
	n   int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.n += int64(n)
	return n, err
}

func writeBundleMember(w *multipart.Writer, name string, write func(io.Writer) error) (bundleMember, error) {
	header := textproto.MIMEHeader{"Content-Disposition": {`attachment; filename="` + name + `"`}, "Content-Type": {"application/octet-stream"}}
	part, err := w.CreatePart(header)
	if err != nil {
		return bundleMember{}, err
	}
	digest := sha256.New()
	count := &countingWriter{out: io.MultiWriter(part, digest)}
	err = write(count)
	return bundleMember{Name: name, Bytes: count.n, SHA256: hex.EncodeToString(digest.Sum(nil))}, err
}

func recoveryProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Installation backup is incomplete.", "Check installation ownership, backup access and store readiness; retry the complete backup.", nil)
}
