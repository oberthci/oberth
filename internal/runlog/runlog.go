package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/oberthci/oberth/internal/runprogress"
)

const (
	maxMarkerBytes   = 64 << 10
	maxReadBytes     = 4 << 20
	maxProgressBytes = 1 << 20
	maxPatternBytes  = 512
	maxContextLines  = 50
)

var ErrLogSliceTooLarge = errors.New("run log slice exceeds response limit")

var ErrInvalidPattern = errors.New("invalid log pattern")

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

type Filter struct {
	Pattern string
	Context int
	Offset  int
	Limit   int
	Tail    bool
}

type Meta struct {
	TotalLines    int   `json:"total_lines"`
	MatchedLines  int   `json:"matched_lines"`
	ReturnedLines int   `json:"returned_lines"`
	Truncated     bool  `json:"truncated"`
	Bytes         int64 `json:"bytes"`
	LineNumbers   []int `json:"line_numbers,omitempty"`
}

type Store struct {
	directory  string
	progressMu sync.RWMutex
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
	body, meta, err := store.ReadFiltered(runID, burn, step, Filter{})
	if err != nil {
		return nil, err
	}
	if meta.Truncated {
		return nil, ErrLogSliceTooLarge
	}
	return body, nil
}

func (store *Store) ReadFiltered(runID, burn, step string, filter Filter) ([]byte, Meta, error) {
	index, err := store.loadIndex(runID)
	if err != nil {
		return nil, Meta{}, err
	}
	var selected []Range
	var total int64
	for _, candidate := range index.Ranges {
		if candidate.Burn != burn || (step != "" && candidate.Step != step) {
			continue
		}
		length := candidate.End - candidate.Start
		if length < 0 {
			continue
		}
		total += length
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, Meta{}, fmt.Errorf("log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	path, _ := store.logPath(runID)
	// logPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, Meta{}, fmt.Errorf("open run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	return filterRanges(file, selected, total, filter)
}

// ReadActive returns the most recent bounded bytes for one step from a run's
// growing redacted log. It builds an in-memory snapshot index only; the durable
// terminal index remains written exclusively by BuildIndex after Job exit.
func (store *Store) ReadActive(runID, burn, step string) ([]byte, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, err
	}
	// logPath validated the run ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open active run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	index, err := indexReader(runID, file)
	if err != nil {
		return nil, fmt.Errorf("index active run log: %w", err)
	}
	ranges := make([]Range, 0)
	for _, candidate := range index.Ranges {
		if candidate.Burn == burn && candidate.Step == step {
			ranges = append(ranges, candidate)
		}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("active log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	return readRangeTail(file, ranges, maxReadBytes)
}

func (store *Store) ReadActiveFiltered(runID, burn, step string, filter Filter) ([]byte, Meta, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, Meta{}, err
	}
	// logPath validated the run ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, Meta{}, fmt.Errorf("open active run log: %w", err)
	}
	defer func() { _ = file.Close() }()
	index, err := indexReader(runID, file)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("index active run log: %w", err)
	}
	ranges := make([]Range, 0)
	var total int64
	for _, candidate := range index.Ranges {
		if candidate.Burn == burn && candidate.Step == step {
			ranges = append(ranges, candidate)
			total += candidate.End - candidate.Start
		}
	}
	if len(ranges) == 0 {
		return nil, Meta{}, fmt.Errorf("active log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	return filterRanges(file, ranges, total, filter)
}

func readRangeTail(file *os.File, ranges []Range, maximum int64) ([]byte, error) {
	selected := make([]Range, 0, len(ranges))
	remaining := maximum
	partialFirst := false
	for index := len(ranges) - 1; index >= 0 && remaining > 0; index-- {
		candidate := ranges[index]
		length := candidate.End - candidate.Start
		if length <= 0 {
			continue
		}
		if length > remaining {
			candidate.Start = candidate.End - remaining
			length = remaining
			partialFirst = true
		}
		remaining -= length
		selected = append(selected, candidate)
	}
	slices.Reverse(selected)
	total := maximum - remaining
	body := make([]byte, int(total))
	offset := 0
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
	if partialFirst {
		if newline := bytes.IndexByte(body, '\n'); newline >= 0 && newline+1 < len(body) {
			body = body[newline+1:]
		}
	}
	return body, nil
}

// ReadFrom returns up to maximum bytes of the run's append-only log starting
// at offset, plus the next poll offset and the current log size. It serves the
// dashboard's live view of a run in progress: the file grows while the Job
// streams, so callers poll with the returned offset. A negative offset means
// "start tailing": the read begins at most maximum bytes before the current
// end so a late-joining viewer is never handed the entire log at once. When
// more bytes remain past the returned chunk, the chunk is trimmed to the last
// complete line so a multi-byte character split at the read boundary can never
// be mangled by JSON re-encoding; a chunk without any newline is returned
// as-is to guarantee forward progress.
func (store *Store) ReadFrom(runID string, offset, maximum int64) ([]byte, int64, int64, error) {
	if maximum <= 0 {
		return nil, 0, 0, errors.New("live log read requires a positive byte bound")
	}
	path, err := store.logPath(runID)
	if err != nil {
		return nil, 0, 0, err
	}
	// logPath validated the owner ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	size := info.Size()
	start := offset
	if start < 0 {
		start = max(int64(0), size-maximum)
	}
	if start > size {
		start = size
	}
	if start == size {
		return []byte{}, size, size, nil
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	chunk, err := io.ReadAll(io.LimitReader(file, maximum))
	if err != nil {
		return nil, 0, 0, err
	}
	if start+int64(len(chunk)) < size {
		if newline := bytes.LastIndexByte(chunk, '\n'); newline >= 0 {
			chunk = chunk[:newline+1]
		}
	}
	return chunk, start + int64(len(chunk)), size, nil
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

// AppendStepProgress durably journals one bounded state transition. The
// scheduler is the sole writer for a run; the lock also keeps dashboard reads
// from observing a partial record in this process.
func (store *Store) AppendStepProgress(runID string, event runprogress.Event) error {
	if err := event.Validate(); err != nil {
		return fmt.Errorf("append step progress: %w", err)
	}
	path, err := store.progressPath(runID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode step progress journal: %w", err)
	}
	body = append(body, '\n')
	store.progressMu.Lock()
	defer store.progressMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open step progress journal: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("inspect step progress journal: %w", statErr)
	}
	size := info.Size()
	if size > maxProgressBytes {
		_ = file.Close()
		return errors.New("step progress journal exceeds its byte bound")
	}
	if size > 0 {
		var last [1]byte
		if _, err := file.ReadAt(last[:], size-1); err != nil {
			_ = file.Close()
			return fmt.Errorf("inspect step progress journal tail: %w", err)
		}
		if last[0] != '\n' {
			existing := make([]byte, size)
			if _, err := file.ReadAt(existing, 0); err != nil {
				_ = file.Close()
				return fmt.Errorf("read step progress journal tail: %w", err)
			}
			lastNewline := bytes.LastIndexByte(existing, '\n')
			size = int64(lastNewline + 1)
			if err := file.Truncate(size); err != nil {
				_ = file.Close()
				return fmt.Errorf("repair step progress journal tail: %w", err)
			}
		}
	}
	if size > maxProgressBytes-int64(len(body)) {
		_ = file.Close()
		return errors.New("step progress journal exceeds its byte bound")
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return fmt.Errorf("seek step progress journal: %w", err)
	}
	written, writeErr := file.Write(body)
	if writeErr == nil && written != len(body) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write step progress journal: %w", err)
	}
	return nil
}

// WriteStepPlan records what the run intends to do, before its first Pod
// exists. It is written once per run, immediately after the pipeline object is
// submitted, and never rewritten: a plan is a statement about the reviewed
// document, and the document cannot change while the run is alive.
//
// The plan lives beside the run's log and progress journal rather than in the
// durable store because a store row is a *result*, and PutStepResult refuses
// anything non-terminal precisely so a half-finished run can never be recorded
// as one. Seeding rows there would have meant relaxing that guard, or a schema
// migration -- and store migrations past v2 are backup-and-replace by design,
// which for a visibility record would cost the audit chain, the run history,
// and every uplink token. The plan belongs with the other run-scoped,
// bounded, non-authoritative observations instead.
func (store *Store) WriteStepPlan(runID string, steps []runprogress.PlannedStep) error {
	plan := runprogress.Plan{Version: runprogress.PlanVersion, Steps: steps}
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("write step plan: %w", err)
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode step plan: %w", err)
	}
	if len(body) > runprogress.MaxPlanBytes {
		return errors.New("step plan exceeds its byte bound")
	}
	path, err := store.planPath(runID)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(store.directory, ".plan-*.tmp")
	if err != nil {
		return fmt.Errorf("create step plan: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write step plan: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace step plan: %w", err)
	}
	return nil
}

// StepPlan returns the run's planned step list. A run submitted before this
// existed, or by an engine that cannot enumerate its own document, has no plan
// file; the error wraps os.ErrNotExist so callers can treat that as "no plan"
// rather than as a failure.
func (store *Store) StepPlan(runID string) ([]runprogress.PlannedStep, error) {
	path, err := store.planPath(runID)
	if err != nil {
		return nil, err
	}
	// planPath validated the run ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open step plan: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, runprogress.MaxPlanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read step plan: %w", err)
	}
	if len(body) > runprogress.MaxPlanBytes {
		return nil, errors.New("step plan exceeds its byte bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var plan runprogress.Plan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode step plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("decode step plan: %w", err)
	}
	sort.Slice(plan.Steps, func(left, right int) bool { return plan.Steps[left].Ordinal < plan.Steps[right].Ordinal })
	return plan.Steps, nil
}

// StepProgress returns the latest transition for every planned step in stable
// ordinal order. A crash-truncated final record is ignored; every preceding
// newline-terminated record remains authoritative.
func (store *Store) StepProgress(runID string) ([]runprogress.Event, error) {
	path, err := store.progressPath(runID)
	if err != nil {
		return nil, err
	}
	store.progressMu.RLock()
	defer store.progressMu.RUnlock()
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("open step progress journal: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxProgressBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read step progress journal: %w", err)
	}
	if len(body) > maxProgressBytes {
		return nil, errors.New("step progress journal exceeds its byte bound")
	}
	lastNewline := bytes.LastIndexByte(body, '\n')
	if lastNewline < 0 {
		return []runprogress.Event{}, nil
	}
	latest := make(map[string]runprogress.Event)
	ordinals := make(map[int]string)
	for index, line := range bytes.Split(body[:lastNewline], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		event, decodeErr := runprogress.Decode(line)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode step progress journal record %d: %w", index, decodeErr)
		}
		key := event.Burn + "\x00" + event.Step
		if previous, exists := latest[key]; exists && previous.Ordinal != event.Ordinal {
			return nil, errors.New("step progress ordinal changed")
		}
		if previousKey, exists := ordinals[event.Ordinal]; exists && previousKey != key {
			return nil, errors.New("step progress ordinal is shared")
		}
		latest[key] = event
		ordinals[event.Ordinal] = key
	}
	result := make([]runprogress.Event, 0, len(latest))
	for _, event := range latest {
		result = append(result, event)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Ordinal < result[right].Ordinal })
	return result, nil
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

