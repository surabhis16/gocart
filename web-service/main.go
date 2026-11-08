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
// Sign-in (all required except the two marked optional):
//
//	PUBLIC_URL           where people reach this gateway, e.g. https://shop.example.com
//	OIDC_ISSUER          the identity provider, as the browser reaches it
//	OIDC_BACKCHANNEL_URL optional: where this server reaches it instead (same path, other host)
//	OIDC_CLIENT_ID       this app's id at the provider      (default gocart-web)
//	OIDC_CLIENT_SECRET   this app's secret at the provider
//	SESSION_SECRET       signs the session cookie (32+ characters)
//	IDENTITY_SECRET      signs what is sent to the services; they hold the same value (32+)
//	ADMIN_KEY            the services' admin key; sent for administrators only
//	ADMIN_EMAILS         optional: comma-separated emails that are administrators
//	SIGNIN_HINT          optional: a line shown on the landing page, such as demo accounts
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
	"strings"
	"syscall"
	"time"

	"gocart/web-service/internal/httpapi"
	"gocart/web-service/internal/oidc"
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
	auth, err := authFromEnv()
	if err != nil {
		return err
	}
	api, err := httpapi.New(up, ui.Files(), log, httpapi.WithVersion(version), httpapi.WithAuth(auth))
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

// authFromEnv reads the sign-in settings. Missing ones are an error: a gateway
// that quietly started without sign-in would be one that lets everyone in.
func authFromEnv() (httpapi.Auth, error) {
	public := strings.TrimRight(env("PUBLIC_URL", ""), "/")
	if public == "" {
		return httpapi.Auth{}, errors.New("PUBLIC_URL is required (for example http://localhost:3000)")
	}
	p, err := oidc.New(oidc.Config{
		Issuer:         env("OIDC_ISSUER", ""),
		BackchannelURL: env("OIDC_BACKCHANNEL_URL", ""),
		ClientID:       env("OIDC_CLIENT_ID", "gocart-web"),
		ClientSecret:   env("OIDC_CLIENT_SECRET", ""),
		RedirectURL:    public + "/auth/callback",
	}, nil)
	if err != nil {
		return httpapi.Auth{}, err
	}
	var admins []string
	for _, e := range strings.Split(env("ADMIN_EMAILS", ""), ",") {
		if e = strings.TrimSpace(e); e != "" {
			admins = append(admins, e)
		}
	}
	return httpapi.Auth{
		OIDC:           p,
		SessionSecret:  []byte(env("SESSION_SECRET", "")),
		IdentitySecret: []byte(env("IDENTITY_SECRET", "")),
		AdminKey:       env("ADMIN_KEY", ""),
		AdminEmails:    admins,
		SignInHint:     env("SIGNIN_HINT", ""),
		PublicURL:      public,
	}, nil
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
