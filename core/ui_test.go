package main

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var timeZero = time.Time{}

type uiEnv struct {
	srv       *httptest.Server
	ui        *uiServer
	client    *http.Client
	sawCookie bool
}

// setupUI starts a UI server in front of a fake Frigate. password "" = open mode.
func setupUI(t *testing.T, password string) *uiEnv {
	t.Helper()
	_, _ = setup(t) // temp state dirs + fake docker
	e := &uiEnv{}
	frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			e.sawCookie = true
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("JPEGDATA"))
		case strings.HasSuffix(r.URL.Path, "/clip.mp4"):
			http.ServeContent(w, r, "clip.mp4", timeZero, strings.NewReader("0123456789"))
		default:
			w.Write([]byte("FRIGATE-SECRET-CONFIG"))
		}
	}))
	t.Cleanup(frigate.Close)
	old := frigateBaseURL
	frigateBaseURL = frigate.URL
	t.Cleanup(func() { frigateBaseURL = old })
	if password == "" {
		os.Unsetenv("GAVETY_UI_PASSWORD")
	} else {
		os.Setenv("GAVETY_UI_PASSWORD", password)
	}
	t.Cleanup(func() { os.Unsetenv("GAVETY_UI_PASSWORD") })
	ui, err := newUIServer(testToken)
	if err != nil {
		t.Fatal(err)
	}
	e.ui = ui
	e.srv = httptest.NewServer(ui.Handler())
	t.Cleanup(e.srv.Close)
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar}
	return e
}

func (e *uiEnv) do(t *testing.T, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var cmdHdr = map[string]string{"X-Requested-With": "gavety", "Content-Type": "application/json"}

// ─── open mode (default): no login ───

func TestUIOpenModeNeedsNoLogin(t *testing.T) {
	e := setupUI(t, "")
	if c, b := e.do(t, "GET", "/", "", nil); c != 200 || !strings.Contains(b, "Gavety Vision") {
		t.Fatalf("page: %d", c)
	}
	_, b := e.do(t, "GET", "/ui/state", "", nil)
	if !strings.Contains(b, `"open":true`) || !strings.Contains(b, `"authed":true`) {
		t.Fatalf("state: %s", b)
	}
	if c, b := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 200 || !strings.Contains(b, "VISION_LIST_RES") {
		t.Fatalf("cmd: %d %s", c, b)
	}
	if c, b := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg?t=1", "", nil); c != 200 || b != "JPEGDATA" {
		t.Fatalf("snapshot: %d %q", c, b)
	}
	if _, b := e.do(t, "POST", "/ui/login", `{"password":"anything"}`, nil); strings.Contains(b, `"ok"`) {
		t.Fatal("login must not 'succeed' in open mode with a guessed password")
	}
}

func TestUIOpenModeStillBlocksCSRFAndBadHosts(t *testing.T) {
	e := setupUI(t, "")
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, map[string]string{"Content-Type": "application/json"}); c != 403 {
		t.Errorf("POST without X-Requested-With must be refused: %d", c)
	}
	// DNS rebinding: a website's own domain in the Host header
	for _, host := range []string{"evil.example.com", "attacker.net:5170", "x.y.z"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/ui/state", nil)
		req.Host = host
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 403 {
			t.Errorf("Host %q must be refused, got %d", host, resp.StatusCode)
		}
	}
	for _, host := range []string{"localhost:5170", "127.0.0.1:5170", "192.168.1.20:5170", "[::1]:5170", "mypc:5170", "mypc.local", "gavety-pi"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/ui/state", nil)
		req.Host = host
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != 200 {
			t.Errorf("Host %q must be allowed, got %d", host, resp.StatusCode)
		}
	}
	os.Setenv("GAVETY_UI_ALLOWED_HOSTS", "vision.mycompany.com")
	defer os.Unsetenv("GAVETY_UI_ALLOWED_HOSTS")
	ui, _ := newUIServer(testToken)
	srv := httptest.NewServer(ui.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/ui/state", nil)
	req.Host = "vision.mycompany.com"
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Errorf("explicitly allowed host refused: %d", resp.StatusCode)
	}
}

