package mcp

import (
	"bytes"
	"fmt"
	"os"
)

// readProcState returns the state letter of a process from /proc.
func readProcState(pid int) (byte, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The state follows the command name, which is in parentheses and may
	// itself hold a parenthesis.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 || end+2 >= len(stat) {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	return stat[end+2], nil
}
