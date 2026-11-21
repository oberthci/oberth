package apicontract

// EventsTools declares the events service operations.
type EventsTools struct {
	EventsWait      Binding[EventsWaitArgs, EventResult]    `tool:"events_wait" summary:"Wait for matching durable events or recover after a gap." desc:"Wait for matching durable events or read events after a gap with after_id. Continue with next_after_id after both results and timeouts." wait:"true"`
	ScheduleList    Binding[ScheduleListArgs, SchedulePage] `tool:"schedule_list" summary:"Read authored schedules and their visible admission state." desc:"Read authored schedules and their visible admission state. Select the exact entry before pausing, resuming or triggering it." get:"/api/schedules"`
	SchedulePause   Binding[SchedulePauseArgs, Receipt]     `tool:"schedule_pause" summary:"Pause future admission for an authored schedule entry." desc:"Pause future admission for an authored schedule entry. Read schedule_list to verify the persisted pause state." access:"repository_admin" mutation:"true" check:"schedule_list"`
	ScheduleResume  Binding[ScheduleResumeArgs, Receipt]    `tool:"schedule_resume" summary:"Resume future admission for a paused schedule entry." desc:"Resume future admission for a paused schedule entry. Read schedule_list to inspect its next occurrence." access:"repository_admin" mutation:"true" check:"schedule_list"`
	ScheduleTrigger Binding[ScheduleTriggerArgs, Receipt]   `tool:"schedule_trigger" summary:"Start an immediate occurrence of an authored schedule." desc:"Request an immediate occurrence of an authored schedule entry. Use run_list to discover and observe the accepted run." access:"repository_admin" mutation:"true" check:"schedule_list"`
	SyncStatus      Binding[SyncStatusArgs, SyncStatus]     `tool:"sync_status" command:"lock status" summary:"Show native lock holders and waiters." desc:"Inspect current native lock holders and waiters in scope. Use the affected run state to decide the next observation."`
	SystemStatus    Binding[SystemStatusArgs, SystemStatus] `tool:"system_status" summary:"Show installation health and readiness." desc:"Read authorized system health and readiness facts. Inspect the reported component before retrying unavailable work." get:"/api/status"`
}
