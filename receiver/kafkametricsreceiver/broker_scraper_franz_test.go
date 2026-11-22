// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scrapererror"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka/kafkatest"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver/internal/metadata"
)

// helper to make a minimal franz config for tests
func franzTestConfig(t *testing.T) Config {
	t.Helper()
	_, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "meta-topic"))
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		ClusterAlias:         "test-cluster",
	}
	// keep retention metric disabled here (kfake does not expose broker config values)
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogRetentionPeriod.Enabled = false
	return cfg
}

func TestBrokerScraperFranz_CreateStartScrapeShutdown(t *testing.T) {
	cfg := franzTestConfig(t)

	// create franz-backed scraper directly (unit test)
	var s scraper.Metrics
	var err error

	s, err = createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	// Start
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))

	// Scrape
	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)

	// Validate resource + at least the brokers count metric exists
	require.Equal(t, 1, md.ResourceMetrics().Len())
	rm := md.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics()
	require.Equal(t, 1, sm.Len())
	ms := sm.At(0).Metrics()

	var foundBrokers bool
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		if m.Name() == "kafka.brokers" {
			foundBrokers = true
			// With one kfake cluster, we should have >=1 broker; exact count comes from kfake
			require.GreaterOrEqual(t, m.Sum().DataPoints().At(0).IntValue(), int64(1))
			break
		}
	}
	require.True(t, foundBrokers, "expected kafka.brokers metric")

	// Shutdown
	require.NoError(t, s.Shutdown(t.Context()))
}

func TestBrokerScraperFranz_EmptyClusterAlias(t *testing.T) {
	cfg := franzTestConfig(t)
	cfg.ClusterAlias = "" // ensure empty alias behaves like Sarama test

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)

	require.Equal(t, 1, md.ResourceMetrics().Len())
	rm := md.ResourceMetrics().At(0)

	// attribute should be absent when alias is empty
	_, ok := rm.Resource().Attributes().Get("kafka.cluster.alias")
	require.False(t, ok)

	require.NoError(t, s.Shutdown(t.Context()))
}

func TestBrokerScraperFranz_Create(t *testing.T) {
	cfg := franzTestConfig(t)
	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)
}

func TestBrokerScraperFranz_Start(t *testing.T) {
	cfg := franzTestConfig(t)
	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	require.NoError(t, s.Shutdown(t.Context()))
}

func TestBrokerScraperFranz_ScrapeMetricValues(t *testing.T) {
	const numBrokers = 3
	const logRetentionHoursValue = 168
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(1, "meta-topic"),
		kfake.NumBrokers(numBrokers),
		kfake.BrokerConfigs(map[string]string{
			logRetentionHours: "168",
		}),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		ClusterAlias:         "test-cluster",
	}
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterAlias.Enabled = true
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterID.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogRetentionPeriod.Enabled = true

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, md.ResourceMetrics().Len())

	rm := md.ResourceMetrics().At(0)
	val, ok := rm.Resource().Attributes().Get("kafka.cluster.alias")
	require.True(t, ok)
	require.Equal(t, "test-cluster", val.Str())

	// cluster id (opt-in, enabled above) is discovered from cluster metadata ("kfake" by default)
	idVal, ok := rm.Resource().Attributes().Get("kafka.cluster.id")
	require.True(t, ok)
	require.Equal(t, "kfake", idVal.Str())

	ms := rm.ScopeMetrics().At(0).Metrics()
	var sawBrokers, sawRetention bool
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		switch m.Name() {
		case "kafka.brokers":
			require.Equal(t, pmetric.MetricTypeSum, m.Type())
			require.Equal(t, int64(numBrokers), m.Sum().DataPoints().At(0).IntValue())
			sawBrokers = true
		case "kafka.broker.log_retention_period":
			require.Equal(t, pmetric.MetricTypeGauge, m.Type())
			dps := m.Gauge().DataPoints()
			require.Equal(t, numBrokers, dps.Len())
			for j := 0; j < dps.Len(); j++ {
				require.Equal(t, int64(logRetentionHoursValue*3600), dps.At(j).IntValue())
			}
			sawRetention = true
		}
	}
	require.True(t, sawBrokers, "kafka.brokers not emitted")
	require.True(t, sawRetention, "kafka.broker.log_retention_period not emitted")
}

