// Package config holds the service-level configuration for u-l10n.
//
// Configuration is supplied entirely through environment variables, following
// the house convention: `configstruct` names the variable, `configdefault`
// supplies the fallback. In Kubernetes these arrive from a ConfigMap via
// envFrom; locally they come from the shell (see Makefile `run-local`).
//
// Infrastructure config (database, APM, secrets) is owned by the shared
// u-common-components packages and is not repeated here.
package config

import (
	"fmt"
	"time"

	commonconfig "github.com/yougroupteam/u-common-components/config"
)

// Config is the u-l10n service configuration.
type Config struct {
	// Env identifies the deployment environment (local, dev, sit, prod).
	Env string `configstruct:"SERVICECONFIG_ENV" configdefault:"local"`

	// HTTPPort is the port the HTTP server listens on.
	HTTPPort int `configstruct:"SERVICECONFIG_HTTP_PORT" configdefault:"8080"`

	// RequestTimeout bounds how long any single HTTP request may run. The
	// deadline propagates through context into the database driver, so a
	// timed-out request also cancels its in-flight query.
	RequestTimeout time.Duration `configstruct:"SERVICECONFIG_REQUEST_TIMEOUT" configdefault:"60s"`

	// ShutdownTimeout bounds how long we wait for in-flight requests to
	// finish after SIGTERM before forcing the process down. Keep it below
	// the Kubernetes terminationGracePeriodSeconds.
	ShutdownTimeout time.Duration `configstruct:"SERVICECONFIG_SHUTDOWN_TIMEOUT" configdefault:"15s"`
}

// ProvideConfig loads and validates the service configuration. It fails fast:
// a service that starts with invalid config only defers the outage.
func ProvideConfig(configStore commonconfig.ConfigStore) (*Config, error) {
	cnf := &Config{}
	if err := configStore.GetConfig(cnf); err != nil {
		return nil, fmt.Errorf("load service config: %w", err)
	}

	if cnf.HTTPPort <= 0 || cnf.HTTPPort > 65535 {
		return nil, fmt.Errorf("invalid SERVICECONFIG_HTTP_PORT %d", cnf.HTTPPort)
	}
	if cnf.RequestTimeout <= 0 {
		return nil, fmt.Errorf("invalid SERVICECONFIG_REQUEST_TIMEOUT %s", cnf.RequestTimeout)
	}
	if cnf.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("invalid SERVICECONFIG_SHUTDOWN_TIMEOUT %s", cnf.ShutdownTimeout)
	}

	return cnf, nil
}
