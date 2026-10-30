package service

// Implementations without a retained active index need no lifecycle hook.
func forgetActiveLogIndex(logs LogStore, runID string) {
	if cache, ok := logs.(interface{ ForgetActiveIndex(string) }); ok {
		cache.ForgetActiveIndex(runID)
	}
}
