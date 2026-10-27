package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Shpuntyara/wms-platform/internal/api"
	"github.com/Shpuntyara/wms-platform/internal/auth"
	"github.com/Shpuntyara/wms-platform/internal/clientmodules/dispatchquality"
	"github.com/Shpuntyara/wms-platform/internal/jobs"
	"github.com/Shpuntyara/wms-platform/internal/modules"
	"github.com/Shpuntyara/wms-platform/internal/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("backend stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn, issuer, audience := os.Getenv("WMS_DATABASE_URL"), os.Getenv("WMS_OIDC_ISSUER"), os.Getenv("WMS_OIDC_AUDIENCE")
	if dsn == "" || issuer == "" || audience == "" {
		return errors.New("required configuration: WMS_DATABASE_URL, WMS_OIDC_ISSUER, WMS_OIDC_AUDIENCE")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	if err := validateDatabaseTLS(&config.ConnConfig.Config); err != nil {
		return err
	}
	config.MaxConns = 20
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute
	if value := os.Getenv("WMS_DB_MAX_CONNS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return errors.New("invalid WMS_DB_MAX_CONNS")
		}
		config.MaxConns = int32(n)
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(startup, config)
	if err != nil {
		return errors.New("database initialization failed")
	}
	defer pool.Close()
	store, err := postgres.New(startup, pool)
	if err != nil {
		return errors.New("database preflight failed; verify role, migrations and backend grants")
	}
	installed, err := store.InstalledModules(startup)
	if err != nil {
		return errors.New("installed module metadata is invalid")
	}
	var required []string
	for value := range strings.SplitSeq(os.Getenv("WMS_REQUIRED_MODULES"), ",") {
		if id := strings.TrimSpace(value); id != "" {
			required = append(required, id)
		}
	}
	registry, err := modules.NewRegistry(installed, []modules.Module{dispatchquality.New()}, required)
	if err != nil {
		return fmt.Errorf("module compatibility: %w", err)
	}
	if err := store.ValidateModules(startup, registry); err != nil {
		return errors.New("required module data projection is unavailable")
	}
	verifier, err := auth.NewOIDC(startup, issuer, audience, nil)
	if err != nil {
		return errors.New("oidc initialization failed; verify HTTPS issuer, audience and discovery")
	}
	handler, err := api.New(api.Config{Backend: store, Auth: verifier, Modules: registry, Logger: logger})
	if err != nil {
		return err
	}
	relayKey, err := base64.StdEncoding.DecodeString(os.Getenv("WMS_OUTBOX_HMAC_KEY"))
	if err != nil {
		return errors.New("invalid WMS_OUTBOX_HMAC_KEY: expected base64")
	}
	sender, err := jobs.NewSender(os.Getenv("WMS_OUTBOX_URL"), relayKey, os.Getenv("WMS_OUTBOX_ALLOW_PRIVATE") == "true")
	if err != nil {
		return err
	}
	workerCtx, stopWorkers := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	worker := jobs.Worker{Store: store, Sender: sender, Logger: logger}
	go func() { defer close(workerDone); worker.Run(workerCtx) }()
	defer func() { stopWorkers(); <-workerDone }()
	address := os.Getenv("WMS_HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return errors.New("invalid WMS_HTTP_ADDR")
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	finished := make(chan error, 1)
	go func() { finished <- server.ListenAndServe() }()
	logger.Info("backend started", "address", address, "core_version", modules.CoreVersion, "sdk_version", modules.SDKVersion)
	select {
	case err := <-finished:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("http listener failed")
		}
		return nil
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("http shutdown timed out")
		}
		return nil
	}
}

func validateDatabaseTLS(config *pgconn.Config) error {
	endpoints := []*pgconn.FallbackConfig{{Host: config.Host, TLSConfig: config.TLSConfig}}
	endpoints = append(endpoints, config.Fallbacks...)
	for _, endpoint := range endpoints {
		host := endpoint.Host
		local := host == "127.0.0.1" || host == "::1" || host == "localhost" || strings.HasPrefix(host, "/")
		if !local && (endpoint.TLSConfig == nil || endpoint.TLSConfig.InsecureSkipVerify) {
			return errors.New("every remote database endpoint requires verified TLS")
		}
	}
	return nil
}
