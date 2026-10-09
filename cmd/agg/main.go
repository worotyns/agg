// Command agg runs the agg server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // alert schedules use IANA timezones; the distroless image has no zoneinfo

	"github.com/worotyns/agg/internal/alert"
	"github.com/worotyns/agg/internal/engine"
	"github.com/worotyns/agg/internal/geo"
	"github.com/worotyns/agg/internal/server"
	"github.com/worotyns/agg/internal/store"
	"github.com/worotyns/agg/internal/webpush"
)

var version = "dev"

const usage = `agg: real-time aggregates for website events.

Usage:
  agg serve [flags]            run the server
  agg reset-admin-token [-db]  generate a new admin token (logs out all sessions)
  agg version

Serve flags (each can also be set with the environment variable in brackets):
`

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		serveFlags(flag.NewFlagSet("serve", flag.ExitOnError)).PrintDefaults()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(log, os.Args[2:]); err != nil {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	case "reset-admin-token":
		fs := flag.NewFlagSet("reset-admin-token", flag.ExitOnError)
		db := fs.String("db", env("AGG_DB", "agg.db"), "SQLite database file [AGG_DB]")
		fs.Parse(os.Args[2:])
		st, err := store.OpenSQLite(*db)
		if err != nil {
			log.Error("cannot open database", "err", err)
			os.Exit(1)
		}
		defer st.Close()
		t, err := server.ResetAdminToken(context.Background(), st)
		if err != nil {
			log.Error("cannot reset token", "err", err)
			os.Exit(1)
		}
		fmt.Println(t)
	case "version", "-v", "--version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

type serveOpts struct {
	addr, db, adminToken, publicURL, metricsToken, vapidSubject, clientIPHeader, geoipURL *string
	retention, maxKeys                                                                    *int
	trustProxy                                                                            *bool
}

var opts serveOpts

func serveFlags(fs *flag.FlagSet) *flag.FlagSet {
	opts = serveOpts{
		addr:         fs.String("addr", env("AGG_ADDR", ":8080"), "listen address [AGG_ADDR]"),
		db:           fs.String("db", env("AGG_DB", "agg.db"), "SQLite database file [AGG_DB]"),
		adminToken:   fs.String("admin-token", env("AGG_ADMIN_TOKEN", ""), "admin token; generated and printed on first start if empty [AGG_ADMIN_TOKEN]"),
		publicURL:    fs.String("public-url", env("AGG_PUBLIC_URL", ""), "public URL of this server, e.g. https://agg.example.com [AGG_PUBLIC_URL]"),
		metricsToken: fs.String("internal-metrics-token", env("AGG_INTERNAL_METRICS_TOKEN", ""), "enables /internal/metrics with this bearer token [AGG_INTERNAL_METRICS_TOKEN]"),
		retention:    fs.Int("raw-retention-days", envInt("AGG_RAW_RETENTION_DAYS", 7), "days to keep raw events (rebuild, tester) [AGG_RAW_RETENTION_DAYS]"),
		maxKeys:      fs.Int("max-keys-per-aggregate", envInt("AGG_MAX_KEYS_PER_AGGREGATE", 50000), "cap on distinct group values per aggregate [AGG_MAX_KEYS_PER_AGGREGATE]"),
		vapidSubject: fs.String("vapid-subject", env("AGG_VAPID_SUBJECT", ""), "contact for Web Push services, mailto: or https: URL; default: the public URL [AGG_VAPID_SUBJECT]"),
		trustProxy:   fs.Bool("trust-proxy", env("AGG_TRUST_PROXY", "") == "1" || env("AGG_TRUST_PROXY", "") == "true", "trust X-Forwarded-For/-Proto from a reverse proxy [AGG_TRUST_PROXY]"),
		geoipURL:     fs.String("geoip-url", env("AGG_GEOIP_URL", ""), "base URL of a GeoIP lookup service, e.g. http://geoip:8082; enables meta.country/meta.city [AGG_GEOIP_URL]"),
		clientIPHeader: fs.String("client-ip-header", env("AGG_CLIENT_IP_HEADER", ""),
			"header set by your proxy with the client IP, e.g. Fly-Client-IP or CF-Connecting-IP; preferred over X-Forwarded-For [AGG_CLIENT_IP_HEADER]"),
	}
	return fs
}

func serve(log *slog.Logger, args []string) error {
	fs := serveFlags(flag.NewFlagSet("serve", flag.ExitOnError))
	fs.Parse(args)

	st, err := store.OpenSQLite(*opts.db)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	if t, err := server.EnsureAdminToken(ctx, st, *opts.adminToken); err != nil {
		return err
	} else if t != "" {
		fmt.Fprintf(os.Stderr, "\n  Admin token (shown once, keep it safe): %s\n  Lost it? Run: agg reset-admin-token -db %s\n\n", t, *opts.db)
	}

	eopts := engine.Options{RawRetentionDays: *opts.retention, MaxKeysPerAggregate: *opts.maxKeys}
	if *opts.geoipURL != "" {
		if eopts.Geo, err = geo.New(*opts.geoipURL, log); err != nil {
			return err
		}
		log.Info("geoip enabled", "url", *opts.geoipURL)
	}
	eng := engine.New(st, log, eopts)
	if err := eng.Start(ctx); err != nil {
		return err
	}
	vapid, err := server.EnsureVAPID(ctx, st)
	if err != nil {
		return err
	}
	vapid.Subject = *opts.vapidSubject
	if vapid.Subject == "" && strings.HasPrefix(*opts.publicURL, "https://") {
		vapid.Subject = *opts.publicURL
	}
	alerts := &alert.Engine{St: st, Push: &webpush.Sender{VAPID: vapid}, Log: log, Now: time.Now}
	srv := server.New(server.Config{
		PublicURL: *opts.publicURL, TrustProxy: *opts.trustProxy, ClientIPHeader: *opts.clientIPHeader, InternalMetricsToken: *opts.metricsToken, Version: version,
	}, st, eng, alerts, log)
	actx, stopAlerts := context.WithCancel(context.Background())
	defer stopAlerts()
	go alerts.Run(actx, time.Minute)

	hs := &http.Server{
		Addr: *opts.addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("agg listening", "addr", *opts.addr, "db", *opts.db, "version", version)
		errc <- hs.ListenAndServe()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			eng.Close()
			return err
		}
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	return eng.Close()
}
