package model

import "errors"

// ErrIssueTrailerPermanent marks immutable policy or history input that cannot
// succeed on retry. Missing Git objects and infrastructure errors remain retryable.
var ErrIssueTrailerPermanent = errors.New("issue trailer input rejected")

// CommitIssueTrailer is a validated reference from a Git trailer block.
type CommitIssueTrailer struct {
	IssueID int64
	SHA     string
	Close   bool
}

// IssueTrailerWork binds processing to one delivered green publication.
type IssueTrailerWork struct {
	PublicationID string
	RepoID        int64
	Branch        string
	SHA           string
	PreviousSHA   string
	RunID         string
}
