// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package sqlqueryreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/sqlqueryreceiver"

import (
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/sqlquery"
)

type Config struct {
	Config sqlquery.Config `mapstructure:",squash"`
	// The maximumn number of open connections to the sql server. <= 0 means unlimited
	MaxOpenConn int `mapstructure:"max_open_conn"`
	// MaxIdleConn caps the idle connections the pool keeps between
	// collections (database/sql SetMaxIdleConns). Unset keeps the database/sql
	// default; 0 keeps none.
	MaxIdleConn *int `mapstructure:"max_idle_conn"`

	// prevent unkeyed literal initialization
	_ struct{}
}

// Validate rejects a negative max_idle_conn (DBM P6). The embedded
// sqlquery.Config is validated on its own.
func (c *Config) Validate() error {
	if c.MaxIdleConn != nil && *c.MaxIdleConn < 0 {
		return errors.New("max_idle_conn must be unset or >= 0")
	}
	return nil
}

func createDefaultConfig() component.Config {
	cfg := scraperhelper.NewDefaultControllerConfig()
	cfg.CollectionInterval = 10 * time.Second
	return &Config{
		Config: sqlquery.Config{
			ControllerConfig: cfg,
		},
	}
}
