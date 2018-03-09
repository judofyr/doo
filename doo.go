package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gobwas/glob"
	"gopkg.in/alecthomas/kingpin.v2"
)

// A Target is something that can be executed (by a runner)
type Target struct {
	Name         string
	Dependencies []string
	Invokes      []string
	Cwd          string
	Runner       string
	Command      string
	Listens      []string
	dependants   []*Target
	config       *dooConfig
}

const (
	// TargetStart means the target should be started
	TargetStart = 0
	// TargetStop means the target should be started
	TargetStop = 1
)

// A Job keep tracks of the execution of a target
type Job struct {
	target          *Target
	mode            int
	dependencyCount int
	dependentJobs   []*Job
	startedAt       *time.Time
	completedAt     *time.Time
	err             error
}

type jobMap map[string]*Job

type doo struct {
	targets            []*Target
	targetMap          map[string]*Target
	jobs               jobMap
	startedJobs        int
	completedJobs      int
	didError           bool
	completion         chan *Job
	homeDir            string
	isExclusiveRunning bool
	ignoreDependencies bool
}

type dooDefault struct {
	Cwd string
}

type dooConfig struct {
	Path     string
	Defaults dooDefault
	Targets  []*Target
}

func newDoo() *doo {
	var d doo
	d.reset()
	d.completion = make(chan *Job)
	usr, err := user.Current()
	if err == nil {
		d.homeDir = usr.HomeDir
	}
	return &d
}

func (d *doo) reset() {
	d.jobs = make(jobMap)
	d.startedJobs = 0
	d.completedJobs = 0
	d.didError = false
}

func (d *doo) validateTargets(errs *[]string) {
	d.targetMap = make(map[string]*Target)

	addError := func(f string, args ...interface{}) {
		*errs = append(*errs, fmt.Sprintf(f, args...))
	}

	// First build targetMap
	for _, target := range d.targets {
		path := target.config.Path
		name := target.Name

		if len(name) == 0 {
			addError("Target without name in %s", path)
			continue
		}

		if other, ok := d.targetMap[name]; ok {
			if other.config == target.config {
				addError("Duplicate definition for %s in %s", name, path)
			} else {
				addError("Duplicate definition for %s in %s and %s", name, path, other.config.Path)
			}
		}

		if !isValidRunner(target.Runner) {
			addError("Target %s in %s has invalid runner: %s", name, path, target.Runner)
		} else if target.Runner != "shell" && len(target.Command) == 0 {
			addError("Target %s in %s is missing command", name, path)
		}

		d.targetMap[name] = target
	}

	// Set up dependants
	for _, target := range d.targets {
		for _, dep := range target.Dependencies {
			other, ok := d.targetMap[dep]
			if ok {
				other.dependants = append(other.dependants, target)
			} else {
				addError("%s depends on unknown target %s", target.Name, dep)
			}
		}
	}
}

func (d *doo) expandPath(path string, from string) string {
	if path[0] == '~' {
		return d.homeDir + path[1:]
	} else if filepath.IsAbs(path) {
		return path
	} else {
		return filepath.Join(from, path)
	}
}

func (d *doo) loadConfigFile(fpath string) error {
	dir := filepath.Dir(fpath)
	conf := dooConfig{Path: fpath, Targets: nil}
	md, err := toml.DecodeFile(fpath, &conf)
	if err != nil {
		return err
	}

	keys := md.Undecoded()
	if len(keys) > 0 {
		return fmt.Errorf("unknown configuration: %v", keys)
	}

	var defaultCwd string
	if len(conf.Defaults.Cwd) > 0 {
		defaultCwd = d.expandPath(conf.Defaults.Cwd, dir)
	}

	for _, target := range conf.Targets {
		target.config = &conf

		if len(target.Cwd) == 0 {
			target.Cwd = defaultCwd
		} else {
			target.Cwd = d.expandPath(target.Cwd, dir)
		}

		if len(target.Runner) == 0 {
			target.Runner = "shell"
		}
	}
	d.targets = append(d.targets, conf.Targets...)
	return nil
}

