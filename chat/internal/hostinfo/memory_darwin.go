package hostinfo

import (
	"os/exec"
	"strconv"
	"strings"
)

// totalMemory reads hw.memsize.
func totalMemory() (uint64, error) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}
