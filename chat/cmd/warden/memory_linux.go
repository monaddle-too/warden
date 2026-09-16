//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// hostMemoryMB reads MemTotal from /proc/meminfo.
func hostMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return meminfoTotalMB(string(data))
}

func meminfoTotalMB(data string) int {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return kb / 1024
		}
	}
	return 0
}
