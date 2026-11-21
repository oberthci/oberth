package gitcache

// AbandonReceiveError acknowledges an audited decision to discard delivery.
// Replay settles through the operator's abandon path while holding its receive slot.
type AbandonReceiveError struct{}

func (*AbandonReceiveError) Error() string {
	return "push delivery abandoned. Ask an administrator to restore access for new pushes."
}
