// Command order-service runs the GoCart Order bounded context.
//
// Configuration comes from the environment:
//
//	PORT                 listen port                               (default 8081)
//	PRODUCT_URL          base URL of the Product service           (default http://localhost:8080)
//	PAYMENT_URL          base URL of the Payment service           (default http://localhost:8082)
//	PAYMENT_TTL          how long stock is held awaiting payment   (default 15m)
//	EVENT_STORE          file | cassandra                          (default file)
//	DATA_DIR             where events.log is kept (file store)     (default ./data)
//	CASSANDRA_HOSTS      comma-separated contact points            (cassandra store)
//	CASSANDRA_KEYSPACE   keyspace holding order_events             (default gocart)
//	RECONCILE_INTERVAL   how often unfinished orders are retried   (default 5s)
//	SYNC_INTERVAL        how often this replica reads other replicas' changes  (default 2s)
//	RESYNC_INTERVAL      how often it also reloads the whole log   (default 5m)
//	ADMIN_KEY    shared secret for admin endpoints   (unset: they refuse everything)
//	LOG_LEVEL            debug | info | warn | error               (default info)
//
// The cassandra store is compiled in only with `-tags cassandra`.
//
// Run with -healthcheck to probe a running instance and exit 0 or 1. The
// container image is built FROM scratch, so there is no curl to do it for us.
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

	"gocart/order-service/internal/eventstore"
	"gocart/order-service/internal/httpapi"
	"gocart/order-service/internal/paymentclient"
	"gocart/order-service/internal/productclient"
	"gocart/order-service/internal/service"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on this machine and exit 0 if healthy, 1 if not")
	flag.Parse()

	port := env("PORT", "8081")
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
	interval, err := time.ParseDuration(env("RECONCILE_INTERVAL", "5s"))
	if err != nil || interval < time.Second {
		return errors.New("RECONCILE_INTERVAL must be a duration of at least 1s, for example 5s")
	}

	syncEvery, err := time.ParseDuration(env("SYNC_INTERVAL", "2s"))
	if err != nil || syncEvery < 100*time.Millisecond {
		return errors.New("SYNC_INTERVAL must be a duration of at least 100ms, for example 2s")
	}
	resync, err := time.ParseDuration(env("RESYNC_INTERVAL", "5m"))
	if err != nil || resync < time.Second {
		return errors.New("RESYNC_INTERVAL must be a duration of at least 1s, for example 5m")
	}

	store, err := openStore(log)
	if err != nil {
		return err
	}
	defer store.Close()

	ttl, err := time.ParseDuration(env("PAYMENT_TTL", "15m"))
	if err != nil || ttl < time.Second {
		return errors.New("PAYMENT_TTL must be a duration of at least 1s, for example 15m")
	}

	products := productclient.New(env("PRODUCT_URL", "http://localhost:8080"))
	payments := paymentclient.New(env("PAYMENT_URL", "http://localhost:8082"))
	svc, err := service.New(store, products, payments, log, service.WithPaymentTTL(ttl), service.WithResync(resync))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunReconciler(ctx, interval)
	go svc.RunSync(ctx, syncEvery)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           httpapi.New(svc, log, apiOptions(log)...).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second, // placing an order waits on the Product and Payment services
		IdleTimeout:       60 * time.Second,
	}

	failed := make(chan error, 1)
	go func() {
		log.Info("order-service listening", "addr", srv.Addr, "version", version,
			"event_store", env("EVENT_STORE", "file"), "product_url", env("PRODUCT_URL", "http://localhost:8080"),
			"payment_url", env("PAYMENT_URL", "http://localhost:8082"), "payment_ttl", ttl.String())
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

func openStore(log *slog.Logger) (eventstore.Store, error) {
	switch kind := env("EVENT_STORE", "file"); kind {
	case "file":
		s, err := eventstore.OpenFile(env("DATA_DIR", "./data"))
		if err != nil {
			return nil, err
		}
		if s.RepairedBytes > 0 {
			log.Warn("discarded a partial write left by a crash; it was never acknowledged", "bytes", s.RepairedBytes)
		}
		return s, nil
	case "cassandra":
		return openCassandra(log)
	default:
		return nil, errors.New(`EVENT_STORE must be "file" or "cassandra", got "` + kind + `"`)
	}
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
		With("service", "order-service")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
