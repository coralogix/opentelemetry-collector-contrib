// Copyright  The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zabbixreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/zabbixreceiver"

import (
	"context"
	// "errors"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

// var errClientNotInit = errors.New("client not initialized")

// // Names of metrics in message_stats
// const (
// 	deliverStat        = "deliver"
// 	publishStat        = "publish"
// 	ackStat            = "ack"
// 	dropUnroutableStat = "drop_unroutable"
// )

// // Metrics to gather from queue message_stats structure
// var messageStatMetrics = []string{
// 	deliverStat,
// 	publishStat,
// 	ackStat,
// 	dropUnroutableStat,
// }

// zabbixScraper handles scraping of RabbitMQ metrics
type zabbixScraper struct {
	// client   client
	logger   *zap.Logger
	cfg      *Config
	settings component.TelemetrySettings
	// mb       *metadata.MetricsBuilder
}

// newScraper creates a new scraper
func newScraper(logger *zap.Logger, cfg *Config, settings component.ReceiverCreateSettings) *zabbixScraper {
	return &zabbixScraper{
		logger:   logger,
		cfg:      cfg,
		settings: settings.TelemetrySettings,
		// mb:       metadata.NewMetricsBuilder(cfg.Metrics, settings.BuildInfo),
	}
}

// start starts the scraper by creating a new HTTP Client on the scraper
func (r *zabbixScraper) start(ctx context.Context, host component.Host) (err error) {
	// r.client, err = newClient(r.cfg, host, r.settings, r.logger)
	return
}

// scrape collects metrics from the Zabbix API
func (r *zabbixScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	now := pcommon.NewTimestampFromTime(time.Now())

	r.logger.Debug("Scraping stuff")
	r.collectQueue(now)
	dummy := pmetric.NewMetrics()
	// TODO: Implement metrics collection
	return dummy, nil
}

// collectQueue collects metrics
func (r *zabbixScraper) collectQueue(now pcommon.Timestamp) {
	r.logger.Debug("running collectQueue")
	// TODO: Implement metrics collection
}
