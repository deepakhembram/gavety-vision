package main

// ui.go - OPTIONAL standalone web UI, so Gavety Vision works on any PC with Docker (no Gavety agent).
//
// Off by default. Enable with GAVETY_UI_ADDR (for example ":5170"). On a Gavety box (Pi) leave it off:
// the dashboard opens the same vision.html as a popup and talks to the core through the agent.
//
// What it serves:
//   GET  /                 the Vision page (same file the dashboard uses; it detects standalone mode)
//   GET  /ui/state         {configured, authed}
//   POST /ui/setup         first run: set the password (needs the setup code printed in the log)
//   POST /ui/login|logout  session cookie (HttpOnly, SameSite=Strict, HMAC-signed)
//   POST /ui/cmd           the same commands the dashboard sends (vision_list_devices, ...)
//   GET  /frigate/...      read-only, allow-listed proxy to Frigate (live snapshots, recordings, clips)
//
// Frigate's own API is NOT exposed in general: /api/config contains the camera passwords.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/vision.html
var visionPageHTML []byte

var (
	frigateBaseURL = "http://127.0.0.1:5150" // GAVETY_FRIGATE_URL; on Docker Desktop use http://host.docker.internal:5150
	pbkdf2Iters    = 150000                  // lowered in tests
)

const (
	uiCookie     = "gv_session"
	uiSessionTTL = 7 * 24 * time.Hour
	uiMinPassLen = 8
)

func uiAuthFile() string   { return filepath.Join(stateDir, "vision_ui_auth.json") }
func uiSecretFile() string { return filepath.Join(stateDir, "vision_ui_secret") }

// ─── password hashing (PBKDF2-HMAC-SHA256, stdlib only) ───

func pbkdf2SHA256(password, salt []byte, iter int) []byte {
	prf := hmac.New(sha256.New, password)
	prf.Write(salt)
	prf.Write([]byte{0, 0, 0, 1})
	u := prf.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < iter; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

type uiAuthRecord struct {
	Salt  string `json:"salt"`
	Hash  string `json:"hash"`
	Iters int    `json:"iters"`
}

type uiServer struct {
	apiToken  string // internal bearer token used to call the same handlers as the loopback API
	api       http.Handler
	envPass   string // GAVETY_UI_PASSWORD (takes precedence; no setup step)
	setupCode string // required for first-run setup when no password exists

	mu      sync.Mutex
	secret  []byte
	fails   map[string][]time.Time
	frigate *httputil.ReverseProxy
	frigURL *url.URL
}

func newUIServer(apiToken string) (*uiServer, error) {
	u, err := url.Parse(frigateBaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad GAVETY_FRIGATE_URL %q", frigateBaseURL)
	}
	s := &uiServer{apiToken: apiToken, api: newMux(apiToken), envPass: os.Getenv("GAVETY_UI_PASSWORD"),
		fails: map[string][]time.Time{}, frigURL: u}
	if s.secret, err = s.loadOrCreateSecret(); err != nil {
		return nil, err
	}
	s.frigate = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/frigate")
			r.Host = u.Host
			r.Header.Del("Cookie") // never forward the session cookie to Frigate
			r.Header.Del("Authorization")
		},
		FlushInterval: 200 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Vision engine not reachable", http.StatusBadGateway)
		},
	}
	if s.envPass == "" && !s.configured() {
		b := make([]byte, 4)
		rand.Read(b)
		s.setupCode = hex.EncodeToString(b)
		log.Printf("🔑 Vision UI first-run setup code: %s  (enter it in the browser to set your password)", s.setupCode)
	}
	return s, nil
}

func (s *uiServer) loadOrCreateSecret() ([]byte, error) {
	if b, err := os.ReadFile(uiSecretFile()); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	os.MkdirAll(stateDir, 0700)
	if err := os.WriteFile(uiSecretFile(), b, 0600); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *uiServer) configured() bool {
	if s.envPass != "" {
		return true
	}
	_, err := os.Stat(uiAuthFile())
	return err == nil
}

