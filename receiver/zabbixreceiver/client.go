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
	"net/http"
	"time"

	// "encoding/json"
	"fmt"
	// "io"
	// "net/http"

	"github.com/cavaliercoder/go-zabbix"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

type client interface {
	GetHistories(ctx context.Context, seconds_in_past int64, history_type int) ([]zabbix.History, error)
	GetItems(ctx context.Context, item_ids []string) ([]zabbix.Item, error)
	GetHosts(ctx context.Context, host_ids []string) ([]zabbix.Host, error)
}

var _ client = (*zabbixClient)(nil)

type zabbixClient struct {
	session zabbix.Session
	logger  *zap.Logger
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

	fmt.Printf("Connected to Zabbix API v%s\n", version)

	// TODO create it here
	return &zabbixClient{
		session: *session,
		logger:  logger,
	}, nil
}

// GetHistories implements client
func (c *zabbixClient) GetHistories(ctx context.Context, seconds_in_past int64, history_type int) ([]zabbix.History, error) {
	params := zabbix.HistoryGetParams{
		GetParameters: zabbix.GetParameters{},
		History:       int(history_type),
		HistoryIDs:    []string{},
		ItemIDs:       []string{},
		TimeFrom:      float64(time.Now().Unix() - seconds_in_past),
		TimeTill:      float64(time.Now().Unix()),
	}

	res, err := c.session.GetHistories(params)

	return res, err
}

// GetHosts implements client
func (c *zabbixClient) GetHosts(ctx context.Context, host_ids []string) ([]zabbix.Host, error) {
	params := zabbix.HostGetParams{
		GetParameters: zabbix.GetParameters{},
		HostIDs:       host_ids,
	}
	res, err := c.session.GetHosts(params)

	return res, err
}

// GetItems implements client
func (c *zabbixClient) GetItems(ctx context.Context, item_ids []string) ([]zabbix.Item, error) {
	params := zabbix.ItemGetParams{
		GetParameters: zabbix.GetParameters{},
		ItemIDs:       item_ids,
	}

	res, err := c.session.GetItems(params)

	return res, err
}
