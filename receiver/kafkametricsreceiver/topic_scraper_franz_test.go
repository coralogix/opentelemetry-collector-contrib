// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver

import (
	"context"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// minimal franz test config for topics
func franzTopicsTestConfig(t *testing.T) Config {
	t.Helper()
	_, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "topic-a"))
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		ClusterAlias:         "franz-topics",
		TopicMatch:           ".*", // allow our seeded topic
	}
	return cfg
}

func TestTopicScraperFranz_CreateStartScrapeShutdown(t *testing.T) {
	cfg := franzTopicsTestConfig(t)

	var s scraper.Metrics
	var err error

	s, err = createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))

	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)
	require.NotNil(t, md)

	// With kfake, depending on metadata & offsets availability, data points may be zero.
	// The key here is: scrape does not error.
	_ = md // intentionally not asserting counts

	require.NoError(t, s.Shutdown(t.Context()))
}

func TestTopicScraperFranz_InvalidTopicRegex(t *testing.T) {
	cfg := franzTopicsTestConfig(t)
	cfg.TopicMatch = "[" // invalid

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.Error(t, err)
	require.Nil(t, s)
}

func TestTopicScraperFranz_EmptyClusterAlias(t *testing.T) {
	cfg := franzTopicsTestConfig(t)
	cfg.ClusterAlias = ""

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NotNil(t, s)

	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)

	// Only assert alias absence if a resource entry exists.
	if md.ResourceMetrics().Len() > 0 {
		rm := md.ResourceMetrics().At(0)
		_, ok := rm.Resource().Attributes().Get("kafka.cluster.alias")
		require.False(t, ok)
	}

	require.NoError(t, s.Shutdown(t.Context()))
}

func TestTopicScraperFranz_ScrapeMetricValues(t *testing.T) {
	const (
		topic         = "topic-a"
		numPartitions = 3
		numBrokers    = 2
	)
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(numPartitions, topic),
		kfake.NumBrokers(numBrokers),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		ClusterAlias:         "franz-topics",
		TopicMatch:           ".*",
	}
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterAlias.Enabled = true
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterID.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicLogRetentionPeriod.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicLogRetentionSize.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicMinInsyncReplicas.Enabled = true
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicReplicationFactor.Enabled = true

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, md.ResourceMetrics().Len())

	rm := md.ResourceMetrics().At(0)
	val, ok := rm.Resource().Attributes().Get("kafka.cluster.alias")
	require.True(t, ok)
	require.Equal(t, "franz-topics", val.Str())

	// cluster id (opt-in, enabled above) is discovered from cluster metadata ("kfake" by default)
	idVal, ok := rm.Resource().Attributes().Get("kafka.cluster.id")
	require.True(t, ok)
	require.Equal(t, "kfake", idVal.Str())

	// kfake topic config defaults
	const (
		expectedRetentionMs    = 604800000 // 7 days
		expectedRetentionBytes = -1
		expectedMinInsync      = 1
	)

	ms := rm.ScopeMetrics().At(0).Metrics()
	seen := map[string]bool{}
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		seen[m.Name()] = true
		switch m.Name() {
		case "kafka.topic.partitions":
			require.Equal(t, pmetric.MetricTypeSum, m.Type())
			dps := m.Sum().DataPoints()
			require.Equal(t, 1, dps.Len())
			require.Equal(t, int64(numPartitions), dps.At(0).IntValue())
		case "kafka.topic.replication_factor":
			require.Equal(t, int64(numBrokers), m.Gauge().DataPoints().At(0).IntValue())
		case "kafka.topic.min_insync_replicas":
			require.Equal(t, int64(expectedMinInsync), m.Gauge().DataPoints().At(0).IntValue())
		case "kafka.topic.log_retention_period":
			require.Equal(t, int64(expectedRetentionMs/1000), m.Gauge().DataPoints().At(0).IntValue())
		case "kafka.topic.log_retention_size":
			require.Equal(t, int64(expectedRetentionBytes), m.Gauge().DataPoints().At(0).IntValue())
		case "kafka.partition.replicas":
			require.Equal(t, numPartitions, m.Sum().DataPoints().Len())
			for j := 0; j < m.Sum().DataPoints().Len(); j++ {
				require.Equal(t, int64(numBrokers), m.Sum().DataPoints().At(j).IntValue())
			}
		case "kafka.partition.replicas_in_sync":
			require.Equal(t, numPartitions, m.Sum().DataPoints().Len())
			for j := 0; j < m.Sum().DataPoints().Len(); j++ {
				require.Equal(t, int64(numBrokers), m.Sum().DataPoints().At(j).IntValue())
			}
		}
	}
	for _, name := range []string{
		"kafka.topic.partitions",
		"kafka.topic.replication_factor",
		"kafka.topic.min_insync_replicas",
		"kafka.topic.log_retention_period",
		"kafka.topic.log_retention_size",
		"kafka.partition.replicas",
		"kafka.partition.replicas_in_sync",
	} {
		require.True(t, seen[name], "metric %s not emitted", name)
	}
}

