package main

// vision_manager.go (Docker edition) - REPLACES the old vision_manager.go.
//
// All camera / Frigate / AI logic now lives in the "gavety-vision-core" container. This file is
// only a thin bridge: it keeps every function name, type and constant the rest of the agent
// already uses (agent.go, local_server.go, docker_manager.go), forwards each call to the core's
// local HTTP API, and makes sure the core container is running.
//
// Nothing here knows how to talk to Frigate, ffprobe or the AI workers any more.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─── DATA MODEL (unchanged: agent.go / local_server.go embed these types) ───

type VisionDevice struct {
	ID                string `json:"id"`
	Nickname          string `json:"nickname"`
	IP                string `json:"ip"`
	Port              int    `json:"port"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	DeviceType        string `json:"device_type"`
	Channels          int    `json:"channels"`
	StreamPathPattern string `json:"stream_path_pattern"`
	StorageDrive      string `json:"storage_drive"`
	StorageLimitGB    int    `json:"storage_limit_gb"`
	RetentionDays     int    `json:"retention_days"`
}

type VisionSettings struct {
	StorageDrive string `json:"storage_drive"`
}

// State files keep their ORIGINAL paths. The core container mounts the same folder, so
// docker_manager.go's "remove Vision + wipe data" code keeps working untouched.
const visionDevicesFile = "/etc/gavety/vision_devices.json"
const visionConfigPath = "/etc/gavety/vision_config.yml"
const visionSettingsFile = "/etc/gavety/vision_settings.json"
const goRTCContainerName = "gavety_vision" // Frigate. Unchanged.
const visionAppID = "vision"

// ─── CORE CONTAINER ───

const (
	visionCoreContainer = "gavety_vision_core" // NOTE: no "gavety.managed" label on purpose (not shown as an app)
	visionCoreAddr      = "127.0.0.1:5160"
	visionCoreTokenFile = "/etc/gavety/vision_core_token"
)

// Default image. The app catalog ("companion_image" on the vision entry) overrides it, and the
// choice is remembered in visionCoreImageFile so it survives restarts. A build-time override
// still works:  -ldflags "-X main.visionCoreImage=youruser/gavety-vision-core:1.0.0"
var visionCoreImage = "deepakdipu/gavety-vision-core:latest"

const visionCoreImageFile = "/etc/gavety/vision_core_image"

var (
	visionCoreMu    sync.Mutex
	visionCoreToken string
	visionHTTP      = &http.Client{Timeout: 10 * time.Minute} // apply/install legitimately take minutes on a Pi
)

func visionCoreGetToken() string {
	if visionCoreToken != "" {
		return visionCoreToken
	}
	if b, err := os.ReadFile(visionCoreTokenFile); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		visionCoreToken = strings.TrimSpace(string(b))
		return visionCoreToken
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	visionCoreToken = hex.EncodeToString(raw)
	os.MkdirAll(filepath.Dir(visionCoreTokenFile), 0700)
	os.WriteFile(visionCoreTokenFile, []byte(visionCoreToken), 0600)
	return visionCoreToken
}

func visionCoreHealthy() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + visionCoreAddr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// ensureVisionCore starts (or restarts) the core container if it is not answering.
func ensureVisionCore() error {
	visionCoreMu.Lock()
	defer visionCoreMu.Unlock()
	if visionCoreHealthy() {
		return nil
	}
	token := visionCoreGetToken()

	// If the container exists but the token/image changed, or it crashed, recreate it.
	exec.Command("docker", "rm", "-f", visionCoreContainer).Run()

	// Same-path mounts: the core starts Frigate through the HOST's docker daemon, which resolves
	// "-v <path>" on the host, so the paths must be identical inside and outside.
	args := []string{"run", "-d",
		"--name", visionCoreContainer,
		"--restart", "unless-stopped",
		"--network", "host",
		"-e", "GAVETY_CORE_TOKEN=" + token,
		"-e", "GAVETY_CORE_ADDR=" + visionCoreAddr,
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", "/etc/gavety:/etc/gavety",
		"-v", HDD_ROOT + ":" + HDD_ROOT,
		visionCoreImageEffective(),
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		// Not present locally and pull failed, or a real run error: surface the docker message.
		return fmt.Errorf("could not start Vision core (%s): %s", visionCoreImageEffective(), strings.TrimSpace(string(out)))
	}
	for i := 0; i < 30; i++ { // up to ~15s
		if visionCoreHealthy() {
			log.Printf("✅ Vision core container running (%s)", visionCoreImageEffective())
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Vision core container started but is not answering on %s", visionCoreAddr)
}

var visionImageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{1,200}(:[A-Za-z0-9._-]{1,128})?(@sha256:[a-f0-9]{64})?$`)

