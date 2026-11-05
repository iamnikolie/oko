package chrome

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	procInfoCallPIDInfo = 2    // PROC_INFO_CALL_PIDINFO
	procPIDPathInfo     = 11   // PROC_PIDPATHINFO
	procPIDPathMax      = 4096 // PROC_PIDPATHINFO_MAXSIZE
)

// osIdentity describes a live process through the kernel: its start time
// (kern.proc.pid, microseconds) and its executable (proc_pidpath, the file
// the process image came from, not anything the process can rewrite).
func osIdentity(pid int) (time.Time, string, string, error) {
	if pid <= 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return time.Time{}, "", "", errNoIdentity
	}
	tv := kp.Proc.P_starttime
	if tv.Sec == 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	buf := make([]byte, procPIDPathMax)
	// proc_pidpath(3) is __proc_info(2) with this flavor; libproc returns
	// strlen of the NUL-terminated path it wrote.
	_, _, e := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if e != 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	if n == 0 {
		return time.Time{}, "", "", errNoIdentity
	}
	start := time.Unix(int64(tv.Sec), int64(tv.Usec)*1000)
	return start, fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), string(buf[:n]), nil
}
