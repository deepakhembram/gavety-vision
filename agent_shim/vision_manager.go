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

// Override at build time:  -ldflags "-X main.visionCoreImage=youruser/gavety-vision-core:1.0.0"
var visionCoreImage = "deepakdipu/gavety-vision-core:latest"

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
		visionCoreImage,
	}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		// Not present locally and pull failed, or a real run error: surface the docker message.
		return fmt.Errorf("could not start Vision core (%s): %s", visionCoreImage, strings.TrimSpace(string(out)))
	}
	for i := 0; i < 30; i++ { // up to ~15s
		if visionCoreHealthy() {
			log.Printf("✅ Vision core container running (%s)", visionCoreImage)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Vision core container started but is not answering on %s", visionCoreAddr)
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

// Called by docker_manager.go when the Vision app is installed.
func startVisionForInstall() error {
	res, err := visionCall("POST", "/v1/install", nil)
	if err != nil {
		return err
	}
	if s, _ := res["status"].(string); s != "ok" {
		m, _ := res["message"].(string)
		return fmt.Errorf("%s", m)
	}
	return nil
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
