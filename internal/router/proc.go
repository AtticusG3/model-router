package router

import (
	"fmt"
	"io"
	"os/exec"
)

// Proc is an OS-agnostic handle for a spawned backend process.
type Proc interface {
	PID() int
	Wait() error
	SignalTerm() error
	Kill() error
	Alive() bool
	Stdout() io.Reader
	Stderr() io.Reader
}

type spawner func(command string, env []string) (Proc, error)

// osProc wraps exec.Cmd. SignalTerm, Kill, and Alive are in the GOOS files.
type osProc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
}

func (p *osProc) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *osProc) Wait() error {
	return p.cmd.Wait()
}

func (p *osProc) Stdout() io.Reader { return p.stdout }
func (p *osProc) Stderr() io.Reader { return p.stderr }

func startProc(cmd *exec.Cmd) (Proc, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}
	return &osProc{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}