func addJobDependency(from, to *Job) {
	from.dependencyCount++
	to.dependentJobs = append(to.dependentJobs, from)
}

func (d *doo) createStartJob(name string) *Job {
	job, ok := d.jobs[name]

	if ok {
		return job
	}

	job = new(Job)
	d.jobs[name] = job

	target := d.targetMap[name]
	job.target = target

	depCount := len(target.Dependencies)

	if d.ignoreDependencies || depCount == 0 {
		return job
	}

	for _, dep := range target.Dependencies {
		other := d.createStartJob(dep)
		addJobDependency(job, other)
	}

	return job
}

func (d *doo) createStopJob(name string) *Job {
	job, ok := d.jobs[name]
	if ok {
		return job
	}

	job = new(Job)
	job.mode = TargetStop
	d.jobs[name] = job

	target := d.targetMap[name]
	job.target = target

	if d.ignoreDependencies {
		return job
	}

	for _, other := range target.dependants {
		otherJob := d.createStopJob(other.Name)
		addJobDependency(job, otherJob)
	}

	return job
}

func (d *doo) hasRunningJobs() bool {
	return d.startedJobs > d.completedJobs
}

func (d *doo) hasCompleted() bool {
	return d.completedJobs == len(d.jobs)
}

func (d *doo) startJob(job *Job) {
	var now = time.Now()
	job.startedAt = &now
	d.startedJobs++
	if job.target.isExclusive() {
		d.isExclusiveRunning = true
	}
	d.logStart(job)
	go func() {
		err := runJob(job)
		var now = time.Now()
		job.completedAt = &now
		job.err = err
		d.completion <- job
	}()
}

func (d *doo) didComplete(job *Job) {
	d.completedJobs++
	if job.target.isExclusive() {
		d.isExclusiveRunning = false
	}
	for _, other := range job.dependentJobs {
		other.dependencyCount--
	}
	if job.err != nil {
		d.didError = true
	}

	if job.mode == TargetStart {
		for _, name := range job.target.Invokes {
			d.createStartJob(name)
		}
	}

	d.logComplete(job)
}

func (d *doo) nextJob() *Job {
	if d.isExclusiveRunning {
		return nil
	}

	for _, job := range d.jobs {
		if job.startedAt != nil {
			// Ignore running targets
			continue
		}
		if job.dependencyCount > 0 {
			// Missing dependencies
			continue
		}

		if job.target.isExclusive() && d.hasRunningJobs() {
			// Exclusive jobs can't run with other jobs
			continue
		}

		return job
	}

	return nil
}

func prettyDuration(dur time.Duration) string {
	if dur >= time.Minute {
		mins := dur / time.Minute
		secs := float64(dur%time.Minute) / float64(time.Second)
		return fmt.Sprintf("%dm%.3gs", mins, secs)
	} else if dur >= time.Second {
		return fmt.Sprintf("%.3gs", float64(dur)/float64(time.Second))
	} else if dur >= time.Millisecond {
		return fmt.Sprintf("%.3gms", float64(dur)/float64(time.Millisecond))
	} else if dur >= time.Microsecond {
		return fmt.Sprintf("%.3gµs", float64(dur)/float64(time.Microsecond))
	} else {
		return fmt.Sprintf("%dns", dur)
	}
}

func bold(s string) string {
	return fmt.Sprintf("\x1b[1m%s\x1b[0m", s)
}

func (d *doo) logStart(job *Job) {
	if job.isNoop() {
		return
	}
	action := "starting"
	if job.mode == TargetStop {
		action = "stopping"
	}
	fmt.Printf(">> %s %s\n", bold(job.target.Name), action)
}

func (d *doo) logComplete(job *Job) {
	if job.isNoop() {
		return
	}
	dur := job.completedAt.Sub(*job.startedAt)
	fmt.Printf("<< %s completed in %s\n", bold(job.target.Name), prettyDuration(dur))
	if job.err != nil {
		fmt.Printf("!! %s failed: %v\n", bold(job.target.Name), job.err)
	}
}

