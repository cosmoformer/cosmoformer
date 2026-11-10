//go:build linux

package worker

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// Wait without reaping first. Keeping the exact child waitable pins its PID
// until the supervisor lock covers reaping and the final group inspection.
func waitProgram(cmd *exec.Cmd, s *processSupervisor) (error, bool) {
	var info [128]byte
	var errno syscall.Errno
	for {
		_, _, errno = syscall.Syscall6(syscall.SYS_WAITID, 1, uintptr(cmd.Process.Pid), uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno != syscall.EINTR {
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errno != 0 {
		// Lost child identity cannot remain a signal capability for a reused PID.
		s.signal = nil
		return errno, false
	}
	e := cmd.Wait()
	empty := syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH
	// Reaping retires the PID/PGID signal capability even when descendants
	// survive. Once the leader is gone, later group-ID reuse cannot be fenced
	// by that exact child. Surviving descendants require independent host proof.
	s.signal = nil
	return e, empty
}
func signalProgram(cmd *exec.Cmd, kind string) bool {
	if kind == "SIGKILL" {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) == nil
	}
	return cmd.Process.Signal(syscall.SIGINT) == nil
}
