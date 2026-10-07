package pipkin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processInfo(pid int) (processIdentity, error) {
	folder := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(folder, "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	// The parenthesized process name can contain spaces and closing parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return processIdentity{}, fmt.Errorf("invalid process information for PID %d", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return processIdentity{}, fmt.Errorf("incomplete process information for PID %d", pid)
	}
	executable, err := os.Readlink(filepath.Join(folder, "exe"))
	if err != nil {
		return processIdentity{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{Started: strings.TrimSpace(string(boot)) + ":" + fields[19], Executable: strings.TrimSuffix(executable, " (deleted)")}, nil
}