func (d *doo) runAllJobs() {
	for true {
		if d.didError {
			break
		}

		if d.hasCompleted() {
			break
		}

		job := d.nextJob()
		if job != nil {
			d.startJob(job)
		} else if d.hasRunningJobs() {
			job = <-d.completion
			d.didComplete(job)
		} else {
			break
		}
	}
}

var (
	stop    = kingpin.Flag("stop", "Stop specified targets").Bool()
	list    = kingpin.Flag("list", "List available targets").Bool()
	load    = kingpin.Flag("load", "Load configuration file").PlaceHolder("CONFIG").ExistingFiles()
	only    = kingpin.Flag("only", "Ignore dependencies").Bool()
	pwd     = kingpin.Flag("pwd", "Prints the directory for the target").Bool()
	targets = kingpin.Arg("target", "Target to start/stop").Strings()
)

func (d *doo) configDirectories() []string {
	var res []string

	addPath := func(path string) {
		if fi, err := os.Stat(path); err == nil {
			if fi.Mode().IsDir() {
				res = append(res, path)
			}
		}
	}

	// ~/.config/doo
	if len(d.homeDir) > 0 {
		addPath(filepath.Join(d.homeDir, ".config", "doo"))
	}

	// .doo in all parent directories
	if dir, err := os.Getwd(); err == nil {
		for true {
			addPath(filepath.Join(dir, ".doo"))
			newDir := filepath.Dir(dir)
			if newDir == dir {
				break
			}
			dir = newDir
		}
	}

	return res
}

func (d *doo) expandTargets(query []string) ([]string, error) {
	var res []string

	for _, q := range query {
		if _, ok := d.targetMap[q]; ok {
			res = append(res, q)
		} else {
			g, err := glob.Compile(q)
			if err != nil {
				return nil, fmt.Errorf("failed to parse pattern '%s': %s", q, err)
			}
			matchedAnything := false
			for _, target := range d.targets {
				if g.Match(target.Name) {
					matchedAnything = true
					res = append(res, target.Name)
				}
			}
			if !matchedAnything {
				return nil, fmt.Errorf("no target matched: %s", q)
			}
		}
	}

	return res, nil
}

func main() {
	kingpin.Parse()

	d := newDoo()
	var l = log.New(os.Stderr, "", 0)

	d.ignoreDependencies = *only

	var loadConfig = func(fpath string) {
		if err := d.loadConfigFile(fpath); err != nil {
			l.Fatalf("failed to parse %s: %s", fpath, err.Error())
		}
	}

	for _, dir := range d.configDirectories() {
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			// shouldn't happen because dooDirectories already checks it
			l.Fatalln(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".toml") {
				loadConfig(filepath.Join(dir, file.Name()))
			}
		}
	}

	for _, fpath := range *load {
		loadConfig(fpath)
	}

	var errs []string
	d.validateTargets(&errs)

	if len(errs) > 0 {
		l.Printf("found %d error(s):", len(errs))
		for _, err := range errs {
			l.Printf("- %s", err)
		}
		os.Exit(1)
	}

	expandedTargets, err := d.expandTargets(*targets)
	if err != nil {
		l.Fatalln(err)
	}

	if *pwd {
		for _, targetName := range expandedTargets {
			target := d.targetMap[targetName]
			fmt.Printf("%s\n", target.Cwd)
		}
		return
	}

	if *list {
		if len(*targets) == 0 {
			for _, target := range d.targets {
				fmt.Printf("%s\n", target.Name)
			}
		} else {
			for _, targetName := range expandedTargets {
				fmt.Printf("%s\n", targetName)
			}
		}
		return
	}

	if len(expandedTargets) == 0 {
		l.Fatalf("no targets. nothing to do.")
	}

	if *stop {
		for _, name := range expandedTargets {
			d.createStopJob(name)
		}
		d.runAllJobs()
	} else {
		for _, name := range expandedTargets {
			d.createStartJob(name)
		}
		d.runAllJobs()
	}

	if d.didError {
		os.Exit(1)
	} else if !d.hasCompleted() {
		l.Fatalln("doo is deadlocked. do you have a dependency cycle?")
	}
}
