package main

// api.go - the HTTP API the agent (or anything else on this machine) uses to drive Vision.
//
// Replies deliberately mirror what the old in-agent handlers sent to the dashboard (same "status" /
// "message" / "devices" keys), always with HTTP 200 for business errors, so the agent can relay
// them unchanged. Auth: "Authorization: Bearer <GAVETY_CORE_TOKEN>" on everything except /healthz.

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

var bgWG sync.WaitGroup // background applies/stops started by handlers

var version = "dev" // set at build time: -ldflags "-X main.version=1.2.3"

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func errBody(msg string) map[string]interface{} {
	return map[string]interface{}{"status": "error", "message": msg}
}

func authed(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The agent owns drive discovery and sends the currently-mounted drives with every call.
		if h := r.Header.Get("X-Gavety-Drives"); h != "" {
			var drives []string
			if json.Unmarshal([]byte(h), &drives) == nil {
				setKnownDrives(drives)
			}
		}
		next(w, r)
	}
}

func readDevice(r *http.Request) (VisionDevice, error) {
	var dev VisionDevice
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(&dev)
	return dev, err
}

func newMux(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	a := func(h http.HandlerFunc) http.HandlerFunc { return authed(token, h) }
	mux.HandleFunc("GET /v1/info", a(apiInfo))
	mux.HandleFunc("GET /v1/devices", a(apiListDevices))
	mux.HandleFunc("POST /v1/devices/test", a(apiTestDevice))
	mux.HandleFunc("POST /v1/devices", a(apiSaveDevice))
	mux.HandleFunc("DELETE /v1/devices/{id}", a(apiDeleteDevice))
	mux.HandleFunc("POST /v1/apply", a(apiApply))
	mux.HandleFunc("POST /v1/install", a(apiInstall))
	mux.HandleFunc("GET /v1/settings", a(apiGetSettings))
	mux.HandleFunc("POST /v1/settings", a(apiSaveSettings))
	mux.HandleFunc("GET /v1/ai/events", a(apiAIEvents))
	mux.HandleFunc("GET /v1/ai/status", a(apiAIStatus))
	mux.HandleFunc("GET /v1/ai/snapshot/{file}", a(apiAISnapshot))
	return mux
}

func apiInfo(w http.ResponseWriter, r *http.Request) {
	out, _ := docker("ps", "--filter", "name=^"+goRTCContainerName+"$", "--format", "{{.Status}}").Output()
	devs, _ := loadVisionDevices()
	writeJSON(w, map[string]interface{}{
		"status": "ok", "version": version, "devices": len(devs),
		"frigate_container": strings.TrimSpace(string(out)), // empty = not running
		"storage_path":      visionStoragePath(),
	})
}

func apiListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := loadVisionDevices()
	if err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	safe := make([]VisionDevice, len(devices))
	copy(safe, devices)
	for i := range safe {
		safe[i].Password = "" // never send saved passwords back to the browser
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "devices": safe})
}

func apiTestDevice(w http.ResponseWriter, r *http.Request) {
	dev, err := readDevice(r)
	if err != nil || dev.IP == "" {
		writeJSON(w, errBody("IP address is required"))
		return
	}
	// Blank password on an existing device means "use the saved one" (so editing doesn't force retyping).
	if dev.Password == "" && dev.ID != "" {
		if existing, err := loadVisionDevices(); err == nil {
			for _, d := range existing {
				if d.ID == dev.ID {
					dev.Password = d.Password
					break
				}
			}
		}
	}
	if err := testDeviceConnection(dev); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "message": "Connection successful"})
}

// apiSaveDevice only SAVES. The caller then calls /v1/apply (it takes seconds), which keeps the
// dashboard's original two-message sequence: "saved", then "applied".
func apiSaveDevice(w http.ResponseWriter, r *http.Request) {
	dev, err := readDevice(r)
	if err != nil {
		writeJSON(w, errBody("invalid request"))
		return
	}
	if dev.Nickname == "" || dev.IP == "" {
		writeJSON(w, errBody("nickname and IP address are required"))
		return
	}
	if dev.Port == 0 {
		dev.Port = 554
	}
	if dev.ID == "" {
		dev.ID = newID(8)
	}
	devices, err := loadVisionDevices()
	if err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	found := false
	for i, existing := range devices {
		if existing.ID == dev.ID {
			if dev.Password == "" {
				dev.Password = existing.Password
			}
			devices[i] = dev
			found = true
			break
		}
	}
	if !found {
		devices = append(devices, dev)
	}
	if err := saveVisionDevices(devices); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "device_id": dev.ID, "message": "Device saved — applying configuration..."})
}

func apiDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	devices, err := loadVisionDevices()
	if err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	var updated []VisionDevice
	for _, d := range devices {
		if d.ID != id {
			updated = append(updated, d)
		}
	}
	if err := saveVisionDevices(updated); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok"})
	bgWG.Add(1)
	go func() { // same as before: last camera removed => stop Frigate, otherwise re-apply
		defer bgWG.Done()
		if len(updated) == 0 {
			stopVisionContainer()
			return
		}
		applyVisionConfig()
	}()
}

func apiApply(w http.ResponseWriter, r *http.Request) {
	if err := applyVisionConfig(); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "message": "Gavety Vision is ready"})
}

func apiInstall(w http.ResponseWriter, r *http.Request) {
	if err := startVisionForInstall(); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok"})
}

func apiGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"status": "ok", "settings": loadVisionSettings()})
}

func apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	var s VisionSettings
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(&s); err != nil {
		writeJSON(w, errBody("invalid request"))
		return
	}
	if err := saveVisionSettings(s); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	if err := applyVisionConfig(); err != nil {
		writeJSON(w, errBody("settings saved, but failed to apply: "+err.Error()))
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok"})
}

func apiAIEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	if limit < 1 || limit > 500 {
		limit = 50
	}
	evs := vaiReadEvents(limit, r.URL.Query().Get("camera"))
	if evs == nil {
		evs = []VisionAIEvent{}
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "events": evs})
}

func apiAIStatus(w http.ResponseWriter, r *http.Request) { writeJSON(w, vaiStatus()) }

func apiAISnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !vaiSnapRe.MatchString(name) { // strict allow-list: no path traversal possible
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, vaiStorageDir()+string(os.PathSeparator)+"snapshots"+string(os.PathSeparator)+name)
}
