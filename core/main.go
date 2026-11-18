package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	token := os.Getenv("GAVETY_CORE_TOKEN")
	if len(token) < 16 {
		log.Fatal("GAVETY_CORE_TOKEN must be set (at least 16 characters). The agent generates and passes it.")
	}
	stateDir = env("GAVETY_STATE_DIR", stateDir)
	hddRoot = env("GAVETY_HDD_ROOT", hddRoot)
	visionFrigateImage = env("GAVETY_FRIGATE_IMAGE", visionFrigateImage)

	addr := env("GAVETY_CORE_ADDR", "127.0.0.1:5160")
	if host, _, err := net.SplitHostPort(addr); err != nil || !(host == "127.0.0.1" || host == "localhost" || host == "::1") {
		log.Printf("⚠️ listening on %q (not loopback): the API controls Docker and holds camera passwords; keep it off the LAN", addr)
	}

	StartVisionStorageEnforcement()
	StartVisionAI()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(token),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		// no WriteTimeout: /v1/apply and /v1/install legitimately take minutes on a Pi
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		bgWG.Wait()
	}()

	log.Printf("✅ gavety-vision-core %s listening on %s (state=%s, storage root=%s)", version, addr, stateDir, hddRoot)
	if err := srv.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "Server closed") {
		log.Fatal(err)
	}
}
