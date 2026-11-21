package admissioncontract

// Boundary limits constrain documents and display representation, never native
// execution or fan-out. The planner collapses expansion to a dynamic entry
// before either plan bound is exceeded; these are not admission run limits.
const (
	MaxDocumentBytes      = 1 << 20
	MaxSourceSpecBytes    = 512 << 10
	MaxNestingDepth       = 64
	MaxPlanEntries        = 2048
	MaxPlanBytes          = 1 << 20
	MaxPlannerVisits      = 65536
	MaxPinnedDestinations = 4
	MaxSecretPaths        = 32
	MaxCapabilityKinds    = 32
	MaxScheduleEntryBytes = 64
)
