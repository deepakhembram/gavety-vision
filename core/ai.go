package main

// ai.go - pluggable AI analysis for Gavety Vision (runs inside the gavety-vision-core container).
//
// Flow:  Frigate (latest.jpg) -> sampler -> change gate -> provider (local GPU / any cloud /
// queue) -> detections -> rules (zones + N-of-M confirmation) -> events.
//
// Only still frames (small JPEGs) ever leave this process - never continuous video - and only to
// the provider configured per camera. If the primary provider fails the fallback provider is used.
// Config: <stateDir>/vision_ai.json (0600, may hold provider tokens), re-read within ~10 s of a change.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─── CONFIG ───────────────────────────────────────────────────

func aiConfigFile() string { return filepath.Join(stateDir, "vision_ai.json") }

type VAIProviderCfg struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // "http" | "queue" | "mock"
	URL        string `json:"url"`
	Token      string `json:"token"`
	TimeoutSec int    `json:"timeout_sec"`
}

type VAIZone struct {
	Name    string       `json:"name"`
	Polygon [][2]float64 `json:"polygon"` // normalised 0..1 (x,y), same as the worker's boxes
}

type VAIRule struct {
	Event       string   `json:"event"`     // name used in alerts, e.g. "fire", "person_in_zone"
	Labels      []string `json:"labels"`    // detection labels that count as this event
	MinConf     float64  `json:"min_conf"`  // default 0.4
	Zone        string   `json:"zone"`      // optional: box foot must be inside this zone
	ConfirmN    int      `json:"confirm_n"` // seen in N of the last M analysed frames (default 3 of 5)
	ConfirmM    int      `json:"confirm_m"`
	CooldownSec int      `json:"cooldown_sec"` // min seconds between repeat alerts (default 30)
	Severity    string   `json:"severity"`     // "critical" | "warning" (default warning)
}

type VAICameraCfg struct {
	Enabled     *bool     `json:"enabled"`
	Provider    string    `json:"provider"`
	Fallback    string    `json:"fallback"`
	Tasks       []string  `json:"tasks"`
	Zones       []VAIZone `json:"zones"`
	Rules       []VAIRule `json:"rules"`
	IntervalSec float64   `json:"interval_sec"`
}

type VAIConfig struct {
	Enabled         bool                    `json:"enabled"`
	FrigateURL      string                  `json:"frigate_url"`      // default http://127.0.0.1:5150
	IntervalSec     float64                 `json:"interval_sec"`     // default 2
	FrameHeight     int                     `json:"frame_height"`     // default 480
	SkipUnchanged   bool                    `json:"skip_unchanged"`   // don't send near-identical frames
	ChangeThreshold float64                 `json:"change_threshold"` // fraction of the frame grid that changed; default 0.005 (~3 of 576 cells)
	HeartbeatSec    float64                 `json:"heartbeat_sec"`    // still send one frame this often; default 30
	RetentionDays   int                     `json:"retention_days"`   // default 30
	Providers       []VAIProviderCfg        `json:"providers"`
	Provider        string                  `json:"provider"` // default provider name
	Fallback        string                  `json:"fallback"`
	Tasks           []string                `json:"tasks"` // default worker engines to run
	Rules           []VAIRule               `json:"rules"` // default rules
	Cameras         map[string]VAICameraCfg `json:"cameras"`
}

func loadVAIConfig() (VAIConfig, error) {
	var c VAIConfig
	data, err := os.ReadFile(aiConfigFile())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.FrigateURL == "" {
		c.FrigateURL = "http://127.0.0.1:5150"
	}
	if c.IntervalSec <= 0 {
		c.IntervalSec = 2
	}
	if c.FrameHeight <= 0 {
		c.FrameHeight = 480
	}
	if c.ChangeThreshold <= 0 {
		c.ChangeThreshold = 0.005
	}
	if c.HeartbeatSec <= 0 {
		c.HeartbeatSec = 30
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = 30
	}
	return c, nil
}

// ─── PROVIDER INTERFACE ───────────────────────────────────────
// One contract for every backend. To add a new cloud: implement a worker that
// speaks POST /v1/analyze (see worker/gavety_worker/server.py) - no agent change.

type VAIRequest struct {
	Camera string
	TS     float64
	Tasks  []string
	JPEG   []byte
}

