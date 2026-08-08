package runlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxMarkerBytes = 64 << 10
	maxReadBytes   = 4 << 20
)

var ErrLogSliceTooLarge = errors.New("run log slice exceeds response limit")

type Range struct {
	Burn  string `json:"burn"`
	Step  string `json:"step,omitempty"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type Index struct {
	RunID  string  `json:"run_id"`
	Size   int64   `json:"size"`
	Ranges []Range `json:"ranges"`
}

type Store struct {
	directory string
}

func Open(directory string) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("log directory is required")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	return &Store{directory: directory}, nil
}

func (store *Store) Create(runID string) (*os.File, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, err
	}
	// logPath validates the run ID and proves the resulting path remains under
	// the configured log directory.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("create run log: %w", err)
	}
	return file, nil
}

func (store *Store) BuildIndex(runID string) (Index, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return Index{}, err
	}
	// logPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return Index{}, fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	index, err := indexReader(runID, file)
	if err != nil {
		return Index{}, err
	}
	if err := store.writeIndex(runID, index); err != nil {
		return Index{}, err
	}
	return index, nil
}

func (store *Store) Read(runID, burn, step string) ([]byte, error) {
	index, err := store.loadIndex(runID)
	if err != nil {
		return nil, err
	}
	var selected []Range
	var total int64
	for _, candidate := range index.Ranges {
		if candidate.Burn != burn || (step != "" && candidate.Step != step) {
			continue
		}
		length := candidate.End - candidate.Start
		if length < 0 || length > maxReadBytes || total > maxReadBytes-length {
			return nil, ErrLogSliceTooLarge
		}
		total += length
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	path, _ := store.logPath(runID)
	// logPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	body := make([]byte, int(total))
	var offset int
	for _, wanted := range selected {
		if _, err := file.Seek(wanted.Start, io.SeekStart); err != nil {
			return nil, err
		}
		length := int(wanted.End - wanted.Start)
		if _, err := io.ReadFull(file, body[offset:offset+length]); err != nil {
			return nil, err
		}
		offset += length
	}
	return body, nil
}

func (store *Store) Tail(runID string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return []byte{}, nil
	}
	path, err := store.logPath(runID)
	if err != nil {
		return nil, err
	}
	// logPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := max(int64(0), info.Size()-maximum)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, maximum))
}

func indexReader(runID string, reader io.Reader) (Index, error) {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	var offset int64
	index := Index{RunID: runID, Ranges: []Range{}}
	for {
		lineStart := offset
		lineLength := 0
		prefix := make([]byte, 0, maxMarkerBytes)
		var readErr error
		for {
			fragment, err := buffered.ReadSlice('\n')
			lineLength += len(fragment)
			offset += int64(len(fragment))
			if remaining := maxMarkerBytes - len(prefix); remaining > 0 {
				prefix = append(prefix, fragment[:min(remaining, len(fragment))]...)
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			readErr = err
			break
		}
		if lineLength == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		burn, step, found := ParseMarker(prefix)
		if found {
			length := len(index.Ranges)
			if length == 0 || index.Ranges[length-1].Burn != burn || index.Ranges[length-1].Step != step {
				if length > 0 {
					index.Ranges[length-1].End = lineStart
				}
				index.Ranges = append(index.Ranges, Range{Burn: burn, Step: step, Start: lineStart})
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return Index{}, fmt.Errorf("scan run log: %w", readErr)
		}
	}
	if length := len(index.Ranges); length > 0 {
		index.Ranges[length-1].End = offset
	}
	index.Size = offset
	return index, nil
}

// ParseMarker returns the burn and step in a runner log line prefix.
func ParseMarker(line []byte) (string, string, bool) {
	text := string(line)
	if !strings.HasPrefix(text, "[") {
		return "", "", false
	}
	end := strings.IndexByte(text, ']')
	if end < 4 {
		return "", "", false
	}
	burn, step, found := strings.Cut(text[1:end], "/")
	if !found || burn == "" || step == "" || strings.ContainsAny(burn+step, "\x00\r\n[]") {
		return "", "", false
	}
	return burn, step, true
}

func (store *Store) writeIndex(runID string, index Index) error {
	path, err := store.indexPath(runID)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(store.directory, ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("create index replacement: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := json.NewEncoder(temporary).Encode(index); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode log index: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace log index: %w", err)
	}
	return nil
}

func (store *Store) loadIndex(runID string) (Index, error) {
	path, err := store.indexPath(runID)
	if err != nil {
		return Index{}, err
	}
	// indexPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return Index{}, fmt.Errorf("open log index: %w", err)
	}
	defer func() { _ = file.Close() }()
	var index Index
	decoder := json.NewDecoder(io.LimitReader(file, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return Index{}, fmt.Errorf("decode log index: %w", err)
	}
	if index.RunID != runID {
		return Index{}, errors.New("log index run ID mismatch")
	}
	return index, nil
}

func (store *Store) logPath(runID string) (string, error) {
	if !safeID(runID) {
		return "", errors.New("invalid run ID")
	}
	return filepath.Join(store.directory, runID+".log"), nil
}

func (store *Store) indexPath(runID string) (string, error) {
	if !safeID(runID) {
		return "", errors.New("invalid run ID")
	}
	return filepath.Join(store.directory, runID+".index.json"), nil
}

func safeID(value string) bool {
	if value == "" || len(value) > 80 || strings.HasPrefix(value, ".") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
