package argojob

import "fmt"

func Reasons() []string {
	return []string{reasonPodRefused, reasonNotReached, reasonRunFailed, reasonFailed, reasonDeleted, reasonObserve, reasonKeep, reasonWaiting, reasonNode, reasonLock, fmt.Sprintf(reasonSyncLimit, "heavy-go"), fmt.Sprintf(reasonMutex, "release"), reasonRunSlot, reasonImage, reasonStarting, reasonBeforeStart, reasonHeld, reasonOOM, reasonDeadline, reasonSource, reasonSecret, reasonDeliveryEvidence, reasonInit, reasonShutdown, reasonDisrupted, reasonEvicted, reasonStopped, reasonSkipped, reasonOmitted, reasonUnknown, reasonPassed, reasonRunning, fmt.Sprintf(reasonExit, int32(-2147483648))}
}
