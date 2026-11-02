package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.1.0"

func env(key, fallback string) string {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return s
	}
	return fallback
}

func main() {
	health := flag.Bool("healthcheck", false, "check the local HTTP server")
	flag.Parse()
	if *health {
		c := &http.Client{Timeout: 3 * time.Second}
		r, err := c.Get("http://127.0.0.1:" + env("PORT", "8080") + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	transport := env("MCP_TRANSPORT", "http")
	if transport != "http" && transport != "stdio" {
		return fmt.Errorf("MCP_TRANSPORT must be http or stdio")
	}
	token := env("MCP_AUTH_TOKEN", "")
	if transport == "http" && token == "" {
		return fmt.Errorf("MCP_AUTH_TOKEN is required for HTTP")
	}
	configToken := env("CONFIG_TOKEN", token)
	shutdown, err := initTracing(context.Background())
	if err != nil {
		return fmt.Errorf("invalid tracing configuration")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}()
	st, err := openStore(env("DATA_DIR", "/data"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	a, err := newApp(st)
	if err != nil {
		return err
	}
	if !a.current().ready() {
		fmt.Fprintln(os.Stderr, "warning: Tibi account not set up yet; account tools fail until the borrower number and PIN are set")
	}
	s := a.server
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if transport == "stdio" {
		return s.Run(ctx, &mcp.StdioTransport{})
	}
	h := newHTTPHandler(s, token, func(mux *http.ServeMux) { a.registerConfigAPI(mux, configToken) })
	srv := &http.Server{Addr: ":" + env("PORT", "8080"), Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(stop)
	}()
	err = srv.ListenAndServe()
	cancel()
	<-done
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server failed")
	}
	return nil
}
