package job

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func (controller *Controller) provisionRepositoryCaches(ctx context.Context, request Request) error {
	if !controller.config.ProvisionCacheDirs {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cacheRoot := requestCacheRoot(controller.config, request)
	rootInfo, err := os.Lstat(cacheRoot)
	if err != nil {
		return fmt.Errorf("inspect cache root %s: %w", cacheRoot, err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("cache root %s is not a physical directory", cacheRoot)
	}
	root, err := os.OpenRoot(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache root %s: %w", cacheRoot, err)
	}
	defer func() { _ = root.Close() }()

	repositoryRoot := repositoryCacheRelativePath(request)
	for _, directory := range []string{"repos", repositoryRoot, filepath.Join(repositoryRoot, "gomod"), filepath.Join(repositoryRoot, "gobuild")} {
		if err := root.MkdirAll(directory, 0o750); err != nil {
			return fmt.Errorf("create repository cache %s: %w", directory, err)
		}
		if err := root.Chmod(directory, 0o750); err != nil {
			return fmt.Errorf("secure repository cache %s: %w", directory, err)
		}
		info, err := root.Stat(directory)
		if err != nil {
			return fmt.Errorf("inspect repository cache %s: %w", directory, err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o750 {
			return fmt.Errorf("repository cache %s is not a mode 0750 directory", directory)
		}
	}
	return nil
}
