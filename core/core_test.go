package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-0123456789"

// fakeDocker writes a shell script that records every call and imitates the few docker
// behaviours the core relies on. A camera whose URL contains "bad.example" fails to probe.
func fakeDocker(t *testing.T) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$@" >> "` + logFile + `"
case "$1" in
  ps)
    if [ -f "` + dir + `/running" ]; then echo "abc123"; fi ;;
  run)
    case "$*" in
      *--rm*)
        case "$*" in
          *bad.example*) echo "Connection refused"; exit 1 ;;
          *) echo "  Stream #0:0: Video: h264, yuv420p, 1920x1080, 25 fps"; exit 0 ;;
        esac ;;
      *) touch "` + dir + `/running"; echo "newcontainerid" ;;
    esac ;;
  exec)
    echo "  Stream #0:0: Video: hevc (Main), yuv420p(tv), 640x360, 100 tbr" ;;
  rm) rm -f "` + dir + `/running" ;;
esac
exit 0
`
	p := filepath.Join(dir, "docker")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dockerBin = p
	return logFile
}

func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	stateDir = t.TempDir()
	hddRoot = t.TempDir()
	applySettle = 0
	log := fakeDocker(t)
	srv := httptest.NewServer(newMux(testToken))
	t.Cleanup(srv.Close)
	return srv, log
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (int, map[string]interface{}) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestAuthRequired(t *testing.T) {
	srv, _ := setup(t)
	if c, _ := call(t, srv, "GET", "/v1/devices", "", false); c != 401 {
		t.Fatalf("no token: want 401 got %d", c)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: want 401 got %d", resp.StatusCode)
	}
	if c, _ := call(t, srv, "GET", "/v1/devices", "", true); c != 200 {
		t.Fatalf("good token: want 200 got %d", c)
	}
	resp, _ = http.Get(srv.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatal("healthz must be open")
	}
}

func TestSaveListDelete_PasswordNeverReturned(t *testing.T) {
	srv, _ := setup(t)
	_, m := call(t, srv, "POST", "/v1/devices", `{"nickname":"Gate","ip":"10.0.0.5","username":"admin","password":"s3cret#@"}`, true)
	if m["status"] != "ok" || m["device_id"] == "" {
		t.Fatalf("save: %v", m)
	}
	id := m["device_id"].(string)

	_, m = call(t, srv, "GET", "/v1/devices", "", true)
	devs := m["devices"].([]interface{})
	if len(devs) != 1 {
		t.Fatalf("want 1 device got %d", len(devs))
	}
	d := devs[0].(map[string]interface{})
	if d["password"] != "" || d["port"].(float64) != 554 {
		t.Fatalf("password must be blank and port default 554: %v", d)
	}
	// blank password on edit keeps the saved one
	call(t, srv, "POST", "/v1/devices", `{"id":"`+id+`","nickname":"Gate2","ip":"10.0.0.5","password":""}`, true)
	saved, _ := loadVisionDevices()
	if saved[0].Password != "s3cret#@" || saved[0].Nickname != "Gate2" {
		t.Fatalf("password not preserved: %+v", saved[0])
	}
	// file permissions: contains passwords
	if st, _ := os.Stat(devicesFile()); st.Mode().Perm() != 0600 {
		t.Fatalf("devices file mode %v", st.Mode().Perm())
	}
	// validation
	_, m = call(t, srv, "POST", "/v1/devices", `{"nickname":"","ip":""}`, true)
	if m["status"] != "error" {
		t.Fatal("empty nickname/ip must error")
	}
	call(t, srv, "DELETE", "/v1/devices/"+id, "", true)
	bgWG.Wait()
	left, _ := loadVisionDevices()
	if len(left) != 0 {
		t.Fatal("delete failed")
	}
}

func TestTestConnection(t *testing.T) {
	srv, _ := setup(t)
	_, m := call(t, srv, "POST", "/v1/devices/test", `{"ip":"10.0.0.9","username":"a","password":"b"}`, true)
	if m["status"] != "ok" {
		t.Fatalf("good camera: %v", m)
	}
	_, m = call(t, srv, "POST", "/v1/devices/test", `{"ip":"bad.example","username":"a","password":"b"}`, true)
	if m["status"] != "error" || !strings.Contains(m["message"].(string), "could not connect") {
		t.Fatalf("bad camera: %v", m)
	}
	_, m = call(t, srv, "POST", "/v1/devices/test", `{}`, true)
	if m["status"] != "error" {
		t.Fatal("missing ip must error")
	}
}

func TestApply_TwoPassAndFinalConfig(t *testing.T) {
	srv, logf := setup(t)
	call(t, srv, "POST", "/v1/devices", `{"id":"cam1","nickname":"Gate","ip":"10.0.0.5","username":"admin","password":"p"}`, true)
	_, m := call(t, srv, "POST", "/v1/apply", "", true)
	if m["status"] != "ok" {
		t.Fatalf("apply: %v", m)
	}
	cfg, err := os.ReadFile(configFile())
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	if !strings.Contains(s, "width: 640") || !strings.Contains(s, "height: 360") {
		t.Fatalf("final config must carry the DETECTED 640x360:\n%s", s)
	}
	if !strings.Contains(s, "cam1_ch1_sub") || !strings.Contains(s, "version: 0.17-0") {
		t.Fatalf("config incomplete:\n%s", s)
	}
	log, _ := os.ReadFile(logf)
	l := string(log)
	if n := strings.Count(l, "run -d --name gavety_vision"); n != 2 {
		t.Fatalf("Frigate must be started twice (two-pass), got %d:\n%s", n, l)
	}
	for _, want := range []string{"--label gavety.managed=true", "--restart unless-stopped", "-p 5150:5000", "--shm-size=178m", ":/config/config.yml", ":/media/frigate"} {
		if !strings.Contains(l, want) {
			t.Errorf("docker run missing %q", want)
		}
	}
	if !strings.Contains(l, "exec gavety_vision ffprobe") {
		t.Error("resolution probe must exec in the Frigate container")
	}
}

func TestApply_NoDevicesAndProbeFailure(t *testing.T) {
	srv, _ := setup(t)
	_, m := call(t, srv, "POST", "/v1/apply", "", true)
	if m["status"] != "error" {
		t.Fatal("apply with no devices must error")
	}
}

func TestDeleteLastCameraStopsFrigate(t *testing.T) {
	srv, logf := setup(t)
	call(t, srv, "POST", "/v1/devices", `{"id":"c1","nickname":"A","ip":"10.0.0.5"}`, true)
	call(t, srv, "POST", "/v1/apply", "", true)
	call(t, srv, "DELETE", "/v1/devices/c1", "", true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(logf)
		if strings.Contains(string(b), "stop gavety_vision") && strings.Contains(string(b), "rm gavety_vision") {
			bgWG.Wait()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Frigate was not stopped after the last camera was deleted")
}

func TestDrivesHeaderAndSettings(t *testing.T) {
	srv, _ := setup(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Gavety-Drives", `["/mnt/gavety_storage/pen010"]`)
	resp, _ := http.DefaultClient.Do(req)
	var m map[string]map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&m)
	if m["settings"]["storage_drive"] != "/mnt/gavety_storage/pen010" {
		t.Fatalf("should fall back to first connected drive, got %v", m)
	}
}

func TestSnapshotPathTraversalBlocked(t *testing.T) {
	srv, _ := setup(t)
	for _, p := range []string{"/v1/ai/snapshot/..%2f..%2fetc%2fpasswd", "/v1/ai/snapshot/x.txt", "/v1/ai/snapshot/..%2fvision_devices.json"} {
		if c, _ := call(t, srv, "GET", p, "", true); c != 404 {
			t.Errorf("%s: want 404 got %d", p, c)
		}
	}
}

func TestAIEndpointsEmpty(t *testing.T) {
	srv, _ := setup(t)
	_, m := call(t, srv, "GET", "/v1/ai/events", "", true)
	if m["status"] != "ok" {
		t.Fatalf("%v", m)
	}
	if _, ok := m["events"].([]interface{}); !ok {
		t.Fatalf("events must be [] not null: %v", m)
	}
	if c, _ := call(t, srv, "GET", "/v1/ai/status", "", true); c != 200 {
		t.Fatal("ai status")
	}
}

func TestParseFFprobe(t *testing.T) {
	d, err := parseFFprobeResolution("Stream #0:0: Video: hevc (Main), yuv420p(tv), 352x288, 100 tbr")
	if err != nil || d.Width != 352 || d.Height != 288 {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := parseFFprobeResolution("nothing here"); err == nil {
		t.Fatal("must fail")
	}
}

func TestStreamURLEncoding(t *testing.T) {
	u := buildStreamURL(VisionDevice{IP: "1.2.3.4", Port: 554, Username: "a", Password: "p#@ss"}, 2, true)
	if !strings.Contains(u, "p%23%40ss@1.2.3.4:554") || !strings.Contains(u, "channel=2&subtype=1") {
		t.Fatal(u)
	}
}
