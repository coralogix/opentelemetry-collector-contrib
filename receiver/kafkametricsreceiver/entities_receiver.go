// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kafkametricsreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/kafkametricsreceiver"

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	metadataPkg "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/experimentalmetricmetadata"
)

const (
	entityTypeCluster = "kafka.cluster"
	entityTypeBroker  = "kafka.broker"
	entityTypeTopic   = "kafka.topic"
	entityTypeGroup   = "kafka.consumer_group"

	attrClusterID    = "kafka.cluster.id"
	attrClusterAlias = "kafka.cluster.alias"
	attrBrokerID     = "kafka.broker.id"
	attrTopicName    = "kafka.topic.name"
	attrGroupName    = "kafka.consumer_group.name"

	kraftMetadataTopic = "__cluster_metadata"
	topicConfigBatch   = 200
)

// Settings attached even when left at their default, so a reader sees the effective value.
var (
	curatedBrokerConfigs = []string{
		"process.roles", "broker.rack", "listeners", "advertised.listeners", "log.dirs",
		"broker.heartbeat.interval.ms", "broker.session.timeout.ms", "zookeeper.session.timeout.ms",
		"replica.lag.time.max.ms", "controlled.shutdown.enable",
		"num.network.threads", "num.io.threads", "num.replica.fetchers", "queued.max.requests",
		"num.recovery.threads.per.data.dir", "message.max.bytes", "replica.fetch.max.bytes",
		"log.retention.hours", "log.retention.ms", "log.retention.bytes", "log.segment.bytes", "log.cleaner.enable",
		"min.insync.replicas", "unclean.leader.election.enable", "default.replication.factor",
		"auto.create.topics.enable", "num.partitions", "auto.leader.rebalance.enable",
		"leader.imbalance.per.broker.percentage", "offsets.topic.replication.factor",
		"transaction.state.log.replication.factor", "transaction.state.log.min.isr",
		"delete.topic.enable", "inter.broker.protocol.version",
		"offsets.retention.minutes", "group.initial.rebalance.delay.ms",
	}
	curatedTopicConfigs = []string{
		"cleanup.policy", "retention.ms", "retention.bytes", "min.insync.replicas", "max.message.bytes",
		"segment.bytes", "segment.ms", "compression.type", "unclean.leader.election.enable",
		"message.timestamp.type", "remote.storage.enable", "local.retention.ms",
	}
)

// entitiesReceiver reports Kafka clusters, brokers, topics and consumer groups as entity events on a logs pipeline.
type entitiesReceiver struct {
	cfg         Config
	settings    receiver.Settings
	next        consumer.Logs
	clients     *franzAdminProvider
	topicFilter *regexp.Regexp
	groupFilter *regexp.Regexp

	host   component.Host
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// IDs reported last cycle, per entity type, so a vanished entity gets a delete event.
	seen map[string]map[string]pcommon.Map
}

func newEntitiesReceiver(cfg Config, settings receiver.Settings, next consumer.Logs) (*entitiesReceiver, error) {
	topicFilter, err := regexp.Compile(cfg.TopicMatch)
	if err != nil {
		return nil, fmt.Errorf("failed to compile topic filter: %w", err)
	}
	groupFilter, err := regexp.Compile(cfg.GroupMatch)
	if err != nil {
		return nil, fmt.Errorf("failed to compile group filter: %w", err)
	}
	return &entitiesReceiver{
		cfg:         cfg,
		settings:    settings,
		next:        next,
		clients:     newFranzAdminProvider(cfg.ClientConfig, settings.Logger),
		topicFilter: topicFilter,
		groupFilter: groupFilter,
		seen:        map[string]map[string]pcommon.Map{},
	}, nil
}

func (r *entitiesReceiver) Start(_ context.Context, host component.Host) error {
	r.host = host
	r.clients.retain()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.wg.Go(func() {
		r.collect(ctx)
		if r.cfg.MetadataCollectionInterval <= 0 {
			return
		}
		ticker := time.NewTicker(r.cfg.MetadataCollectionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.collect(ctx)
			case <-ctx.Done():
				return
			}
		}
	})
	return nil
}

func (r *entitiesReceiver) Shutdown(context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	if r.clients != nil {
		r.clients.release()
	}
	return nil
}

// collect sends the full current state; a failed send is not retried because the next cycle resends everything.
func (r *entitiesReceiver) collect(ctx context.Context) {
	events, err := r.buildEvents(ctx)
	if err != nil {
		r.settings.Logger.Warn("Failed to collect Kafka entities", zap.Error(err))
		if events.Len() == 0 {
			return
		}
	}
	if err := r.next.ConsumeLogs(ctx, events.ConvertAndMoveToLogs()); err != nil {
		r.settings.Logger.Error("Failed to send Kafka entity events", zap.Error(err))
	}
}

