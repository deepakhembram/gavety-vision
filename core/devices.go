package main

// devices.go - camera/DVR device list, settings and stream URLs.
// Ported from the agent's vision_manager.go. State files keep their ORIGINAL names
// and live in the same host folder (/etc/gavety), so existing installs carry over.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type VisionDevice struct {
	ID                string `json:"id"`
	Nickname          string `json:"nickname"`
	IP                string `json:"ip"`
	Port              int    `json:"port"` // default 554, editable
	Username          string `json:"username"`
	Password          string `json:"password"`
	DeviceType        string `json:"device_type"`         // "single" | "multi"
	Channels          int    `json:"channels"`            // only meaningful if DeviceType == "multi"
	StreamPathPattern string `json:"stream_path_pattern"` // e.g. "/cam/realmonitor?channel={n}&subtype={sub}"
	StorageDrive      string `json:"storage_drive"`
	StorageLimitGB    int    `json:"storage_limit_gb"`
	RetentionDays     int    `json:"retention_days"`
}

type VisionSettings struct {
	StorageDrive string `json:"storage_drive"` // e.g. "/mnt/gavety_storage/pen010"
}

// Paths. stateDir and hddRoot are set from the environment in main(); tests point them at temp dirs.
// IMPORTANT: stateDir and hddRoot are bind-mounted into this container at the SAME path as on the
// host, because the Frigate container is started through the host's Docker daemon, which resolves
// "-v <path>:..." against the HOST filesystem.
var (
	stateDir = "/etc/gavety"
	hddRoot  = "/mnt/gavety_storage"
)

func devicesFile() string  { return filepath.Join(stateDir, "vision_devices.json") }
func configFile() string   { return filepath.Join(stateDir, "vision_config.yml") }
func settingsFile() string { return filepath.Join(stateDir, "vision_settings.json") }

const (
	goRTCContainerName       = "gavety_vision" // MUST stay: vision.html and the app proxy find Frigate by this name
	defaultStreamPathPattern = "/cam/realmonitor?channel={n}&subtype={sub}"
)

var visionDevicesMu sync.Mutex

func newID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// The agent owns drive discovery (it knows which drives are really mounted). It sends the
// current list with every request; we remember the latest one.
var (
	drivesMu    sync.RWMutex
	knownDrives []string
)

func setKnownDrives(d []string) {
	drivesMu.Lock()
	knownDrives = append([]string(nil), d...)
	drivesMu.Unlock()
}

func listAvailableDrives() []string {
	drivesMu.RLock()
	defer drivesMu.RUnlock()
	return append([]string(nil), knownDrives...)
}

func loadVisionDevices() ([]VisionDevice, error) {
	visionDevicesMu.Lock()
	defer visionDevicesMu.Unlock()
	data, err := os.ReadFile(devicesFile())
	if os.IsNotExist(err) {
		return []VisionDevice{}, nil
	}
	if err != nil {
		return nil, err
	}
	var devices []VisionDevice
	if err := json.Unmarshal(data, &devices); err != nil {
		return nil, err
	}
	return devices, nil
}

func saveVisionDevices(devices []VisionDevice) error {
	visionDevicesMu.Lock()
	defer visionDevicesMu.Unlock()
	os.MkdirAll(stateDir, 0700)
	data, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return err
	}
	// 0600: contains camera passwords in plain text, same sensitivity as the agent secret.
	return os.WriteFile(devicesFile(), data, 0600)
}

func loadVisionSettings() VisionSettings {
	data, err := os.ReadFile(settingsFile())
	if err != nil {
		// HDD_ROOT is the PARENT of the drives, not a drive: recordings there would land on the
		// Pi's SD card. Fall back to the first really-connected drive instead.
		fallback := hddRoot
		if drives := listAvailableDrives(); len(drives) > 0 {
			fallback = drives[0]
		}
		return VisionSettings{StorageDrive: fallback}
	}
	var s VisionSettings
	json.Unmarshal(data, &s)
	if s.StorageDrive == "" {
		s.StorageDrive = hddRoot
	}
	return s
}

func saveVisionSettings(s VisionSettings) error {
	os.MkdirAll(stateDir, 0700)
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(settingsFile(), data, 0600)
}

func visionStoragePath() string {
	drive := loadVisionSettings().StorageDrive
	if drive == "" {
		if drives := listAvailableDrives(); len(drives) > 0 {
			drive = drives[0]
		} else {
			drive = hddRoot
		}
	}
	return filepath.Join(drive, "_vision_storage")
}

type resolvedCamera struct {
	Device  VisionDevice
	Channel int
}

func resolveCameras(devices []VisionDevice) []resolvedCamera {
	var out []resolvedCamera
	for _, dev := range devices {
		channels := dev.Channels
		if dev.DeviceType != "multi" || channels < 1 {
			channels = 1
		}
		for ch := 1; ch <= channels; ch++ {
			out = append(out, resolvedCamera{Device: dev, Channel: ch})
		}
	}
	return out
}

// buildStreamURL builds one channel's RTSP URL, URL-encoding user/password so characters
// like # and @ in a password work.
func buildStreamURL(dev VisionDevice, channel int, subStream bool) string {
	pattern := dev.StreamPathPattern
	if pattern == "" {
		pattern = defaultStreamPathPattern
	}
	sub := "0"
	if subStream {
		sub = "1"
	}
	path := strings.ReplaceAll(pattern, "{n}", fmt.Sprintf("%d", channel))
	path = strings.ReplaceAll(path, "{sub}", sub)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	userinfo := url.UserPassword(dev.Username, dev.Password)
	return fmt.Sprintf("rtsp://%s@%s:%d%s", userinfo.String(), dev.IP, dev.Port, path)
}

// channelName returns the stream/camera id used in the generated config, e.g. "dev123_ch1", "dev123_ch1_sub".
func channelName(dev VisionDevice, channel int, subStream bool) string {
	base := fmt.Sprintf("%s_ch%d", dev.ID, channel)
	if subStream {
		return base + "_sub"
	}
	return base
}
