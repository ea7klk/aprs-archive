// Command aprs-archive-web serves the read-only APRS message archive.
//
// It opens the collector's database read-only, so a bug or a bad query in a
// handler cannot corrupt the archive, and a web rollout cannot block
// collection. Access is anonymous: everything served here is public APRS-IS
// traffic, and the archive holds nothing a visitor could not have heard on the
// air.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"aprs-archive/internal/store"
	"aprs-archive/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg := web.Config{
		Addr:      env("APRS_ARCHIVE_ADDR", ":8080"),
		DBPath:    env("APRS_ARCHIVE_DB", "/var/lib/aprs-archive/archive.db"),
		AppName:   env("APRS_ARCHIVE_APP_NAME", "APRS Archive"),
		Location:  env("APRS_ARCHIVE_LOCATION", "https://aprs.ea7klk.es"),
		Topic:     env("APRS_ARCHIVE_TOPIC", "Every APRS message and bulletin heard on APRS-IS, searchable."),
		Version:   version,
		Collector: env("APRS_ARCHIVE_COLLECTOR_CALLSIGN", "EA7KLK-15"),
	}
	retentionDays, _ := strconv.Atoi(env("APRS_ARCHIVE_RETENTION_DAYS", "365"))
	cfg.RetentionDays = retentionDays
	stale, _ := strconv.Atoi(env("APRS_ARCHIVE_COLLECTOR_STALE_MINUTES", "15"))
	cfg.CollectorStaleFor = time.Duration(stale) * time.Minute

	db, err := store.WaitForReader(cfg.DBPath, 5*time.Minute)
	if err != nil {
		return err
	}
	defer db.Close()

	srv, err := web.New(cfg, db)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A report over a year of traffic can take a while; the write timeout
		// is generous but still finite so a stalled client cannot pin a
		// connection forever.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	}()
	log.Printf("aprs-archive-web %s listening on %s (db %s)", version, cfg.Addr, cfg.DBPath)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
