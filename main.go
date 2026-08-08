// Command u-l10n is the in-house localization service: the source of truth for
// mobile translation keys, the byte-exact export engine, and (later) the
// over-the-air string delivery endpoint.
//
// See docs/ and the Confluence guide "u-l10n Backend Guide" for design detail.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli"

	ctxutil "github.com/yougroupteam/u-common-util/context"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/config"
)

const serviceName = "u-l10n"

var log = ulog.GetLogger(serviceName)

// Service is the fully-wired application graph, built by Wire.
type Service struct {
	Config  *config.Config
	Handler http.Handler
}

func main() {
	ctx := ctxutil.NewContext(ctxutil.WithStan("main-process"))

	service, err := injectService(ctx)
	if err != nil {
		log.Fatale(ctx, "failed to initialise service", err)
	}

	app := cli.NewApp()
	app.Name = serviceName
	app.Usage = "In-house localization service by YouTech"
	app.Action = func(*cli.Context) error {
		return serve(ctx, service)
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatale(ctx, "service exited with error", err)
	}
}

// serve runs the HTTP server until SIGINT/SIGTERM, then drains in-flight
// requests within the configured shutdown timeout.
//
// The drain matters in Kubernetes: on rollout the pod receives SIGTERM at the
// same moment it is removed from the Service endpoints, and requests already in
// flight would otherwise be severed mid-response.
func serve(ctx context.Context, service *Service) error {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", service.Config.HTTPPort),
		Handler: service.Handler,
	}

	// Buffered so a signal arriving before we select is not dropped.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		log.Infow(ctx, "http server listening",
			"port", service.Config.HTTPPort, "env", service.Config.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server failed: %w", err)

	case sig := <-shutdown:
		log.Infow(ctx, "shutdown signal received, draining", "signal", sig.String())

		drainCtx, cancel := context.WithTimeout(ctx, service.Config.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(drainCtx); err != nil {
			// Drain deadline exceeded: report it rather than pretending the
			// shutdown was clean, then let the process exit.
			return fmt.Errorf("graceful shutdown incomplete after %s: %w",
				service.Config.ShutdownTimeout, err)
		}

		log.Infow(ctx, "shutdown complete")
		return nil
	}
}