func (s *uiServer) checkPassword(pw string) bool {
	if s.envPass != "" {
		return subtle.ConstantTimeCompare([]byte(pw), []byte(s.envPass)) == 1
	}
	data, err := os.ReadFile(uiAuthFile())
	if err != nil {
		return false
	}
	var rec uiAuthRecord
	if json.Unmarshal(data, &rec) != nil {
		return false
	}
	salt, _ := hex.DecodeString(rec.Salt)
	want, _ := hex.DecodeString(rec.Hash)
	got := pbkdf2SHA256([]byte(pw), salt, rec.Iters)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *uiServer) savePassword(pw string) error {
	salt := make([]byte, 16)
	rand.Read(salt)
	rec := uiAuthRecord{Salt: hex.EncodeToString(salt), Iters: pbkdf2Iters,
		Hash: hex.EncodeToString(pbkdf2SHA256([]byte(pw), salt, pbkdf2Iters))}
	b, _ := json.Marshal(rec)
	os.MkdirAll(stateDir, 0700)
	return os.WriteFile(uiAuthFile(), b, 0600)
}

// ─── sessions ───

func (s *uiServer) sign(msg string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *uiServer) newSession() string {
	n := make([]byte, 12)
	rand.Read(n)
	body := strconv.FormatInt(time.Now().Add(uiSessionTTL).Unix(), 10) + "." + hex.EncodeToString(n)
	return body + "." + s.sign(body)
}

func (s *uiServer) validSession(r *http.Request) bool {
	c, err := r.Cookie(uiCookie)
	if err != nil {
		return false
	}
	i := strings.LastIndex(c.Value, ".")
	if i < 0 {
		return false
	}
	body, sig := c.Value[:i], c.Value[i+1:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(body))) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(strings.SplitN(body, ".", 2)[0], 10, 64)
	return err == nil && time.Now().Unix() < exp
}

func (s *uiServer) setCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: s.newSession(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(uiSessionTTL.Seconds())})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// tooManyFails: 5 failed attempts per minute per client address.
func (s *uiServer) tooManyFails(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	kept := s.fails[ip][:0]
	for _, t := range s.fails[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	s.fails[ip] = kept
	return len(kept) >= 5
}

func (s *uiServer) noteFail(ip string) {
	s.mu.Lock()
	s.fails[ip] = append(s.fails[ip], time.Now())
	s.mu.Unlock()
}

// ─── handlers ───

func (s *uiServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /ui/state", s.state)
	mux.HandleFunc("POST /ui/setup", s.setup)
	mux.HandleFunc("POST /ui/login", s.login)
	mux.HandleFunc("POST /ui/logout", s.logout)
	mux.HandleFunc("POST /ui/cmd", s.cmd)
	mux.HandleFunc("GET /frigate/", s.frigateProxy)
	mux.HandleFunc("HEAD /frigate/", s.frigateProxy)
	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *uiServer) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(visionPageHTML)
}

func (s *uiServer) state(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]interface{}{"configured": s.configured(), "authed": s.validSession(r)})
}

func readSmallJSON(r *http.Request, v interface{}) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10)).Decode(v)
}

func (s *uiServer) setup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password, Code string }
	if readSmallJSON(r, &in) != nil {
		writeJSON(w, errBody("invalid request"))
		return
	}
	ip := clientIP(r)
	if s.tooManyFails(ip) {
		http.Error(w, "too many attempts, wait a minute", http.StatusTooManyRequests)
		return
	}
	if s.configured() {
		writeJSON(w, errBody("already set up — please log in"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Code)), []byte(s.setupCode)) != 1 {
		s.noteFail(ip)
		writeJSON(w, errBody("wrong setup code (see the container log)"))
		return
	}
	if len(in.Password) < uiMinPassLen {
		writeJSON(w, errBody(fmt.Sprintf("password must be at least %d characters", uiMinPassLen)))
		return
	}
	if err := s.savePassword(in.Password); err != nil {
		writeJSON(w, errBody(err.Error()))
		return
	}
	s.setCookie(w, r)
	writeJSON(w, map[string]interface{}{"status": "ok"})
}

func (s *uiServer) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password string }
	if readSmallJSON(r, &in) != nil {
		writeJSON(w, errBody("invalid request"))
		return
	}
	ip := clientIP(r)
	if s.tooManyFails(ip) {
		http.Error(w, "too many attempts, wait a minute", http.StatusTooManyRequests)
		return
	}
	if !s.configured() || !s.checkPassword(in.Password) {
		s.noteFail(ip)
		writeJSON(w, errBody("wrong password"))
		return
	}
	s.setCookie(w, r)
	writeJSON(w, map[string]interface{}{"status": "ok"})
}

