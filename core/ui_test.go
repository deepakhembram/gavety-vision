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
	frigate   *httptest.Server
	sawCookie bool
}

func setupUI(t *testing.T) *uiEnv {
	t.Helper()
	_, _ = setup(t) // temp state dirs + fake docker
	pbkdf2Iters = 1000
	e := &uiEnv{}
	e.frigate = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(e.frigate.Close)
	old := frigateBaseURL
	frigateBaseURL = e.frigate.URL
	t.Cleanup(func() { frigateBaseURL = old })
	os.Unsetenv("GAVETY_UI_PASSWORD")
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

func (e *uiEnv) login(t *testing.T) {
	t.Helper()
	_, b := e.do(t, "POST", "/ui/setup", `{"password":"correct-horse","code":"`+e.ui.setupCode+`"}`, nil)
	if !strings.Contains(b, `"ok"`) {
		t.Fatalf("setup: %s", b)
	}
}

func TestUIRequiresLogin(t *testing.T) {
	e := setupUI(t)
	if c, b := e.do(t, "GET", "/", "", nil); c != 200 || !strings.Contains(b, "Gavety Vision") {
		t.Fatalf("page: %d", c)
	}
	_, b := e.do(t, "GET", "/ui/state", "", nil)
	if !strings.Contains(b, `"configured":false`) || !strings.Contains(b, `"authed":false`) {
		t.Fatalf("state: %s", b)
	}
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 401 {
		t.Errorf("cmd without login: %d", c)
	}
	if c, _ := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg", "", nil); c != 401 {
		t.Errorf("frigate without login: %d", c)
	}
}

func TestUISetupLoginLogout(t *testing.T) {
	e := setupUI(t)
	_, b := e.do(t, "POST", "/ui/setup", `{"password":"correct-horse","code":"nope"}`, nil)
	if !strings.Contains(b, "wrong setup code") {
		t.Fatal(b)
	}
	_, b = e.do(t, "POST", "/ui/setup", `{"password":"short","code":"`+e.ui.setupCode+`"}`, nil)
	if !strings.Contains(b, "at least 8") {
		t.Fatal(b)
	}
	e.login(t)
	if st, _ := os.Stat(uiAuthFile()); st.Mode().Perm() != 0600 {
		t.Errorf("auth file mode %v", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(uiAuthFile())
	if strings.Contains(string(raw), "correct-horse") {
		t.Fatal("password stored in clear")
	}
	_, b = e.do(t, "GET", "/ui/state", "", nil)
	if !strings.Contains(b, `"authed":true`) {
		t.Fatalf("not logged in after setup: %s", b)
	}
	if _, b = e.do(t, "POST", "/ui/setup", `{"password":"another-pass","code":"`+e.ui.setupCode+`"}`, nil); !strings.Contains(b, "already set up") {
		t.Fatalf("setup must be one-time: %s", b)
	}
	e.do(t, "POST", "/ui/logout", "", cmdHdr)
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, cmdHdr); c != 401 {
		t.Errorf("after logout: %d", c)
	}
	if _, b = e.do(t, "POST", "/ui/login", `{"password":"wrong"}`, nil); !strings.Contains(b, "wrong password") {
		t.Fatal(b)
	}
	if _, b = e.do(t, "POST", "/ui/login", `{"password":"correct-horse"}`, nil); !strings.Contains(b, `"ok"`) {
		t.Fatal(b)
	}
}

func TestUILoginRateLimit(t *testing.T) {
	e := setupUI(t)
	e.login(t)
	e.do(t, "POST", "/ui/logout", "", cmdHdr)
	for i := 0; i < 5; i++ {
		e.do(t, "POST", "/ui/login", `{"password":"bad"}`, nil)
	}
	if c, _ := e.do(t, "POST", "/ui/login", `{"password":"correct-horse"}`, nil); c != 429 {
		t.Fatalf("want 429 after 5 failures, got %d", c)
	}
}

func TestUIEnvPassword(t *testing.T) {
	e := setupUI(t)
	os.Setenv("GAVETY_UI_PASSWORD", "from-env-pass")
	defer os.Unsetenv("GAVETY_UI_PASSWORD")
	ui, _ := newUIServer(testToken)
	srv := httptest.NewServer(ui.Handler())
	defer srv.Close()
	e.srv, e.ui = srv, ui
	if _, b := e.do(t, "POST", "/ui/login", `{"password":"from-env-pass"}`, nil); !strings.Contains(b, `"ok"`) {
		t.Fatal(b)
	}
	if ui.setupCode != "" {
		t.Error("no setup code when a password is provided")
	}
}

func TestUISessionTamperAndCSRFHeader(t *testing.T) {
	e := setupUI(t)
	e.login(t)
	if c, _ := e.do(t, "POST", "/ui/cmd", `{"command":"vision_list_devices"}`, map[string]string{"Content-Type": "application/json"}); c != 403 {
		t.Errorf("POST without X-Requested-With must be refused: %d", c)
	}
	u, _ := http.NewRequest("GET", e.srv.URL, nil)
	good := e.client.Jar.Cookies(u.URL)[0].Value
	forged := good[:len(good)-3] + "AAA"
	req, _ := http.NewRequest("POST", e.srv.URL+"/ui/cmd", strings.NewReader(`{"command":"vision_list_devices"}`))
	req.Header.Set("X-Requested-With", "gavety")
	req.AddCookie(&http.Cookie{Name: uiCookie, Value: forged})
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("forged cookie accepted: %d", resp.StatusCode)
	}
}

func TestUICommands(t *testing.T) {
	e := setupUI(t)
	e.login(t)
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
	e := setupUI(t)
	e.login(t)
	if c, b := e.do(t, "GET", "/frigate/api/cam1_ch1/latest.jpg?t=1", "", nil); c != 200 || b != "JPEGDATA" {
		t.Fatalf("snapshot: %d %q", c, b)
	}
	if e.sawCookie {
		t.Error("session cookie leaked to Frigate")
	}
	// secrets live in /api/config: must never be reachable
	for _, p := range []string{"/frigate/api/config", "/frigate/api/config/raw", "/frigate/api/version", "/frigate/", "/frigate/api/cam1_ch1/latest.jpg/../../config"} {
		if c, b := e.do(t, "GET", p, "", nil); c == 200 && strings.Contains(b, "SECRET") {
			t.Errorf("%s leaked frigate config", p)
		} else if c == 200 {
			t.Errorf("%s should not be allowed (got 200)", p)
		}
	}
	if c, _ := e.do(t, "POST", "/frigate/api/cam1_ch1/latest.jpg", "", cmdHdr); c == 200 {
		t.Error("POST through the proxy must not work")
	}
	// video seeking needs Range passthrough
	req, _ := http.NewRequest("GET", e.srv.URL+"/frigate/api/cam1_ch1/start/100.5/end/200.5/clip.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, _ := e.client.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(b) != "2345" {
		t.Errorf("range: %d %q", resp.StatusCode, b)
	}
}
