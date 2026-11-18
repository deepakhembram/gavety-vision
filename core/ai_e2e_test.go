package main

// Cross-language test: Go agent pipeline -> REAL Python worker.
// Skipped unless GAVETY_E2E_URL is set. See ../README.md ("Run the end-to-end test").
//   GAVETY_E2E_TYPE = http | queue      GAVETY_E2E_URL    GAVETY_E2E_TOKEN

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestE2EAgainstRealWorker(t *testing.T) {
	url := os.Getenv("GAVETY_E2E_URL")
	if url == "" {
		t.Skip("GAVETY_E2E_URL not set")
	}
	typ := os.Getenv("GAVETY_E2E_TYPE")
	useTempDirs(t)
	fr := fakeFrigate(t, func(n int) []byte { return mkJPEG(t, 10+n*12, n >= 3) })
	defer fr.Close()

	prov, err := vaiBuildProvider(VAIProviderCfg{Name: "real", Type: typ, URL: url,
		Token: os.Getenv("GAVETY_E2E_TOKEN"), TimeoutSec: 10})
	if err != nil {
		t.Fatal(err)
	}
	rules := []VAIRule{
		{Event: "person_in_zone", Labels: []string{"person"}, Zone: "press", ConfirmN: 2, ConfirmM: 3, CooldownSec: 60},
		{Event: "fire", Labels: []string{"fire"}, ConfirmN: 3, ConfirmM: 5, CooldownSec: 60, Severity: "critical"},
	}
	zones := []VAIZone{{Name: "press", Polygon: [][2]float64{{0.5, 0.2}, {1, 0.2}, {1, 1}, {0.5, 1}}}}
	m := newTestManager(t, fr.URL, prov, rules, zones, false)
	m.cfg.IntervalSec = 0.1

	var mu sync.Mutex
	got := map[string]VisionAIEvent{}
	VisionAIOnEvent = func(ev VisionAIEvent) { mu.Lock(); got[ev.Event] = ev; mu.Unlock() }
	defer func() { VisionAIOnEvent = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.camLoop(ctx, m.effective("cam1"))
	waitFor(t, "fire + zone events from the real worker", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
	cancel()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%s mode: fire via worker=%q model=%q latency=%dms", typ, got["fire"].Provider, got["fire"].Model, got["fire"].LatencyMS)
	if got["fire"].Model == "" || got["fire"].Severity != "critical" {
		t.Errorf("unexpected fire event: %+v", got["fire"])
	}
	_ = time.Second
}