func TestTopicScraperFranz_EmptyClusterID(t *testing.T) {
	// An empty cluster_id in metadata must not yield an empty-string attribute, even when enabled.
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(1, "topic-a"),
		kfake.ClusterID(""),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	}
	cfg.MetricsBuilderConfig.ResourceAttributes.KafkaClusterID.Enabled = true

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
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

func TestTopicScraperFranz_TopicFilterExcludes(t *testing.T) {
	_, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "include-me", "_internal"))
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           "include-.*",
	}

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.NoError(t, err)

	if md.ResourceMetrics().Len() == 0 {
		return
	}
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		if m.Name() != "kafka.topic.partitions" {
			continue
		}
		dps := m.Sum().DataPoints()
		for j := 0; j < dps.Len(); j++ {
			topic, _ := dps.At(j).Attributes().Get("topic")
			require.Equal(t, "include-me", topic.Str())
		}
	}
}

func TestTopicScraperFranz_ScrapePartialError_UnparseableConfig(t *testing.T) {
	const topic = "topic-a"
	_, clientCfg := kafkatest.NewCluster(t,
		kfake.SeedTopics(2, topic),
		// kfake propagates min.insync.replicas from broker config into the topic config.
		kfake.BrokerConfigs(map[string]string{
			minInsyncReplicas: "not-a-number",
		}),
	)
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	}
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicMinInsyncReplicas.Enabled = true

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	md, err := s.ScrapeMetrics(t.Context())
	require.Error(t, err)
	var partial scrapererror.PartialScrapeError
	require.ErrorAs(t, err, &partial, "expected PartialScrapeError, got %T: %v", err, err)
	require.Equal(t, 1, partial.Failed)

	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	var sawPartitions, sawMinInsync bool
	for i := 0; i < ms.Len(); i++ {
		switch ms.At(i).Name() {
		case "kafka.topic.partitions":
			sawPartitions = true
		case "kafka.topic.min_insync_replicas":
			sawMinInsync = true
		}
	}
	require.True(t, sawPartitions)
	require.False(t, sawMinInsync, "min_insync_replicas should be skipped on parse failure")
}

func TestTopicScraperFranz_ScrapeUnreachable(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "topic-a"))
	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	}

	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })

	cluster.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = s.ScrapeMetrics(ctx)
	require.Error(t, err)
}

// Optional parity check: default regex compiles (like Sarama test style)
func TestTopicScraperFranz_FilterCompilesLikeSarama(t *testing.T) {
	r := regexp.MustCompile(defaultTopicMatch)
	require.NotNil(t, r)
}

