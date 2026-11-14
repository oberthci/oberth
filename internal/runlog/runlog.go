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
	"strings"
	"sync"

	"github.com/oberthci/oberth/internal/runprogress"
)

// DefaultRunLimitBytes bounds retained output per run across execution engines.
const DefaultRunLimitBytes int64 = 64 << 20

const (
	maxMarkerBytes  = 64 << 10
	maxReadBytes    = 4 << 20
	maxPatternBytes = 512
	maxContextLines = 50
)

var ErrLogSliceTooLarge = errors.New("run log slice exceeds response limit")

var ErrInvalidPattern = errors.New("invalid log pattern")

type Range struct {
	Stage string `json:"burn"`
	Step  string `json:"step,omitempty"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type Index struct {
	Format     string         `json:"format,omitempty"`
	Containers []ContainerLog `json:"containers,omitempty"`
	RunID      string         `json:"run_id"`
	Size       int64          `json:"size"`
	Ranges     []Range        `json:"ranges"`
}

// DefaultWindowLines bounds an unfiltered interactive log read.
const DefaultWindowLines = 200

const DefaultWindowDescription = "last 200 lines by default; pass tail, limit or pattern for more"

type Filter struct {
	Raw     bool // Display historical run bytes without interpreting markers.
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
	limitBytes     int64
	containerMu    sync.Mutex
	directory      string
	progressMu     sync.RWMutex
	runningMu      sync.Mutex
	running        map[string]*runningIndex
	runningClock   uint64
	runningSlots   int
	runningEntries int
}

// WithRunLimitBytes applies the existing server run-log bound to ordinary
// observation writes as well as container copies. Set it only at construction.
func WithRunLimitBytes(limit int64) func(*Store) {
	return func(store *Store) { store.limitBytes = limit }
}

func Open(directory string, options ...func(*Store)) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("log directory is required")
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	store := &Store{directory: directory, limitBytes: DefaultRunLimitBytes}
	for _, option := range options {
		option(store)
	}
	if store.limitBytes <= 0 {
		return nil, errors.New("run log limit must be positive")
	}
	return store, nil
}

func (store *Store) Create(runID string) (*os.File, error) {
	// logPath validates the run ID and proves the resulting path remains under
	// the configured log directory.
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("create run log: %w", err)
	}
	return file, nil
}

// Resume opens retained bytes for append without reading retired progress files.
// A run can have a Workflow even if the owner stopped before its first log write.
func (store *Store) Resume(runID string) (*os.File, error) {
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("resume run log: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("resumed run log must be a regular file")
	}
	return file, nil
}

func (store *Store) BuildIndex(runID string) (Index, error) {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	if index, err := store.loadIndexContext(context.Background(), runID); err == nil && index.Format == "containers" {
		path, err := store.logPath(runID)
		if err != nil {
			return Index{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return Index{}, err
		}
		index.Size = info.Size()
		return index, store.writeIndex(runID, index)
	}
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

func (store *Store) Read(runID, stage, step string) ([]byte, error) {
	body, meta, err := store.ReadFiltered(runID, stage, step, Filter{})
	if err != nil {
		return nil, err
	}
	if meta.Truncated {
		return nil, ErrLogSliceTooLarge
	}
	return body, nil
}

func (store *Store) ReadFiltered(runID, stage, step string, filter Filter) ([]byte, Meta, error) {
	index, err := store.loadIndexContext(filter.ctx(), runID)
	if err != nil {
		return nil, Meta{}, err
	}
	if index.Format == "containers" {
		return store.readNativeStepLog(filter.ctx(), runID, stage, step, index, filter)
	}
	var selected []Range
	var total int64
	for _, candidate := range index.Ranges {
		if candidate.Stage != stage || (step != "" && candidate.Step != step) {
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
		return nil, Meta{}, fmt.Errorf("log slice %s/%s: %w", stage, step, os.ErrNotExist)
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
func (store *Store) ReadActive(runID, stage, step string) ([]byte, error) {
	body, _, err := store.ReadActiveFiltered(runID, stage, step, Filter{Tail: true})
	return body, err
}

func (store *Store) ReadActiveFiltered(runID, stage, step string, filter Filter) ([]byte, Meta, error) {
	index, err := store.loadIndexContext(filter.ctx(), runID)
	if err == nil && index.Format == "containers" {
		return store.readNativeStepLog(filter.ctx(), runID, stage, step, index, filter)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, Meta{}, err
	}
	return store.readActiveIndexed(runID, stage, step, filter)
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
	chunk, err := io.ReadAll(io.LimitReader(file, maximum))
	if err != nil {
		return nil, err
	}
	if start > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], start-1); err != nil {
			return nil, err
		}
		if previous[0] != '\n' {
			// Drop the partial first record, including any split UTF-8 rune.
			// A record larger than the entire excerpt budget is omitted.
			if newline := bytes.IndexByte(chunk, '\n'); newline >= 0 {
				chunk = chunk[newline+1:]
			} else {
				chunk = chunk[:0]
			}
		}
	}
	return chunk, nil
}

// LegacyRetryStage normalizes historical display labels for retained readers.
func LegacyRetryStage(stage string) string {
	if !strings.HasSuffix(stage, ")") {
		return stage
	}
	open := strings.LastIndexByte(stage, '(')
	if open < 1 || open == len(stage)-2 {
		return stage
	}
	for _, digit := range stage[open+1 : len(stage)-1] {
		if digit < '0' || digit > '9' {
			return stage
		}
	}
	return stage[:open]
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
		stage, step, found := ParseMarker(prefix)
		if found {
			length := len(index.Ranges)
			if !activeRange || index.Ranges[length-1].Stage != stage || index.Ranges[length-1].Step != step {
				if activeRange {
					index.Ranges[length-1].End = lineStart
				}
				index.Ranges = append(index.Ranges, Range{Stage: stage, Step: step, Start: lineStart})
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
	stage, step, found := strings.Cut(text[1:end], "/")
	if !found || stage == "" || step == "" || strings.ContainsAny(stage+step, "\x00\r\n[]") {
		return "", "", false
	}
	return stage, step, true
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
