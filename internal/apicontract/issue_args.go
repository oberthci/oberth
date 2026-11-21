package apicontract

type IssueAppendArgs struct {
	ID     int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Text   string `json:"text,omitempty" desc:"Choose the text used by this operation; omit it to use the service policy." required:"true" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueCloseArgs struct {
	ID     int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueCreateArgs struct {
	Body     *string `json:"body,omitempty" desc:"Supply the complete issue body; it is never silently shortened." required:"true" sensitive:"false" position:"2"`
	Title    string  `json:"title,omitempty" desc:"Supply the issue title visible to callers authorized for this issue." required:"true" sensitive:"false" position:"1"`
	Upstream string  `json:"upstream,omitempty" desc:"Choose the registered upstream that owns the repository or issue." sensitive:"false" position:"0"`
	Reason   string  `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueDeleteArgs struct {
	ID     int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueGetArgs struct {
	ActiveStaleAfter string `json:"active_stale_after,omitempty" desc:"Select the active-issue stale window without acquiring or extending any lock." sensitive:"false" position:"0"`
	ID               int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	StaleAfter       string `json:"stale_after,omitempty" desc:"Select the general issue stale-duration window used for this observation." sensitive:"false" position:"0"`
}

type IssueGetManyArgs struct {
	ActiveStaleAfter string  `json:"active_stale_after,omitempty" desc:"Select the active-issue stale window without acquiring or extending any lock." sensitive:"false" position:"0"`
	IDs              []int64 `json:"ids,omitempty" desc:"Read the requested positive issue IDs in order; each result remains an independently complete row." required:"true" maxItems:"50" minItems:"1" sensitive:"false" position:"1"`
	StaleAfter       string  `json:"stale_after,omitempty" desc:"Select the general issue stale-duration window used for this observation." sensitive:"false" position:"0"`
}

type IssueListArgs struct {
	ActiveStaleAfter string `json:"active_stale_after,omitempty" desc:"Select the active-issue stale window without acquiring or extending any lock." sensitive:"false" position:"0"`
	Before           int64  `json:"before,omitempty" desc:"Continue an older page using the record-ID cursor from the preceding reply." min:"0" sensitive:"false" position:"0"`
	Kind             string `json:"kind,omitempty" desc:"Choose the resource category the operation reads or changes." sensitive:"false" position:"0"`
	Limit            int    `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"1" max:"50" sensitive:"false" position:"0"`
	Query            string `json:"query,omitempty" desc:"Search the authorized issue metadata before pagination." sensitive:"false" position:"0"`
	Repo             string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	StaleAfter       string `json:"stale_after,omitempty" desc:"Select the general issue stale-duration window used for this observation." sensitive:"false" position:"0"`
	State            string `json:"state,omitempty" desc:"Choose the state used by this operation; omit it to use the service policy." sensitive:"false" position:"0"`
	WorkStatus       string `json:"work_status,omitempty" desc:"Set the issue work state independently of its open or closed state." sensitive:"false" position:"0"`
}

type IssueLockArgs struct {
	ID     int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueReopenArgs struct {
	ID     int64  `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type IssueUpdateArgs struct {
	Body             *string `json:"body,omitempty" desc:"Supply the complete issue body; it is never silently shortened." sensitive:"false" position:"0"`
	ExpectedRevision *int64  `json:"expected_revision,omitempty" desc:"Require this current issue revision before changing it; omit for an unconditional authorized update." min:"1" sensitive:"false" position:"0"`
	ID               int64   `json:"id,omitempty" desc:"Select the integer issue ID returned by issue_list or issue_create." required:"true" min:"1" sensitive:"false" position:"1" flag:"-"`
	Title            *string `json:"title,omitempty" desc:"Supply the issue title visible to callers authorized for this issue." sensitive:"false" position:"0"`
	Upstream         *string `json:"upstream,omitempty" desc:"Choose the registered upstream that owns the repository or issue." sensitive:"false" position:"0"`
	WorkStatus       *string `json:"work_status,omitempty" desc:"Set the issue work state independently of its open or closed state." sensitive:"false" position:"0"`
	Reason           string  `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}