type VAIDetection struct {
	Label  string    `json:"label"`
	Conf   float64   `json:"conf"`
	Box    []float64 `json:"box,omitempty"` // [x1,y1,x2,y2] normalised; absent for VLM verdicts
	Source string    `json:"source,omitempty"`
}

type VAIResult struct {
	JobID      string         `json:"job_id"`
	Camera     string         `json:"camera"`
	Worker     string         `json:"worker"`
	Model      string         `json:"model"`
	LatencyMS  int            `json:"latency_ms"`
	Detections []VAIDetection `json:"detections"`
	Error      string         `json:"error"`
}

type VAIProvider interface {
	Name() string
	Analyze(ctx context.Context, r VAIRequest) (*VAIResult, error)
}

func vaiMultipart(r VAIRequest) (*bytes.Buffer, string, error) {
	meta, _ := json.Marshal(map[string]interface{}{"camera": r.Camera, "ts": r.TS, "tasks": r.Tasks})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("meta", string(meta)); err != nil {
		return nil, "", err
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="image"; filename="frame.jpg"`)
	h.Set("Content-Type", "image/jpeg")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	part.Write(r.JPEG)
	mw.Close()
	return &buf, mw.FormDataContentType(), nil
}

func vaiDo(ctx context.Context, cl *http.Client, method, url, token, ctype string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return cl.Do(req)
}

// HTTP provider: any machine that runs the Gavety worker (your GPU PC, a cloud VM...).
type vaiHTTPProvider struct {
	cfg VAIProviderCfg
	cl  *http.Client
}

func (p *vaiHTTPProvider) Name() string { return p.cfg.Name }

func (p *vaiHTTPProvider) Analyze(ctx context.Context, r VAIRequest) (*VAIResult, error) {
	body, ctype, err := vaiMultipart(r)
	if err != nil {
		return nil, err
	}
	resp, err := vaiDo(ctx, p.cl, "POST", strings.TrimRight(p.cfg.URL, "/")+"/v1/analyze", p.cfg.Token, ctype, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d: %s", p.cfg.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var res VAIResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("%s: bad response: %v", p.cfg.Name, err)
	}
	return &res, nil
}

// Queue provider: for workers that cannot accept inbound connections (a notebook,
// a machine behind NAT). The agent submits a job to a broker; a worker pulls it.
type vaiQueueProvider struct {
	cfg VAIProviderCfg
	cl  *http.Client
}

func (p *vaiQueueProvider) Name() string { return p.cfg.Name }

func (p *vaiQueueProvider) Analyze(ctx context.Context, r VAIRequest) (*VAIResult, error) {
	base := strings.TrimRight(p.cfg.URL, "/")
	body, ctype, err := vaiMultipart(r)
	if err != nil {
		return nil, err
	}
	resp, err := vaiDo(ctx, p.cl, "POST", base+"/v1/jobs", p.cfg.Token, ctype, body)
	if err != nil {
		return nil, err
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 || json.Unmarshal(raw, &sub) != nil || sub.JobID == "" {
		return nil, fmt.Errorf("%s: submit failed: HTTP %d %s", p.cfg.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	for {
		resp, err := vaiDo(ctx, p.cl, "GET", base+"/v1/jobs/"+sub.JobID+"/result?wait=3", p.cfg.Token, "", nil)
		if err != nil {
			return nil, err // includes ctx deadline: no worker picked the frame up in time
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		switch resp.StatusCode {
		case 202:
			continue
		case 200:
			var res VAIResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return nil, err
			}
			if res.Error != "" {
				return nil, fmt.Errorf("%s: worker error: %s", p.cfg.Name, res.Error)
			}
			return &res, nil
		default:
			return nil, fmt.Errorf("%s: result HTTP %d (job expired?)", p.cfg.Name, resp.StatusCode)
		}
	}
}

// Mock provider: no network, returns nothing. For dry runs and tests.
type vaiMockProvider struct {
	name string
	fn   func(VAIRequest) (*VAIResult, error)
}

func (p *vaiMockProvider) Name() string { return p.name }
func (p *vaiMockProvider) Analyze(ctx context.Context, r VAIRequest) (*VAIResult, error) {
	if p.fn != nil {
		return p.fn(r)
	}
	return &VAIResult{Camera: r.Camera, Model: "mock"}, nil
}

func vaiBuildProvider(c VAIProviderCfg) (VAIProvider, error) {
	to := time.Duration(c.TimeoutSec) * time.Second
	if to <= 0 {
		to = 10 * time.Second
	}
	cl := &http.Client{Timeout: to + 5*time.Second}
	switch c.Type {
	case "http":
		return &vaiHTTPProvider{cfg: c, cl: cl}, nil
	case "queue":
		return &vaiQueueProvider{cfg: c, cl: cl}, nil
	case "mock":
		return &vaiMockProvider{name: c.Name}, nil
	}
	return nil, fmt.Errorf("provider %q: unknown type %q (use http, queue or mock)", c.Name, c.Type)
}

// ─── GEOMETRY + CONFIRMATION ──────────────────────────────────

func vaiPointInPolygon(p [2]float64, poly [][2]float64) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi, xj, yj := poly[i][0], poly[i][1], poly[j][0], poly[j][1]
		if (yi > p[1]) != (yj > p[1]) && p[0] < (xj-xi)*(p[1]-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// vaiTracker = "seen in N of the last M frames" with a cooldown, per camera+event.
type vaiTracker struct {
	mu   sync.Mutex
	hist map[string][]bool
	last map[string]time.Time
}

func newVAITracker() *vaiTracker {
	return &vaiTracker{hist: map[string][]bool{}, last: map[string]time.Time{}}
}

func (t *vaiTracker) Update(key string, seen bool, n, m int, cooldown time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := append(t.hist[key], seen)
	if len(h) > m {
		h = h[len(h)-m:]
	}
	t.hist[key] = h
	count := 0
	for _, v := range h {
		if v {
			count++
		}
	}
	if count >= n && now.Sub(t.last[key]) >= cooldown {
		t.last[key] = now
		return true
	}
	return false
}

// vaiFrameSig: 32x18 luminance grid sampled straight from the decoded JPEG.
func vaiFrameSig(jpg []byte) ([]uint8, error) {
	img, _, err := image.Decode(bytes.NewReader(jpg))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	const gx, gy = 32, 18
	sig := make([]uint8, 0, gx*gy)
	for y := 0; y < gy; y++ {
		for x := 0; x < gx; x++ {
			px := b.Min.X + (2*x+1)*b.Dx()/(2*gx)
			py := b.Min.Y + (2*y+1)*b.Dy()/(2*gy)
			r, g, bl, _ := img.At(px, py).RGBA()
			sig = append(sig, uint8((299*(r>>8)+587*(g>>8)+114*(bl>>8))/1000))
		}
	}
	return sig, nil
}

// vaiSigDiff returns the FRACTION OF GRID CELLS whose brightness changed by more than
// ~8% (20/255). Counting cells, not averaging brightness, keeps it sensitive to a small
// object (a person, a flame) entering a large static scene, while ignoring sensor noise
// and slow lighting drift.
func vaiSigDiff(a, b []uint8) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 1
	}
	changed := 0
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		if d > 20 {
			changed++
		}
	}
	return float64(changed) / float64(len(a))
}

// ─── EVENTS ───────────────────────────────────────────────────

type VisionAIEvent struct {
	ID        string         `json:"id"`
	TS        time.Time      `json:"ts"`
	Camera    string         `json:"camera"`
	Event     string         `json:"event"`
	Severity  string         `json:"severity"`
	Conf      float64        `json:"conf"`
	Detail    string         `json:"detail"`
	Snapshot  string         `json:"snapshot"` // file name; served at /vision-ai-snapshot/<id>.jpg
	Provider  string         `json:"provider"`
	Model     string         `json:"model"`
	LatencyMS int            `json:"latency_ms"`
	Detected  []VAIDetection `json:"detections,omitempty"`
}

// VisionAIOnEvent is called for every confirmed event (wire Telegram/siren here).
var VisionAIOnEvent func(ev VisionAIEvent)

var vaiStorageDir = func() string { return filepath.Join(visionStoragePath(), "_ai_events") }

func vaiNewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

func vaiStoreEvent(ev VisionAIEvent, jpg []byte) error {
	dir := vaiStorageDir()
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshots", ev.ID+".jpg"), jpg, 0644); err != nil {
		return err
	}
	line, _ := json.Marshal(ev)
	f, err := os.OpenFile(filepath.Join(dir, "events-"+ev.TS.UTC().Format("2006-01-02")+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func vaiPrune(days int) {
	dir := vaiStorageDir()
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, pat := range []string{"events-*.jsonl", "snapshots/*.jpg"} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.ModTime().Before(cutoff) {
				os.Remove(f)
			}
		}
	}
}

func vaiReadEvents(limit int, camera string) []VisionAIEvent {
	files, _ := filepath.Glob(filepath.Join(vaiStorageDir(), "events-*.jsonl"))
	var out []VisionAIEvent
	for i := len(files) - 1; i >= 0 && len(out) < limit; i-- { // newest file first
		data, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for j := len(lines) - 1; j >= 0 && len(out) < limit; j-- {
			var ev VisionAIEvent
			if json.Unmarshal([]byte(lines[j]), &ev) == nil && (camera == "" || ev.Camera == camera) {
				out = append(out, ev)
			}
		}
	}
	return out
}

// ─── MANAGER ──────────────────────────────────────────────────

type vaiStats struct {
	Sent, Skipped, FrameErrors, AnalyzeErrors, Events int
	LastLatencyMS                                     int
	LastProvider, LastError                           string
}

type vaiProvState struct {
	fails int
	until time.Time
}

type vaiManager struct {
	mu        sync.Mutex
	cfg       VAIConfig
	providers map[string]VAIProvider
	state     map[string]*vaiProvState
	stats     map[string]*vaiStats
	tracker   *vaiTracker
	cancels   map[string]context.CancelFunc
	frameHTTP *http.Client
}

var visionAI = &vaiManager{
	state: map[string]*vaiProvState{}, stats: map[string]*vaiStats{},
	tracker: newVAITracker(), cancels: map[string]context.CancelFunc{},
	frameHTTP: &http.Client{Timeout: 6 * time.Second},
}

func (m *vaiManager) setConfig(c VAIConfig) error {
	provs := map[string]VAIProvider{}
	for _, pc := range c.Providers {
		p, err := vaiBuildProvider(pc)
		if err != nil {
			return err
		}
		provs[pc.Name] = p
	}
	m.mu.Lock()
	m.cfg, m.providers = c, provs
	m.mu.Unlock()
	return nil
}

type vaiCam struct {
	name     string
	provider string
	fallback string
	tasks    []string
	zones    map[string]VAIZone
	rules    []VAIRule
	interval time.Duration
	enabled  bool
}

func (m *vaiManager) effective(cam string) vaiCam {
	m.mu.Lock()
	c := m.cfg
	m.mu.Unlock()
	cc := c.Cameras[cam]
	v := vaiCam{name: cam, provider: c.Provider, fallback: c.Fallback, tasks: c.Tasks, rules: c.Rules,
		interval: time.Duration(c.IntervalSec * float64(time.Second)), zones: map[string]VAIZone{}, enabled: true}
	if cc.Provider != "" {
		v.provider = cc.Provider
	}
	if cc.Fallback != "" {
		v.fallback = cc.Fallback
	}
	if len(cc.Tasks) > 0 {
		v.tasks = cc.Tasks
	}
	if len(cc.Rules) > 0 {
		v.rules = append(append([]VAIRule{}, c.Rules...), cc.Rules...) // camera rules ADD to the global ones (e.g. fire always applies)
	}
	if cc.IntervalSec > 0 {
		v.interval = time.Duration(cc.IntervalSec * float64(time.Second))
	}
	if cc.Enabled != nil {
		v.enabled = *cc.Enabled
	}
	for _, z := range cc.Zones {
		v.zones[z.Name] = z
	}
	return v
}

func (m *vaiManager) callable(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[name]
	return st == nil || time.Now().After(st.until)
}

func (m *vaiManager) mark(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[name]
	if st == nil {
		st = &vaiProvState{}
		m.state[name] = st
	}
	if err == nil {
		st.fails, st.until = 0, time.Time{}
		return
	}
	st.fails++
	if st.fails >= 3 { // circuit breaker: stop hammering a dead provider for 30s
		st.until = time.Now().Add(30 * time.Second)
	}
}

// analyze tries the primary provider, then the fallback.
func (m *vaiManager) analyze(ctx context.Context, cv vaiCam, req VAIRequest) (*VAIResult, string, error) {
	var lastErr error
	for _, name := range []string{cv.provider, cv.fallback} {
		if name == "" {
			continue
		}
		m.mu.Lock()
		p := m.providers[name]
		m.mu.Unlock()
		if p == nil {
			lastErr = fmt.Errorf("provider %q is not defined in vision_ai.json", name)
			continue
		}
		if !m.callable(name) && name == cv.provider && cv.fallback != "" {
			continue // skip a tripped primary when a fallback exists
		}
		res, err := p.Analyze(ctx, req)
		m.mark(name, err)
		if err == nil {
			return res, name, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no provider configured")
	}
	return nil, "", lastErr
}

func (m *vaiManager) stat(cam string) *vaiStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats[cam]
	if s == nil {
		s = &vaiStats{}
		m.stats[cam] = s
	}
	return s
}

func (m *vaiManager) fetchFrame(cam string) ([]byte, error) {
	m.mu.Lock()
	base, h := m.cfg.FrigateURL, m.cfg.FrameHeight
	m.mu.Unlock()
	resp, err := m.frameHTTP.Get(fmt.Sprintf("%s/api/%s/latest.jpg?h=%d", strings.TrimRight(base, "/"), cam, h))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("frigate HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (m *vaiManager) camLoop(ctx context.Context, cv vaiCam) {
	var prev []uint8
	var lastSent time.Time
	t := time.NewTicker(cv.interval) // a slow provider just drops ticks: always the newest frame, never a backlog
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st := m.stat(cv.name)
		jpg, err := m.fetchFrame(cv.name)
		if err != nil {
			m.mu.Lock()
			st.FrameErrors++
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		skip, thr, hb := m.cfg.SkipUnchanged, m.cfg.ChangeThreshold, time.Duration(m.cfg.HeartbeatSec*float64(time.Second))
		m.mu.Unlock()
		if skip {
			if sig, err := vaiFrameSig(jpg); err == nil {
				if prev != nil && vaiSigDiff(prev, sig) < thr && time.Since(lastSent) < hb {
					m.mu.Lock()
					st.Skipped++
					m.mu.Unlock()
					continue
				}
				prev = sig
			}
		}
		lastSent = time.Now()
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		res, prov, err := m.analyze(actx, cv, VAIRequest{
			Camera: cv.name, TS: float64(time.Now().UnixNano()) / 1e9, Tasks: cv.tasks, JPEG: jpg})
		cancel()
		m.mu.Lock()
		st.Sent++
		if err != nil {
			st.AnalyzeErrors++
			st.LastError = err.Error()
		} else {
			st.LastLatencyMS, st.LastProvider = res.LatencyMS, prov
		}
		m.mu.Unlock()
		if err != nil {
			log.Printf("⚠️ Vision AI %s: %v", cv.name, err)
			continue
		}
		m.evaluate(cv, jpg, res, prov)
	}
}

func vaiFoot(box []float64) [2]float64 { return [2]float64{(box[0] + box[2]) / 2, box[3]} }

func (m *vaiManager) evaluate(cv vaiCam, jpg []byte, res *VAIResult, prov string) {
	now := time.Now()
	for _, r := range cv.rules {
		n, mm, cd, minc := r.ConfirmN, r.ConfirmM, time.Duration(r.CooldownSec)*time.Second, r.MinConf
		if n <= 0 {
			n = 3
		}
		if mm < n {
			mm = n + 2
		}
		if cd <= 0 {
			cd = 30 * time.Second
		}
		if minc <= 0 {
			minc = 0.4
		}
		var hits []VAIDetection
		best := 0.0
		for _, d := range res.Detections {
			if d.Conf < minc || !vaiLabelIn(d.Label, r.Labels) {
				continue
			}
			if r.Zone != "" {
				z, ok := cv.zones[r.Zone]
				if !ok || len(d.Box) != 4 || !vaiPointInPolygon(vaiFoot(d.Box), z.Polygon) {
					continue
				}
			}
			hits = append(hits, d)
			if d.Conf > best {
				best = d.Conf
			}
		}
		if !m.tracker.Update(cv.name+"|"+r.Event, len(hits) > 0, n, mm, cd, now) {
			continue
		}
		sev := r.Severity
		if sev == "" {
			sev = "warning"
		}
		ev := VisionAIEvent{ID: vaiNewID(), TS: now, Camera: cv.name, Event: r.Event, Severity: sev, Conf: best,
			Detail:   fmt.Sprintf("%d detection(s) in %d of last %d frames", len(hits), n, mm),
			Provider: prov, Model: res.Model, LatencyMS: res.LatencyMS, Detected: hits}
		ev.Snapshot = ev.ID + ".jpg"
		if err := vaiStoreEvent(ev, jpg); err != nil {
			log.Printf("⚠️ Vision AI: could not store event: %v", err)
		}
		st := m.stat(cv.name) // takes the lock itself, so call it before locking
		m.mu.Lock()
		st.Events++
		m.mu.Unlock()
		log.Printf("🚨 Vision AI: %s on %s (conf %.2f, via %s)", r.Event, cv.name, best, prov)
		if VisionAIOnEvent != nil {
			go VisionAIOnEvent(ev)
		}
	}
}

func vaiLabelIn(l string, set []string) bool {
	for _, s := range set {
		if strings.EqualFold(s, l) {
			return true
		}
	}
	return false
}

// sync (re)starts one loop per camera. Called at start and whenever the config file changes.
func (m *vaiManager) sync(cams []string) {
	m.mu.Lock()
	for _, c := range m.cancels {
		c()
	}
	m.cancels = map[string]context.CancelFunc{}
	enabled := m.cfg.Enabled
	m.mu.Unlock()
	if !enabled {
		return
	}
	for _, cam := range cams {
		cv := m.effective(cam)
		if !cv.enabled || cv.provider == "" {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		m.cancels[cam] = cancel
		m.mu.Unlock()
		go m.camLoop(ctx, cv)
	}
}

func vaiCameraNames() []string {
	devs, err := loadVisionDevices()
	if err != nil {
		return nil
	}
	var out []string
	for _, rc := range resolveCameras(devs) {
		out = append(out, channelName(rc.Device, rc.Channel, false))
	}
	return out
}

// StartVisionAI: call once from main(), next to StartVisionStorageEnforcement().
// It is a no-op until /etc/gavety/vision_ai.json exists with "enabled": true,
// and it picks up edits to that file within ~10 seconds, no restart needed.
func StartVisionAI() {
	go func() {
		var lastMod time.Time
		var lastCams string
		lastPrune := time.Time{}
		for {
			st, err := os.Stat(aiConfigFile())
			cams := vaiCameraNames()
			key := strings.Join(cams, ",")
			if err == nil && (st.ModTime() != lastMod || key != lastCams) {
				lastMod, lastCams = st.ModTime(), key
				cfg, lerr := loadVAIConfig()
				if lerr != nil {
					log.Printf("⚠️ Vision AI config: %v", lerr)
				} else if serr := visionAI.setConfig(cfg); serr != nil {
					log.Printf("⚠️ Vision AI config: %v", serr)
				} else {
					visionAI.sync(cams)
					log.Printf("✅ Vision AI configured: enabled=%v, %d camera(s), %d provider(s)", cfg.Enabled, len(cams), len(cfg.Providers))
				}
			}
			if time.Since(lastPrune) > 24*time.Hour {
				lastPrune = time.Now()
				visionAI.mu.Lock()
				days := visionAI.cfg.RetentionDays
				visionAI.mu.Unlock()
				if days > 0 {
					vaiPrune(days)
				}
			}
			time.Sleep(10 * time.Second)
		}
	}()
}

// ─── DATA FOR THE HTTP API (see api.go) ───────────────────────

var vaiSnapRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}-[0-9a-f]{12}\.jpg$`)

// vaiStatus reports provider health and per-camera counters. It never includes provider URLs or tokens.
func vaiStatus() map[string]interface{} {
	v := visionAI
	v.mu.Lock()
	defer v.mu.Unlock()
	type prov struct {
		Name    string `json:"name"`
		Healthy bool   `json:"healthy"`
		Fails   int    `json:"consecutive_failures"`
	}
	var provs []prov
	for _, pc := range v.cfg.Providers {
		p := prov{Name: pc.Name, Healthy: true}
		if st := v.state[pc.Name]; st != nil {
			p.Fails, p.Healthy = st.fails, time.Now().After(st.until)
		}
		provs = append(provs, p)
	}
	cams := map[string]vaiStats{}
	for k, s := range v.stats {
		cams[k] = *s
	}
	return map[string]interface{}{"status": "ok", "enabled": v.cfg.Enabled, "providers": provs, "cameras": cams}
}
