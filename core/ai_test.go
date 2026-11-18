package main

// Stand-ins for the agent symbols vision_ai.go uses. In your real agent these already
// exist, so DELETE this stubs block (and stub main) if you copy the tests in.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ──

func mkJPEG(t testing.TB, personX int, fire bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{60, 60, 60, 255})
		}
	}
	if personX >= 0 {
		for y := 75; y < 150; y++ {
			for x := personX; x < personX+20 && x < 320; x++ {
				img.Set(x, y, color.RGBA{220, 220, 220, 255})
			}
		}
	}
	if fire {
		for y := 40; y < 65; y++ {
			for x := 40; x < 65; x++ {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func useTempDirs(t *testing.T) {
	dir := t.TempDir()
	old := vaiStorageDir
	vaiStorageDir = func() string { return filepath.Join(dir, "_ai_events") }
	t.Cleanup(func() { vaiStorageDir = old })
}

// ── unit tests ──

func TestPointInPolygon(t *testing.T) {
	zone := [][2]float64{{0.5, 0.2}, {1, 0.2}, {1, 1}, {0.5, 1}}
	if !vaiPointInPolygon([2]float64{0.75, 0.6}, zone) {
		t.Error("inside point rejected")
	}
	if vaiPointInPolygon([2]float64{0.25, 0.6}, zone) || vaiPointInPolygon([2]float64{0.75, 0.1}, zone) {
		t.Error("outside point accepted")
	}
}

func TestTrackerNofMAndCooldown(t *testing.T) {
	tr := newVAITracker()
	now := time.Now()
	fire := func(seen bool, dt time.Duration) bool {
		return tr.Update("c|fire", seen, 3, 5, 30*time.Second, now.Add(dt))
	}
	if fire(true, 0) || fire(false, 1*time.Second) || fire(true, 2*time.Second) {
		t.Fatal("fired before 3 of 5")
	}
	if !fire(true, 3*time.Second) {
		t.Fatal("did not fire at 3 of 5")
	}
	if fire(true, 4*time.Second) {
		t.Fatal("cooldown ignored")
	}
	if !fire(true, 40*time.Second) {
		t.Fatal("did not re-fire after cooldown")
	}
}

func TestFrameSigDetectsChange(t *testing.T) {
	a, _ := vaiFrameSig(mkJPEG(t, -1, false))
	same, _ := vaiFrameSig(mkJPEG(t, -1, false))
	moved, _ := vaiFrameSig(mkJPEG(t, 150, true))
	if d := vaiSigDiff(a, same); d > 0.005 {
		t.Errorf("identical frames differ by %.4f", d)
	}
	if d := vaiSigDiff(a, moved); d < 0.005 {
		t.Errorf("changed frame only differs by %.4f (would be skipped at default threshold)", d)
	}
}

func TestFallbackAndCircuitBreaker(t *testing.T) {
	var primCalls, fbCalls int32
	m := &vaiManager{state: map[string]*vaiProvState{}, stats: map[string]*vaiStats{}, tracker: newVAITracker(),
		providers: map[string]VAIProvider{
			"cloud": &vaiMockProvider{name: "cloud", fn: func(VAIRequest) (*VAIResult, error) {
				atomic.AddInt32(&primCalls, 1)
				return nil, errors.New("no internet")
			}},
			"local": &vaiMockProvider{name: "local", fn: func(VAIRequest) (*VAIResult, error) {
				atomic.AddInt32(&fbCalls, 1)
				return &VAIResult{Model: "cpu"}, nil
			}},
		}}
	cv := vaiCam{provider: "cloud", fallback: "local"}
	for i := 0; i < 6; i++ {
		res, prov, err := m.analyze(context.Background(), cv, VAIRequest{})
		if err != nil || prov != "local" || res.Model != "cpu" {
			t.Fatalf("call %d: %v %q", i, err, prov)
		}
	}
	if primCalls != 3 { // tripped after 3 consecutive failures, then skipped
		t.Errorf("primary called %d times, want 3", primCalls)
	}
	if fbCalls != 6 {
		t.Errorf("fallback called %d times, want 6", fbCalls)
	}
	// no fallback configured and primary down -> surfaces the error
	if _, _, err := m.analyze(context.Background(), vaiCam{provider: "cloud"}, VAIRequest{}); err == nil {
		t.Error("expected error with no fallback")
	}
}

func TestHTTPProviderContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/analyze" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nope", 401)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var meta map[string]interface{}
		json.Unmarshal([]byte(r.FormValue("meta")), &meta)
		f, _, err := r.FormFile("image")
		if err != nil || meta["camera"] != "cam1" {
			http.Error(w, "bad", 400)
			return
		}
		f.Close()
		json.NewEncoder(w).Encode(VAIResult{Model: "m", Detections: []VAIDetection{{Label: "fire", Conf: 0.9}}})
	}))
	defer srv.Close()
	p, _ := vaiBuildProvider(VAIProviderCfg{Name: "gpu", Type: "http", URL: srv.URL + "/", Token: "tok"})
	res, err := p.Analyze(context.Background(), VAIRequest{Camera: "cam1", Tasks: []string{"fire"}, JPEG: mkJPEG(t, -1, true)})
	if err != nil || len(res.Detections) != 1 || res.Detections[0].Label != "fire" {
		t.Fatalf("%v %+v", err, res)
	}
	bad, _ := vaiBuildProvider(VAIProviderCfg{Name: "gpu", Type: "http", URL: srv.URL, Token: "wrong"})
	if _, err := bad.Analyze(context.Background(), VAIRequest{Camera: "cam1", JPEG: []byte("x")}); err == nil {
		t.Error("401 should be an error")
	}
}

