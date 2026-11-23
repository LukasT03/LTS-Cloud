package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Boards that have not connected for this long are deleted along with their last status.
const retention = 180 * 24 * time.Hour

func main() {
	addr := env("LISTEN_ADDR", "127.0.0.1:8080")
	store, err := openStore(env("DB_PATH", "cloud.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.db.Close()

	hub := newHub(store)
	srv := &http.Server{Addr: addr, Handler: routes(hub), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cleanup(ctx, store)
	go func() {
		<-ctx.Done()
		hub.shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func routes(hub *Hub) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/board", hub.serveBoard)
	mux.HandleFunc("GET /v1/app", hub.serveApp)
	mux.HandleFunc("GET /v1/status", hub.serveStatus)
	mux.HandleFunc("POST /v1/command", hub.serveCommand)
	mux.HandleFunc("POST /v1/rekey", hub.serveRekey)
	return mux
}

func cleanup(ctx context.Context, store *Store) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		n, err := store.deleteStale(time.Now().Add(-retention))
		if err != nil {
			log.Printf("cleanup: %v", err)
		} else if n > 0 {
			log.Printf("cleanup: deleted %d boards", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
