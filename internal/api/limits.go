package api

import "time"

const (
	pollsPerIdentity        = 64
	sessionsPerIdentity     = 64
	sessionsPerInstallation = 512
	eventsPerIdentity       = 32
	nativePerIdentity       = 64
	streamsPerInstallation  = 256
	sessionIdleTimeout      = time.Hour
	resourcesPerSession     = 64
)

func (r *reservations) stream(kind, identity string, perIdentity int) (func(), error) {
	releaseIdentity, ok := r.acquire(kind+":"+identity, perIdentity)
	if !ok {
		return nil, refusal(kind + "Identity")
	}
	releaseInstallation, ok := r.acquire(kind, streamsPerInstallation)
	if !ok {
		releaseIdentity()
		return nil, refusal(kind + "Installation")
	}
	return func() { releaseInstallation(); releaseIdentity() }, nil
}
