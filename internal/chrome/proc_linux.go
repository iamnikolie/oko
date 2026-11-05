package chrome

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// osIdentity describes a live process through /proc: its start time (field
// 22 of /proc/<pid>/stat, clock ticks since boot) and its executable (the
// /proc/<pid>/exe link the kernel keeps).
func osIdentity(pid int) (time.Time, string, string, error) {
	if pid <= 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	dir := "/proc/" + strconv.Itoa(pid)
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return time.Time{}, "", "", errNoIdentity
	}
	// comm may hold spaces and parentheses: fields start after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return time.Time{}, "", "", errNoIdentity
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}, "", "", errNoIdentity
	}
	exe, err := os.Readlink(dir + "/exe")
	if err != nil {
		return time.Time{}, "", "", errNoIdentity
	}
	// USER_HZ is 100 on every Linux oko targets; the wall-clock start is
	// used only for the "started after launch" bound, the key is exact.
	start := bootTime().Add(time.Duration(ticks) * 10 * time.Millisecond)
	return start, "ticks:" + f[19], exe, nil
}

func bootTime() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "btime ") {
			if s, err := strconv.ParseInt(strings.TrimSpace(l[6:]), 10, 64); err == nil {
				return time.Unix(s, 0)
			}
		}
	}
	return time.Time{}
}
