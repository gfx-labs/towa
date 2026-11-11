package mcp

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// readProcState returns the state letter of a process from ps.
func readProcState(pid int) (byte, error) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	state := strings.TrimSpace(string(out))
	if state == "" {
		return 0, fmt.Errorf("ps printed no state for %d", pid)
	}
	return state[0], nil
}
