package main

// storage.go - per-camera storage limits. Frigate has no size-based limit of its own, so this
// periodically deletes the oldest recordings of any camera over its configured GB limit.
// Ported unchanged from the agent's vision_manager.go.

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type recordingFile struct {
	path    string
	size    int64
	modTime time.Time
}

func enforceVisionStorageLimits() {
	devices, err := loadVisionDevices()
	if err != nil {
		return
	}
	for _, rc := range resolveCameras(devices) {
		if rc.Device.StorageLimitGB <= 0 {
			continue
		}
		enforceOneCameraLimit(channelName(rc.Device, rc.Channel, false), int64(rc.Device.StorageLimitGB)*1024*1024*1024)
	}
}

func enforceOneCameraLimit(cameraID string, limitBytes int64) {
	storagePath := visionStoragePath()

	var files []recordingFile
	var total int64
	filepath.Walk(storagePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+cameraID+string(os.PathSeparator)) {
			files = append(files, recordingFile{path: path, size: info.Size(), modTime: info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if total <= limitBytes {
		return
	}

	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })

	freed, deleted := int64(0), 0
	for _, f := range files {
		if total-freed <= limitBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			freed += f.size
			deleted++
		}
	}
	if deleted > 0 {
		log.Printf("🗑️ Vision storage enforcement: %s was %.2fGB over its limit, removed %d oldest files (%.2fGB freed)",
			cameraID, float64(total-limitBytes)/(1<<30), deleted, float64(freed)/(1<<30))
	}
}

func StartVisionStorageEnforcement() {
	go func() {
		for {
			enforceVisionStorageLimits()
			time.Sleep(15 * time.Minute)
		}
	}()
}
