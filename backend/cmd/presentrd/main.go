// Command presentrd is the presentr service daemon. It exposes presentr's HTTP surface under
// /api/services/presentr/, validates the shared holistic session (a signed JWT in the h_access
// cookie) without any RPC to the holistic backend, and enforces the holistic rights standard.
// It runs unprivileged behind the holistic Caddy proxy.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"presentr/internal/api"
	"presentr/internal/auth"
	"presentr/internal/store"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8783", "address to listen on")
	flag.Parse()

	secret, err := auth.LoadSecret()
	if err != nil {
		log.Fatalf("presentrd: %v", err)
	}
	// Admin = membership in this group (the single Linux source of truth). The systemd unit
	// sets PRESENTR_ADMIN_GROUP; the verifier defaults to "sudo" when it is empty.
	v := auth.NewVerifier(secret, os.Getenv("PRESENTR_ADMIN_GROUP"))

	// The room's document pool: the single access point to the knowledge presentr owns.
	// `service setup` provisions the data dir and marks it writable in the unit (ReadWritePaths).
	dataRoot := getenv("PRESENTR_DATA", "/var/lib/presentr")
	docs, err := store.OpenDocs(filepath.Join(dataRoot, "docs.json"))
	if err != nil {
		log.Fatalf("presentrd: open document pool: %v", err)
	}
	chats, err := store.OpenChats(filepath.Join(dataRoot, "chats.json"))
	if err != nil {
		log.Fatalf("presentrd: open chat pool: %v", err)
	}
	diagram, err := store.OpenDiagram(filepath.Join(dataRoot, "diagram.json"))
	if err != nil {
		log.Fatalf("presentrd: open diagram pool: %v", err)
	}

	srv := &http.Server{
		Handler:           api.New(v, docs, chats, diagram).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind synchronously so an "address in use" surfaces here, not in a goroutine.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("presentrd: listen %s: %v", *listen, err)
	}
	go func() {
		log.Printf("presentrd listening on %s (data=%s)", *listen, dataRoot)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("presentrd: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Print("presentrd stopped")
}

// getenv returns the value of key, or def when it is unset or empty.
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