func (r *entitiesReceiver) buildEvents(ctx context.Context) (metadataPkg.EntityEventsSlice, error) {
	out := metadataPkg.NewEntityEventsSlice()
	adm, err := r.clients.admin(ctx, r.host)
	if err != nil {
		return out, err
	}
	md, err := adm.Metadata(ctx)
	if err != nil {
		return out, fmt.Errorf("metadata: %w", err)
	}
	clusterID := md.Cluster
	if clusterID == "" {
		clusterID = r.cfg.ClusterAlias
	}
	if clusterID == "" {
		return out, errors.New("cluster has no ID and no cluster_alias is set")
	}

	now := pcommon.NewTimestampFromTime(time.Now())
	current := map[string]map[string]pcommon.Map{}
	var errs []error

	state := func(entityType string, id map[string]string) pcommon.Map {
		ev := out.AppendEmpty()
		ev.SetTimestamp(now)
		idMap := ev.ID()
		for k, v := range id {
			idMap.PutStr(k, v)
		}
		st := ev.SetEntityState()
		st.SetEntityType(entityType)
		if r.cfg.MetadataCollectionInterval > 0 {
			st.SetInterval(r.cfg.MetadataCollectionInterval)
		}
		if current[entityType] == nil {
			current[entityType] = map[string]pcommon.Map{}
		}
		keep := pcommon.NewMap()
		idMap.CopyTo(keep)
		current[entityType][idKey(id)] = keep
		attrs := st.Attributes()
		if r.cfg.ClusterAlias != "" {
			attrs.PutStr(attrClusterAlias, r.cfg.ClusterAlias)
		}
		return attrs
	}

	brokerConfigs, cfgErr := r.brokerConfigs(ctx, adm, md)
	errs = append(errs, cfgErr...)

	// Cluster
	cluster := state(entityTypeCluster, map[string]string{attrClusterID: clusterID})
	cluster.PutStr(attrClusterID, clusterID)
	versions, verErr := adm.ApiVersions(ctx)
	if verErr != nil {
		errs = append(errs, fmt.Errorf("api versions: %w", verErr))
	}
	controller, voters, mode := r.quorum(ctx, md.Controller, versions)
	cluster.PutStr("kafka.cluster.metadata_mode", mode)
	if controller >= 0 {
		cluster.PutStr("kafka.cluster.controller.id", strconv.Itoa(int(controller)))
	}
	if len(voters) > 0 {
		vs := cluster.PutEmptySlice("kafka.cluster.controller.voters")
		for _, v := range voters {
			vs.AppendEmpty().SetStr(strconv.Itoa(int(v)))
		}
	}
	r.putConfigs(cluster, "kafka.cluster", brokerConfigs[""], curatedBrokerConfigs)

	// Brokers
	for _, b := range md.Brokers {
		bid := strconv.Itoa(int(b.NodeID))
		attrs := state(entityTypeBroker, map[string]string{attrClusterID: clusterID, attrBrokerID: bid})
		attrs.PutStr(attrClusterID, clusterID)
		attrs.PutStr(attrBrokerID, bid)
		attrs.PutStr("kafka.broker.host", b.Host)
		attrs.PutInt("kafka.broker.port", int64(b.Port))
		if b.Rack != nil {
			attrs.PutStr("kafka.broker.rack", *b.Rack)
		}
		attrs.PutBool("kafka.broker.is_controller", b.NodeID == controller)
		if v, ok := versions[b.NodeID]; ok && v.Err == nil {
			attrs.PutStr("kafka.broker.version_estimate", v.VersionGuess())
		}
		r.putConfigs(attrs, "kafka.broker", brokerConfigs[bid], curatedBrokerConfigs)
	}

	// Topics
	var topics []string
	for name, t := range md.Topics {
		if t.Err == nil && r.topicFilter.MatchString(name) {
			topics = append(topics, name)
		}
	}
	sort.Strings(topics)
	topicConfigs, cfgErr := r.topicConfigs(ctx, adm, topics)
	errs = append(errs, cfgErr...)
	for _, name := range topics {
		t := md.Topics[name]
		attrs := state(entityTypeTopic, map[string]string{attrClusterID: clusterID, attrTopicName: name})
		attrs.PutStr(attrClusterID, clusterID)
		attrs.PutStr(attrTopicName, name)
		attrs.PutInt("kafka.topic.partitions", int64(len(t.Partitions)))
		attrs.PutInt("kafka.topic.replication_factor", int64(replicationFactor(t)))
		attrs.PutBool("kafka.topic.internal", t.IsInternal)
		hosts := attrs.PutEmptySlice("kafka.topic.brokers")
		for _, id := range replicaBrokers(t) {
			hosts.AppendEmpty().SetStr(strconv.Itoa(int(id)))
		}
		r.putConfigs(attrs, "kafka.topic", topicConfigs[name], curatedTopicConfigs)
	}

	// Consumer groups
	groupErr := r.groups(ctx, adm, clusterID, state)
	if groupErr != nil {
		errs = append(errs, groupErr)
	}

	r.appendDeletes(out, now, current, groupErr != nil)
	return out, errors.Join(errs...)
}