func TestUICommands(t *testing.T) {
	e := setupUI(t, "")
	os.MkdirAll(filepath.Join(hddRoot, "diskA"), 0755)

	_, b := e.do(t, "POST", "/ui/cmd", `{"command":"vision_save_device","vision_device":{"id":"c1","nickname":"Gate","ip":"10.0.0.5","username":"a","password":"pw"}}`, cmdHdr)
	if !strings.Contains(b, `"type":"VISION_SAVE_RES"`) || !strings.Contains(b, `"status":"ok"`) {
		t.Fatalf("save: %s", b)
	}
	_, b = e.do(t, "POST", "/ui/cmd", `{"command":"vision_apply"}`, cmdHdr)
	if !strings.Contains(b, `"type":"VISION_APPLY_RES"`) || !strings.Contains(b, "Gavety Vision is ready") {
		t.Fatalf("apply: %s", b)
	}
	_, b = e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr)
	if !strings.Contains(b, "VISION_LIST_RES") || !strings.Contains(b, `"nickname":"Gate"`) || strings.Contains(b, `"password":"pw"`) {
		t.Fatalf("list must not leak passwords: %s", b)
	}
	_, b = e.do(t, "POST", "/ui/cmd", `{"command":"vision_get_settings"}`, cmdHdr)
	if !strings.Contains(b, "available_drives") || !strings.Contains(b, "diskA") {
		t.Fatalf("settings/drives: %s", b)
	}
	_, b = e.do(t, "POST", "/ui/cmd", `{"command":"vision_test_connection","vision_device":{"ip":"bad.example"}}`, cmdHdr)
	if !strings.Contains(b, "VISION_TEST_RES") || !strings.Contains(b, `"status":"error"`) {
		t.Fatalf("test conn: %s", b)
	}
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_delete_device","device_id":"../x"}`, cmdHdr); c != 400 {
		t.Errorf("bad device id: %d", c)
	}
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"rm_rf"}`, cmdHdr); c != 400 {
		t.Errorf("unknown command: %d", c)
	}
	e.do(t, "POST", "/ui/cmd", `{"command":"vision_delete_device","device_id":"c1"}`, cmdHdr)
	bgWG.Wait()
	if left, _ := loadVisionDevices(); len(left) != 0 {
		t.Fatal("delete failed")
	}
}

func TestUIFrigateProxy(t *testing.T) {
	e := setupUI(t, "")
	if c, b := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg?t=1", "", nil); c != 200 || b != "JPEGDATA" {
		t.Fatalf("snapshot: %d %q", c, b)
	}
	if e.sawCookie {
		t.Error("cookie/authorization leaked to Frigate")
	}
	// secrets live in /api/config: must never be reachable
	for _, p := range []string{"/frigate/api/config", "/frigate/api/config/raw", "/frigate/api/version", "/frigate/", "/frigate/api/cam1_ch1/latest.jpg/../../config"} {
		if c, b := e.do(t, "GET", p, "", nil); c == 200 {
			t.Errorf("%s should not be allowed (got 200: %.30q)", p, b)
		}
	}
	if c, _ := e.do(t, "POST", "/frigate/api/cam1_ch1/latest.jpg", "", cmdHdr); c == 200 {
		t.Error("POST through the proxy must not work")
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/frigate/api/cam1_ch1/start/100.5/end/200.5/clip.mp4", nil)
	req.Header.Set("Range", "bytes=2-5") // video seeking needs Range passthrough
	resp, _ := e.client.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(b) != "2345" {
		t.Errorf("range: %d %q", resp.StatusCode, b)
	}
}

// ─── password mode (GAVETY_UI_PASSWORD set) ───

func TestUIPasswordModeRequiresLogin(t *testing.T) {
	e := setupUI(t, "s3cret-pass")
	_, b := e.do(t, "GET", "/ui/state", "", nil)
	if !strings.Contains(b, `"open":false`) || !strings.Contains(b, `"authed":false`) {
		t.Fatalf("state: %s", b)
	}
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 401 {
		t.Errorf("cmd without login: %d", c)
	}
	if c, _ := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg", "", nil); c != 401 {
		t.Errorf("frigate without login: %d", c)
	}
	if _, b = e.do(t, "POST", "/ui/login", `{"password":"wrong"}`, nil); !strings.Contains(b, "wrong password") {
		t.Fatal(b)
	}
	if _, b = e.do(t, "POST", "/ui/login", `{"password":"s3cret-pass"}`, nil); !strings.Contains(b, `"ok"`) {
		t.Fatal(b)
	}
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 200 {
		t.Errorf("after login: %d", c)
	}
	if c, _ := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg", "", nil); c != 200 {
		t.Errorf("frigate after login: %d", c)
	}
	e.do(t, "POST", "/ui/logout", "", cmdHdr)
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 401 {
		t.Errorf("after logout: %d", c)
	}
}

func TestUIPasswordModeRateLimitAndForgery(t *testing.T) {
	e := setupUI(t, "s3cret-pass")
	for i := 0; i < 5; i++ {
		e.do(t, "POST", "/ui/login", `{"password":"bad"}`, nil)
	}
	if c, _ := e.do(t, "POST", "/ui/login", `{"password":"s3cret-pass"}`, nil); c != 429 {
		t.Fatalf("want 429 after 5 failures, got %d", c)
	}
	// forged / tampered cookie
	e2 := setupUI(t, "s3cret-pass")
	e2.do(t, "POST", "/ui/login", `{"password":"s3cret-pass"}`, nil)
	u, _ := http.NewRequest("GET", e2.srv.URL, nil)
	good := e2.client.Jar.Cookies(u.URL)[0].Value
	req, _ := http.NewRequest("POST", e2.srv.URL+"/ui/cmd", strings.NewReader(`{"command":"vision_list_devices"}`))
	req.Header.Set("X-Requested-With", "gavety")
	req.AddCookie(&http.Cookie{Name: uiCookie, Value: good[:len(good)-3] + "AAA"})
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Errorf("forged cookie accepted: %d", resp.StatusCode)
	}
}