// visionCoreImageEffective returns the image to run: the one remembered from the catalog,
// otherwise the built-in default.
func visionCoreImageEffective() string {
	if b, err := os.ReadFile(visionCoreImageFile); err == nil {
		if img := strings.TrimSpace(string(b)); visionImageRe.MatchString(img) {
			return img
		}
	}
	return visionCoreImage
}

// visionApplyCatalogImage is called after every catalog refresh with the vision entry's
// "companion_image". If it differs from what is running, the new image is pulled FIRST (a failed
// pull changes nothing and is retried at the next refresh), then remembered, then the core is
// recreated. Frigate and the recordings are not touched by a core restart.
func visionApplyCatalogImage(img string) {
	img = strings.TrimSpace(img)
	if img == "" || !visionImageRe.MatchString(img) || img == visionCoreImageEffective() {
		return
	}
	go func() {
		installed := visionInstalled() // decide before touching anything
		if !visionSwitchCoreImage(img) {
			return
		}
		if installed {
			if err := ensureVisionCore(); err != nil { // start it on the new image right away
				log.Printf("⚠️ %v", err)
			} else {
				log.Printf("✅ Vision core updated to %s", img)
			}
		}
	}()
}

// visionSwitchCoreImage pulls, remembers and retires the old core. Returns true if the switch happened.
func visionSwitchCoreImage(img string) bool {
	visionCoreMu.Lock()
	defer visionCoreMu.Unlock()
	if out, err := exec.Command("docker", "pull", img).CombinedOutput(); err != nil {
		log.Printf("⚠️ Vision core update to %s failed (pull): %s", img, strings.TrimSpace(string(out)))
		return false
	}
	os.MkdirAll(filepath.Dir(visionCoreImageFile), 0700)
	if err := os.WriteFile(visionCoreImageFile, []byte(img), 0600); err != nil {
		log.Printf("⚠️ could not remember Vision core image: %v", err)
		return false
	}
	exec.Command("docker", "rm", "-f", visionCoreContainer).Run()
	return true
}

// StartVisionStorageEnforcement keeps its old name because agent.go calls it at boot
// (`go StartVisionStorageEnforcement()`). It now means "start and watch the Vision core".
// Storage-limit enforcement itself runs inside the core.
func StartVisionStorageEnforcement() {
	if !visionInstalled() {
		return // Vision not installed on this box: startVisionForInstall brings the core up on demand
	}
	for {
		if err := ensureVisionCore(); err != nil {
			log.Printf("⚠️ %v", err)
		}
		time.Sleep(30 * time.Second)
	}
}

func visionInstalled() bool {
	out, _ := exec.Command("docker", "ps", "-a", "--filter", "name=^"+goRTCContainerName+"$", "--format", "{{.ID}}").Output()
	return strings.TrimSpace(string(out)) != ""
}

// visionCall sends one request to the core and returns the decoded JSON object.
func visionCall(method, path string, body interface{}) (map[string]interface{}, error) {
	if err := ensureVisionCore(); err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+visionCoreAddr+path, rdr)
	req.Header.Set("Authorization", "Bearer "+visionCoreGetToken())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if drives, err := json.Marshal(listAvailableDrives()); err == nil {
		req.Header.Set("X-Gavety-Drives", string(drives))
	}
	resp, err := visionHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Vision core unreachable: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Vision core returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("bad reply from Vision core: %v", err)
	}
	return out, nil
}

// reply copies the core's answer into a dashboard message of the given type.
func visionReply(d interface{ SendText(string) error }, msgType string, res map[string]interface{}, err error) {
	if err != nil {
		sendJSON(d, map[string]interface{}{"type": msgType, "status": "error", "message": err.Error()})
		return
	}
	out := map[string]interface{}{}
	for k, v := range res {
		out[k] = v
	}
	out["type"] = msgType
	sendJSON(d, out)
}

// ─── FUNCTIONS THE REST OF THE AGENT CALLS (same names/signatures as before) ───

// Called by docker_manager.go when the Vision app is installed (no progress messages).
func startVisionForInstall() error { return visionInstallRun(nil) }

// Same, but forwards download progress to the dashboard as "docker_install_progress" messages
// (the same message the generic app installer already sends). docker_manager.go calls this one.
func startVisionForInstallProgress(d interface{ SendText(string) error }) error {
	return visionInstallRun(func(msg string) {
		sendJSON(d, map[string]interface{}{"type": "docker_install_progress", "step": "pulling", "message": msg})
	})
}