func (store *Store) progressPath(runID string) (string, error) {
	if !safeID(runID) {
		return "", errors.New("invalid run ID")
	}
	return filepath.Join(store.directory, runID+".progress.jsonl"), nil
}

func (store *Store) planPath(runID string) (string, error) {
	if !safeID(runID) {
		return "", errors.New("invalid run ID")
	}
	return filepath.Join(store.directory, runID+".plan.json"), nil
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

func stepContent(line string) string {
	if !strings.HasPrefix(line, "[") {
		return line
	}
	close := strings.Index(line, "] ")
	if close < 0 {
		return line
	}
	return line[close+2:]
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > maxPatternBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrInvalidPattern, maxPatternBytes)
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPattern, err)
	}
	return compiled, nil
}

type numberedLine struct {
	number int
	text   string
}

func filterRanges(file *os.File, ranges []Range, total int64, filter Filter) ([]byte, Meta, error) {
	meta := Meta{Bytes: total}
	if filter.Context < 0 || filter.Context > maxContextLines {
		return nil, Meta{}, fmt.Errorf("%w: context must be between 0 and %d", ErrInvalidPattern, maxContextLines)
	}
	pattern, err := compilePattern(filter.Pattern)
	if err != nil {
		return nil, Meta{}, err
	}
	collector := newCollector(pattern, filter)
	number := 0
	for _, wanted := range ranges {
		if _, err := file.Seek(wanted.Start, io.SeekStart); err != nil {
			return nil, Meta{}, err
		}
		reader := bufio.NewReader(io.LimitReader(file, wanted.End-wanted.Start))
		for {
			line, readErr := reader.ReadString('\n')
			if line != "" {
				number++
				meta.TotalLines++
				collector.offer(numberedLine{number: number, text: line})
			}
			if readErr != nil {
				break
			}
		}
	}
	collector.close()
	meta.MatchedLines = collector.matched
	if pattern == nil {
		meta.MatchedLines = meta.TotalLines
	}
	out, numbers, truncatedByBytes := collector.bytes(maxReadBytes)
	meta.ReturnedLines = len(numbers)
	meta.LineNumbers = numbers
	meta.Truncated = truncatedByBytes || collector.withheld()
	return out, meta, nil
}

