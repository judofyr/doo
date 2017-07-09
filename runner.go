package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"
)

type runner interface {
	start(*Target) error
	stop(*Target) error
}

var runners = map[string]runner{
	"shell":   shellRunner{},
	"tmux":    tmuxRunner{},
	"launchd": &launchdRunner{},
}

func isValidRunner(str string) bool {
	_, ok := runners[str]
	return ok
}

func (t *Target) isExclusive() bool {
	return t.Runner == "shell"
}

func (job *Job) isNoop() bool {
	if job.mode == TargetStop {
		return job.target.Runner == "shell"
	}
	return job.target.Command == ""
}

func runJob(job *Job) error {
	if len(job.target.Command) == 0 {
		return nil
	}

	runner := runners[job.target.Runner]
	if job.mode == TargetStop {
		return runner.stop(job.target)
	}

	err := runner.start(job.target)
	if err != nil {
		return err
	}

	for _, addr := range job.target.Listens {
		for i := 0; ; i++ {
			if i >= 10 {
				return fmt.Errorf("service didn't listen to: %s", addr)
			}
			listens, err := checkListens(addr)
			if err != nil {
				return err
			}
			if listens {
				break
			}
			time.Sleep(expSleepTime(i))
		}
	}
	return nil
}

func checkListens(addr string) (bool, error) {
	if addr[0] == '/' {
		_, err := os.Stat(addr)
		return !os.IsNotExist(err), nil
	}

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		operr := err.(*net.OpError)
		if syscallErr, ok := operr.Err.(*os.SyscallError); ok {
			if syscallErr.Err == syscall.ECONNREFUSED {
				return false, nil
			}
		}
	} else {
		conn.Close()
	}
	return true, err
}

func combinedOutputError(cmd *exec.Cmd) ([]byte, error) {
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 1 {
			err = fmt.Errorf("%s\n%s", err, string(output))
		}
		return nil, err
	}
	return output, nil
}

// Shell
type shellRunner struct{}

func (r shellRunner) start(t *Target) error {
	cmd := exec.Command("bash", "-c", t.Command)
	cmd.Dir = t.Cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r shellRunner) stop(t *Target) error {
	return nil
}

// Tmux
type tmuxRunner struct{}

func tmuxSessionExists(t *Target) bool {
	cmd := exec.Command("tmux", "has-session", "-t", t.Name)
	return cmd.Run() == nil
}

func (r tmuxRunner) start(t *Target) error {
	if tmuxSessionExists(t) {
		return nil
	}
	cmd := exec.Command("tmux", "new-session", "-d", "-s", t.Name)
	if len(t.Cwd) > 0 {
		cmd.Args = append(cmd.Args, "-c", t.Cwd)
	}
	cmd.Args = append(cmd.Args, ";", "send-keys", t.Command, "Enter")
	_, err := combinedOutputError(cmd)
	return err
}

func (r tmuxRunner) stop(t *Target) error {
	if !tmuxSessionExists(t) {
		return nil
	}
	cmd := exec.Command("tmux", "kill-session", "-t", t.Name)
	return cmd.Run()
}

// Launchd
type launchdRunner struct {
	loadedServices map[string]bool
}

func (r *launchdRunner) findLabel(filename string) (string, error) {
	cmd := exec.Command("defaults", "read", filename, "Label")
	output, err := combinedOutputError(cmd)
	return strings.TrimSpace(string(output)), err
}

func (r *launchdRunner) start(t *Target) error {
	user, err := user.Current()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%s", user.Uid)
	cmd := exec.Command("launchctl", "bootstrap", domain, t.Command)
	_, err = combinedOutputError(cmd)
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if status == 34048 {
			// service already loaded
			err = nil
		}
	}
	return err
}

func (r *launchdRunner) stop(t *Target) error {
	label, err := r.findLabel(t.Command)
	if err != nil {
		return err
	}

	user, err := user.Current()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%s/%s", user.Uid, label)

	for i := 0; ; i++ {
		cmd := exec.Command("launchctl", "bootout", domain)
		_, err = combinedOutputError(cmd)
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status == 9216 {
				// Operation now in progress
				time.Sleep(expSleepTime(i))
				continue
			}
			if status == 768 {
				// No such process aka: everything is okay!
				err = nil
			}
			break
		}
	}
	return err
}

func expSleepTime(i int) time.Duration {
	var res = 50 * time.Millisecond
	for ; i > 0; i-- {
		res *= 2
	}
	return res
}
