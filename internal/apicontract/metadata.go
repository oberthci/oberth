package apicontract

import "time"

const (
	RequestBytes     = 1 << 20
	AdminBytes       = 128 << 10
	EnrollmentBytes  = 96 << 10
	ResponseBytes    = 4 << 20
	RPCEnvelopeBytes = RequestBytes + 1024
)

type Access string

const (
	Read        Access = "authenticated"
	Admin       Access = "unscoped_admin"
	ScopedAdmin Access = "repository_admin"
)

// Metadata drives visibility, HTTP aliases, validation budgets and deadlines.
type Metadata struct {
	Command      string        `json:"-"`
	CommandFlow  bool          `json:"-"`
	Summary      string        `json:"-"`
	Check        string        `json:"-"`
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	InputSchema  Schema        `json:"inputSchema"`
	OutputSchema Schema        `json:"outputSchema"`
	Access       Access        `json:"-"`
	Mutation     bool          `json:"-"`
	LongPoll     bool          `json:"-"`
	InputBytes   int           `json:"-"`
	OutputBytes  int           `json:"-"`
	Deadline     time.Duration `json:"-"`
	Annotations  Annotations   `json:"annotations"`
	Get          string        `json:"-"`
	AdminAction  string        `json:"-"`
	Post         string        `json:"-"`
	Tail         bool          `json:"-"`
}

type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
}

// Schema describes the concrete wire value of a declaration.
// Declaration consumers treat the map and its nested values as read-only.
type Schema map[string]any