type collector struct {
	pattern  *regexp.Regexp
	context  int
	offset   int
	limit    int
	tail     bool
	budget   int64
	retained int64
	lines    int
	ring     []numberedLine
	groups   [][]numberedLine
	open     []numberedLine
	trail    int
	matched  int
	taken    int
	lastKept int
	dropped  bool
}

func newCollector(pattern *regexp.Regexp, filter Filter) *collector {
	return &collector{
		pattern: pattern, context: filter.Context, offset: filter.Offset,
		limit: filter.Limit, tail: filter.Tail, budget: maxReadBytes,
	}
}

func (c *collector) offer(line numberedLine) {
	if c.pattern == nil {
		c.offerPlain(line)
		return
	}
	matches := c.pattern.MatchString(stepContent(line.text))
	if matches {
		c.matched++
		if c.selects() {
			c.startOrExtend(line)
			c.appendLine(line)
			c.trail = c.context
			c.taken++
			c.push(line)
			return
		}
		c.dropped = true
		c.closeOpen()
		c.push(line)
		return
	}
	if len(c.open) > 0 && c.trail > 0 {
		c.appendLine(line)
		c.trail--
		c.push(line)
		return
	}
	c.closeOpen()
	c.push(line)
}

func (c *collector) selects() bool {
	if c.matched <= c.offset {
		return false
	}
	if c.tail {
		return true
	}
	return c.limit <= 0 || c.taken < c.limit
}

