// Command web-service is GoCart's front door: it serves the web app and
// forwards its API calls to the four services.
//
// Configuration comes from the environment:
//
//	PORT         listen port                         (default 3000)
//	PRODUCT_URL  base URL of the Product service     (default http://localhost:8080)
//	ORDER_URL    base URL of the Order service       (default http://localhost:8081)
//	PAYMENT_URL  base URL of the Payment service     (default http://localhost:8082)
//	CART_URL     base URL of the Cart service        (default http://localhost:8083)
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

	"gocart/web-service/internal/httpapi"
	"gocart/web-service/internal/ui"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on this machine and exit 0 if healthy, 1 if not")
	flag.Parse()

	port := env("PORT", "3000")
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
	up := httpapi.Upstreams{
		Product: env("PRODUCT_URL", "http://localhost:8080"),
		Order:   env("ORDER_URL", "http://localhost:8081"),
		Payment: env("PAYMENT_URL", "http://localhost:8082"),
		Cart:    env("CART_URL", "http://localhost:8083"),
	}
	api, err := httpapi.New(up, ui.Files(), log, httpapi.WithVersion(version))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      40 * time.Second, // checkout waits for the Cart service, which waits for the Order service, which waits for two more
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := make(chan error, 1)
	go func() {
		log.Info("web-service listening", "addr", srv.Addr, "version", version,
			"product_url", up.Product, "order_url", up.Order, "payment_url", up.Payment, "cart_url", up.Cart)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
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
		With("service", "web-service")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