func (s *uiServer) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]interface{}{"status": "ok"})
}

// requireAuth also demands a custom header on POSTs: browsers will not send it cross-site without a
// CORS preflight, which this server never approves (belt and braces on top of SameSite=Strict).
func (s *uiServer) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if !s.validSession(r) {
		http.Error(w, "login required", http.StatusUnauthorized)
		return false
	}
	if r.Method == http.MethodPost && r.Header.Get("X-Requested-With") != "gavety" {
		http.Error(w, "bad request", http.StatusForbidden)
		return false
	}
	return true
}

// uiDrives lists the folders under the storage root, which play the role of "drives".
func uiDrives() []string {
	var out []string
	ents, _ := os.ReadDir(hddRoot)
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, filepath.Join(hddRoot, e.Name()))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = []string{hddRoot}
	}
	return out
}

// call runs one of the core's own API handlers in-process and returns its JSON reply.
func (s *uiServer) call(method, path string, body interface{}) map[string]interface{} {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+s.apiToken)
	rec := httptest.NewRecorder()
	s.api.ServeHTTP(rec, req)
	var out map[string]interface{}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		return errBody(fmt.Sprintf("engine returned %d", rec.Code))
	}
	return out
}

type uiCmd struct {
	Command      string          `json:"command"`
	DeviceID     string          `json:"device_id"`
	VisionDevice json.RawMessage `json:"vision_device"`
	Settings     json.RawMessage `json:"settings"`
}

var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *uiServer) cmd(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	var c uiCmd
	if readSmallJSON(r, &c) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	setKnownDrives(uiDrives())
	var typ string
	var payload map[string]interface{}
	var dev, set interface{}
	if len(c.VisionDevice) > 0 {
		json.Unmarshal(c.VisionDevice, &dev)
	}
	if len(c.Settings) > 0 {
		json.Unmarshal(c.Settings, &set)
	}
	switch c.Command {
	case "vision_list_devices":
		typ, payload = "VISION_LIST_RES", s.call("GET", "/v1/devices", nil)
	case "vision_test_connection":
		typ, payload = "VISION_TEST_RES", s.call("POST", "/v1/devices/test", dev)
	case "vision_save_device":
		typ, payload = "VISION_SAVE_RES", s.call("POST", "/v1/devices", dev)
	case "vision_apply":
		typ, payload = "VISION_APPLY_RES", s.call("POST", "/v1/apply", nil)
	case "vision_delete_device":
		if !deviceIDRe.MatchString(c.DeviceID) {
			http.Error(w, "bad device id", http.StatusBadRequest)
			return
		}
		typ, payload = "VISION_DELETE_RES", s.call("DELETE", "/v1/devices/"+c.DeviceID, nil)
	case "vision_get_settings":
		typ, payload = "VISION_SETTINGS_RES", s.call("GET", "/v1/settings", nil)
		payload["available_drives"] = uiDrives()
	case "vision_save_settings":
		typ, payload = "VISION_SETTINGS_SAVE_RES", s.call("POST", "/v1/settings", set)
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]interface{}{"type": typ, "payload": payload})
}

// Frigate paths the page needs. Everything else (notably /api/config) is refused.
var frigateAllowed = regexp.MustCompile(`^/frigate/api/(recordings/summary|[A-Za-z0-9_]+/(latest\.jpg|recordings|start/[0-9.]+/end/[0-9.]+/clip\.mp4))$`)

func (s *uiServer) frigateProxy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	if !frigateAllowed.MatchString(r.URL.Path) {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	s.frigate.ServeHTTP(w, r)
}

// startUI runs the standalone web server. Returns the server so main can shut it down.
func startUI(addr, apiToken string) (*http.Server, error) {
	ui, err := newUIServer(apiToken)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Addr: addr, Handler: ui.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		log.Printf("🖥️  Vision web UI on %s (Frigate at %s)", addr, frigateBaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ Vision UI failed: %v", err)
		}
	}()
	return srv, nil
}
