package main

// frigate.go - generates Frigate's config and manages the Frigate container.
// Ported from the agent's vision_manager.go: same two-pass resolution detection, same docker
// commands, same container name/ports/label. The only structural changes are that docker is
// invoked through dockerBin (so tests can substitute a fake) and applies are serialised.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	dockerBin          = "docker"
	visionFrigateImage = "ghcr.io/blakeblackshear/frigate:stable"
	applySettle        = 8 * time.Second // how long go2rtc gets to open streams before we probe them
	applyMu            sync.Mutex        // two quick saves must not race docker rm / docker run
)

const (
	// Frigate must carry this label: the agent lists installed apps by it.
	// The CORE container itself must NOT carry it (it would show up as a second app).
	gavetyDockerLabel = "gavety.managed=true"
	// Placeholder size for the first pass only; never written to the final config.
)

var safePlaceholderDims = camDimensions{Width: 640, Height: 480}

type camDimensions struct {
	Width  int
	Height int
}

func docker(args ...string) *exec.Cmd { return exec.Command(dockerBin, args...) }

func generateVisionConfig(devices []VisionDevice, detectDims map[string]camDimensions) (string, error) {
	cameras := resolveCameras(devices)

	var b strings.Builder
	b.WriteString("mqtt:\n  enabled: false\n\ngo2rtc:\n  streams:\n")
	for _, rc := range cameras {
		name := channelName(rc.Device, rc.Channel, false)
		subName := channelName(rc.Device, rc.Channel, true)
		fmt.Fprintf(&b, "    %s:\n      - %s\n", name, buildStreamURL(rc.Device, rc.Channel, false))
		fmt.Fprintf(&b, "    %s:\n      - %s\n", subName, buildStreamURL(rc.Device, rc.Channel, true))
	}

	b.WriteString("\ncameras:\n")
	for _, rc := range cameras {
		name := channelName(rc.Device, rc.Channel, false)
		subName := channelName(rc.Device, rc.Channel, true)

		dims, ok := detectDims[subName]
		if !ok {
			// Fail loudly rather than write a guessed resolution (that produced stretched video once).
			return "", fmt.Errorf("no detected resolution for %s — run detection before generating config", subName)
		}

		fmt.Fprintf(&b, "  %s:\n", name)
		b.WriteString("    ffmpeg:\n      inputs:\n")
		fmt.Fprintf(&b, "        - path: rtsp://127.0.0.1:8554/%s\n", subName)
		b.WriteString("          input_args: preset-rtsp-restream\n          roles:\n            - detect\n")
		fmt.Fprintf(&b, "        - path: rtsp://127.0.0.1:8554/%s\n", name)
		b.WriteString("          input_args: preset-rtsp-restream\n          roles:\n            - record\n")
		fmt.Fprintf(&b, "    detect:\n      width: %d\n      height: %d\n      fps: 5\n", dims.Width, dims.Height)
		retentionDays := rc.Device.RetentionDays
		if retentionDays <= 0 {
			retentionDays = 7
		}
		fmt.Fprintf(&b, "    record:\n      enabled: true\n      motion:\n        days: %d\n", retentionDays)
	}
	b.WriteString("\nversion: 0.17-0\n")

	os.MkdirAll(filepath.Dir(configFile()), 0700)
	if err := os.WriteFile(configFile(), []byte(b.String()), 0644); err != nil {
		return "", err
	}
	return b.String(), nil
}

func detectCameraResolution(streamName string) (camDimensions, error) {
	out, err := docker("exec", goRTCContainerName,
		"ffprobe",
		"-rtsp_transport", "tcp",
		"-analyzeduration", "15000000",
		"-probesize", "10000000",
		fmt.Sprintf("rtsp://127.0.0.1:8554/%s", streamName),
	).CombinedOutput()
	if err != nil {
		// ffprobe can exit non-zero even when it printed the stream info: parse first.
		if dims, perr := parseFFprobeResolution(string(out)); perr == nil {
			return dims, nil
		}
		return camDimensions{}, fmt.Errorf("ffprobe failed for %s: %v — %s", streamName, err, string(out))
	}
	return parseFFprobeResolution(string(out))
}

var ffprobeResolutionRe = regexp.MustCompile(`(\d{2,5})x(\d{2,5})`)

// parseFFprobeResolution extracts WxH from e.g.
//
//	Stream #0:0: Video: hevc (Main), yuv420p(tv), 352x288, 100 tbr, 90k tbn
func parseFFprobeResolution(output string) (camDimensions, error) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Video:") {
			continue
		}
		if m := ffprobeResolutionRe.FindStringSubmatch(line); len(m) == 3 {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			if w > 0 && h > 0 {
				return camDimensions{Width: w, Height: h}, nil
			}
		}
	}
	return camDimensions{}, fmt.Errorf("could not find video resolution in ffprobe output")
}