func TestQueueProviderPollsUntilDone(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/jobs":
			json.NewEncoder(w).Encode(map[string]string{"job_id": "j1"})
		case strings.HasSuffix(r.URL.Path, "/j1/result"):
			if atomic.AddInt32(&polls, 1) < 3 {
				w.WriteHeader(202)
				w.Write([]byte(`{"status":"pending"}`))
				return
			}
			json.NewEncoder(w).Encode(VAIResult{Worker: "kaggle-1", Detections: []VAIDetection{{Label: "smoke", Conf: 0.8}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, _ := vaiBuildProvider(VAIProviderCfg{Name: "kaggle", Type: "queue", URL: srv.URL})
	res, err := p.Analyze(context.Background(), VAIRequest{Camera: "c", JPEG: []byte("x")})
	if err != nil || res.Worker != "kaggle-1" || polls != 3 {
		t.Fatalf("err=%v res=%+v polls=%d", err, res, polls)
	}
	// worker-reported error must surface so the agent can fall back
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			json.NewEncoder(w).Encode(map[string]string{"job_id": "j2"})
			return
		}
		json.NewEncoder(w).Encode(VAIResult{Error: "all engines failed"})
	}))
	defer srv2.Close()
	p2, _ := vaiBuildProvider(VAIProviderCfg{Name: "q", Type: "queue", URL: srv2.URL})
	if _, err := p2.Analyze(context.Background(), VAIRequest{JPEG: []byte("x")}); err == nil {
		t.Error("worker error not surfaced")
	}
}

// ── pipeline test: fake Frigate -> provider -> rules -> stored event ──

func fakeFrigate(t *testing.T, frameFn func(n int) []byte) *httptest.Server {
	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cam1/latest.jpg" || r.URL.Query().Get("h") != "480" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(frameFn(int(atomic.AddInt32(&n, 1))))
	}))
}

