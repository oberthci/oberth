package store

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/stateport"
)

type member struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Format   string             `json:"format"`
	Protocol stateport.Protocol `json:"protocol"`
	stateport.BackupReport
	Members []member `json:"members"`
}

type archiveOutput struct {
	path    string
	options stateport.Options
	report  stateport.BackupReport
	out     io.Writer
}

func emitArchive(ctx context.Context, output archiveOutput) (resultErr error) {
	file, err := os.Open(output.path)
	if err != nil {
		return databaseError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return databaseError(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, Reader: file}); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	m := manifest{
		Format:       "oberth.database-backup/v1",
		Protocol:     output.options.Protocol,
		BackupReport: output.report,
		Members:      []member{{"oberth.sqlite", info.Size(), hex.EncodeToString(hash.Sum(nil))}},
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	archive := tar.NewWriter(contextWriter{ctx, output.out})
	if err := archive.WriteHeader(archiveHeader("oberth.sqlite", info.Size())); err != nil {
		return err
	}
	if _, err := io.Copy(archive, contextReader{ctx: ctx, Reader: file}); err != nil {
		return err
	}
	if err := archive.WriteHeader(archiveHeader("manifest.json", int64(len(body)))); err != nil {
		return err
	}
	if _, err := archive.Write(body); err != nil {
		return err
	}
	return errors.Join(archive.Close(), ctx.Err())
}

func archiveHeader(name string, size int64) *tar.Header {
	return &tar.Header{Name: name, Size: size, Mode: 0600, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(body []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(body)
	if cause := r.ctx.Err(); cause != nil {
		return n, errors.Join(err, cause)
	}
	return n, err
}

type contextWriter struct {
	ctx context.Context
	io.Writer
}

func (w contextWriter) Write(body []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.Writer.Write(body)
	return n, errors.Join(err, w.ctx.Err())
}

func inspectArchive(ctx context.Context, options stateport.Options, in io.Reader, stage string) (stateport.BackupReport, error) {
	limit, _ := options.Limit()
	counter := contextReader{ctx, io.LimitReader(in, limit+(64<<10)+1)}
	reader := tar.NewReader(counter)
	item, err := extractDatabase(ctx, reader, stage, limit)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	body, err := readManifest(reader)
	if err != nil {
		return stateport.BackupReport{}, refuse(damagedArchive, err)
	}
	if err := archiveEnd(reader, counter); err != nil {
		return stateport.BackupReport{}, err
	}
	m, err := decodeManifest(body, item)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	report, err := verifyDatabase(ctx, filepath.Join(stage, "oberth.sqlite"), options)
	if err != nil {
		return report, err
	}
	if report != m.BackupReport {
		return report, refuse(damagedArchive, nil)
	}
	return report, ctx.Err()
}

func extractDatabase(ctx context.Context, reader *tar.Reader, stage string, limit int64) (member, error) {
	header, err := reader.Next()
	if err != nil {
		return member{}, refuse(damagedArchive, err)
	}
	if !validHeader(header, "oberth.sqlite") {
		return member{}, refuse(damagedArchive, nil)
	}
	if header.Size > limit {
		return member{}, sizeError(header.Size, limit)
	}
	file, err := os.OpenFile(filepath.Join(stage, "oberth.sqlite"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return member{}, databaseError(err)
	}
	hash := sha256.New()
	n, copyErr := io.CopyN(io.MultiWriter(file, hash), contextReader{ctx, reader}, header.Size)
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return member{}, archiveError(err)
	}
	return member{Name: "oberth.sqlite", Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func readManifest(reader *tar.Reader) ([]byte, error) {
	header, err := reader.Next()
	if err != nil {
		return nil, err
	}
	if !validHeader(header, "manifest.json") || header.Size > 8192 {
		return nil, refuse(damagedArchive, nil)
	}
	return io.ReadAll(io.LimitReader(reader, 8193))
}

func validHeader(h *tar.Header, name string) bool {
	return h.Name == name && h.Size > 0 && h.Mode == 0600 && h.Typeflag == tar.TypeReg && h.Linkname == "" && len(h.PAXRecords) == 0 &&
		h.Uid == 0 && h.Gid == 0
}

func archiveEnd(reader *tar.Reader, counter io.Reader) error {
	_, err := reader.Next()
	if !errors.Is(err, io.EOF) {
		return refuse(damagedArchive, err)
	}
	var tail [1]byte
	n, err := counter.Read(tail[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		return refuse(damagedArchive, err)
	}
	return nil
}

func decodeManifest(body []byte, item member) (manifest, error) {
	var m manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, refuse(damagedArchive, err)
	}
	if m.Format != "oberth.database-backup/v1" || len(m.Members) != 1 || m.Members[0] != item {
		return m, refuse(damagedArchive, nil)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return m, refuse(damagedArchive, err)
	}
	return m, nil
}

func archiveError(err error) error {
	var path *os.PathError
	if errors.As(err, &path) {
		return databaseError(err)
	}
	return refuse(damagedArchive, err)
}