// groups adds one entity per consumer group, with its members and the topics it has committed offsets for.
func (r *entitiesReceiver) groups(ctx context.Context, adm *kadm.Client, clusterID string, state func(string, map[string]string) pcommon.Map) error {
	listed, err := adm.ListGroupsByType(ctx, []string{"classic", "consumer"})
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}
	var names []string
	for _, g := range listed {
		if r.groupFilter.MatchString(g.Group) {
			names = append(names, g.Group)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	lags, err := adm.Lag(ctx, names...)
	if err != nil {
		return fmt.Errorf("describe groups: %w", err)
	}
	for _, name := range names {
		l, ok := lags[name]
		attrs := state(entityTypeGroup, map[string]string{attrClusterID: clusterID, attrGroupName: name})
		attrs.PutStr(attrClusterID, clusterID)
		attrs.PutStr(attrGroupName, name)
		if !ok || l.DescribeErr != nil {
			attrs.PutStr("kafka.consumer_group.state", listed[name].State)
			continue
		}
		attrs.PutStr("kafka.consumer_group.state", l.State)
		putNonEmpty(attrs, "kafka.consumer_group.protocol_type", l.ProtocolType)
		putNonEmpty(attrs, "kafka.consumer_group.assignor", l.Protocol)
		attrs.PutStr("kafka.consumer_group.coordinator.id", strconv.Itoa(int(l.Coordinator.NodeID)))
		attrs.PutInt("kafka.consumer_group.members", int64(len(l.Members)))
		members := attrs.PutEmptySlice("kafka.consumer_group.member_list")
		for _, m := range l.Members {
			mm := members.AppendEmpty().SetEmptyMap()
			mm.PutStr("client_id", m.ClientID)
			mm.PutStr("host", memberHost(m.ClientHost))
			if a, ok := m.Assigned.AsConsumer(); ok {
				parts := mm.PutEmptySlice("assigned")
				for _, t := range a.Topics {
					for _, p := range t.Partitions {
						parts.AppendEmpty().SetStr(t.Topic + "/" + strconv.Itoa(int(p)))
					}
				}
			}
		}
		var read []string
		for topic := range l.Lag {
			if r.topicFilter.MatchString(topic) {
				read = append(read, topic)
			}
		}
		sort.Strings(read)
		ts := attrs.PutEmptySlice("kafka.consumer_group.topics")
		for _, t := range read {
			ts.AppendEmpty().SetStr(t)
		}
	}
	return nil
}

// memberHost drops the "/" Kafka puts in front of a member's client address ("/10.0.1.114").
func memberHost(clientHost string) string {
	return strings.TrimPrefix(clientHost, "/")
}

// quorum returns the active controller, the voting controllers and the metadata mode.
// On KRaft the Metadata response names a random broker as controller, so the quorum leader is used instead.
func (r *entitiesReceiver) quorum(ctx context.Context, metadataController int32, versions kadm.BrokersApiVersions) (int32, []int32, string) {
	if len(versions) > 0 && !supportsDescribeQuorum(versions) {
		return metadataController, nil, "zookeeper"
	}
	kcl, err := r.clients.client(ctx, r.host)
	if err != nil {
		return metadataController, nil, "unknown"
	}
	req := kmsg.NewPtrDescribeQuorumRequest()
	topic := kmsg.NewDescribeQuorumRequestTopic()
	topic.Topic = kraftMetadataTopic
	topic.Partitions = []kmsg.DescribeQuorumRequestTopicPartition{kmsg.NewDescribeQuorumRequestTopicPartition()}
	req.Topics = []kmsg.DescribeQuorumRequestTopic{topic}
	resp, err := req.RequestWith(ctx, kcl)
	if err != nil || resp.ErrorCode != 0 || len(resp.Topics) == 0 || len(resp.Topics[0].Partitions) == 0 {
		r.settings.Logger.Debug("DescribeQuorum failed", zap.Error(err))
		return metadataController, nil, "unknown"
	}
	p := resp.Topics[0].Partitions[0]
	if p.ErrorCode != 0 {
		return metadataController, nil, "kraft"
	}
	voters := make([]int32, 0, len(p.CurrentVoters))
	for _, v := range p.CurrentVoters {
		voters = append(voters, v.ReplicaID)
	}
	slices.Sort(voters)
	return p.LeaderID, voters, "kraft"
}

// supportsDescribeQuorum is false on ZooKeeper clusters, whose brokers do not implement the request.
func supportsDescribeQuorum(versions kadm.BrokersApiVersions) bool {
	key := kmsg.NewPtrDescribeQuorumRequest().Key()
	for _, v := range versions {
		if v.Err != nil {
			continue
		}
		if _, ok := v.KeyMaxVersion(key); ok {
			return true
		}
	}
	return false
}

// brokerConfigs returns each broker's settings keyed by broker ID, and the cluster-wide dynamic defaults under "".
func (r *entitiesReceiver) brokerConfigs(ctx context.Context, adm *kadm.Client, md kadm.Metadata) (map[string]kadm.ResourceConfig, []error) {
	out := map[string]kadm.ResourceConfig{}
	if !r.cfg.EntityConfigs.Enabled {
		return out, nil
	}
	var errs []error
	ids := make([]int32, 0, len(md.Brokers))
	for _, b := range md.Brokers {
		ids = append(ids, b.NodeID)
	}
	if len(ids) > 0 {
		rcs, err := adm.DescribeBrokerConfigs(ctx, ids...)
		if err != nil {
			errs = append(errs, fmt.Errorf("describe broker configs: %w", err))
		}
		for _, rc := range rcs {
			if rc.Err == nil {
				out[rc.Name] = rc
			}
		}
	}
	defaults, err := adm.DescribeBrokerConfigs(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("describe cluster configs: %w", err))
	}
	for _, rc := range defaults {
		if rc.Err == nil {
			out[""] = rc
		}
	}
	return out, errs
}

