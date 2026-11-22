// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka/kafkatest"
	metadataPkg "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver/internal/metadata"
)

type entityRecord struct {
	eventType  string
	entityType string
	id         map[string]any
	attrs      map[string]any
}

func entityRecords(t *testing.T, logs plog.Logs) []entityRecord {
	t.Helper()
	var out []entityRecord
	rls := logs.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		sls := rls.At(i).ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			asLog, ok := sls.At(j).Scope().Attributes().Get(metadataPkg.SemconvOtelEntityEventAsScope)
			require.True(t, ok && asLog.Bool(), "scope must mark entity events")
			lrs := sls.At(j).LogRecords()
			for k := 0; k < lrs.Len(); k++ {
				a := lrs.At(k).Attributes()
				rec := entityRecord{attrs: map[string]any{}}
				if v, ok := a.Get("otel.entity.event.type"); ok {
					rec.eventType = v.Str()
				}
				if v, ok := a.Get("otel.entity.type"); ok {
					rec.entityType = v.Str()
				}
				if v, ok := a.Get("otel.entity.id"); ok {
					rec.id = v.Map().AsRaw()
				}
				if v, ok := a.Get("otel.entity.attributes"); ok {
					rec.attrs = v.Map().AsRaw()
				}
				out = append(out, rec)
			}
		}
	}
	return out
}

func byType(recs []entityRecord, eventType, entityType string) []entityRecord {
	var out []entityRecord
	for _, r := range recs {
		if r.eventType == eventType && r.entityType == entityType {
			out = append(out, r)
		}
	}
	return out
}

func newTestEntitiesReceiver(t *testing.T, cfg Config) *entitiesReceiver {
	t.Helper()
	r, err := newEntitiesReceiver(cfg, receivertest.NewNopSettings(metadata.Type), consumertest.NewNop())
	require.NoError(t, err)
	r.host = componenttest.NewNopHost()
	r.clients.retain()
	t.Cleanup(r.clients.release)
	return r
}

func build(t *testing.T, r *entitiesReceiver) []entityRecord {
	t.Helper()
	events, err := r.buildEvents(t.Context())
	require.NoError(t, err)
	return entityRecords(t, events.ConvertAndMoveToLogs())
}

func TestEntities_ReportsClusterBrokersTopicsAndGroups(t *testing.T) {
	const (
		topic = "orders"
		group = "billing"
	)
	cluster, clientCfg := kafkatest.NewCluster(t,
		kfake.NumBrokers(3),
		kfake.SeedTopics(2, topic, "_internal-like"),
	)
	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	require.NoError(t, err)
	t.Cleanup(cl.Close)
	adm := kadm.NewClient(cl)

	retention := "3600000"
	_, err = adm.AlterTopicConfigs(t.Context(), []kadm.AlterConfig{{Name: "retention.ms", Value: &retention}}, topic)
	require.NoError(t, err)
	var offs kadm.Offsets
	offs.AddOffset(topic, 0, 0, -1)
	_, err = adm.CommitOffsets(t.Context(), group, offs)
	require.NoError(t, err)

	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig = clientCfg
	cfg.ClusterAlias = "orders-prod"
	r := newTestEntitiesReceiver(t, *cfg)
	recs := build(t, r)

	clusters := byType(recs, "entity_state", entityTypeCluster)
	require.Len(t, clusters, 1)
	clusterID := clusters[0].id[attrClusterID]
	require.NotEmpty(t, clusterID)
	assert.Equal(t, "orders-prod", clusters[0].attrs[attrClusterAlias])
	assert.Contains(t, []any{"KRaft", "ZooKeeper"}, clusters[0].attrs["kafka.cluster.metadata_mode"])

	brokers := byType(recs, "entity_state", entityTypeBroker)
	require.Len(t, brokers, 3)
	for _, b := range brokers {
		assert.Equal(t, clusterID, b.id[attrClusterID], "child IDs carry the cluster so two clusters never collide")
		assert.NotEmpty(t, b.id[attrBrokerID])
		assert.NotEmpty(t, b.attrs["kafka.broker.host"])
		assert.NotEmpty(t, b.attrs["kafka.broker.version_estimate"])
	}

	topics := byType(recs, "entity_state", entityTypeTopic)
	require.Len(t, topics, 1, "the default topic_match skips names starting with _")
	tp := topics[0]
	assert.Equal(t, topic, tp.id[attrTopicName])
	assert.Equal(t, int64(2), tp.attrs["kafka.topic.partitions"])
	brokerIDs := map[any]bool{}
	for _, b := range brokers {
		brokerIDs[b.id[attrBrokerID]] = true
	}
	hosts, ok := tp.attrs["kafka.topic.brokers"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, hosts)
	for _, h := range hosts {
		assert.True(t, brokerIDs[h], "topic broker %v is one of the cluster's brokers", h)
	}
	assert.Equal(t, retention, tp.attrs["kafka.topic.config.retention.ms"])
	assert.Equal(t, "DYNAMIC_TOPIC_CONFIG", tp.attrs["kafka.topic.config_source.retention.ms"])

	groups := byType(recs, "entity_state", entityTypeGroup)
	require.Len(t, groups, 1)
	assert.Equal(t, group, groups[0].id[attrGroupName])
	assert.Equal(t, []any{topic}, groups[0].attrs["kafka.consumer_group.topics"])

	for _, rec := range recs {
		for k := range rec.attrs {
			assert.NotContains(t, k, "password", "sensitive setting %q must not be sent", k)
		}
	}
}

