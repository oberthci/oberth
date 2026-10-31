package resourcesampler

import (
	"bufio"
	"bytes"
	"errors"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

type Summary struct {
	Node struct {
		NodeName string `json:"nodeName"`
	} `json:"node"`
	Pods []PodStats `json:"pods"`
}
type PodStats struct {
	PodRef           struct{ Name, Namespace, UID string } `json:"podRef"`
	EphemeralStorage *struct {
		Time      time.Time `json:"time"`
		UsedBytes *uint64   `json:"usedBytes"`
	} `json:"ephemeral-storage"`
	IO *struct {
		Time time.Time `json:"time"`
		PSI  *struct {
			Some, Full struct {
				Total *uint64 `json:"total"`
			}
		} `json:"psi"`
	} `json:"io"`
}
type Metric struct {
	Name   string
	Labels map[string]string
	Value  float64
	At     time.Time
}

var metricNames = map[string]bool{
	"container_memory_usage_bytes": true, "container_memory_max_usage_bytes": true, "container_cpu_usage_seconds_total": true,
	"container_fs_reads_bytes_total": true, "container_fs_writes_bytes_total": true,
	"container_fs_reads_total": true, "container_fs_writes_total": true,
}

// Parse only server-provided cAdvisor families. Metric timestamps are required:
// container_last_seen is scrape time and cannot establish sample freshness.
func ParseMetrics(raw []byte) ([]Metric, error) {
	if len(raw) > MaxResponseBytes {
		return nil, errors.New("metrics response exceeds bound")
	}
	out := []Metric{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		end := strings.IndexAny(line, "{ \t")
		if end < 0 {
			continue
		}
		name := line[:end]
		if !metricNames[name] {
			continue
		}
		if len(out) >= 65536 {
			return nil, errors.New("metric count exceeds bound")
		}
		rest := line[end:]
		labels := map[string]string{}
		if strings.HasPrefix(rest, "{") {
			var err error
			labels, rest, err = parseLabels(rest)
			if err != nil {
				return nil, err
			}
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, errors.New("invalid cAdvisor counter")
		}
		stamp, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || stamp <= 0 {
			return nil, errors.New("invalid cAdvisor timestamp")
		}
		out = append(out, Metric{Name: name, Labels: labels, Value: value, At: time.UnixMilli(stamp).UTC()})
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("metric line exceeds bound")
	}
	return out, nil
}
func parseLabels(text string) (map[string]string, string, error) {
	labels := map[string]string{}
	i := 1
	bad := errors.New("invalid cAdvisor labels")
	for {
		for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		if i >= len(text) {
			return nil, "", bad
		}
		if text[i] == '}' {
			return labels, text[i+1:], nil
		}
		start := i
		for i < len(text) && ((text[i] >= 'a' && text[i] <= 'z') || (text[i] >= 'A' && text[i] <= 'Z') || (text[i] >= '0' && text[i] <= '9') || text[i] == '_') {
			i++
		}
		name := text[start:i]
		if name == "" || len(labels) >= 32 || i >= len(text) || text[i] != '=' {
			return nil, "", bad
		}
		i++
		if i >= len(text) || text[i] != '"' {
			return nil, "", bad
		}
		i++
		var value strings.Builder
		for {
			if i >= len(text) {
				return nil, "", bad
			}
			ch := text[i]
			i++
			if ch == '"' {
				break
			}
			if ch == '\\' {
				if i >= len(text) {
					return nil, "", bad
				}
				ch = text[i]
				i++
				switch ch {
				case '\\', '"':
				case 'n':
					ch = '\n'
				default:
					return nil, "", bad
				}
			}
			value.WriteByte(ch)
		}
		if _, duplicate := labels[name]; duplicate {
			return nil, "", bad
		}
		labels[name] = value.String()
		if i >= len(text) {
			return nil, "", bad
		}
		if text[i] == ',' {
			i++
			continue
		}
		if text[i] != '}' {
			return nil, "", bad
		}
	}
}
func CgroupMatches(id, podUID, containerID string) bool {
	if !validContainerID(containerID) || !strings.HasPrefix(id, "/") || path.Clean(id) != id {
		return false
	}
	_, cid, ok := strings.Cut(containerID, "://")
	if !ok || len(cid) != 64 {
		return false
	}
	pod := false
	container := false
	for _, part := range strings.Split(id, "/") {
		if part == "pod"+podUID || strings.HasSuffix(part, "-pod"+strings.ReplaceAll(podUID, "-", "_")+".slice") {
			pod = true
		}
		if part == cid || part == "cri-containerd-"+cid+".scope" || part == "crio-"+cid+".scope" || part == "docker-"+cid+".scope" {
			container = true
		}
	}
	return pod && container
}
func fresh(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now.Add(2*time.Second)) && now.Sub(at) <= MaxSampleAge
}
