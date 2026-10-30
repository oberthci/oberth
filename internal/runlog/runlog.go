package runlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	// ByteOffset is an absolute offset in the retained run log, returned by
	// NextByteOffset. Head reads continue forward; tail reads continue backward.
	ByteOffset  *int64
	readContext context.Context
}

func (filter Filter) WithContext(ctx context.Context) Filter {
	filter.readContext = ctx
	return filter
}

func (filter Filter) ctx() context.Context {
	if filter.readContext != nil {
		return filter.readContext
	}
	return context.Background()
}

type Meta struct {
	// Aggregate I/O includes discovery plus filtering (including read-ahead and
	// repeated tail windows). Existing per-phase counters retain their meaning.
	TotalScannedBytes int64   `json:"total_scanned_bytes"`
	ScanLimitBytes    int64   `json:"scan_limit_bytes"`
	TotalLines        int     `json:"total_lines"`
	MatchedLines      int     `json:"matched_lines"`
	ReturnedLines     int     `json:"returned_lines"`
	Truncated         bool    `json:"truncated"`
	Bytes             int64   `json:"bytes"`
	LineNumbers       []int   `json:"line_numbers,omitempty"`
	LineOffsets       []int64 `json:"line_offsets,omitempty"`
	ScanComplete      bool    `json:"scan_complete"`
	BytesExact        bool    `json:"bytes_exact"`
	ScannedBytes      int64   `json:"scanned_bytes"`
	DiscoveryBytes    int64   `json:"discovery_bytes"`
	WithheldBytes     int64   `json:"withheld_bytes"`
	WithheldLines     int     `json:"withheld_lines"`
	OmittedLongLines  int     `json:"omitted_long_lines"`
	HasMore           bool    `json:"has_more"`
	NextByteOffset    int64   `json:"next_byte_offset"`
}

