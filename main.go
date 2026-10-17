// bootwatchd is boot.watch's server: the test endpoints OmaBoot? Live's network checks move
// their data through (README.md), and later the phone relay. It keeps no request log and no
// addresses; on disk only today's byte count, for the daily cap.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// version is the build, set by make: `git describe --tags --always --dirty` (e.g. v0.1.0).
var version = "dev"

func main() {
	listen := flag.String("listen", ":8470", "address to listen on")
	cert := flag.String("cert", "", "TLS certificate (PEM; the Cloudflare Origin CA certificate in production); without it, plain HTTP")
	key := flag.String("key", "", "the certificate's private key (PEM)")
	cloudflare := flag.Bool("cloudflare", false, "behind Cloudflare's proxy: a client's address is its CF-Connecting-IP (only with a firewall that admits Cloudflare alone)")
	dayFile := flag.String("day-file", stateFile("day"), "today's byte count, kept so a restart doesn't reset the daily cap (default $STATE_DIRECTORY/day; empty: memory only)")
	flag.Parse()
	if (*cert == "") != (*key == "") {
		fmt.Fprintln(os.Stderr, "bootwatchd: -cert and -key go together")
		os.Exit(2)
	}

	h := newServer(defaultLimits, *cloudflare)
	if *dayFile != "" {
		if err := h.loadDay(*dayFile); err != nil {
			log.Printf("bootwatchd: today's byte count starts again: %v", err)
		}
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0), // its lines name client addresses: nothing about clients is kept
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	down := make(chan struct{})
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		close(down)
	}()
	if *dayFile != "" {
		go keepDay(ctx, h, *dayFile)
	}

	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	log.Printf("bootwatchd %s: %s on %s (cloudflare %v)", version, scheme, *listen, *cloudflare)
	var err error
	if *cert != "" {
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		err = srv.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("bootwatchd: %v", err)
	}
	<-down // the requests under way are counted
	if *dayFile != "" {
		if err := h.saveDay(*dayFile); err != nil {
			log.Printf("bootwatchd: saving today's byte count: %v", err)
		}
	}
}

// stateFile is name in the directory systemd gives the service ($STATE_DIRECTORY), or "" (none).
func stateFile(name string) string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return filepath.Join(d, name)
	}
	return ""
}

// keepDay saves today's byte count every 10 s while it changes: a crash forgets 10 s at most.
func keepDay(ctx context.Context, s *server, path string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := s.saveDay(path)
		if (err != nil) != failing {
			if err != nil {
				log.Printf("bootwatchd: saving today's byte count: %v", err)
			} else {
				log.Printf("bootwatchd: saving today's byte count again")
			}
			failing = err != nil
		}
	}
}
