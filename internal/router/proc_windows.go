//go:build windows

package router

import (
	"os"
	"os/exec"
	"syscall"
)

func spawnProc(command string, env []string) (Proc, error) {
	cmd := exec.Command("cmd.exe", "/c", command)
	cmd.Env = append(os.Environ(), env...)
	return startProc(cmd)
}

func (p *osProc) SignalTerm() error {
	return p.Kill()
}

func (p *osProc) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *osProc) Alive() bool {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(p.cmd.Process.Pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	s, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return s == syscall.WAIT_TIMEOUT
}