func TestEntities_DefaultSettingsOnlyWhenCurated(t *testing.T) {
	_, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "orders"))
	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig = clientCfg
	recs := build(t, newTestEntitiesReceiver(t, *cfg))

	tp := byType(recs, "entity_state", entityTypeTopic)[0]
	for k, v := range tp.attrs {
		if !strings.HasPrefix(k, "kafka.topic.config_source.") || v != "DEFAULT_CONFIG" {
			continue
		}
		key := strings.TrimPrefix(k, "kafka.topic.config_source.")
		assert.Contains(t, curatedTopicConfigs, key, "default %q is sent only when curated", key)
	}

	cfg.EntityConfigs.Enabled = false
	recs = build(t, newTestEntitiesReceiver(t, *cfg))
	for k := range byType(recs, "entity_state", entityTypeTopic)[0].attrs {
		assert.NotContains(t, k, ".config", "no settings when entity_configs is off, got %q", k)
	}
}

func TestEntities_DeletedTopicGetsDeleteEvent(t *testing.T) {
	cluster, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "orders", "payments"))
	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	require.NoError(t, err)
	t.Cleanup(cl.Close)

	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig = clientCfg
	r := newTestEntitiesReceiver(t, *cfg)
	first := build(t, r)
	require.Len(t, byType(first, "entity_state", entityTypeTopic), 2)
	require.Empty(t, byType(first, "entity_delete", entityTypeTopic))

	_, err = kadm.NewClient(cl).DeleteTopics(t.Context(), "payments")
	require.NoError(t, err)

	// kadm serves metadata from franz-go's cache for a few seconds, so wait for it to expire.
	var second []entityRecord
	require.Eventually(t, func() bool {
		second = build(t, r)
		return len(byType(second, "entity_delete", entityTypeTopic)) > 0
	}, 15*time.Second, 500*time.Millisecond)
	require.Len(t, byType(second, "entity_state", entityTypeTopic), 1)
	deleted := byType(second, "entity_delete", entityTypeTopic)
	require.Len(t, deleted, 1)
	assert.Equal(t, "payments", deleted[0].id[attrTopicName])

	third := build(t, r)
	assert.Empty(t, byType(third, "entity_delete", entityTypeTopic), "a delete is sent once")
}

func TestEntities_StartSendsOnScheduleAndShutdownStops(t *testing.T) {
	_, clientCfg := kafkatest.NewCluster(t, kfake.SeedTopics(1, "orders"))
	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig = clientCfg
	cfg.MetadataCollectionInterval = 50 * time.Millisecond
	sink := new(consumertest.LogsSink)

	r, err := newEntitiesReceiver(*cfg, receivertest.NewNopSettings(metadata.Type), sink)
	require.NoError(t, err)
	require.NoError(t, r.Start(t.Context(), componenttest.NewNopHost()))
	require.Eventually(t, func() bool { return len(sink.AllLogs()) >= 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, r.Shutdown(t.Context()))

	recs := entityRecords(t, sink.AllLogs()[0])
	require.NotEmpty(t, byType(recs, "entity_state", entityTypeCluster))
	interval, ok := sink.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().Get("otel.entity.interval")
	require.True(t, ok)
	assert.Equal(t, pcommon.ValueTypeInt, interval.Type())
	assert.Equal(t, int64(50), interval.Int(), "interval is in milliseconds, which the backend uses to expire entities")
}

func TestEntities_UnreachableClusterSendsNothing(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig.Brokers = []string{"127.0.0.1:1"}
	sink := new(consumertest.LogsSink)
	r, err := newEntitiesReceiver(*cfg, receivertest.NewNopSettings(metadata.Type), sink)
	require.NoError(t, err)
	r.host = componenttest.NewNopHost()
	r.clients.retain()
	t.Cleanup(r.clients.release)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r.collect(ctx)
	assert.Empty(t, sink.AllLogs())
}

func TestMemberHost_DropsTheLeadingSlash(t *testing.T) {
	assert.Equal(t, "10.0.1.114", memberHost("/10.0.1.114"))
	assert.Equal(t, "10.0.1.114", memberHost("10.0.1.114"))
	assert.Empty(t, memberHost(""))
}

func TestEntities_ClusterCarriesTheSettingsBrokersStartFrom(t *testing.T) {
	_, clientCfg := kafkatest.NewCluster(t, kfake.NumBrokers(2), kfake.SeedTopics(1, "orders"))
	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig = clientCfg
	recs := build(t, newTestEntitiesReceiver(t, *cfg))

	cluster := byType(recs, "entity_state", entityTypeCluster)[0]
	brokerValues := map[string]any{}
	for _, b := range byType(recs, "entity_state", entityTypeBroker) {
		for k, v := range b.attrs {
			brokerValues[k] = v
		}
	}
	found := 0
	for k, v := range cluster.attrs {
		if !strings.HasPrefix(k, "kafka.broker.config.") {
			continue
		}
		found++
		assert.Contains(t, clusterStartingConfigs, strings.TrimPrefix(k, "kafka.broker.config."))
		assert.Equal(t, brokerValues[k], v, "the cluster reports a broker's own value for %q", k)
	}
	assert.Positive(t, found)
}
