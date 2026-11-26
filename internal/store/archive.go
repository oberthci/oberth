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
	"slices"

	"github.com/oberthci/oberth/internal/publicerr"
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
	pins    []byte
}

// The server's forge host pins live beside the database outside it. They were
// confirmed in a reviewed upstream add, so an archive carries them as an
// optional member and a restore writes them back.
const (
	hostPinsName  = "known_hosts"
	hostPinsLimit = 64 << 10
)

// HostPinsPath is the forge host pin file kept beside a database file.
func HostPinsPath(database string) string {
	return filepath.Join(filepath.Dir(database), "host-pins", hostPinsName)
}

func readHostPins(database string) ([]byte, error) {
	path := HostPinsPath(database)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > hostPinsLimit {
		return nil, databaseError(errors.Join(err, errors.New("host pins are not a bounded regular file")))
	}
	pins, err := os.ReadFile(path)
	if err != nil || len(pins) > hostPinsLimit {
		return nil, databaseError(errors.Join(err, errors.New("host pins changed while read")))
	}
	return pins, nil
}

func pinsMember(pins []byte) member {
	sum := sha256.Sum256(pins)
	return member{hostPinsName, int64(len(pins)), hex.EncodeToString(sum[:])}
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
	if len(output.pins) > 0 {
		m.Members = append(m.Members, pinsMember(output.pins))
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
	if len(output.pins) > 0 {
		if err := writeMember(archive, hostPinsName, output.pins); err != nil {
			return err
		}
	}
	if err := writeMember(archive, "manifest.json", body); err != nil {
		return err
	}
	return errors.Join(archive.Close(), ctx.Err())
}

func writeMember(archive *tar.Writer, name string, data []byte) error {
	if err := archive.WriteHeader(archiveHeader(name, int64(len(data)))); err != nil {
		return err
	}
	_, err := archive.Write(data)
	return err
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

// inspectArchive stages the database and returns the archived host pins.
func inspectArchive(ctx context.Context, options stateport.Options, in io.Reader, stage string) (report stateport.BackupReport, pins []byte, resultErr error) {
	file, err := os.OpenFile(filepath.Join(stage, "oberth.sqlite"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return stateport.BackupReport{}, nil, databaseError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	target := archiveInspection{database: file, pins: &pins, verify: func(ctx context.Context, options stateport.Options) (stateport.BackupReport, error) {
		return verifyDatabase(ctx, filepath.Join(stage, "oberth.sqlite"), options)
	}}
	report, err = inspectArchiveTo(ctx, options, in, target)
	return report, pins, err
}

type archiveInspection struct {
	database io.Writer
	pins     *[]byte
	verify   func(context.Context, stateport.Options) (stateport.BackupReport, error)
}

func inspectArchiveTo(ctx context.Context, options stateport.Options, in io.Reader, target archiveInspection) (stateport.BackupReport, error) {
	limit, _ := options.Limit()
	counter := contextReader{ctx, io.LimitReader(in, limit+hostPinsLimit+(64<<10)+1)}
	reader := tar.NewReader(counter)
	item, err := extractDatabase(ctx, reader, target.database, limit)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	pins, body, err := readTrailer(reader)
	if err != nil {
		return stateport.BackupReport{}, archiveError(err)
	}
	if err := archiveEnd(reader, counter); err != nil {
		return stateport.BackupReport{}, err
	}
	m, err := decodeManifest(body, item, pins)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	report, err := target.verify(ctx, options)
	if err != nil {
		return report, err
	}
	if report != m.BackupReport {
		return report, refuse(damagedArchive, nil)
	}
	if target.pins != nil {
		*target.pins = pins
	}
	return report, ctx.Err()
}

func extractDatabase(ctx context.Context, reader *tar.Reader, out io.Writer, limit int64) (member, error) {
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
	hash := sha256.New()
	n, err := io.CopyN(io.MultiWriter(out, hash), contextReader{ctx, reader}, header.Size)
	if err != nil {
		return member{}, archiveError(err)
	}
	return member{Name: "oberth.sqlite", Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// readTrailer reads the optional host pins member, then the manifest.
func readTrailer(reader *tar.Reader) (pins, manifest []byte, err error) {
	header, err := reader.Next()
	if err == nil && header.Name == hostPinsName {
		if !validHeader(header, hostPinsName) || header.Size > hostPinsLimit {
			return nil, nil, refuse(damagedArchive, nil)
		}
		if pins, err = io.ReadAll(io.LimitReader(reader, hostPinsLimit+1)); err != nil {
			return nil, nil, err
		}
		header, err = reader.Next()
	}
	if err != nil {
		return nil, nil, err
	}
	if header.Name != "manifest.json" {
		return nil, nil, refuse(unsupportedArchive, nil)
	}
	if !validHeader(header, "manifest.json") || header.Size > 8192 {
		return nil, nil, refuse(damagedArchive, nil)
	}
	manifest, err = io.ReadAll(io.LimitReader(reader, 8193))
	return pins, manifest, err
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

func decodeManifest(body []byte, item member, pins []byte) (manifest, error) {
	members := []member{item}
	if pins != nil {
		members = append(members, pinsMember(pins))
	}
	var m manifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, refuse(damagedArchive, err)
	}
	if m.Format != "oberth.database-backup/v1" || !slices.Equal(m.Members, members) {
		return m, refuse(damagedArchive, nil)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return m, refuse(damagedArchive, err)
	}
	return m, nil
}

func archiveError(err error) error {
	var public *publicerr.Error
	if errors.As(err, &public) {
		return err
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return databaseError(err)
	}
	return refuse(damagedArchive, err)
}
