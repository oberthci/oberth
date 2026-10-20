//go:build linux

// This program is PID 1 inside the disposable guest, never a host helper.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type testConfig struct {
	Args []string
	Env  []string
}

func run() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("guest init must be PID 1")
	}
	for _, m := range [][3]string{
		{"devtmpfs", "/dev", "devtmpfs"},
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"tmpfs", "/tmp", "tmpfs"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2"},
		{"securityfs", "/sys/kernel/security", "securityfs"},
		{"tracefs", "/sys/kernel/tracing", "tracefs"},
	} {
		if err := os.MkdirAll(m[1], 0755); err != nil {
			return err
		}
		if err := syscall.Mount(m[0], m[1], m[2], 0, ""); err != nil {
			return fmt.Errorf("mount %s: %w", m[1], err)
		}
	}
	if err := syscall.Setrlimit(8 /* Linux RLIMIT_MEMLOCK */, &syscall.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)}); err != nil {
		return err
	}
	data, err := os.ReadFile("/test-config.json")
	if err != nil {
		return err
	}
	var config testConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	cmd := exec.Command("/tests", config.Args...)
	cmd.Env = append([]string{"PATH=/bin", "HOME=/tmp", "TMPDIR=/tmp", "GOMAXPROCS=2"}, config.Env...)
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer reader.Close()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return err
	}
	_ = writer.Close()
	passed, skipped, scanErr := testOutput(reader, os.Stdout)
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
		fmt.Fprintln(output, line)
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
