package main

// install.go - Vision install as a background job with visible progress.
// The Frigate image is large (well over 1 GB). Pulling it inside one blocking HTTP call made the
// dashboard look frozen and could hit client timeouts, so install now runs as a job: the caller
// starts it (POST /v1/install) and polls GET /v1/install/status for a human-readable message.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

type installJob struct {
	mu      sync.Mutex
	State   string `json:"state"` // idle | running | done | error
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	Started time.Time
}

var install = &installJob{State: "idle"}

func (j *installJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]interface{}{"status": "ok", "state": j.State, "message": j.Message}
	if j.Error != "" {
		out["error"] = j.Error
	}
	return out
}

func (j *installJob) set(state, msg, errMsg string) {
	j.mu.Lock()
	j.State, j.Message, j.Error = state, msg, errMsg
	j.mu.Unlock()
}

// startInstallJob starts the job unless one is already running. Returns true if it started one.
func startInstallJob() bool {
	install.mu.Lock()
	if install.State == "running" {
		install.mu.Unlock()
		return false
	}
	install.State, install.Message, install.Error, install.Started = "running", "Starting…", "", time.Now()
	install.mu.Unlock()
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		runInstall()
	}()
	return true
}

func runInstall() {
	install.set("running", "Downloading the Vision engine…", "")
	if err := pullWithProgress(visionFrigateImage, func(m string) { install.set("running", m, "") }); err != nil {
		log.Printf("⚠️ Vision install: pull failed: %v", err)
		install.set("error", "", "download failed: "+err.Error())
		return
	}
	install.set("running", "Starting the Vision engine…", "")
	if err := startVisionForInstall(); err != nil {
		install.set("error", "", err.Error())
		return
	}
	install.set("done", "Gavety Vision is ready", "")
}

// pullStats tracks layers seen in `docker pull` output (non-TTY mode prints one line per state change).
type pullStats struct {
	seen map[string]bool
	done map[string]bool
}

func newPullStats() *pullStats { return &pullStats{seen: map[string]bool{}, done: map[string]bool{}} }

// feed consumes one output line and returns a progress message ("" = nothing new to say).
func (p *pullStats) feed(line string) string {
	i := strings.Index(line, ": ")
	if i <= 0 || i > 20 || strings.ContainsAny(line[:i], " /") {
		return ""
	}
	id, state := line[:i], line[i+2:]
	switch {
	case strings.HasPrefix(state, "Pulling fs layer"), strings.HasPrefix(state, "Waiting"),
		strings.HasPrefix(state, "Downloading"), strings.HasPrefix(state, "Extracting"):
		p.seen[id] = true
	case strings.HasPrefix(state, "Pull complete"), strings.HasPrefix(state, "Already exists"):
		p.seen[id] = true
		p.done[id] = true
	default:
		return ""
	}
	return fmt.Sprintf("Downloading the Vision engine: %d of %d parts done (the first install can take 10+ minutes)", len(p.done), len(p.seen))
}

func pullWithProgress(image string, report func(string)) error {
	cmd := docker("pull", image)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	var tail []string
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		st := newPullStats()
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
			if m := st.feed(line); m != "" {
				report(m)
			}
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-scanDone
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.Join(tail, " | "))
	}
	return nil
}
