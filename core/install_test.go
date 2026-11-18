package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func waitInstall(t *testing.T, srv interface{}, want string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := install.snapshot()
		if m["state"] == want {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("install never reached %q, last: %v", want, install.snapshot())
	return nil
}

func TestPullStats(t *testing.T) {
	p := newPullStats()
	if m := p.feed("aaa111: Pulling fs layer"); !strings.Contains(m, "0 of 1") {
		t.Fatal(m)
	}
	p.feed("bbb222: Pulling fs layer")
	if m := p.feed("aaa111: Pull complete"); !strings.Contains(m, "1 of 2") {
		t.Fatal(m)
	}
	for _, noise := range []string{"Status: Downloaded newer image for x", "latest: Pulling from blakeblackshear/frigate", "Digest: sha256:abc", ""} {
		if m := p.feed(noise); m != "" {
			t.Errorf("noise %q produced %q", noise, m)
		}
	}
}

func TestInstallJobPullsThenStarts(t *testing.T) {
	srv, logf := setup(t)
	install.set("idle", "", "")
	_, m := call(t, srv, "POST", "/v1/install", "", true)
	if m["status"] != "ok" || m["started"] != true {
		t.Fatalf("%v", m)
	}
	done := waitInstall(t, srv, "done")
	bgWG.Wait()
	if done["message"] != "Gavety Vision is ready" {
		t.Errorf("%v", done)
	}
	_, st := call(t, srv, "GET", "/v1/install/status", "", true)
	if st["state"] != "done" {
		t.Errorf("status endpoint: %v", st)
	}
	l, _ := os.ReadFile(logf)
	ls := string(l)
	pi, ri := strings.Index(ls, "pull ghcr.io/blakeblackshear/frigate"), strings.Index(ls, "run -d --name gavety_vision")
	if pi < 0 || ri < 0 || pi > ri {
		t.Fatalf("must pull the image BEFORE starting Frigate:\n%s", ls)
	}
	if _, err := os.Stat(configFile()); err != nil {
		t.Error("empty config must be written at install")
	}
}

func TestInstallJobPullFailure(t *testing.T) {
	srv, _ := setup(t)
	old := visionFrigateImage
	visionFrigateImage = "example.com/badpull:x"
	defer func() { visionFrigateImage = old }()
	install.set("idle", "", "")
	call(t, srv, "POST", "/v1/install", "", true)
	m := waitInstall(t, srv, "error")
	bgWG.Wait()
	if !strings.Contains(m["error"].(string), "download failed") {
		t.Errorf("%v", m)
	}
}

func TestInstallIsNotStartedTwice(t *testing.T) {
	srv, _ := setup(t)
	install.set("running", "busy", "")
	_, m := call(t, srv, "POST", "/v1/install", "", true)
	if m["started"] != false {
		t.Fatalf("a running install must not be restarted: %v", m)
	}
	install.set("idle", "", "")
}