type Store struct {
	directory      string
	progressMu     sync.RWMutex
	runningMu      sync.Mutex
	running        map[string]*runningIndex
	runningClock   uint64
	runningSlots   int
	runningEntries int
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

// Resume opens an existing run log for append and restores its progress journal.
// A run can have a Workflow even if the owner stopped before its first log write.
func (store *Store) Resume(runID string) (*os.File, []runprogress.Event, int64, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, nil, 0, err
	}
	events, err := store.StepProgress(runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, 0, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) //nolint:gosec
	if err != nil {
		return nil, nil, 0, fmt.Errorf("resume run log: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, 0, errors.New("resumed run log must be a regular file")
	}
	return file, events, info.Size(), nil
}

func (store *Store) BuildIndex(runID string) (Index, error) {
	store.ForgetActiveIndex(runID)
	defer store.ForgetActiveIndex(runID)
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
	index, err := store.loadIndexContext(filter.ctx(), runID)
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
// growing redacted log. It extends a bounded in-memory index with new bytes;
// BuildIndex alone publishes the durable terminal index after Job exit.
func (store *Store) ReadActive(runID, burn, step string) ([]byte, error) {
	body, _, err := store.ReadActiveFiltered(runID, burn, step, Filter{Tail: true})
	return body, err
}

func (store *Store) ReadActiveFiltered(runID, burn, step string, filter Filter) ([]byte, Meta, error) {
	return store.readActiveIndexed(runID, burn, step, filter)
}

// LiveLogSize stats the append-only log without opening or reading its bytes.
func (store *Store) LiveLogSize(runID string) (int64, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ReadFrom returns up to maximum bytes of the run's append-only log starting
// at offset, plus the next poll offset and the current log size. It serves the
// dashboard's live view of a run in progress: the file grows while the Job
// streams, so callers poll with the returned offset. A negative offset means
// "start tailing": the read begins near maximum bytes before the current
// end, rounded back to the start of that record to preserve attribution. A
// late-joining viewer still receives at most maximum bytes per call. When
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
		start, err = recordStart(file, start)
		if err != nil {
			return nil, 0, 0, err
		}
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

// recordStart scans backward with fixed memory. A log record can exceed the
// poll response size, so one look-behind chunk is not necessarily sufficient.
// Only initial tail reads align; explicit continuation cursors stay exact.
func recordStart(file *os.File, offset int64) (int64, error) {
	var block [64 << 10]byte
	for offset > 0 {
		start := max(int64(0), offset-int64(len(block)))
		n, err := file.ReadAt(block[:offset-start], start)
		if err != nil {
			return 0, err
		}
		if newline := bytes.LastIndexByte(block[:n], '\n'); newline >= 0 {
			return start + int64(newline) + 1, nil
		}
		offset = start
	}
	return 0, nil
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
	plan, err := store.StepPlanRecord(runID)
	return plan.Steps, err
}

// StepPlanRecord retains version provenance for fail-closed replay decisions.
func (store *Store) StepPlanRecord(runID string) (runprogress.Plan, error) {
	path, err := store.planPath(runID)
	if err != nil {
		return runprogress.Plan{}, err
	}
	// planPath validated the run ID and confined this path to the log root.
	file, err := os.Open(path) //nolint:gosec
	if err != nil {
		return runprogress.Plan{}, fmt.Errorf("open step plan: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, runprogress.MaxPlanBytes+1))
	if err != nil {
		return runprogress.Plan{}, fmt.Errorf("read step plan: %w", err)
	}
	if len(body) > runprogress.MaxPlanBytes {
		return runprogress.Plan{}, errors.New("step plan exceeds its byte bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var plan runprogress.Plan
	if err := decoder.Decode(&plan); err != nil {
		return runprogress.Plan{}, fmt.Errorf("decode step plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return runprogress.Plan{}, fmt.Errorf("decode step plan: %w", err)
	}
	if plan.Version == runprogress.PlanVersion {
		var wire struct {
			Steps []struct {
				Credentialed *bool `json:"credentialed"`
			}
		}
		if err := json.Unmarshal(body, &wire); err != nil {
			return runprogress.Plan{}, err
		}
		for _, step := range wire.Steps {
			if step.Credentialed == nil {
				return runprogress.Plan{}, errors.New("current plan lacks explicit credential classification")
			}
		}
	}
	sort.Slice(plan.Steps, func(left, right int) bool { return plan.Steps[left].Ordinal < plan.Steps[right].Ordinal })
	return plan, nil
}

// StepProgress returns the latest transition for every planned step in stable
// ordinal order. A crash-truncated final record is ignored; every preceding
// newline-terminated record remains authoritative.
func (store *Store) StepProgress(runID string) ([]runprogress.Event, error) {
	events, err := store.readStepProgress(runID, false)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool)
	for _, event := range events {
		known[event.Burn+"\x00"+event.Step] = true
	}
	// Legacy aliases remain filtered only in this display projection.
	planned := make(map[string]bool)
	if plan, err := store.StepPlan(runID); err == nil {
		for _, step := range plan {
			key := step.Burn + "\x00" + step.Step
			planned[key], known[key] = true, true
		}
	}
	latest := make(map[string]runprogress.Event)
	ordinals := make(map[int]string)
	for _, event := range events {
		key := event.Burn + "\x00" + event.Step
		if canonical := LegacyRetryBurn(event.Burn); canonical != event.Burn &&
			!planned[key] && known[canonical+"\x00"+event.Step] {
			continue
		}
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

// StepProgressHistory returns every validated observation, retaining earlier
// starts and legacy attempts. Replay decisions must not use the latest-only
// display projection, and must not ignore a torn final record.
func (store *Store) StepProgressHistory(runID string) ([]runprogress.Event, error) {
	return store.readStepProgress(runID, true)
}

func (store *Store) readStepProgress(runID string, strict bool) ([]runprogress.Event, error) {
	path, err := store.progressPath(runID)
	if err != nil {
		return nil, err
	}
	// Writers append one validated record and only repair an unterminated tail.
	// Readers consume the immutable newline-terminated prefix below, so a
	// slow append for any run must not hold all run_get requests behind a lock.
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
	if strict && len(body) > 0 && lastNewline != len(body)-1 {
		return nil, errors.New("step progress journal has an incomplete record")
	}
	if lastNewline < 0 {
		return []runprogress.Event{}, nil
	}
	var events []runprogress.Event
	for index, line := range bytes.Split(body[:lastNewline], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		event, decodeErr := runprogress.Decode(line)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode step progress journal record %d: %w", index, decodeErr)
		}
		events = append(events, event)
	}
	return events, nil
}

// LegacyRetryBurn removes an exact numeric attempt suffix, task(N), from a
// legacy burn name. Callers must also establish that the original key is
// unplanned and the canonical key is known before treating it as an alias.
// Hook names such as task(N).onExit do not have an attempt suffix.
func LegacyRetryBurn(burn string) string {
	if !strings.HasSuffix(burn, ")") {
		return burn
	}
	open := strings.LastIndexByte(burn, '(')
	if open < 1 || open == len(burn)-2 {
		return burn
	}
	for _, digit := range burn[open+1 : len(burn)-1] {
		if digit < '0' || digit > '9' {
			return burn
		}
	}
	return burn[:open]
}

func indexReader(runID string, reader io.Reader) (Index, error) {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	var offset int64
	index := Index{RunID: runID, Ranges: []Range{}}
	activeRange := false
	for {
		lineStart := offset
		lineLength := 0
		// Allocate for the actual prefix, rather than 64 KiB for every short line.
		var prefix []byte
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
		if bytes.HasPrefix(prefix, []byte(runprogress.LogDeliveryPrefix)) {
			if activeRange {
				index.Ranges[len(index.Ranges)-1].End = lineStart
			}
			activeRange = false
		}
		burn, step, found := ParseMarker(prefix)
		if found {
			length := len(index.Ranges)
			if !activeRange || index.Ranges[length-1].Burn != burn || index.Ranges[length-1].Step != step {
				if activeRange {
					index.Ranges[length-1].End = lineStart
				}
				index.Ranges = append(index.Ranges, Range{Burn: burn, Step: step, Start: lineStart})
			}
			activeRange = true
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return Index{}, fmt.Errorf("scan run log: %w", readErr)
		}
	}
	if length := len(index.Ranges); activeRange {
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

func (store *Store) loadIndexContext(ctx context.Context, runID string) (Index, error) {
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
	decoder := json.NewDecoder(contextReader{ctx, io.LimitReader(file, 8<<20)})
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
	number     int
	text       string
	byteOffset int64
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
	meta.ScanComplete, meta.BytesExact, meta.ScannedBytes = true, true, int64(len(raw))
	meta.WithheldLines, meta.WithheldBytes = max(0, meta.TotalLines-meta.ReturnedLines), max(int64(0), int64(len(raw))-int64(len(out)))
	return out, meta, nil
}
