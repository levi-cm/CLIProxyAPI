//go:build linux

package auth

import (
	"fmt"
	"os"
	"testing"
)

func TestCapacityAvailableMemoryCgroup(t *testing.T) {
	for _, tc := range []struct {
		name, max, current, parentMax string
		want                          int64
		fail                          bool
	}{
		{"unlimited", "max", "100", "max", 5 << 30, false},
		{"container", "4294967296", "2147483648", "max", 2 << 30, false},
		{"browser pressure", "4294967296", "3758096384", "max", 512 << 20, false},
		{"ancestor", "max", "100", "1073741824", 1<<30 - 100, false},
		{"measurement failed", "bad", "100", "max", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"/proc/meminfo":                             "MemAvailable: 5242880 kB\n",
				"/proc/self/cgroup":                         "0::/scope/child\n",
				"/proc/self/mountinfo":                      "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
				"/sys/fs/cgroup/scope/child/memory.max":     tc.max,
				"/sys/fs/cgroup/scope/child/memory.current": tc.current,
				"/sys/fs/cgroup/scope/memory.max":           tc.parentMax,
				"/sys/fs/cgroup/scope/memory.current":       "100",
				"/sys/fs/cgroup/memory.max":                 "max",
				"/sys/fs/cgroup/memory.current":             "100",
			}
			got, err := measureCapacityMemory(func(path string) ([]byte, error) {
				v, ok := files[path]
				if !ok {
					return nil, fmt.Errorf("missing fixture %s", path)
				}
				return []byte(v), nil
			})
			if (err != nil) != tc.fail || err == nil && got != tc.want {
				t.Fatalf("got %d err %v want %d fail %v", got, err, tc.want, tc.fail)
			}
		})
	}
}

func TestCapacityMemoryRealV2RootHasNoLimitFile(t *testing.T) {
	files := map[string]string{
		"/proc/meminfo":                     "MemAvailable: 5242880 kB\n",
		"/proc/self/cgroup":                 "0::/scope\n",
		"/proc/self/mountinfo":              "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
		"/sys/fs/cgroup/scope/memory.max":   "max",
		"/sys/fs/cgroup/cgroup.controllers": "cpuset cpu io memory pids\n",
	}
	got, err := measureCapacityMemory(func(path string) ([]byte, error) {
		if value, ok := files[path]; ok {
			return []byte(value), nil
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	})
	if err != nil || got != 5<<30 {
		t.Fatalf("real hierarchy root rejected: memory=%d err=%v", got, err)
	}
}
