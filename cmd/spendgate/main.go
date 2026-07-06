// Command spendgate is the self-hosted LLM cost-attribution gateway.
//
// Subcommands:
//
//	spendgate serve                 start the HTTP gateway + batch metering writer
//	spendgate migrate               apply database migrations
//	spendgate create-tenant <name>  create a tenant and print its API key once
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sh-rest/spendgate/internal/auth"
	"github.com/sh-rest/spendgate/internal/budget"
	"github.com/sh-rest/spendgate/internal/config"
	"github.com/sh-rest/spendgate/internal/dashboard"
	"github.com/sh-rest/spendgate/internal/meter"
	"github.com/sh-rest/spendgate/internal/prices"
	"github.com/sh-rest/spendgate/internal/proxy"
	"github.com/sh-rest/spendgate/internal/server"
	"github.com/sh-rest/spendgate/internal/store"
	"github.com/sh-rest/spendgate/internal/tenant"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// Load .env (provider keys) before reading config; real env wins.
	if err := config.LoadDotenv(".env"); err != nil {
		log.Printf("warning: reading .env: %v", err)
	}
	cfg := config.Load()
	ctx := context.Background()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, cfg)
	case "migrate":
		err = migrate(ctx, cfg)
	case "create-tenant":
		err = createTenant(ctx, cfg, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("spendgate %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: spendgate <serve|migrate|create-tenant> [args]")
}

func migrate(ctx context.Context, cfg config.Config) error {
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	log.Println("migrations applied")
	return nil
}

func createTenant(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) < 1 || args[0] == "" {
		return errors.New("usage: spendgate create-tenant <name>")
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	key, err := tenant.Create(ctx, st.Pool, args[0])
	if err != nil {
		return err
	}
	fmt.Printf("tenant %q created.\nAPI key (shown once, store it now):\n\n  %s\n\n", args[0], key)
	return nil
}

func serve(ctx context.Context, cfg config.Config) error {
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	priceList, err := prices.Load("prices.yaml")
	if err != nil {
		return err
	}
	if err := prices.Seed(ctx, st.Pool, priceList); err != nil {
		return err
	}
	log.Printf("seeded %d model prices", len(priceList))
	priceTable := prices.BuildTable(priceList) // cache in memory (DESIGN.md)

	// Async metering writer.
	writer := meter.New(meter.PGSink{Pool: st.Pool}, meter.DefaultBatchSize, meter.DefaultInterval)
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter() // safe to call twice; guarantees no context leak on error paths
	go writer.Run(writerCtx)

	bud, err := budget.New(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer bud.Close()

	authr := auth.New(tenant.LookupByHash(st.Pool), 30*time.Second)
	px := proxy.New(writer, priceTable, nil, bud,
		proxy.Provider{Name: "openai", Prefix: "/openai", BaseURL: cfg.OpenAIBaseURL, APIKey: cfg.OpenAIKey},
		proxy.Provider{Name: "anthropic", Prefix: "/anthropic", BaseURL: cfg.AnthropicBaseURL, APIKey: cfg.AnthropicKey},
	)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: server.New(st, bud, authr, px),
	}

	// Dashboard runs on its own listener (DASHBOARD_ADDR, empty disables) so
	// its localhost-only default doesn't constrain the proxy port.
	var dashSrv *http.Server
	if cfg.DashboardAddr != "" {
		dashSrv = &http.Server{
			Addr:    cfg.DashboardAddr,
			Handler: dashboard.Handler(st.Pool, 0),
		}
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("spendgate serving on :%s", cfg.Port)
		errCh <- srv.ListenAndServe()
	}()
	if dashSrv != nil {
		go func() {
			log.Printf("dashboard serving on %s", cfg.DashboardAddr)
			if err := dashSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("dashboard server: %v", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-sigCtx.Done():
		log.Println("shutdown: draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	if dashSrv != nil {
		if err := dashSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("dashboard shutdown: %v", err)
		}
	}
	// Flush metering buffer before exit.
	stopWriter()
	writer.Wait()
	log.Println("shutdown complete")
	return nil
}