// visionInstallRun starts the core's install job and waits for it. The Frigate image is large, so
// this can legitimately take many minutes; progress is reported whenever the message changes.
func visionInstallRun(report func(string)) error {
	res, err := visionCall("POST", "/v1/install", nil)
	if err != nil {
		return err
	}
	if s, _ := res["status"].(string); s == "error" {
		m, _ := res["message"].(string)
		return fmt.Errorf("%s", m)
	}
	if _, ok := res["started"]; !ok {
		return nil // an older core ran the whole install inside this one call: nothing to wait for
	}
	last, errCount, restarts := "", 0, 0
	deadline := time.Now().Add(45 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		st, err := visionCall("GET", "/v1/install/status", nil)
		if err != nil {
			if errCount++; errCount > 8 { // the core may be restarting for a moment: tolerate a few failures
				return err
			}
			continue
		}
		errCount = 0
		switch st["state"] {
		case "done":
			return nil
		case "error":
			m, _ := st["error"].(string)
			return fmt.Errorf("%s", m)
		case "idle": // the core restarted and lost the job: start it again (a few times at most)
			if restarts++; restarts > 3 {
				return fmt.Errorf("Vision install was interrupted")
			}
			if _, err := visionCall("POST", "/v1/install", nil); err != nil {
				return err
			}
			continue
		}
		if msg, _ := st["message"].(string); msg != "" && msg != last && report != nil {
			report(msg)
			last = msg
		}
	}
	return fmt.Errorf("Vision install is taking too long — check the internet connection and try again")
}

func handleVisionListDevices(d interface{ SendText(string) error }) {
	res, err := visionCall("GET", "/v1/devices", nil)
	visionReply(d, "vision_list_res", res, err)
}

func handleVisionTestConnection(d interface{ SendText(string) error }, dev VisionDevice) {
	res, err := visionCall("POST", "/v1/devices/test", dev)
	visionReply(d, "vision_test_res", res, err)
}

// Same two-message sequence as before: "saved" immediately, then "applied" when Frigate is ready.
func handleVisionSaveDevice(d interface{ SendText(string) error }, dev VisionDevice) {
	res, err := visionCall("POST", "/v1/devices", dev)
	visionReply(d, "vision_save_res", res, err)
	if err != nil || res["status"] != "ok" {
		return
	}
	res, err = visionCall("POST", "/v1/apply", nil)
	visionReply(d, "vision_apply_res", res, err)
}

func handleVisionDeleteDevice(d interface{ SendText(string) error }, deviceID string) {
	res, err := visionCall("DELETE", "/v1/devices/"+deviceID, nil)
	visionReply(d, "vision_delete_res", res, err)
}

func handleVisionGetSettings(d interface{ SendText(string) error }) {
	res, err := visionCall("GET", "/v1/settings", nil)
	if err == nil {
		res["available_drives"] = listAvailableDrives()
	}
	visionReply(d, "vision_settings_res", res, err)
}

func handleVisionSaveSettings(d interface{ SendText(string) error }, settings VisionSettings) {
	res, err := visionCall("POST", "/v1/settings", settings)
	visionReply(d, "vision_settings_save_res", res, err)
}

// listAvailableDrives: unchanged (the agent owns drive discovery).
func listAvailableDrives() []string {
	activeDrivesMu.RLock()
	defer activeDrivesMu.RUnlock()
	var drives []string
	for name := range activeDrives {
		drives = append(drives, filepath.Join(HDD_ROOT, name))
	}
	return drives
}

// Events/recordings proxy: unchanged. Frigate still publishes 127.0.0.1:5150.
func handleVisionEventsProxy(w http.ResponseWriter, r *http.Request, user GavetyUser) {
	frigatePath := r.URL.Path[len("/vision-events-proxy"):]
	target := fmt.Sprintf("http://127.0.0.1:5150%s", frigatePath)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := http.Get(target)
	if err != nil {
		http.Error(w, "could not reach Vision engine: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// ─── OPTIONAL: AI hazard endpoints (register in local_server.go when the dashboard tab exists) ───
//
//	mux.HandleFunc("/api/vision-ai/events",       withLocalAuth(handleVisionAIEvents))
//	mux.HandleFunc("/api/vision-ai/status",       withLocalAuth(handleVisionAIStatus))
//	mux.HandleFunc("/vision-ai-snapshot/",        withLocalAuth(handleVisionAISnapshot))

func visionCoreForward(w http.ResponseWriter, r *http.Request, path string) {
	if err := ensureVisionCore(); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req, _ := http.NewRequest("GET", "http://"+visionCoreAddr+path, nil)
	req.Header.Set("Authorization", "Bearer "+visionCoreGetToken())
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		http.Error(w, "Vision core unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleVisionAIEvents(w http.ResponseWriter, r *http.Request, user GavetyUser) {
	path := "/v1/ai/events"
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	visionCoreForward(w, r, path)
}

func handleVisionAIStatus(w http.ResponseWriter, r *http.Request, user GavetyUser) {
	visionCoreForward(w, r, "/v1/ai/status")
}

func handleVisionAISnapshot(w http.ResponseWriter, r *http.Request, user GavetyUser) {
	name := strings.TrimPrefix(r.URL.Path, "/vision-ai-snapshot/")
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	visionCoreForward(w, r, "/v1/ai/snapshot/"+name) // the core re-validates the name strictly
}