func TestBrokerScraperFranz_EmptyClusterID(t *testing.T) {
	// An empty cluster_id in metadata must not yield an empty-string attribute, even when enabled.
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(1, "meta-topic"),
		kfake.ClusterID(""),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
	}
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterID.Enabled = true

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, md.ResourceMetrics().Len())

	rm := md.ResourceMetrics().At(0)
	_, ok := rm.Resource().Attributes().Get("kafka.cluster.id")
	require.False(t, ok, "kafka.cluster.id must be omitted when the broker reports an empty cluster id")
}

func TestBrokerScraperFranz_ScrapePartialError_UnparseableRetention(t *testing.T) {
	const numBrokers = 2
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(1, "meta-topic"),
		kfake.NumBrokers(numBrokers),
		kfake.BrokerConfigs(map[string]string{
			logRetentionHours: "not-a-number",
		}),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
	}
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogRetentionPeriod.Enabled = true

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	// Per-broker parse failure should produce a partial scrape error, not a hard error.
	require.Error(t, err)
	var partial scrapererror.PartialScrapeError
	require.ErrorAs(t, err, &partial, "expected PartialScrapeError, got %T: %v", err, err)
	require.Equal(t, numBrokers, partial.Failed)

	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	var sawBrokers, sawRetention bool
	for i := 0; i < ms.Len(); i++ {
		switch ms.At(i).Name() {
		case "kafka.brokers":
			sawBrokers = true
		case "kafka.broker.log_retention_period":
			sawRetention = true
		}
	}
	require.True(t, sawBrokers)
	require.False(t, sawRetention, "log retention metric should be skipped on parse failure")
}

func TestBrokerScraperFranz_ScrapeUnreachable(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "meta-topic"))
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
	}

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	cluster.Close() // simulate unreachable cluster

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = s.ScrapeMetrics(ctx)
	require.Error(t, err)
}

func TestBrokerScraperFranz_ShutdownWithoutStart_OK(t *testing.T) {
	// create a legit cfg but never start
	_, _ = kafkatest.NewCluster(t, kfake.SeedTopics(1, "meta-topic"))
	cfg := franzTestConfig(t)

	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	// should not panic / should return nil
	require.NoError(t, s.Shutdown(t.Context()))
}

func TestBrokerScraperFranz_PartitionsLed(t *testing.T) {
	const topic = "topic-a"
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(4, topic), kfake.NumBrokers(3))
	for partition, leader := range []int32{0, 1, 1, 1} {
		require.NoError(t, cluster.MoveTopicPartition(topic, int32(partition), leader))
	}

	md := scrapeBrokers(t, Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()})

	require.Equal(t, map[string]int64{"broker=0": 1, "broker=1": 3, "broker=2": 0}, intDataPoints(md, "kafka.broker.partitions_led"))
}

func TestBrokerScraperFranz_PartitionsLedSkipsOfflinePartitions(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(3, "topic-a"), kfake.NumBrokers(2))
	controlMetadataLeaders(cluster, "topic-a", []int32{0, -1, 0})

	md := scrapeBrokers(t, Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()})

	require.Equal(t, map[string]int64{"broker=0": 2, "broker=1": 0}, intDataPoints(md, "kafka.broker.partitions_led"))
}

func TestBrokerScraperFranz_LogDirs(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(3, "topic-a"), kfake.NumBrokers(2))
	// Volume sizes are only carried by DescribeLogDirs v4+ (Kafka 3.3+).
	clientCfg.ProtocolVersion = "3.5.0"
	controlLogDirs(t, cluster, func(node int32, _ *kmsg.DescribeLogDirsRequest) []kmsg.DescribeLogDirsResponseDir {
		if node == 0 {
			return []kmsg.DescribeLogDirsResponseDir{
				logDir("/a", 1000, 400, map[int32]int64{0: 100, 1: 50}),
				logDir("/b", 2000, 1000, map[int32]int64{2: 25}),
			}
		}
		// A broker or volume that does not report its sizes.
		return []kmsg.DescribeLogDirsResponseDir{logDir("/a", -1, -1, map[int32]int64{0: 10})}
	})

	cfg := Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()}
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsUsed.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsTotal.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsUsable.Enabled = true
	md := scrapeBrokers(t, cfg)

	require.Equal(t, map[string]int64{"broker=0": 175, "broker=1": 10}, intDataPoints(md, "kafka.broker.log_dirs.used"))
	require.Equal(t, map[string]int64{"broker=0": 3000}, intDataPoints(md, "kafka.broker.log_dirs.total"))
	require.Equal(t, map[string]int64{"broker=0": 1400}, intDataPoints(md, "kafka.broker.log_dirs.usable"))
}

