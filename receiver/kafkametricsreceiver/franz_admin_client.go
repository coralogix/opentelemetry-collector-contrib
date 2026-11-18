// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver"

import (
	"context"
	"fmt"
	"sync"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/kafka/configkafka"
)

// franzAdminProvider hands out a single franz-go admin client shared by all of
// the receiver's scrapers: effectively an idle pool of size one, reference
// counted by the scrapers using it. The client is created lazily on first use
// and closed only once the last scraper has released it.
type franzAdminProvider struct {
	clientConfig configkafka.ClientConfig
	logger       *zap.Logger

	mu       sync.Mutex
	adm      *kadm.Client
	kcl      *kgo.Client
	refCount int
}

func newFranzAdminProvider(clientConfig configkafka.ClientConfig, logger *zap.Logger) *franzAdminProvider {
	return &franzAdminProvider{
		clientConfig: clientConfig,
		logger:       logger,
	}
}

// retain registers a user of the shared client; balance with one release.
func (p *franzAdminProvider) retain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refCount++
}

// admin returns the shared kadm client, creating it lazily on first use.
func (p *franzAdminProvider) admin(ctx context.Context, host component.Host) (*kadm.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.adm != nil {
		return p.adm, nil
	}
	// kadm.Client wraps and owns the underlying *kgo.Client, so we only need to
	// hold on to the admin client (its Close also closes the kgo client).
	adm, kcl, err := kafka.NewFranzClusterAdminClient(ctx, host, p.clientConfig, p.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create franz-go admin client: %w", err)
	}
	p.adm = adm
	p.kcl = kcl
	return p.adm, nil
}

// client returns the kgo client behind the shared admin client, for requests kadm does not wrap.
func (p *franzAdminProvider) client(ctx context.Context, host component.Host) (*kgo.Client, error) {
	if _, err := p.admin(ctx, host); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.kcl, nil
}

// release drops a reference, closing the client when the last one is released.
func (p *franzAdminProvider) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refCount > 0 {
		p.refCount--
	}
	if p.refCount > 0 {
		return
	}
	if p.adm != nil {
		// kadm.Client.Close also closes the underlying *kgo.Client.
		p.adm.Close()
		p.adm = nil
		p.kcl = nil
	}
}
