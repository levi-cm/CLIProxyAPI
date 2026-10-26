//go:build linux

package auth

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func capacityAvailableMemory() (int64, error) { return measureCapacityMemory(os.ReadFile) }

// Bound host MemAvailable by every applicable memory cgroup ancestor. Reading
// the mount root matters inside containers and systemd slices; checking only
// the leaf can miss a tighter parent. Any uncertain measurement fails closed.
func measureCapacityMemory(read func(string) ([]byte, error)) (int64, error) {
	info, err := read("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	available := int64(-1)
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			kb, errParse := strconv.ParseInt(fields[1], 10, 64)
			if errParse != nil || kb < 0 || kb > math.MaxInt64/1024 {
				return 0, fmt.Errorf("invalid available memory")
			}
			available = kb * 1024
		}
	}
	if available < 0 {
		return 0, fmt.Errorf("available memory missing")
	}
	groupData, err := read("/proc/self/cgroup")
	if err != nil {
		return 0, err
	}
	mountData, err := read("/proc/self/mountinfo")
	if err != nil {
		return 0, err
	}
	for _, group := range strings.Split(strings.TrimSpace(string(groupData)), "\n") {
		parts := strings.SplitN(group, ":", 3)
		if len(parts) != 3 {
			return 0, fmt.Errorf("invalid cgroup membership")
		}
		v2 := parts[0] == "0" && parts[1] == ""
		if !v2 && !containsMemoryController(parts[1]) {
			continue
		}
		found := false
		for _, line := range strings.Split(string(mountData), "\n") {
			halves := strings.SplitN(line, " - ", 2)
			if len(halves) != 2 {
				continue
			}
			left, right := strings.Fields(halves[0]), strings.Fields(halves[1])
			if len(left) < 5 || len(right) < 3 {
				continue
			}
			if v2 && right[0] != "cgroup2" || !v2 && (right[0] != "cgroup" || !containsMemoryController(right[2])) {
				continue
			}
			root, mount := unescapeMountPath(left[3]), unescapeMountPath(left[4])
			rel, errRel := filepath.Rel(root, parts[2])
			if errRel != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return 0, fmt.Errorf("cgroup hierarchy cannot be resolved")
			}
			leaf := filepath.Join(mount, rel)
			for current := leaf; ; current = filepath.Dir(current) {
				maxName, usedName := "memory.max", "memory.current"
				if !v2 {
					maxName = "memory.limit_in_bytes"
					usedName = "memory.usage_in_bytes"
				}
				maxData, errMax := read(filepath.Join(current, maxName))
				if errMax != nil {
					// The genuine cgroup-v2 hierarchy root has no memory.max/current.
					// A limited mounted subtree still has controls and must be measured.
					if v2 && root == "/" && current == mount && os.IsNotExist(errMax) {
						if _, errControllers := read(filepath.Join(current, "cgroup.controllers")); errControllers == nil {
							break
						}
					}
					return 0, errMax
				}
				raw := strings.TrimSpace(string(maxData))
				if raw != "max" {
					limit, errLimit := strconv.ParseInt(raw, 10, 64)
					if errLimit != nil || limit < 0 {
						return 0, fmt.Errorf("invalid cgroup memory limit")
					}
					usedData, errUsed := read(filepath.Join(current, usedName))
					if errUsed != nil {
						return 0, errUsed
					}
					used, errParse := strconv.ParseInt(strings.TrimSpace(string(usedData)), 10, 64)
					if errParse != nil || used < 0 {
						return 0, fmt.Errorf("invalid cgroup memory usage")
					}
					headroom := limit - used
					if headroom < 0 {
						headroom = 0
					}
					if headroom < available {
						available = headroom
					}
				}
				if current == mount {
					break
				}
				if current == "/" {
					return 0, fmt.Errorf("cgroup mount boundary missing")
				}
			}
			found = true
			break
		}
		if !found {
			return 0, fmt.Errorf("memory cgroup mount missing")
		}
	}
	return available, nil
}
func containsMemoryController(s string) bool {
	for _, v := range strings.Split(s, ",") {
		if v == "memory" {
			return true
		}
	}
	return false
}
func unescapeMountPath(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
