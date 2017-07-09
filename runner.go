package main

import (
	"fmt"
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
	"":        shellRunner{},
	"tmux":    tmuxRunner{},
	"launchd": &launchdRunner{},
}

func isValidRunner(str string) bool {
	_, ok := runners[str]
	return ok
}

func (t *Target) isExclusive() bool {
	return len(t.Runner) == 0
}

func (job *Job) isNoop() bool {
	if job.mode == TargetStop {
		return job.target.Runner == ""
	}
	return job.target.Command == ""
}

func runJob(job *Job) error {
	if len(job.target.Command) == 0 {
		return nil
	}

	runner := runners[job.target.Runner]
	if job.mode == TargetStart {
		return runner.start(job.target)
	}
	return runner.stop(job.target)
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

func (r *launchdRunner) isLoaded(label string) bool {
	if r.loadedServices == nil {
		services := make(map[string]bool)
		cmd := exec.Command("launchctl", "list")
		output, err := combinedOutputError(cmd)
		if err != nil {
			return false
		}

		column := 0
		start := 0
		for i, ch := range output {
			if ch == '\n' {
				label := output[start:i]
				services[string(label)] = true
				column = 0
			}
			if ch == '\t' {
				column++
				if column == 2 {
					// third column == label
					start = i + 1
				}
			}
		}
		r.loadedServices = services
	}
	return r.loadedServices[label]
}

func (r *launchdRunner) findLabel(filename string) (string, error) {
	cmd := exec.Command("defaults", "read", filename, "Label")
	output, err := combinedOutputError(cmd)
	return strings.TrimSpace(string(output)), err
}

func (r *launchdRunner) start(t *Target) error {
	label, err := r.findLabel(t.Command)
	if err != nil {
		return err
	}
	if r.isLoaded(label) {
		return nil
	}
	user, err := user.Current()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%s", user.Uid)
	cmd := exec.Command("launchctl", "bootstrap", domain, t.Command)
	_, err = combinedOutputError(cmd)
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

	for true {
		cmd := exec.Command("launchctl", "bootout", domain)
		_, err = combinedOutputError(cmd)
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			if status == 9216 {
				// Operation now in progress
				time.Sleep(500 * time.Millisecond)
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