func (r *entitiesReceiver) topicConfigs(ctx context.Context, adm *kadm.Client, topics []string) (map[string]kadm.ResourceConfig, []error) {
	out := map[string]kadm.ResourceConfig{}
	if !r.cfg.EntityConfigs.Enabled {
		return out, nil
	}
	var errs []error
	for start := 0; start < len(topics); start += topicConfigBatch {
		end := min(start+topicConfigBatch, len(topics))
		rcs, err := adm.DescribeTopicConfigs(ctx, topics[start:end]...)
		if err != nil {
			errs = append(errs, fmt.Errorf("describe topic configs: %w", err))
		}
		for _, rc := range rcs {
			if rc.Err == nil {
				out[rc.Name] = rc
			}
		}
	}
	return out, errs
}

// putConfigs adds `<prefix>.config.<key>` and `<prefix>.config_source.<key>`; sensitive values are never sent.
func (r *entitiesReceiver) putConfigs(attrs pcommon.Map, prefix string, rc kadm.ResourceConfig, curated []string) {
	if !r.cfg.EntityConfigs.Enabled {
		return
	}
	always := make(map[string]struct{}, len(curated))
	for _, k := range curated {
		always[k] = struct{}{}
	}
	for _, c := range rc.Configs {
		if c.Sensitive || c.Value == nil {
			continue
		}
		_, isCurated := always[c.Key]
		isDefault := c.Source == kmsg.ConfigSourceDefaultConfig
		if isDefault && !isCurated && !r.cfg.EntityConfigs.IncludeDefaults {
			continue
		}
		attrs.PutStr(prefix+".config."+c.Key, *c.Value)
		attrs.PutStr(prefix+".config_source."+c.Key, c.Source.String())
	}
}

// appendDeletes reports entities seen last cycle but not this one. Groups are kept when listing them failed.
func (r *entitiesReceiver) appendDeletes(out metadataPkg.EntityEventsSlice, now pcommon.Timestamp, current map[string]map[string]pcommon.Map, groupsFailed bool) {
	for entityType, prev := range r.seen {
		if entityType == entityTypeGroup && groupsFailed {
			if current[entityType] == nil {
				current[entityType] = prev
			}
			continue
		}
		for key, id := range prev {
			if _, ok := current[entityType][key]; ok {
				continue
			}
			ev := out.AppendEmpty()
			ev.SetTimestamp(now)
			id.CopyTo(ev.ID())
			ev.SetEntityDelete().SetEntityType(entityType)
		}
	}
	r.seen = current
}

func putNonEmpty(attrs pcommon.Map, key, value string) {
	if value != "" {
		attrs.PutStr(key, value)
	}
}

func idKey(id map[string]string) string {
	keys := make([]string, 0, len(id))
	for k := range id {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + id[k] + ";")
	}
	return b.String()
}

// replicaBrokers returns the sorted IDs of the brokers holding a replica of any of the topic's partitions.
func replicaBrokers(t kadm.TopicDetail) []int32 {
	seen := map[int32]struct{}{}
	for _, p := range t.Partitions {
		for _, id := range p.Replicas {
			seen[id] = struct{}{}
		}
	}
	ids := make([]int32, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func replicationFactor(t kadm.TopicDetail) int {
	rf := 0
	for _, p := range t.Partitions {
		rf = max(rf, len(p.Replicas))
	}
	return rf
}
