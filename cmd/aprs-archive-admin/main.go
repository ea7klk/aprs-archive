// Command aprs-archive-admin serves the archive's administration application.
//
// It is a separate binary from aprs-archive-web and shares no route table with
// it. The public server serves the archive read-only to anonymous visitors; this
// one serves exceptions, maintenance and SQL to a signed-in operator, on its own
// hostname, with its own read-write handle.
//
// Keeping them as distinct programs is what makes "the public host has no admin
// path" a property of the build rather than of a conditional somebody has to
// remember. A route added here cannot appear on the public host because the
// public binary does not contain this code.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aprs-archive/internal/admin"
	"aprs-archive/internal/prefixes"
	"aprs-archive/internal/store"
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

	cfg := admin.Config{
		Addr:         env("APRS_ARCHIVE_ADDR", ":8080"),
		DBPath:       env("APRS_ARCHIVE_DB", "/var/lib/aprs-archive/archive.db"),
		AppName:      env("APRS_ARCHIVE_APP_NAME", "APRS Archive"),
		Version:      version,
		Location:     env("APRS_ARCHIVE_LOCATION", "https://aprs-admin.ea7klk.es"),
		Password:     os.Getenv("APRS_ARCHIVE_ADMIN_PASSWORD"),
		PrefixSource: env("APRS_ARCHIVE_PREFIX_SOURCE", prefixes.DefaultSource),
	}
	keep, _ := strconv.Atoi(env("APRS_ARCHIVE_BACKUP_KEEP", "7"))
	cfg.BackupKeep = keep
	retention, _ := strconv.Atoi(env("APRS_ARCHIVE_RETENTION_DAYS", "365"))
	cfg.RetentionDays = retention

	if cfg.Password == "" {
		log.Printf("WARNING: APRS_ARCHIVE_ADMIN_PASSWORD is empty; the admin application will refuse every page")
	}

	// The admin application is the one web component with a write handle. It
	// waits for the collector to have created the archive, exactly as the public
	// server does.
	db, err := waitForWriter(cfg.DBPath, 5*time.Minute)
	if err != nil {
		return err
	}
	defer db.Close()

	srv, err := admin.New(cfg, db)
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
		// A SQL statement has its own 30-second budget; the write timeout is a
		// backstop for a client that stops reading.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
		}
	}()
	log.Printf("aprs-archive-admin %s listening on %s (db %s)", version, cfg.Addr, cfg.DBPath)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// waitForWriter opens the archive read-write, waiting for the collector to create
// it on a cold start. A schema mismatch is fatal immediately.
func waitForWriter(path string, timeout time.Duration) (*sql.DB, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	logged := false
	for {
		db, err := store.OpenAdminWriter(path)
		if err == nil {
			if logged {
				log.Printf("archive opened after waiting")
			}
			return db, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "no such file") && !strings.Contains(err.Error(), "unable to open database file") {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("archive did not become available within %s: %w", timeout, lastErr)
		}
		if !logged {
			log.Printf("waiting for the archive at %s: %v", path, err)
			logged = true
		}
		time.Sleep(time.Second)
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
