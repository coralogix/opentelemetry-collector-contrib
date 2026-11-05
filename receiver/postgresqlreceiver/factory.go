// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// topQueryCache is the top_query delta cache: one entry per statement, holding
// the statement's cumulative counters from the previous scrape. It counts
// evictions so the scraper can report them, because an evicted statement is
// silently re-seeded (reported as 0) on its next scrape.
type topQueryCache struct {
	entries *lru.Cache[string, statementCounters]
	// evictions is written by the LRU eviction callback and drained once per
	// scrape. Scrapes are sequential today; the atomic keeps the counter correct
	// if the cache is ever read concurrently, at negligible cost.
	evictions atomic.Int64
}

// newCache creates a top_query delta cache holding up to size statements.
// If the size is less or equal to 0, it will be set to 1.
func newCache(size int) *topQueryCache {
	if size <= 0 {
		size = 1
	}
	c := &topQueryCache{}
	// lru will only return error when the size is less than 0
	c.entries, _ = lru.NewWithEvict[string, statementCounters](size, func(string, statementCounters) {
		c.evictions.Add(1)
	})
	return c
}

func (c *topQueryCache) get(key string) (statementCounters, bool) {
	return c.entries.Get(key)
}

func (c *topQueryCache) add(key string, counters statementCounters) {
	c.entries.Add(key, counters)
}

func (c *topQueryCache) len() int {
	return c.entries.Len()
}

// takeEvictions returns the number of evictions since the previous call.
func (c *topQueryCache) takeEvictions() int64 {
	return c.evictions.Swap(0)
}

func newTTLCache[v any](size int, ttl time.Duration) *expirable.LRU[string, v] {
	if size <= 0 {
		size = 1
	}
	cache := expirable.NewLRU[string, v](size, nil, ttl)
	return cache
}

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		metadata.Type,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, metadata.MetricsStability),
		receiver.WithLogs(createLogsReceiver, metadata.LogsStability),
	)
}

func createDefaultConfig() component.Config {
	cfg := scraperhelper.NewDefaultControllerConfig()
	cfg.CollectionInterval = 10 * time.Second
	cfg.Timeout = time.Minute

	return &Config{
		ControllerConfig: cfg,
		ConnectDatabase:  "postgres",
		AddrConfig: confignet.AddrConfig{
			Endpoint:  "localhost:5432",
			Transport: confignet.TransportTypeTCP,
		},
		ClientConfig: configtls.ClientConfig{
			Insecure:           false,
			InsecureSkipVerify: true,
		},
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		LogsBuilderConfig:    metadata.DefaultLogsBuilderConfig(),
		QuerySampleCollection: QuerySampleCollection{
			MaxRowsPerQuery: 1000,
		},
		TopQueryCollection: TopQueryCollection{
			CollectionInterval:     time.Minute,
			TopNQuery:              200,
			MaxRowsPerQuery:        1000,
			MaxExplainEachInterval: 1000,
			QueryPlanCacheSize:     1000,
			QueryPlanCacheTTL:      time.Hour,
		},
	}
}

func createMetricsReceiver(
	_ context.Context,
	params receiver.Settings,
	rConf component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	cfg := rConf.(*Config)

	clientFactory := newClientFactory(cfg)

	ns, err := newPostgreSQLScraper(params, cfg, clientFactory, newCache(1), newTTLCache[string](1, time.Second))
	if err != nil {
		return nil, err
	}
	s, err := scraper.NewMetrics(ns.scrape, scraper.WithStart(ns.start), scraper.WithShutdown(ns.shutdown))
	if err != nil {
		return nil, err
	}

	return scraperhelper.NewMetricsController(
		&cfg.ControllerConfig, params, consumer,
		scraperhelper.AddMetricsScraper(metadata.Type, s),
	)
}

// createLogsReceiver create a logs receiver based on provided config.
func createLogsReceiver(
	_ context.Context,
	params receiver.Settings,
	receiverCfg component.Config,
	logsConsumer consumer.Logs,
) (receiver.Logs, error) {
	cfg := receiverCfg.(*Config)

	clientFactory := newClientFactory(cfg)

	opts := make([]scraperhelper.ControllerOption, 0)

	if cfg.LogsBuilderConfig.Events.DbServerQuerySample.Enabled {
		// query sample collection does not need cache, but we do not want to make it
		// nil, so create one size 1 cache as a placeholder.
		ns, err := newPostgreSQLScraper(params, cfg, clientFactory, newCache(1), newTTLCache[string](1, time.Second))
		if err != nil {
			return nil, err
		}
		s, err := scraper.NewLogs(func(ctx context.Context) (plog.Logs, error) {
			return ns.scrapeQuerySamples(ctx, cfg.QuerySampleCollection.MaxRowsPerQuery)
		}, scraper.WithStart(ns.start), scraper.WithShutdown(ns.shutdown))
		if err != nil {
			return nil, err
		}
		opt := scraperhelper.AddFactoryWithConfig(
			scraper.NewFactory(metadata.Type, nil,
				scraper.WithLogs(func(context.Context, scraper.Settings, component.Config) (scraper.Logs, error) {
					return s, nil
				}, component.StabilityLevelAlpha)), nil,
		)
		opts = append(opts, opt)
	}

	if cfg.LogsBuilderConfig.Events.DbServerTopQuery.Enabled {
		ns, err := newTopQueryScraper(params, cfg, clientFactory)
		if err != nil {
			return nil, err
		}
		s, err := scraper.NewLogs(func(ctx context.Context) (plog.Logs, error) {
			return ns.scrapeTopQuery(ctx, cfg.TopQueryCollection.MaxRowsPerQuery, cfg.TopQueryCollection.TopNQuery, cfg.TopQueryCollection.MaxExplainEachInterval, cfg.TopQueryCollection.CollectionInterval)
		}, scraper.WithStart(ns.start), scraper.WithShutdown(ns.shutdown))
		if err != nil {
			return nil, err
		}
		opt := scraperhelper.AddFactoryWithConfig(
			scraper.NewFactory(metadata.Type, nil,
				scraper.WithLogs(func(context.Context, scraper.Settings, component.Config) (scraper.Logs, error) {
					return s, nil
				}, component.StabilityLevelAlpha)), nil,
		)
		opts = append(opts, opt)
	}

	return scraperhelper.NewLogsController(
		&cfg.ControllerConfig, params, logsConsumer, opts...,
	)
}

func newTopQueryScraper(params receiver.Settings, cfg *Config, clientFactory postgreSQLClientFactory) (*postgreSQLScraper, error) {
	// Deltas are calculated for every fetched statement before selecting the top queries.
	// The cache holds one entry per statement, with room for twice the fetched rows so
	// statements that drop out of and back into the fetched set keep their counters.
	return newPostgreSQLScraper(params, cfg, clientFactory,
		newCache(int(cfg.TopQueryCollection.MaxRowsPerQuery*2)),
		newTTLCache[string](cfg.TopQueryCollection.QueryPlanCacheSize, cfg.TopQueryCollection.QueryPlanCacheTTL))
}
