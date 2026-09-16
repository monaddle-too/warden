package hostinfo

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// totalMemory reads MemTotal from /proc/meminfo.
func totalMemory() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseMemInfo(bufio.NewScanner(f))
}

func parseMemInfo(scanner *bufio.Scanner) (uint64, error) {
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, errors.New("MemTotal not found in /proc/meminfo")
}