func TestTopicScraperFranz_PartitionLeaders(t *testing.T) {
	const topic = "topic-a"
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(3, topic), kfake.NumBrokers(3))
	for partition, leader := range []int32{0, 2, 2} {
		require.NoError(t, cluster.MoveTopicPartition(topic, int32(partition), leader))
	}

	md := scrapeTopics(t, Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	})

	require.Equal(t, map[string]int64{
		"partition=0,topic=topic-a": 0,
		"partition=1,topic=topic-a": 2,
		"partition=2,topic=topic-a": 2,
	}, intDataPoints(md, "kafka.partition.leader_id"))
	require.Equal(t, map[string]int64{
		"partition=0,topic=topic-a": 0,
		"partition=1,topic=topic-a": 0,
		"partition=2,topic=topic-a": 0,
	}, intDataPoints(md, "kafka.partition.offline"))
}

func TestTopicScraperFranz_OfflinePartition(t *testing.T) {
	const topic = "topic-a"
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(2, topic), kfake.NumBrokers(2))
	controlMetadataLeaders(cluster, topic, []int32{1, -1})
	// The client otherwise keeps retrying to find the missing leader before listing offsets.
	clientCfg.Metadata.Retry.Max = 0

	md, _ := scrapeTopicsErr(t, Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	})

	require.Equal(t, map[string]int64{
		"partition=0,topic=topic-a": 0,
		"partition=1,topic=topic-a": 1,
	}, intDataPoints(md, "kafka.partition.offline"))
	require.Equal(t, map[string]int64{
		"partition=0,topic=topic-a": 1,
		"partition=1,topic=topic-a": -1,
	}, intDataPoints(md, "kafka.partition.leader_id"))
}

func TestTopicScraperFranz_TopicSize(t *testing.T) {
	const topic = "topic-a"
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(2, topic, "other"), kfake.NumBrokers(2))
	require.NoError(t, cluster.MoveTopicPartition(topic, 0, 0))
	require.NoError(t, cluster.MoveTopicPartition(topic, 1, 1))

	// Each broker holds both partitions: the leader replica and a lagging follower of the other.
	var requested []string
	controlLogDirs(t, cluster, func(node int32, req *kmsg.DescribeLogDirsRequest) []kmsg.DescribeLogDirsResponseDir {
		for _, rt := range req.Topics {
			requested = append(requested, rt.Topic)
		}
		switch node {
		case 0:
			return []kmsg.DescribeLogDirsResponseDir{
				logDir("/data", -1, -1, map[int32]int64{0: 100, 1: 90}),
				futureLogDir("/moving", map[int32]int64{0: 7}),
			}
		default:
			return []kmsg.DescribeLogDirsResponseDir{logDir("/data", -1, -1, map[int32]int64{0: 95, 1: 200})}
		}
	})

	cfg := Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           "^topic-a$",
	}
	cfg.MetricsBuilderConfig.Metrics.KafkaTopicSize.Enabled = true
	md := scrapeTopics(t, cfg)

	require.Equal(t, map[string]int64{"topic=topic-a": 300}, intDataPoints(md, "kafka.topic.size"))
	require.NotEmpty(t, requested)
	for _, topic := range requested {
		require.Equal(t, "topic-a", topic, "log dirs must be described only for matched topics")
	}
}

func TestTopicScraperFranz_TopicSizeDisabledByDefault(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "topic-a"))
	var described bool
	cluster.ControlKey(kmsg.DescribeLogDirs.Int16(), func(kmsg.Request) (kmsg.Response, error, bool) {
		described = true
		return nil, nil, false
	})

	md := scrapeTopics(t, Config{
		ClientConfig:         clientCfg,
		MetricsBuilderConfig: metadata.NewDefaultMetricsBuilderConfig(),
		TopicMatch:           ".*",
	})

	require.Empty(t, intDataPoints(md, "kafka.topic.size"))
	require.False(t, described)
}