func (c *collector) startOrExtend(line numberedLine) {
	if len(c.open) == 0 {
		for _, candidate := range c.ring {
			if candidate.number > c.lastKept {
				c.appendLine(candidate)
			}
		}
	}
}

func (c *collector) appendLine(line numberedLine) {
	if line.number <= c.lastKept {
		return
	}
	if !c.tail && ((c.limit > 0 && c.lines >= c.limit) || c.retained+int64(len(line.text)) > c.budget) {
		c.dropped = true
		return
	}
	c.open = append(c.open, line)
	c.retained += int64(len(line.text))
	c.lines++
	c.lastKept = line.number
	// Bound actual returned lines, including context, rather than match groups.
	// Evict oldest lines while scanning so a tail always reaches the newest data.
	for c.tail && ((c.limit > 0 && c.lines > c.limit) || (c.retained > c.budget && c.lines > 1)) {
		var evicted numberedLine
		if len(c.groups) > 0 {
			evicted = c.groups[0][0]
			c.groups[0] = c.groups[0][1:]
			if len(c.groups[0]) == 0 {
				c.groups = c.groups[1:]
			}
		} else {
			evicted = c.open[0]
			c.open = c.open[1:]
		}
		c.retained -= int64(len(evicted.text))
		c.lines--
		c.dropped = true
	}
}

