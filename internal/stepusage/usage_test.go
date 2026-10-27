package stepusage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCgroupV2Fixtures(t *testing.T) {
	u := ReadCgroupV2("testdata/v2", []byte("0::/\n"))
	if u.MemoryPeakBytes == nil || *u.MemoryPeakBytes != 120<<20 || u.UserCPU == nil || *u.UserCPU != 1500*time.Millisecond || u.SystemCPU == nil || *u.SystemCPU != 250500*time.Microsecond || u.MaxRSSBytes != nil {
		t.Fatalf("usage %+v", u)
	}
	zero := ReadCgroupV2("testdata/zero", []byte("0::/\n"))
	if zero.MemoryPeakBytes == nil || *zero.MemoryPeakBytes != 0 || zero.UserCPU == nil || *zero.UserCPU != 0 || zero.SystemCPU == nil || *zero.SystemCPU != 0 {
		t.Fatalf("zero lost: %+v", zero)
	}
	for _, fixture := range []string{"missing", "bad"} {
		unknown := ReadCgroupV2(filepath.Join("testdata", fixture), []byte("0::/\n"))
		raw, _ := json.Marshal(unknown)
		if string(raw) != `{"max_rss_bytes":null,"memory_peak_bytes":null,"user_cpu_ns":null,"system_cpu_ns":null}` {
			t.Fatalf("%s: %s", fixture, raw)
		}
	}
}

func TestCgroupV2DoesNotReadParentOnMissingSelf(t *testing.T) {
	for _, self := range []string{"", "1:cpu:/\n", "0::relative\n", "0::/missing\n", "0::/../\n", "0::/\n0::/\n"} {
		u := ReadCgroupV2("testdata/v2", []byte(self))
		if u.MemoryPeakBytes != nil || u.UserCPU != nil || u.SystemCPU != nil {
			t.Fatalf("read parent for %q: %+v", self, u)
		}
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "cpu.stat"), []byte("user_usec 42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u := ReadCgroupV2(root, []byte("0::/child\n"))
	if u.MemoryPeakBytes != nil || u.SystemCPU != nil || u.UserCPU == nil || *u.UserCPU != 42*time.Microsecond {
		t.Fatalf("partial %+v", u)
	}
}

func TestCgroupV2MalformedAndBounded(t *testing.T) {
	for _, cpu := range []string{"user_usec 1\nuser_usec 2\nsystem_usec 1", "user_usec -1", "system_usec nope", "system_usec 1 extra", strings.Repeat("x", 4097)} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "cpu.stat"), []byte(cpu), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "memory.peak"), []byte("42\n"), 0600); err != nil {
			t.Fatal(err)
		}
		u := ReadCgroupV2(root, []byte("0::/\n"))
		if u.MemoryPeakBytes == nil || *u.MemoryPeakBytes != 42 || u.UserCPU != nil || u.SystemCPU != nil {
			t.Fatalf("invalid cpu %q: %+v", cpu, u)
		}
	}
}

func TestSummaryRejectsMalformedAndPreservesNulls(t *testing.T) {
	for _, message := range []string{"", Prefix + `{"user_cpu_ns":-1}`, Prefix + `{"max_rss_bytes":1,"secret":"no"}`, Prefix + `{"user_cpu_ns":1} {}`, Prefix + strings.Repeat(" ", MaxMessageBytes)} {
		u := Decode(message)
		if u.MaxRSSBytes != nil || u.UserCPU != nil || u.SystemCPU != nil || u.MemoryPeakBytes != nil {
			t.Fatalf("accepted %q", message)
		}
	}
	u := ReadCgroupV2("testdata/zero", []byte("0::/\n"))
	got := Decode(Encode(u))
	if got.UserCPU == nil || *got.UserCPU != 0 || got.MemoryPeakBytes == nil || *got.MemoryPeakBytes != 0 || got.MaxRSSBytes != nil {
		t.Fatalf("round trip %+v", got)
	}
}