func detectAllResolutions(devices []VisionDevice) (map[string]camDimensions, error) {
	dims := make(map[string]camDimensions)
	for _, rc := range resolveCameras(devices) {
		subName := channelName(rc.Device, rc.Channel, true)
		d, err := detectCameraResolution(subName)
		if err != nil {
			return nil, fmt.Errorf("resolution detection failed for %s (%s channel %d): %v",
				subName, rc.Device.Nickname, rc.Channel, err)
		}
		dims[subName] = d
	}
	return dims, nil
}

func ensureVisionContainerRunning() error {
	storagePath := visionStoragePath()
	os.MkdirAll(storagePath, 0755)

	checkOut, _ := docker("ps", "-a", "--filter", "name=^"+goRTCContainerName+"$", "--format", "{{.ID}}").Output()
	if strings.TrimSpace(string(checkOut)) != "" {
		docker("stop", goRTCContainerName).Run()
		docker("rm", goRTCContainerName).Run()
	}

	args := []string{
		"run", "-d",
		"--name", goRTCContainerName,
		"--label", gavetyDockerLabel,
		"--restart", "unless-stopped",
		"--shm-size=178m",
		"-v", configFile() + ":/config/config.yml",
		"-v", storagePath + ":/media/frigate",
		"-p", "5150:5000",
		// 8554 (go2rtc restream) is intentionally NOT published: with two ports the app proxy's
		// "last port listed" lookup picked the wrong one (the old Live View bug).
		visionFrigateImage,
	}
	if out, err := docker(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to start Vision container: %s", string(out))
	}
	log.Println("✅ Gavety Vision (Frigate engine) started")
	return nil
}

// applyVisionConfig: placeholder config -> start Frigate -> detect real resolutions -> final config -> restart.
func applyVisionConfig() error {
	applyMu.Lock()
	defer applyMu.Unlock()

	devices, err := loadVisionDevices()
	if err != nil {
		return fmt.Errorf("could not load devices: %v", err)
	}
	if len(devices) == 0 {
		return fmt.Errorf("no devices configured")
	}

	placeholder := map[string]camDimensions{}
	for _, rc := range resolveCameras(devices) {
		placeholder[channelName(rc.Device, rc.Channel, true)] = safePlaceholderDims
	}
	if _, err := generateVisionConfig(devices, placeholder); err != nil {
		return fmt.Errorf("pass 1 config generation failed: %v", err)
	}
	if err := ensureVisionContainerRunning(); err != nil {
		return err
	}

	time.Sleep(applySettle)

	realDims, err := detectAllResolutions(devices)
	if err != nil {
		return fmt.Errorf("resolution detection failed: %v — check camera credentials and connectivity", err)
	}
	if _, err := generateVisionConfig(devices, realDims); err != nil {
		return fmt.Errorf("pass 2 config generation failed: %v", err)
	}
	if err := ensureVisionContainerRunning(); err != nil {
		return err
	}
	log.Println("✅ Gavety Vision config applied with real, detected resolutions")
	return nil
}

// startVisionForInstall gets Frigate running at install time with zero cameras.
func startVisionForInstall() error {
	applyMu.Lock()
	defer applyMu.Unlock()
	devices, _ := loadVisionDevices()
	if _, err := generateVisionConfig(devices, map[string]camDimensions{}); err != nil {
		return err
	}
	return ensureVisionContainerRunning()
}

// testDeviceConnection validates a device BEFORE it is saved, using Frigate's own ffprobe.
func testDeviceConnection(dev VisionDevice) error {
	out, err := docker("run", "--rm",
		"--entrypoint", "ffprobe",
		visionFrigateImage,
		"-timeout", "10000000",
		"-rtsp_transport", "tcp",
		"-analyzeduration", "15000000",
		"-probesize", "10000000",
		buildStreamURL(dev, 1, false),
	).CombinedOutput()

	if _, perr := parseFFprobeResolution(string(out)); perr == nil {
		return nil // a real video stream was found, regardless of ffprobe's exit code
	}
	if err != nil {
		return fmt.Errorf("could not connect: %v — %s", err, strings.TrimSpace(string(out)))
	}
	return fmt.Errorf("connected, but no video stream found — check the port and stream path: %s", strings.TrimSpace(string(out)))
}

func stopVisionContainer() {
	docker("stop", goRTCContainerName).Run()
	docker("rm", goRTCContainerName).Run()
}
