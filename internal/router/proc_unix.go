//go:build unix

package router

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func spawnProc(command string, env []string) (Proc, error) {
	argv := splitCommand(command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	// Exec the binary directly. Wrapping /bin/sh -c made Wait() return when
	// the shell died on SIGTERM while llama-server kept the GPU.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return startProc(cmd)
}

func (p *osProc) SignalTerm() error {
	return p.signalGroup(syscall.SIGTERM)
}

func (p *osProc) Kill() error {
	return p.signalGroup(syscall.SIGKILL)
}

func (p *osProc) signalGroup(sig syscall.Signal) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, sig)
	}
	if sig == syscall.SIGKILL {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(sig)
}

func (p *osProc) Alive() bool {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	return p.cmd.Process.Signal(syscall.Signal(0)) == nil
}
