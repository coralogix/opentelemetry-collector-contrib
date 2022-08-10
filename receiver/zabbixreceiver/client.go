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
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/cavaliercoder/go-zabbix"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

type client interface {
	GetHistories(ctx context.Context, unix_time_now int64) ([]zabbix.History, error)
	GetItems(ctx context.Context, item_ids []string) ([]zabbix.Item, error)
	GetHosts(ctx context.Context, host_ids []string) ([]zabbix.Host, error)
}

var _ client = (*zabbixClient)(nil)

type zabbixClient struct {
	session                 zabbix.Session
	scrape_interval_seconds int
	logger                  *zap.Logger
}

func newClient(cfg *Config, settings component.TelemetrySettings, logger *zap.Logger) (client, error) {

	endpoint := fmt.Sprintf("%s/api_jsonrpc.php", cfg.Endpoint)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	cache := zabbix.NewSessionFileCache().SetFilePath("./zabbix_session")
	session, err := zabbix.CreateClient(endpoint).
		WithCache(cache).
		WithHTTPClient(client).
		WithCredentials(cfg.Username, cfg.Password).
		Connect()
	if err != nil {
		return nil, fmt.Errorf("failed to create Zabbix session: %w", err)
	}

	version, err := session.GetVersion()

	if err != nil {
		return nil, fmt.Errorf("failed to get Zabbix version: %w", err)
	}
	// // TODO panics: panic: runtime error: invalid memory address or nil pointer dereference [recovered]
	// logger.Info("Connected to Zabbix API", zap.String("version", version))

	fmt.Printf("Connected to Zabbix API v%s\n", version)

	return &zabbixClient{
		session:                 *session,
		scrape_interval_seconds: int(cfg.CollectionInterval.Seconds()),
		logger:                  logger,
	}, nil
}

func (c *zabbixClient) get_history(from_seconds float64, till_seconds float64, history_type int) ([]zabbix.History, error) {
	params := zabbix.HistoryGetParams{
		GetParameters: zabbix.GetParameters{},
		History:       int(history_type),
		TimeFrom:      from_seconds,
		TimeTill:      till_seconds,
	}

	res, err := c.session.GetHistories(params)

	if err != nil {
		c.logger.Debug("failed to get histories", zap.Error(err))
	}

	return res, err

}

// GetHistories implements client
func (c *zabbixClient) GetHistories(ctx context.Context, unix_time_now int64) ([]zabbix.History, error) {

	// TODO - use unix_time_now to get the history from the past,
	//   revisit it if usine scrape_interval_seconds * 2 is not enough
	from := unix_time_now - int64(c.scrape_interval_seconds*2)

	// TODO understand why this is needed - if we need to scrape them one by one
	//  and what each of them is about
	history_types := []int{0, 1, 2, 3, 4, 5}

	var res_histories []zabbix.History
	var errs []error
	for _, x := range history_types {
		res, err := c.get_history(float64(from), float64(unix_time_now), x)
		res_histories = append(res_histories, res...)
		errs = append(errs, err)
	}

	if errs != nil {
		c.logger.Debug("failed to get histories")
	}

	// TODO what to return in error channel here?
	return res_histories, nil
}

// GetHosts implements client
func (c *zabbixClient) GetHosts(ctx context.Context, host_ids []string) ([]zabbix.Host, error) {
	params := zabbix.HostGetParams{
		GetParameters: zabbix.GetParameters{},
		HostIDs:       host_ids,
	}
	res, err := c.session.GetHosts(params)

	if err != nil {
		c.logger.Debug("failed to get hosts", zap.Error(err))
	}

	return res, err
}

// GetItems implements client
func (c *zabbixClient) GetItems(ctx context.Context, item_ids []string) ([]zabbix.Item, error) {
	params := zabbix.ItemGetParams{
		GetParameters: zabbix.GetParameters{},
		ItemIDs:       item_ids,
	}

	res, err := c.session.GetItems(params)

	if err != nil {
		c.logger.Debug("failed to get items", zap.Error(err))
	}

	return res, err
}
