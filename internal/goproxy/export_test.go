package goproxy

func (c *artifactCache) lookup(repository, modulePath, version string) (*artifactVersion, error) {
	return c.lookupStage(repository, modulePath, version, "")
}