func scrapeTopics(t *testing.T, cfg Config) pmetric.Metrics {
	t.Helper()
	md, err := scrapeTopicsErr(t, cfg)
	require.NoError(t, err)
	return md
}

func scrapeTopicsErr(t *testing.T, cfg Config) (pmetric.Metrics, error) {
	t.Helper()
	s, err := createTopicsScraperFranz(t.Context(), cfg, receivertest.NewNopSettings(metadata.Type), nil)
	require.NoError(t, err)
	require.NoError(t, s.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, s.Shutdown(t.Context())) })
	return s.ScrapeMetrics(t.Context())
}

// intDataPoints maps each data point of the named metric, keyed by its sorted "k=v" attributes, to its value.
func intDataPoints(md pmetric.Metrics, name string) map[string]int64 {
	out := map[string]int64{}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				if m.Name() != name {
					continue
				}
				var dps pmetric.NumberDataPointSlice
				if m.Type() == pmetric.MetricTypeSum {
					dps = m.Sum().DataPoints()
				} else {
					dps = m.Gauge().DataPoints()
				}
				for l := 0; l < dps.Len(); l++ {
					var attrs []string
					for key, v := range dps.At(l).Attributes().All() {
						attrs = append(attrs, key+"="+v.AsString())
					}
					sort.Strings(attrs)
					out[strings.Join(attrs, ",")] = dps.At(l).IntValue()
				}
			}
		}
	}
	return out
}

// controlMetadataLeaders answers every Metadata request with one topic whose partition i is led by leaders[i] (-1 for none).
func controlMetadataLeaders(cluster *kfake.Cluster, topic string, leaders []int32) {
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
		for i, leader := range leaders {
			rp := kmsg.NewMetadataResponseTopicPartition()
			rp.Partition = int32(i)
			rp.Leader = leader
			if leader < 0 {
				rp.ErrorCode = kerr.LeaderNotAvailable.Code
			} else {
				rp.Replicas, rp.ISR = []int32{leader}, []int32{leader}
			}
			rt.Partitions = append(rt.Partitions, rp)
		}
		resp.Topics = append(resp.Topics, rt)
		return resp, nil, true
	})
}

// controlLogDirs answers every DescribeLogDirs request with the directories dirsFor returns for the receiving broker.
func controlLogDirs(t *testing.T, cluster *kfake.Cluster, dirsFor func(node int32, req *kmsg.DescribeLogDirsRequest) []kmsg.DescribeLogDirsResponseDir) {
	t.Helper()
	cluster.ControlKey(kmsg.DescribeLogDirs.Int16(), func(kreq kmsg.Request) (kmsg.Response, error, bool) {
		cluster.KeepControl()
		req := kreq.(*kmsg.DescribeLogDirsRequest)
		resp := req.ResponseKind().(*kmsg.DescribeLogDirsResponse)
		resp.Dirs = dirsFor(cluster.CurrentNode(), req)
		return resp, nil, true
	})
}

// logDir builds a directory holding the given topic-a partition sizes.
func logDir(dir string, total, usable int64, sizes map[int32]int64) kmsg.DescribeLogDirsResponseDir {
	d := kmsg.NewDescribeLogDirsResponseDir()
	d.Dir, d.TotalBytes, d.UsableBytes = dir, total, usable
	rt := kmsg.NewDescribeLogDirsResponseDirTopic()
	rt.Topic = "topic-a"
	for p, size := range sizes {
		rp := kmsg.NewDescribeLogDirsResponseDirTopicPartition()
		rp.Partition, rp.Size = p, size
		rt.Partitions = append(rt.Partitions, rp)
	}
	d.Topics = append(d.Topics, rt)
	return d
}

func futureLogDir(dir string, sizes map[int32]int64) kmsg.DescribeLogDirsResponseDir {
	d := logDir(dir, -1, -1, sizes)
	for i := range d.Topics[0].Partitions {
		d.Topics[0].Partitions[i].IsFuture = true
	}
	return d
}
