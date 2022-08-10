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
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	// "go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

var errClientNotInit = errors.New("client not initialized")

// zabbixScraper handles scraping of Zabbix metrics, and converting them to OpenTelemetry metrics
type zabbixScraper struct {
	client   client
	logger   *zap.Logger
	cfg      *Config
	settings component.TelemetrySettings
}

// newScraper creates a new scraper
func newScraper(logger *zap.Logger, cfg *Config, settings component.ReceiverCreateSettings) *zabbixScraper {
	return &zabbixScraper{
		logger:   logger,
		cfg:      cfg,
		settings: settings.TelemetrySettings,
	}
}

// start starts the scraper by creating a new zabbix client on the scraper
func (r *zabbixScraper) start(ctx context.Context, host component.Host) (err error) {
	r.client, err = newClient(r.cfg, r.settings, r.logger)
	return
}

// scrape collects metrics from the Zabbix API
func (r *zabbixScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	timeNow := time.Now()
	unixTimeNow := timeNow.Unix()
	// now := pcommon.NewTimestampFromTime(timeNow)

	// Validate we don't attempt to scrape without initializing the client
	if r.client == nil {
		return pmetric.NewMetrics(), errClientNotInit
	}

	// TODO all the logic goes here
	// Get queues for processing
	histories, err := r.client.GetHistories(ctx, unixTimeNow)

	// TODO Can/should we do it like this?
	if err != nil {
		return pmetric.NewMetrics(), err
	}

	// TODO do we need to filter unique values?
	item_ids := []string{}
	for _, history := range histories {
		item_ids = append(item_ids, fmt.Sprint(history.ItemID))
	}
	items, err := r.client.GetItems(ctx, item_ids)

	// TODO Can/should we do it like this?
	if err != nil {
		return pmetric.NewMetrics(), err
	}

	// TODO do we need to filter unique values?
	host_ids := []string{}
	for _, item := range items {
		host_ids = append(host_ids, fmt.Sprint(item.HostID))
	}
	hosts, err := r.client.GetHosts(ctx, host_ids)

	r.logger.Debug("Scraping metrics", zap.Any("hosts", hosts))

	// TODO Can/should we do it like this?
	if err != nil {
		return pmetric.NewMetrics(), err
	}

	// TODO - data to Metrics

	panic("not implemented")
}
