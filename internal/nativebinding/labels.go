package nativebinding

// TierValue encodes an already-admitted release decision for the trigger/tier
// labels. It never derives release authority from the run-trigger vocabulary.
func TierValue(release bool) string {
	if release {
		return "release"
	}
	return "ci"
}
