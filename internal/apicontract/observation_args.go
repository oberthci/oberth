package apicontract

import "github.com/oberthci/oberth/internal/model"

type AuditListArgs struct {
	Action string `json:"action,omitempty" desc:"Select the lifecycle operation or repair action to observe or record." sensitive:"false" position:"0"`
	Actor  string `json:"actor,omitempty" desc:"Filter by the public action initiator after authorization." sensitive:"false" position:"0"`
	Before int64  `json:"before,omitempty" desc:"Continue an older page using the record-ID cursor from the preceding reply." min:"0" sensitive:"false" position:"0"`
	Limit  int    `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"1" max:"200" sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	Since  string `json:"since,omitempty" desc:"Select the beginning of the history window without changing stored state." sensitive:"false" position:"0"`
	Until  string `json:"until,omitempty" desc:"Select the end of the audit history window without changing stored state." sensitive:"false" position:"0"`
}

type EventsWaitArgs struct {
	AfterID  *int64   `json:"after_id,omitempty" desc:"Resume after this event ID; use next_after_id even after an empty page or timeout." min:"0" sensitive:"false" position:"0"`
	Kinds    []string `json:"kinds,omitempty" desc:"Select durable event kinds, including issue.updated for issue changes; an empty list observes every authorized kind." maxItems:"32" sensitive:"false" position:"0"`
	Limit    int      `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"1" max:"200" default:"100" sensitive:"false" position:"0"`
	Own      bool     `json:"own,omitempty" desc:"Include only observations attributed to the authenticated identity." sensitive:"false" position:"0"`
	RefsGlob []string `json:"refs_glob,omitempty" desc:"Select references with shell-style patterns; a star does not cross a slash." maxItems:"32" sensitive:"false" position:"0"`
	Repos    []string `json:"repos,omitempty" desc:"Restrict observations to these repositories after the service applies credential scope." maxItems:"32" format:"repos" sensitive:"false" position:"0"`
	RunIDs   []string `json:"run_ids,omitempty" desc:"Restrict observations to these exact runs or unambiguous visible prefixes." maxItems:"32" sensitive:"false" position:"0"`
	TimeoutS int      `json:"timeout_s,omitempty" desc:"Wait at most this many seconds; zero or omission selects the advertised default." min:"0" max:"600" default:"600" timeout:"true" sensitive:"false" position:"0"`
}

type ScheduleListArgs struct {
	Repo string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
}

type SchedulePauseArgs struct {
	Entry  string `json:"entry,omitempty" desc:"Select the authored native schedule entry inside the repository." required:"true" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
}

type ScheduleResumeArgs struct {
	Entry  string `json:"entry,omitempty" desc:"Select the authored native schedule entry inside the repository." required:"true" sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type ScheduleTriggerArgs struct {
	Entry  string `json:"entry,omitempty" desc:"Select the authored native schedule entry inside the repository." required:"true" sensitive:"false" position:"0"`
	Force  bool   `json:"force,omitempty" desc:"Confirm the explicitly authorized override; it grants no additional authority." sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type SyncStatusArgs struct {
	Lock string `json:"lock,omitempty" desc:"Select one native synchronization key; omission observes current authorized locks." sensitive:"false" position:"0"`
}

type SystemStatusArgs struct {
}

func (v EventsWaitArgs) timeoutResult() EventResult {
	page := model.EventPage{Events: []model.AgentEvent{}}
	if v.AfterID != nil {
		page.NextAfterID = *v.AfterID
	}
	return EventResult{EventPage: page, TimedOut: true}
}
