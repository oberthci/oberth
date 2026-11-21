package gitcache

func (c *Cache) trackReceiveFiles(store, owner string) *receiveFiles {
	files := receiveResources(store)
	files.owner = owner
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if c.receives == nil {
		c.receives = make(map[string]*receiveFiles)
	}
	c.receives[store] = files
	return files
}

func (c *Cache) receiveCleanupFor(store string) *receiveFiles {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	return c.receives[store]
}

func (c *Cache) forgetReceiveFiles(store string, files *receiveFiles) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if c.receives[store] == files {
		delete(c.receives, store)
	}
}
