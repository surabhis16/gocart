// Command cart-service runs the GoCart Cart bounded context: what a customer
// intends to buy, and the checkout that turns it into an order.
//
// Configuration comes from the environment:
//
//	PORT         listen port                         (default 8083)
//	PRODUCT_URL  base URL of the Product service     (default http://localhost:8080)
//	ORDER_URL    base URL of the Order service       (default http://localhost:8081)
//	DATA_DIR     where carts.json is kept            (default ./data)
//	IDENTITY_SECRET  signs the gateway's identity assertion (32+ characters)
//	REQUIRE_IDENTITY "true": every cart route refuses callers that are not signed in
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

	"gocart/cart-service/internal/cart"
	"gocart/cart-service/internal/httpapi"
	"gocart/cart-service/internal/identity"
	"gocart/cart-service/internal/orderclient"
	"gocart/cart-service/internal/productclient"
)

// version is stamped at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz on this machine and exit 0 if healthy, 1 if not")
	flag.Parse()

	port := env("PORT", "8083")
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
	store, err := cart.Open(env("DATA_DIR", "./data"))
	if err != nil {
		return err
	}
	productURL, orderURL := env("PRODUCT_URL", "http://localhost:8080"), env("ORDER_URL", "http://localhost:8081")

	secret := []byte(env("IDENTITY_SECRET", ""))
	switch {
	case len(secret) == 0:
		log.Warn("IDENTITY_SECRET is not set: signed identities from the gateway will be refused, and every cart is open to anyone who can reach this port")
	case len(secret) < identity.MinSecretLen:
		return errors.New("IDENTITY_SECRET must be at least 32 characters")
	}
	require := env("REQUIRE_IDENTITY", "") == "true"
	if require && len(secret) == 0 {
		return errors.New("REQUIRE_IDENTITY needs IDENTITY_SECRET")
	}

	api := httpapi.New(store, productclient.New(productURL, nil), orderclient.New(orderURL, nil), log,
		httpapi.WithIdentity(secret, require), httpapi.WithVersion(version))
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second, // checkout waits for the Order service, which waits for two others
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := make(chan error, 1)
	go func() {
		log.Info("cart-service listening", "addr", srv.Addr, "version", version,
			"product_url", productURL, "order_url", orderURL)
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
		With("service", "cart-service")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
