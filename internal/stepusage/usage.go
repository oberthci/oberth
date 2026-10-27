// Package stepusage collects advisory resource measurements inside a step's own
// container. It never reads host cgroups or requires access to the Kubernetes API.
package stepusage

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Prefix = "oberth-step-usage-v1 "
const MaxMessageBytes = 1024
const TerminationPath = "/dev/termination-log"

// Usage uses pointers so an unavailable measurement differs from measured zero.
// MaxRSSBytes is Linux wait4's maximum resident set of a waited-for descendant
// (not the sum of concurrent processes). MemoryPeakBytes is cgroup-accounted
// peak memory, including page cache; it must not be called RSS. CPU is cumulative
// container CPU, including executor/wrapper overhead, in nanoseconds.
type Usage struct {
	MaxRSSBytes     *int64         `json:"max_rss_bytes"`
	MemoryPeakBytes *int64         `json:"memory_peak_bytes"`
	UserCPU         *time.Duration `json:"user_cpu_ns"`
	SystemCPU       *time.Duration `json:"system_cpu_ns"`
}

func (u Usage) Valid() bool {
	return (u.MaxRSSBytes == nil || *u.MaxRSSBytes >= 0) &&
		(u.MemoryPeakBytes == nil || *u.MemoryPeakBytes >= 0) &&
		(u.UserCPU == nil || *u.UserCPU >= 0) && (u.SystemCPU == nil || *u.SystemCPU >= 0)
}

func Encode(u Usage) string {
	data, _ := json.Marshal(u)
	return Prefix + string(data)
}

func Decode(message string) Usage {
	if len(message) > MaxMessageBytes || !strings.HasPrefix(message, Prefix) {
		return Usage{}
	}
	var u Usage
	d := json.NewDecoder(strings.NewReader(strings.TrimPrefix(message, Prefix)))
	d.DisallowUnknownFields()
	if d.Decode(&u) != nil || !u.Valid() {
		return Usage{}
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Usage{}
	}
	return u
}

// ReadCgroupV2 reads only this container's unified cgroup. With private cgroup
// namespaces self is "/"; older namespaces expose a path beneath the mount.
// A missing/invalid self path must never fall back to the mount's aggregate.
func ReadCgroupV2(mount string, self []byte) Usage {
	var relative string
	found := false
	for _, line := range strings.Split(string(self), "\n") {
		if strings.HasPrefix(line, "0::/") {
			if found {
				return Usage{}
			}
			relative, found = strings.TrimPrefix(line, "0::"), true
		}
	}
	if !found || filepath.Clean(relative) != relative {
		return Usage{}
	}
	root := filepath.Join(mount, strings.TrimPrefix(relative, "/"))
	var u Usage
	if raw, err := readBounded(filepath.Join(root, "memory.peak")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && n >= 0 {
			u.MemoryPeakBytes = &n
		}
	}
	if raw, err := readBounded(filepath.Join(root, "cpu.stat")); err == nil {
		// Duplicate, malformed or overflowing counters invalidate CPU independently
		// of memory. Unknown kernel keys are deliberately ignored.
		counters := map[string]time.Duration{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			key := fields[0]
			if key != "user_usec" && key != "system_usec" {
				continue
			}
			if len(fields) != 2 {
				return u
			}
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if _, duplicate := counters[key]; duplicate || err != nil || n < 0 || n > math.MaxInt64/int64(time.Microsecond) {
				return u
			}
			counters[key] = time.Duration(n) * time.Microsecond
		}
		if n, ok := counters["user_usec"]; ok {
			u.UserCPU = &n
		}
		if n, ok := counters["system_usec"]; ok {
			u.SystemCPU = &n
		}
	}
	return u
}

func readBounded(name string) ([]byte, error) {
	f, err := os.Open(name) // #nosec G304 G703 -- fixed kernel filenames beneath the container's cgroup mount; self path must be clean and absolute.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if len(raw) > 4096 {
		return nil, io.ErrShortBuffer
	}
	return raw, err
}