func TestBrokerScraperFranz_LogDirsBeforeKafka33(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "topic-a"), kfake.NumBrokers(1))
	// The default test protocol version predates DescribeLogDirs v4, so set sizes never reach the client.
	controlLogDirs(t, cluster, func(int32, *kmsg.DescribeLogDirsRequest) []kmsg.DescribeLogDirsResponseDir {
		return []kmsg.DescribeLogDirsResponseDir{logDir("/a", 1000, 400, map[int32]int64{0: 100})}
	})

	cfg := Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()}
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsUsed.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsTotal.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsUsable.Enabled = true
	md := scrapeBrokers(t, cfg)

	require.Equal(t, map[string]int64{"broker=0": 100}, intDataPoints(md, "kafka.broker.log_dirs.used"))
	require.Empty(t, intDataPoints(md, "kafka.broker.log_dirs.total"))
	require.Empty(t, intDataPoints(md, "kafka.broker.log_dirs.usable"))
}

func TestBrokerScraperFranz_LogDirError(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "topic-a"), kfake.NumBrokers(1))
	controlLogDirs(t, cluster, func(int32, *kmsg.DescribeLogDirsRequest) []kmsg.DescribeLogDirsResponseDir {
		offline := logDir("/b", -1, -1, nil)
		offline.ErrorCode = kerr.KafkaStorageError.Code
		return []kmsg.DescribeLogDirsResponseDir{logDir("/a", -1, -1, map[int32]int64{0: 100}), offline}
	})

	cfg := Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()}
	cfg.MetricsBuilderConfig.Metrics.KafkaBrokerLogDirsUsed.Enabled = true
	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.True(t, scrapererror.IsPartialScrapeError(err), "expected partial scrape error, got %v", err)
	require.Empty(t, intDataPoints(md, "kafka.broker.log_dirs.used"), "a partial sum must not be reported")
}

func scrapeBrokers(t *testing.T, cfg Config) pmetric.Metrics {
	t.Helper()
	s, err := createBrokerScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)
	return md
}

func TestBrokerScraperFranz_Replicas(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(2, "topic-a"), kfake.NumBrokers(3))
	controlMetadataReplicas(cluster, "topic-a", [][2][]int32{
		{{0, 1, 2}, {0, 1, 2}},
		{{1, 2}, {1}},
	})

	md := scrapeBrokers(t, Config{ClientConfig: clientCfg, MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig()})

	require.Equal(t, map[string]int64{"broker=0": 1, "broker=1": 2, "broker=2": 2}, intDataPoints(md, "kafka.broker.replicas"))
	require.Equal(t, map[string]int64{"broker=0": 0, "broker=1": 1, "broker=2": 1}, intDataPoints(md, "kafka.broker.replicas.under_replicated"))
}

// controlMetadataReplicas answers Metadata with one partition per {replicas, isr} pair, led by its first replica.
func controlMetadataReplicas(cluster *kfake.Cluster, topic string, partitions [][2][]int32) {
	cluster.ControlKey(kmsg.Metadata.Int16(), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
		cluster.KeepControl()
		resp := kreq.ResponseKind().(*kmsg.MetadataResponse)
		for i, addr := range cluster.ListenAddrs() {
			host, port, _ := net.SplitHostPort(addr)
			p, _ := strconv.ParseInt(port, 10, 32)
			b := kmsg.NewMetadataResponseBroker()
			b.NodeID, b.Host, b.Port = int32(i), host, int32(p)
			resp.Brokers = append(resp.Brokers, b)
		}
		resp.ClusterID = new("kfake")
		rt := kmsg.NewMetadataResponseTopic()
		rt.Topic = new(topic)
		for i, replicasAndISR := range partitions {
			rp := kmsg.NewMetadataResponseTopicPartition()
			rp.Partition = int32(i)
			rp.Replicas, rp.ISR = replicasAndISR[0], replicasAndISR[1]
			rp.Leader = rp.Replicas[0]
			rt.Partitions = append(rt.Partitions, rp)
		}
		resp.Topics = append(resp.Topics, rt)
		return resp, nil, true
	})
}