func newTestManager(t *testing.T, frigate string, provider VAIProvider, rules []VAIRule, zones []VAIZone, skip bool) *vaiManager {
	m := &vaiManager{state: map[string]*vaiProvState{}, stats: map[string]*vaiStats{}, tracker: newVAITracker(),
		cancels: map[string]context.CancelFunc{}, frameHTTP: &http.Client{Timeout: 2 * time.Second},
		providers: map[string]VAIProvider{"p": provider}}
	m.cfg = VAIConfig{Enabled: true, FrigateURL: frigate, IntervalSec: 0.03, FrameHeight: 480, Provider: "p",
		Tasks: []string{"person", "fire"}, Rules: rules, SkipUnchanged: skip, ChangeThreshold: 0.005, HeartbeatSec: 60,
		Cameras: map[string]VAICameraCfg{"cam1": {Zones: zones}}}
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPipelineZoneIntrusionAndFire(t *testing.T) {
	useTempDirs(t)
	// person walks left->right; fire visible from frame 3 on
	fr := fakeFrigate(t, func(n int) []byte { return mkJPEG(t, 10+n*12, n >= 3) })
	defer fr.Close()

	detect := &vaiMockProvider{name: "p", fn: func(r VAIRequest) (*VAIResult, error) {
		// a tiny stand-in "model": same colour rules as the worker's mock engine
		img, err := jpeg.Decode(bytes.NewReader(r.JPEG))
		if err != nil {
			return nil, err
		}
		b := img.Bounds()
		var res VAIResult
		var px0, px1, py0, py1, fx0, fx1, fy0, fy1 = 1 << 30, -1, 0, 0, 1 << 30, -1, 0, 0
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				rr, gg, bb, _ := img.At(x, y).RGBA()
				R, G, B := rr>>8, gg>>8, bb>>8
				if R > 180 && G > 180 && B > 180 {
					if x < px0 {
						px0 = x
					}
					if x > px1 {
						px1 = x
					}
					py0, py1 = pick(py0, y, px1 == x && py0 == 0), y
				}
				if R > 200 && G < 60 && B < 60 {
					if x < fx0 {
						fx0 = x
					}
					if x > fx1 {
						fx1 = x
					}
					fy0, fy1 = pick(fy0, y, fx1 == x && fy0 == 0), y
				}
			}
		}
		W, H := float64(b.Dx()), float64(b.Dy())
		if px1 >= 0 {
			res.Detections = append(res.Detections, VAIDetection{Label: "person", Conf: 0.9,
				Box: []float64{float64(px0) / W, float64(py0) / H, float64(px1+1) / W, float64(py1+1) / H}})
		}
		if fx1 >= 0 {
			res.Detections = append(res.Detections, VAIDetection{Label: "fire", Conf: 0.9,
				Box: []float64{float64(fx0) / W, float64(fy0) / H, float64(fx1+1) / W, float64(fy1+1) / H}})
		}
		res.Model = "stub"
		return &res, nil
	}}
	rules := []VAIRule{
		{Event: "person_in_zone", Labels: []string{"person"}, Zone: "press", ConfirmN: 2, ConfirmM: 3, CooldownSec: 60, Severity: "warning"},
		{Event: "fire", Labels: []string{"fire", "smoke"}, ConfirmN: 3, ConfirmM: 5, CooldownSec: 60, Severity: "critical"},
	}
	zones := []VAIZone{{Name: "press", Polygon: [][2]float64{{0.5, 0.2}, {1, 0.2}, {1, 1}, {0.5, 1}}}}
	m := newTestManager(t, fr.URL, detect, rules, zones, false)

	var mu sync.Mutex
	var got []VisionAIEvent
	VisionAIOnEvent = func(ev VisionAIEvent) { mu.Lock(); got = append(got, ev); mu.Unlock() }
	defer func() { VisionAIOnEvent = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.camLoop(ctx, m.effective("cam1"))
	waitFor(t, "both events", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 2 })
	cancel()

	mu.Lock()
	defer mu.Unlock()
	kinds := map[string]VisionAIEvent{}
	for _, e := range got {
		kinds[e.Event] = e
	}
	if kinds["fire"].Severity != "critical" || kinds["person_in_zone"].Event == "" {
		t.Fatalf("events: %+v", kinds)
	}
	// cooldown: no duplicates despite many more frames
	time.Sleep(150 * time.Millisecond)
	if len(vaiReadEvents(50, "")) != 2 {
		t.Errorf("expected exactly 2 stored events, got %d", len(vaiReadEvents(50, "")))
	}
	// snapshot + jsonl really on disk, and the snapshot is a valid JPEG
	ev := vaiReadEvents(1, "cam1")[0]
	snap, err := os.ReadFile(filepath.Join(vaiStorageDir(), "snapshots", ev.Snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(snap)); err != nil {
		t.Error("snapshot not a jpeg")
	}
	if !vaiSnapRe.MatchString(ev.Snapshot) {
		t.Errorf("snapshot name %q would be rejected by the handler", ev.Snapshot)
	}
	if len(vaiReadEvents(50, "other-cam")) != 0 {
		t.Error("camera filter broken")
	}
}

func pick(cur, y int, first bool) int {
	if first {
		return y
	}
	return cur
}

func TestUnchangedFramesAreNotSent(t *testing.T) {
	useTempDirs(t)
	fr := fakeFrigate(t, func(n int) []byte { return mkJPEG(t, -1, false) }) // static empty scene
	defer fr.Close()
	var calls int32
	prov := &vaiMockProvider{name: "p", fn: func(VAIRequest) (*VAIResult, error) {
		atomic.AddInt32(&calls, 1)
		return &VAIResult{}, nil
	}}
	m := newTestManager(t, fr.URL, prov, nil, nil, true)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.camLoop(ctx, m.effective("cam1"))
	st := m.stat("cam1")
	if calls != 1 || st.Skipped < 5 {
		t.Errorf("sent=%d skipped=%d (want exactly 1 sent, rest skipped)", calls, st.Skipped)
	}
}

func TestBadProviderType(t *testing.T) {
	if _, err := vaiBuildProvider(VAIProviderCfg{Name: "x", Type: "ftp"}); err == nil {
		t.Error("unknown type accepted")
	}
}
