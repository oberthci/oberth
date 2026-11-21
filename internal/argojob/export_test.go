package argojob

import "fmt"

func Reasons() []string {
	return []string{reasonPodRefused, reasonNotReached, reasonRunFailed, reasonFailed, reasonDeleted, reasonObserve, reasonKeep, reasonWaiting, reasonNode, reasonLock, reasonImage, reasonStarting, reasonBeforeStart, reasonHeld, reasonOOM, reasonDeadline, reasonSource, reasonInit, reasonShutdown, reasonDisrupted, reasonEvicted, reasonStopped, reasonSkipped, reasonOmitted, reasonUnknown, reasonPassed, reasonRunning, fmt.Sprintf(reasonExit, int32(-2147483648))}
}