func (c *collector) push(line numberedLine) {
	if c.context == 0 {
		return
	}
	c.ring = append(c.ring, line)
	if len(c.ring) > c.context {
		c.ring = c.ring[len(c.ring)-c.context:]
	}
}

func (c *collector) closeOpen() {
	if len(c.open) == 0 {
		return
	}
	c.groups = append(c.groups, c.open)
	c.open = nil
}

func (c *collector) close() {
	c.closeOpen()
}

func (c *collector) offerPlain(line numberedLine) {
	limit := c.limit
	lineBytes := int64(len(line.text))
	if limit <= 0 {
		if line.number <= c.offset {
			return
		}
		if c.tail {
			c.open = append(c.open, line)
			c.retained += lineBytes
			for c.retained > c.budget && len(c.open) > 1 {
				c.retained -= int64(len(c.open[0].text))
				c.open = c.open[1:]
				c.dropped = true
			}
			return
		}
		if c.retained+lineBytes > c.budget {
			c.dropped = true
			return
		}
		c.open = append(c.open, line)
		c.retained += lineBytes
		return
	}
	if line.number <= c.offset {
		return
	}
	if c.tail {
		c.open = append(c.open, line)
		c.retained += lineBytes
		if len(c.open) > limit {
			c.retained -= int64(len(c.open[0].text))
			c.open = c.open[len(c.open)-limit:]
			c.dropped = true
		}
		for c.retained > c.budget && len(c.open) > 1 {
			c.retained -= int64(len(c.open[0].text))
			c.open = c.open[1:]
			c.dropped = true
		}
		return
	}
	if len(c.open) < limit && c.retained+lineBytes <= c.budget {
		c.open = append(c.open, line)
		c.retained += lineBytes
		return
	}
	c.dropped = true
}

func (c *collector) withheld() bool {
	return c.dropped
}

func (c *collector) bytes(budget int64) ([]byte, []int, bool) {
	flat := make([]numberedLine, 0)
	for _, group := range c.groups {
		flat = append(flat, group...)
	}
	if c.tail {
		return takeFromEnd(flat, budget)
	}
	return takeFromStart(flat, budget)
}

func takeFromStart(lines []numberedLine, budget int64) ([]byte, []int, bool) {
	var out []byte
	var numbers []int
	var used int64
	for _, line := range lines {
		if used+int64(len(line.text)) > budget {
			return out, numbers, true
		}
		used += int64(len(line.text))
		out = append(out, line.text...)
		numbers = append(numbers, line.number)
	}
	return out, numbers, false
}

func takeFromEnd(lines []numberedLine, budget int64) ([]byte, []int, bool) {
	var used int64
	first := len(lines)
	for index := len(lines) - 1; index >= 0; index-- {
		if used+int64(len(lines[index].text)) > budget {
			break
		}
		used += int64(len(lines[index].text))
		first = index
	}
	out, numbers, _ := takeFromStart(lines[first:], budget)
	return out, numbers, first > 0
}

func FilterBytes(raw []byte, filter Filter) ([]byte, Meta, error) {
	meta := Meta{Bytes: int64(len(raw))}
	if filter.Context < 0 || filter.Context > maxContextLines {
		return nil, Meta{}, fmt.Errorf("%w: context must be between 0 and %d", ErrInvalidPattern, maxContextLines)
	}
	pattern, err := compilePattern(filter.Pattern)
	if err != nil {
		return nil, Meta{}, err
	}
	collector := newCollector(pattern, filter)
	reader := bufio.NewReader(bytes.NewReader(raw))
	number := 0
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			number++
			meta.TotalLines++
			collector.offer(numberedLine{number: number, text: line})
		}
		if readErr != nil {
			break
		}
	}
	collector.close()
	meta.MatchedLines = collector.matched
	if pattern == nil {
		meta.MatchedLines = meta.TotalLines
	}
	out, numbers, truncatedByBytes := collector.bytes(maxReadBytes)
	meta.ReturnedLines = len(numbers)
	meta.LineNumbers = numbers
	meta.Truncated = truncatedByBytes || collector.withheld()
	return out, meta, nil
}
