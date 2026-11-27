package api

const (
	pollsPerIdentity       = 64
	nativePerIdentity      = 64
	streamsPerInstallation = 256
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
