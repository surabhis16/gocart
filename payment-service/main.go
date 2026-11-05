// Command payment-service runs the GoCart Payment bounded context.
//
// There is no real money. Charges go to a mock provider (internal/gateway)
// that approves, declines or fails depending on the payment token.
//
// Configuration comes from the environment:
//
//	PORT         listen port                         (default 8082)
//	DATA_DIR     where ledger.json is kept           (default ./data)
//	SLOW_DELAY   how long tok_slow takes             (default 5s)
//	ADMIN_KEY    shared secret for admin endpoints   (unset: they refuse everything)
//	LOG_LEVEL    debug | info | warn | error         (default info)
//
// Run with -healthcheck to probe a running instance and exit 0 or 1.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gocart/payment-service/internal/gateway"
	"gocart/payment-service/internal/httpapi"
	"gocart/payment-service/internal/ledger"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on this machine and exit 0 if healthy, 1 if not")
	flag.Parse()

	port := env("PORT", "8082")
	if *healthcheck {
		os.Exit(probe(port))
	}

	log := newLogger(env("LOG_LEVEL", "info"))
	if err := run(log, port); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, port string) error {
	slow, err := time.ParseDuration(env("SLOW_DELAY", "5s"))
	if err != nil {
		return errors.New("SLOW_DELAY is not a duration such as 5s: " + err.Error())
	}
	store, err := ledger.Open(env("DATA_DIR", "./data"), gateway.New(slow))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpapi.New(store, log, apiOptions(log)...).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second, // longer than SLOW_DELAY, so tok_slow can finish
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := make(chan error, 1)
	go func() {
		log.Info("payment-service listening", "addr", srv.Addr, "version", version)
		failed <- srv.ListenAndServe()
	}()

	select {
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down; finishing in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// apiOptions reads the admin key. Without one the admin endpoints refuse every
// request, so forgetting to set it fails closed rather than open.
func apiOptions(log *slog.Logger) []httpapi.Option {
	key := env("ADMIN_KEY", "")
	if key == "" {
		log.Warn("ADMIN_KEY is not set: admin endpoints will refuse every request")
	}
	return []httpapi.Option{httpapi.WithAdminKey(key), httpapi.WithVersion(version)}
}

func probe(port string) int {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})).
		With("service", "payment-service")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
