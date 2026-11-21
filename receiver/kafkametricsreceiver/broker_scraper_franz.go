// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver"

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver/internal/metadata"
)

const logRetentionHours = "log.retention.hours"

type brokerScraperFranz struct {
	// clients is the shared franz-go admin client provider.
	clients *franzAdminProvider

	settings receiver.Settings
	config   Config
	mb       *metadata.MetricsBuilder
	host     component.Host
}

func (s *brokerScraperFranz) start(_ context.Context, host component.Host) error {
	s.mb = metadata.NewMetricsBuilder(s.config.MetricsBuilderConfig, s.settings)
	s.host = host
	if s.clients == nil {
		s.clients = newFranzAdminProvider(s.config.ClientConfig, s.settings.Logger)
	}
	s.clients.retain()
	return nil
}

func (s *brokerScraperFranz) shutdown(context.Context) error {
	if s.clients != nil {
		s.clients.release()
	}
	return nil
}

func (s *brokerScraperFranz) scrape(ctx context.Context) (pmetric.Metrics, error) {
	scrapeErrs := scrapererror.ScrapeErrors{}

	adm, err := s.clients.admin(ctx, s.host)
	if err != nil {
		return pmetric.Metrics{}, err
	}

	now := pcommon.NewTimestampFromTime(time.Now())
	rb := s.mb.NewResourceBuilder()
	rb.SetKafkaClusterAlias(s.config.ClusterAlias)

	metrics := s.config.MetricsBuilderConfig.Metrics
	// Partition leadership needs every topic; otherwise a topic-less request is enough.
	var meta kadm.Metadata
	if metrics.KafkaBrokerPartitionsLed.Enabled {
		meta, err = adm.Metadata(ctx)
	} else {
		meta, err = adm.BrokerMetadata(ctx)
	}
	if err != nil {
		scrapeErrs.Add(err)
		return s.mb.Emit(metadata.WithResource(rb.Emit())), scrapeErrs.Combine()
	}
	if meta.Cluster != "" { // skip empty IDs so we never emit an empty-string attribute
		rb.SetKafkaClusterID(meta.Cluster)
	}
	brokerIDs := meta.Brokers.NodeIDs()
	s.mb.RecordKafkaBrokersDataPoint(now, int64(len(brokerIDs)))

	if metrics.KafkaBrokerPartitionsLed.Enabled {
		led := make(map[int32]int64, len(brokerIDs))
		meta.Topics.EachPartition(func(pd kadm.PartitionDetail) { led[pd.Leader]++ })
		for _, bid := range brokerIDs {
			s.mb.RecordKafkaBrokerPartitionsLedDataPoint(now, led[bid], strconv.Itoa(int(bid)))
		}
	}

	if metrics.KafkaBrokerLogDirsUsed.Enabled || metrics.KafkaBrokerLogDirsTotal.Enabled || metrics.KafkaBrokerLogDirsUsable.Enabled {
		s.recordLogDirs(ctx, adm, now, &scrapeErrs)
	}

	// If log retention metric is disabled, we are done.
	if !metrics.KafkaBrokerLogRetentionPeriod.Enabled {
		return s.mb.Emit(metadata.WithResource(rb.Emit())), scrapeErrs.Combine()
	}

	res, err := adm.DescribeBrokerConfigs(ctx, brokerIDs...)
	if err != nil {
		s.settings.Logger.Warn("franz-go: DescribeBrokerConfigs failed", zap.Error(err))
		scrapeErrs.AddPartial(len(brokerIDs), fmt.Errorf("DescribeBrokerConfigs: %w", err))
	}

	// Iterate the result and record the metric for each broker entry we can parse.
	for _, bid := range brokerIDs {
		bidStr := strconv.Itoa(int(bid))

		// Look up this broker's config set by resource name (broker id as string).
		cfg, _ := res.On(bidStr, nil) // fn can be nil to just return the entry
		if cfg.Err != nil {
			scrapeErrs.AddPartial(1, fmt.Errorf("broker %s: %w", bidStr, cfg.Err))
			continue
		}

		for _, kv := range cfg.Configs {
			// kadm.Config has Key and MaybeValue() for the string value.
			// We only care about log.retention.hours here.
			if kv.Key != logRetentionHours {
				continue
			}
			raw := kv.MaybeValue()
			hrs, convErr := strconv.Atoi(raw)
			if convErr != nil {
				scrapeErrs.AddPartial(1, fmt.Errorf("broker %s: cannot parse %s=%q: %w", bidStr, logRetentionHours, raw, convErr))
				continue
			}
			sec := int64(hrs) * 3600
			s.mb.RecordKafkaBrokerLogRetentionPeriodDataPoint(now, sec, bidStr)
		}
	}

	return s.mb.Emit(metadata.WithResource(rb.Emit())), scrapeErrs.Combine()
}

func (s *brokerScraperFranz) recordLogDirs(ctx context.Context, adm *kadm.Client, now pcommon.Timestamp, scrapeErrs *scrapererror.ScrapeErrors) {
	all, err := adm.DescribeAllLogDirs(ctx, nil)
	if err != nil {
		s.settings.Logger.Warn("franz-go: DescribeAllLogDirs failed", zap.Error(err))
		scrapeErrs.AddPartial(1, fmt.Errorf("DescribeAllLogDirs: %w", err))
	}
	for bid, dirs := range all {
		bidStr := strconv.Itoa(int(bid))
		var used, total, usable int64
		dirsOK, volumesKnown := true, true
		for _, dir := range dirs {
			if dir.Err != nil {
				scrapeErrs.AddPartial(1, fmt.Errorf("broker %s log dir %s: %w", bidStr, dir.Dir, dir.Err))
				dirsOK = false
				continue
			}
			used += dir.Size()
			// Brokers before Kafka 3.3 report -1 for both.
			if dir.TotalBytes < 0 || dir.UsableBytes < 0 {
				volumesKnown = false
			}
			total += dir.TotalBytes
			usable += dir.UsableBytes
		}
		if !dirsOK {
			continue
		}
		s.mb.RecordKafkaBrokerLogDirsUsedDataPoint(now, used, bidStr)
		if volumesKnown {
			s.mb.RecordKafkaBrokerLogDirsTotalDataPoint(now, total, bidStr)
			s.mb.RecordKafkaBrokerLogDirsUsableDataPoint(now, usable, bidStr)
		}
	}
}

// factory for franz-go scraper (internal; selected via gate at the call site later)
func createBrokerScraperFranz(_ context.Context, cfg Config, settings receiver.Settings, clients *franzAdminProvider) (scraper.Metrics, error) {
	s := &brokerScraperFranz{
		settings: settings,
		config:   cfg,
		clients:  clients,
	}
	return scraper.NewMetrics(
		s.scrape,
		scraper.WithStart(s.start),
		scraper.WithShutdown(s.shutdown),
	)
}
