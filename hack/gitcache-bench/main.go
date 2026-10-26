package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

func main() {
	backend := flag.String("backend", "go-git", "cli or go-git")
	remote := flag.String("remote", "", "read-only upstream mirror/URL")
	root := flag.String("root", "", "dedicated benchmark cache root; never the source mirror")
	operation := flag.String("operation", "ensure", "ensure, checkout, gc or compact")
	key := flag.String("ssh-key", "", "operator SSH private key path")
	hosts := flag.String("known-hosts", "", "pinned known_hosts path")
	isolated := flag.Bool("cgroup-isolated", false, "collect cgroup peak only in a dedicated benchmark container")
	flag.Parse()
	if *remote == "" || *root == "" {
		fmt.Fprintln(os.Stderr, "remote and root are required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cache, err := gitcache.New(gitcache.Config{Root: *root, Backend: gitcache.Backend(*backend), Upstream: func(string) (string, error) { return *remote, nil }, SSH: gitcache.SSHConfig{PrivateKeyPath: *key, KnownHostsPath: *hosts}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var peakHeap atomic.Uint64
	var peakSys atomic.Uint64
	done := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Go(func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			if memory.HeapAlloc > peakHeap.Load() {
				peakHeap.Store(memory.HeapAlloc)
			}
			if memory.HeapSys > peakSys.Load() {
				peakSys.Store(memory.HeapSys)
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	})
	started := time.Now()
	switch *operation {
	case "ensure":
		_, err = cache.Ensure(ctx, "mirror")
	case "checkout":
		var sha string
		ready, readyErr := cache.Ensure(ctx, "mirror")
		err = readyErr
		if err == nil {
			sha, err = cache.RefSHA(ctx, "mirror", ready.DefaultBranch)
		}
		if err == nil {
			err = cache.Checkout(ctx, "mirror", sha, filepath.Join(*root, "checkout-"+fmt.Sprint(started.UnixNano())))
		}
	case "gc":
		err = cache.GC(ctx, "mirror")
	case "compact":
		err = cache.Compact(ctx, "mirror")
	default:
		err = fmt.Errorf("unknown operation %s", *operation)
	}
	elapsed := time.Since(started)
	close(done)
	sampler.Wait()
	var usage syscall.Rusage
	if usageErr := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); usageErr != nil {
		fmt.Fprintln(os.Stderr, usageErr)
		os.Exit(1)
	}
	rss := usage.Maxrss
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	bytes := int64(0)
	walkErr := filepath.WalkDir(*root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			info, e := entry.Info()
			if e != nil {
				return e
			}
			bytes += info.Size()
		}
		return nil
	})
	if err == nil {
		err = walkErr
	}
	result := map[string]any{"backend": *backend, "operation": *operation, "elapsed_seconds": elapsed.Seconds(), "peak_rss_bytes": rss, "peak_go_heap_bytes": peakHeap.Load(), "peak_go_heap_sys_bytes": peakSys.Load(), "cache_bytes": bytes, "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0)}
	if body, e := os.ReadFile("/sys/fs/cgroup/memory.peak"); *isolated && e == nil {
		if peak, e := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64); e == nil {
			result["cgroup_peak_bytes"] = peak
		}
	}
	if body, e := os.ReadFile("/sys/fs/cgroup/memory.stat"); *isolated && e == nil {
		result["cgroup_memory_stat"] = string(body)
	}
	var children syscall.Rusage
	if e := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); e == nil {
		childRSS := children.Maxrss
		if runtime.GOOS != "darwin" {
			childRSS *= 1024
		}
		result["largest_child_rss_bytes"] = childRSS
	}
	if err != nil {
		result["error"] = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if err != nil {
		os.Exit(1)
	}
}
