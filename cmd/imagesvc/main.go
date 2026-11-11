package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/itsvovovovova/image_service/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type service struct {
	db        *pgxpool.Pool
	processor string
	broker    string
	secret    []byte
	lease     time.Duration
	client    *http.Client
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && ctx.Err() == nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: imagesvc api|dispatch|worker|migrate|gc|gc-delete")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer db.Close()
	lease := 90 * time.Second
	if raw := os.Getenv("JOB_LEASE_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 5 || seconds > 600 {
			return errors.New("JOB_LEASE_SECONDS must be between 5 and 600")
		}
		lease = time.Duration(seconds) * time.Second
	}
	s := &service{db: db, broker: os.Getenv("AMQP_URL"), processor: os.Getenv("PROCESSOR_URL"), secret: []byte(os.Getenv("JWT_SECRET")), lease: lease,
		client: &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	switch args[0] {
	case "migrate":
		return migrate(ctx, db)
	case "gc", "gc-delete":
		if err := s.validateProcessor(); err != nil {
			return err
		}
		return s.collectOrphans(ctx, args[0] == "gc-delete")
	case "dispatch", "worker":
		stopMetrics := serveMetrics()
		defer stopMetrics()
		if s.broker == "" {
			return errors.New("AMQP_URL is required")
		}
		if args[0] == "dispatch" {
			return s.brokerLoop(ctx, s.dispatch)
		}
		if err := s.validateProcessor(); err != nil {
			return err
		}
		return s.brokerLoop(ctx, s.consume)
	case "api":
		if len(s.secret) < 32 {
			return errors.New("JWT_SECRET must be at least 32 bytes")
		}
		if err := s.validateProcessor(); err != nil {
			return err
		}
		return s.serve(ctx)
	default:
		return errors.New("unknown service mode")
	}
}

func (s *service) validateProcessor() error {
	u, err := url.Parse(s.processor)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid PROCESSOR_URL")
	}
	return nil
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(76182783)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS image_schema_migrations (name text PRIMARY KEY)"); err != nil {
		return err
	}
	files, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	for _, file := range files {
		var applied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM image_schema_migrations WHERE name=$1)", file.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrations.Files.ReadFile(file.Name())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql), pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("apply %s: %w", file.Name(), err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO image_schema_migrations(name) VALUES($1)", file.Name()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func serveMetrics() func() {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	server := &http.Server{Addr: ":9091", Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server stopped", "error", err)
		}
	}()
	return func() { server.Close() }
}
