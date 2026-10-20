//go:build linux

// This program is PID 1 inside the disposable guest, never a host helper.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type testConfig struct {
	Profile string
	Args    []string
	Env     []string
}

func run() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("guest init must be PID 1")
	}
	data, err := os.ReadFile("/test-config.json")
	if err != nil {
		return err
	}
	var config testConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	mounts, err := guestMounts(config.Profile)
	if err != nil {
		return err
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m[1], 0750); err != nil {
			return err
		}
		if err := syscall.Mount(m[0], m[1], m[2], 0, ""); err != nil {
			return fmt.Errorf("mount %s: %w", m[1], err)
		}
	}
	if config.Profile == "fuse" {
		if err := requireFuse("/dev/fuse"); err != nil {
			return err
		}
	}
	if err := syscall.Setrlimit(8 /* Linux RLIMIT_MEMLOCK */, &syscall.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)}); err != nil {
		return err
	}
	return runTests(config)
}

func guestMounts(profile string) ([][3]string, error) {
	mounts := [][3]string{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"tmpfs", "/tmp", "tmpfs"},
	}
	switch profile {
	case "", "bpf":
		mounts = append(mounts, [3]string{"cgroup2", "/sys/fs/cgroup", "cgroup2"}, [3]string{"securityfs", "/sys/kernel/security", "securityfs"}, [3]string{"tracefs", "/sys/kernel/tracing", "tracefs"})
	case "fuse":
	default:
		return nil, fmt.Errorf("unknown guest test profile %q", profile)
	}
	return mounts, nil
}

func requireFuse(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required guest FUSE device: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("required guest FUSE device is not a character device")
	}
	return nil
}

func runTests(config testConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	// #nosec G204 -- The fixed candidate test binary and arguments run only inside the disposable guest.
	cmd := exec.CommandContext(ctx, "/tests", config.Args...)
	cmd.Env = append([]string{"PATH=/bin", "HOME=/tmp", "TMPDIR=/tmp", "GOMAXPROCS=2"}, config.Env...)
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return err
	}
	_ = writer.Close()
	passed, skipped, scanErr := testOutput(reader, os.Stdout)
	if scanErr != nil {
		cancel()
	}
	err = cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	if err != nil {
		return err
	}
	if passed == 0 || skipped != 0 {
		return fmt.Errorf("guest tests: %d passes, %d skips", passed, skipped)
	}
	fmt.Printf("guest tests: %d passes, zero skips\n", passed)
	return nil
}

func testOutput(reader io.Reader, output io.Writer) (int, int, error) {
	passed, skipped := 0, 0
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintln(output, line); err != nil {
			return passed, skipped, err
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "--- PASS:") {
			passed++
		}
		if strings.HasPrefix(line, "--- SKIP:") {
			skipped++
		}
	}
	return passed, skipped, scanner.Err()
}

func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "guest init refuses host execution")
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Printf("OBERTH_GUEST_TEST_FAIL: %v\n", err)
	} else {
		fmt.Println("OBERTH_GUEST_TEST_PASS")
	}
	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}
