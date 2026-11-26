package logcontract

// Selector selects an admitted plan path, native attempt or exact container.
// Empty selectors read ordinary run output and every captured span. StepSet
// distinguishes the logical root (empty plan path) from an omitted step.
type Selector struct {
	Step, NodeID, Container string
	StepSet                 bool
}

// ReadRequest carries read options and caller facts from the admitted plan and
// the service purge record. These facts are not written to the log index.
type ReadRequest struct {
	Selector     Selector
	Filter       Filter
	PlannedSteps []string
	OwnOnly      bool
	Purged       bool
	// BeforeMarker confines a predecessor read to its failed attempt in the
	// successor's shared container archive. Filtering happens after this bound.
	BeforeMarker string
	AfterMarker  string
}

// Filter selects a window within the bounded scan. Offset skips selected lines
// within that window; Tail with Limit returns its last N lines in display order.
// ByteOffset is an absolute forward (head) or backward (tail) cursor.
type Filter struct {
	Pattern    string
	Context    int
	Offset     int
	Limit      int
	Tail       bool
	ByteOffset *int64
}

// Meta carries a complete reason of at most 40 characters and separate detail.
type Meta struct {
	Waiting          bool    `json:"waiting,omitempty"` // Selected planned output has no capture yet; an active view may retry.
	Copying          bool    `json:"copying"`           // Selected retained output has an active copy; readers may poll.
	Reason           string  `json:"reason,omitempty"`
	Follow           bool    `json:"follow"` // The view may follow the selected running step through its live transport.
	Detail           string  `json:"detail,omitempty"`
	ScanLimitBytes   int64   `json:"scan_limit_bytes"`       // Maximum reads, including boundary probes.
	TotalLines       int     `json:"total_lines"`            // Whole-selection total, zero for incomplete scans.
	MatchedLines     int     `json:"matched_lines"`          // Matches before context; zero for incomplete scans.
	ReturnedLines    int     `json:"returned_lines"`         // Complete records returned.
	Tail             bool    `json:"tail"`                   // Whether the request applied a tail window.
	Truncated        bool    `json:"truncated"`              // A window, omitted records or capture gaps withheld output.
	Bytes            int64   `json:"bytes"`                  // Available bytes in all selected spans.
	LineNumbers      []int   `json:"line_numbers,omitempty"` // One-based, only for complete scans.
	LineOffsets      []int64 `json:"line_offsets,omitempty"` // Absolute starts of returned records.
	ScanComplete     bool    `json:"scan_complete"`          // All selected bytes were scanned.
	BytesExact       bool    `json:"bytes_exact"`            // False when invalid UTF-8 was replaced.
	ScannedBytes     int64   `json:"scanned_bytes"`          // Actual reads, including probes.
	WithheldBytes    int64   `json:"withheld_bytes"`         // Raw bytes omitted from this scanned window.
	WithheldLines    int     `json:"withheld_lines"`         // Withheld records or boundary fragments.
	OmittedLongLines int     `json:"omitted_long_lines"`     // Records over 64 KiB in the window.
	HasMore          bool    `json:"has_more"`               // The cursor has not reached the selection boundary.
	NextByteOffset   int64   `json:"next_byte_offset"`       // Absolute continuation cursor in the requested direction.
}
