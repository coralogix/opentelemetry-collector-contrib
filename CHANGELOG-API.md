<!-- This file is autogenerated. See CONTRIBUTING.md for instructions to add an entry. -->

# GO API Changelog

This changelog includes only developer-facing changes.
If you are looking for user-facing changes, check out [CHANGELOG.md](./CHANGELOG.md).

<!-- next version -->

## v0.103.0

### 🛑 Breaking changes 🛑

- `stanza`: remove deprecated code (#33519)
  This change removes:
    - adapter.LogEmitter, use helper.LogEmitter instead
    - adapter.NewLogEmitter, use helper.NewLogEmitter instead
    - fileconsumer.Manager's SugaredLogger struct member
    - pipeline.DirectedPipeline's SugaredLogger struct member
    - testutil.Logger, use zaptest.NewLogger instead
  

### 💡 Enhancements 💡

- `pkg/winperfcounters`: It is now possible to force a `watcher` to re-create the PDH query of a given counter via the `Reset()` function. (#32798)

## v0.102.0

### 💡 Enhancements 💡

- `prometheusreceiver`: Allow to configure http client used by target allocator generated scrape targets (#18054)

### 🧰 Bug fixes 🧰

- `exp/metrics`: fixes staleness.Evict such that it only ever evicts actually stale metrics (#33265)

## v0.101.0

### 🛑 Breaking changes 🛑

- `opampextension`: Move custom message interfaces to separate package (#32950)
  Moves `CustomCapabilityRegistry`, `CustomCapabilityHandler`, and `CustomCapabilityRegisterOption` to a new module.
  These types can now be found in the new `github.com/open-telemetry/opentelemetry-collector-contrib/extension/opampcustommessages` module.
  
- `pkg/stanza`: The internal logger has been changed from zap.SugaredLogger to zap.Logger. (#32177)
  Functions accepting a SugaredLogger, and fields of type SugaredLogger, have been deprecated.

### 💡 Enhancements 💡

- `testbed`: Add the use of connectors to the testbed (#30165)

## v0.100.0

### 🛑 Breaking changes 🛑

- `pkg/stanza`: Pass TelemetrySettings to the Build method of the Builder interface (#32662, #31256)
  The reason for this breaking change is to pass in the component.TelemetrySettings
  so as to use them later in various ways:
    - be able to report state statistics and telemetry in general
    - be able to switch from SugaredLogger to Logger
  

### 🚩 Deprecations 🚩

- `confmap/provider/s3`: Deprecate `s3provider.New` in favor of `s3provider.NewFactory` (#32742)
- `confmap/provider/secretsmanager`: Deprecate `secretsmanagerprovider.New` in favor of `secretsmanagerprovider.NewFactory` (#32743)

### 🚀 New components 🚀

- `roundrobinconnector`: Add a roundrobin connector, that can help single thread components to scale (#32853)

### 💡 Enhancements 💡

- `opampextension`: Added support for other components to register custom capabilities and receive custom messages from an opamp extension (#32021)
- `kafkaexporter`: add an ability to publish kafka messages with message key based on metric resource attributes - it will allow partitioning metrics in Kafka. (#29433, #30666, #31675)
- `sshcheckreceiver`: Add support for running this receiver on Windows (#30650)

## v0.99.0

### 💡 Enhancements 💡

- `prometheusremotewrite`: Optimize the prometheusremotewrite.FromMetrics function, based around more performant metric identifier hashing. (#31385)
- `pkg/pdatatest/plogtest`: Add an option to ignore log timestamp (#32540)
- `filelogreceiver`: Add `exclude_older_than` configuration setting (#31053)

## v0.98.0

### 💡 Enhancements 💡

- `pkg/sampling`: Usability improvements in the sampling API. (#31918)

## v0.97.0

### 🛑 Breaking changes 🛑

- `datadogexporter`: Remove config structs `LimitedClientConfig` and `LimitedTLSClientSettings` (#31733)
  This should have no impact to end users as long as they do not import Datadog exporter config structs in their source code.
- `cmd/mdatagen`: Delete deprecated cmd/mdatagen from this project. Use go.opentelemetry.io/collector/cmd/mdatagen instead. (#30497)
- `azuremonitorreceiver`: Reduce the public API for this receiver. (#24850)
  This unexports the following types ArmClient, MetricsDefinitionsClientInterface, MetricsValuesClient.
- `general`: Update any component using `scraperhelper.ScraperControllerSettings` to use `scraperhelper.ControllerConfig` (#31816)
  This changes the config field name from `ScraperControllerSettings` to `ControllerConfig`
- `prometheusreceiver`: Remove enable_protobuf_negotiation option on the prometheus receiver. Use config.global.scrape_protocols = [ PrometheusProto, OpenMetricsText1.0.0, OpenMetricsText0.0.1, PrometheusText0.0.4 ] instead. (#30883)
  See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#configuration-file for details on setting scrape_protocols.

### 🚩 Deprecations 🚩

- `pkg/stanza`: Deprecate fileconsumer.BuildWithSplitFunc in favor of Build with options pattern (#31596)

### 💡 Enhancements 💡

- `clickhouseexporter`: Allow configuring `ON CLUSTER` and `ENGINE` when creating database and tables (#24649)
  Increases table creation flexibility with the ability to add replication for fault tolerance
- `all`: Remove explicit checks in all receivers to check if the next consumer is nil (#31793)
  The nil check is now done by the pipeline builder.

## v0.96.0

### 🛑 Breaking changes 🛑

- `cmd/mdatagen`: Use enum for stability levels in the Metadata struct (#31530)
- `httpforwarder`: Remove extension named httpforwarder, use httpforwarderextension instead. (#24171)

## v0.95.0

### 🛑 Breaking changes 🛑

- `pkg/stanza`: Remove deprecated pkg/stanza/attrs (#30449)
- `httpforwarderextension`: Rename the extension httpforwarder to httpforwarderextension (#24171)
- `extension/storage`: The `filestorage` and `dbstorage` extensions are now standalone modules. (#31040)
  If using the OpenTelemetry Collector Builder, you will need to update your import paths to use the new module(s).
  - `github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/filestorage`
  - `github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/dbstorage`
  

### 💡 Enhancements 💡

- `pkg/golden`: Added an option to skip the metric timestamp normalization for WriteMetrics. (#30919)
- `healthcheckextension`: Remove usage of deprecated `host.ReportFatalError` (#30582)

## v0.94.0

### 🚩 Deprecations 🚩

- `testbed`: Deprecate testbed.GetAvailablePort in favor of testutil.GetAvailablePort (#30811)
  Move healthcheckextension to use testutil.GetAvailablePort

### 🚀 New components 🚀

- `pkg_sampling`: Package of code for parsing OpenTelemetry tracestate probability sampling fields. (#29738)

## v0.93.0

### 🛑 Breaking changes 🛑

- `testbed`: Remove unused AWS XRay mock receiver (#30381)
- `docker`: Adopt api_version as strings to correct invalid float truncation (#24025)
- `prometheusreceiver`: Consolidate Config members and remove the need of placeholders. (#29901)
- `all`: Remove obsolete "// +build" directive (#30651)
- `testbed`: Expand TestCase capabilities with broken out LoadGenerator interface (#30303)

### 🚩 Deprecations 🚩

- `pkg/stanza`: Deprecate pkg/stanza/attrs package in favor of pkg/stanza/fileconsumer/attrs (#30449)

### 💡 Enhancements 💡

- `testbed`: Adds and adopts new WithEnvVar child process option, moving GOMAXPROCS=2 to initializations (#30491)

## v0.92.0

### 🛑 Breaking changes 🛑

- `carbonexporter`: Change Config member names (#29862)
- `carbonreceiver`: Hide unnecessary public API (#29895)
- `pkg/ottl`: Unexport `ADD`, `SUB`, `MULT`, `DIV`, `EQ`, `NE`, `LT`, `LTE`, `GT`, and `GTE` (#29925)
- `pkg/ottl`: Change `Path` to be an Interface instead of the grammar struct. (#29897)
  Affects creators of custom contexts.
- `golden`: Use testing.TB for golden.WriteMetrics, golden.WriteTraces and golden.WriteLogs over *testing.T (#30277)

### 💡 Enhancements 💡

- `kafkaexporter`: add ability to publish kafka messages with message key of TraceID - it will allow partitioning of the kafka Topic. (#12318)
- `kafkaexporter`: Adds the ability to configure the Kafka client's Client ID. (#30144)

## v0.91.0

### 🛑 Breaking changes 🛑

- `pkg/ottl`: Rename `Statements` to `StatementSequence`. Remove `Eval` function from `StatementSequence`, use `ConditionSequence` instead. (#29598)

### 💡 Enhancements 💡

- `pkg/ottl`: Add `ConditionSequence` for evaluating lists of conditions (#29339)

## v0.90.0

### 🛑 Breaking changes 🛑

- `clickhouseexporter`: Replace `Config.QueueSettings` field with `exporterhelper.QueueSettings` and remove `QueueSettings` struct (#27653)
- `kafkareceiver`: Do not export the function `WithTracesUnmarshalers`, `WithMetricsUnmarshalers`, `WithLogsUnmarshalers` (#26304)

### 💡 Enhancements 💡

- `datadogreceiver`: The datadogreceiver supports the new datadog protocol that is sent by the datadog agent API/v0.2/traces. (#27045)
- `pkg/ottl`: Add ability to independently parse OTTL conditions. (#29315)

### 🧰 Bug fixes 🧰

- `cassandraexporter`: Exist check for keyspace and dynamic timeout (#27633)

## v0.89.0

### 🛑 Breaking changes 🛑

- `carbonreceiver`: Do not export function New and pass checkapi. (#26304)
- `collectdreceiver`: Move to use confighttp.HTTPServerSettings (#28811)
- `kafkaexporter`: Do not export function WithTracesMarshalers, WithMetricsMarshalers, WithLogsMarshalers and pass checkapi (#26304)
- `remoteobserverprocessor`: Rename remoteobserverprocessor to remotetapprocessor (#27873)

### 💡 Enhancements 💡

- `extension/encoding`: Introduce interfaces for encoding extensions. (#28686)
- `exporter/awss3exporter`: This feature allows role assumption for s3 exportation. It is especially useful on Kubernetes clusters that are using IAM roles for service accounts (#28674)

## v0.88.0

### 🚩 Deprecations 🚩

- `pkg/stanza`: Deprecate 'flush.WithPeriod'. Use 'flush.WithFunc' instead. (#27843)

## v0.87.0

### 🛑 Breaking changes 🛑

- `exporter/kafka, receiver/kafka, receiver/kafkametrics`: Move configuration parts to an internal pkg (#27093)
- `pulsarexporter`: Do not export function WithTracesMarshalers, add test for that and pass checkapi (#26304)
- `pulsarreceiver`: Do not export the functions `WithLogsUnmarshalers`, `WithMetricsUnmarshalers`, `WithTracesUnmarshalers`, add tests and pass checkapi. (#26304)

### 💡 Enhancements 💡

- `mdatagen`: allows adding warning section to resource_attribute configuration (#19174)
- `mdatagen`: allow setting empty metric units (#27089)

## v0.86.0

### 🛑 Breaking changes 🛑

- `azuremonitorexporter`: Unexport `Accept` to comply with checkapi (#26304)
- `tailsamplingprocessor`: Unexport `SamplingProcessorMetricViews` to comply with checkapi (#26304)
- `awskinesisexporter`: Do not export the functions `NewTracesExporter`, `NewMetricsExporter`, `NewLogsExporter` and pass checkapi. (#26304)
- `dynatraceexporter`: Rename struct to keep expected `exporter.Factory` and pass checkapi. (#26304)
- `ecsobserver`: Do not export the function `DefaultConfig` and pass checkapi. (#26304)
- `f5cloudexporter`: Do not export the function `NewFactoryWithTokenSourceGetter` and pass checkapi. (#26304)
- `fluentforwardreceiver`: rename `Logs` and `DetermineNextEventMode` functions and all usage to lowercase to stop exporting method and pass checkapi (#26304)
- `groupbyattrsprocessor`: Do not export the function `MetricViews` and pass checkapi. (#26304)
- `groupbytraceprocessor`: Do not export the function `MetricViews` and pass checkapi. (#26304)
- `jaegerreceiver`: Do not export the function `DefaultServerConfigUDP` and pass checkapi. (#26304)
- `lokiexporter`: Do not export the function `MetricViews` and pass checkapi. (#26304)
- `mongodbatlasreceiver`: Rename struct to pass checkapi. (#26304)
- `mongodbreceiver`: Do not export the function `NewClient` and pass checkapi. (#26304)
- `mysqlreceiver`: Do not export the function `Query` (#26304)
- `pkg/ottl`: Remove support for `ottlarg`. The struct's field order is now the function parameter order. (#25705)
- `pkg/stanza`: Make trim func composable (#26536)
  - Adds trim.WithFunc to allow trim funcs to wrap bufio.SplitFuncs.
  - Removes trim.Func from split.Config.Func. Use trim.WithFunc instead.
  - Removes trim.Func from flush.WithPeriod. Use trim.WithFunc instead.
  
- `pkg/stanza`: Rename syslog and tcp MultilineBuilders (#26631)
  - Rename syslog.OctetMultiLineBuilder to syslog.OctetSplitFuncBuilder
  - Rename tc.MultilineBuilder to tcp.SplitFuncBuilder
  
- `probabilisticsamplerprocessor`: Do not export the function `SamplingProcessorMetricViews` and pass checkapi. (#26304)
- `sentryexporter`: Do not export the functions `CreateSentryExporter` and pass checkapi. (#26304)
- `sumologicexporter`: Do not export the function `CreateDefaultHTTPClientSettings` and pass checkapi. (#26304)

### 💡 Enhancements 💡

- `pkg/ottl`: Add support for optional parameters (#20879)
  The new `ottl.Optional` type can now be used in a function's `Arguments` struct
  to indicate that a parameter is optional.
  

## v0.85.0

### 🛑 Breaking changes 🛑

- `alibabacloudlogserviceexporter`: Do not export the function `NewLogServiceClient` (#26304)
- `awss3exporter`: Do not export the function `NewMarshaler` (#26304)
- `statsdreceiver`: rename and do not export function `New` to `newReceiver` to pass checkapi (#26304)
- `chronyreceiver`: Removes duplicate `Timeout` field. This change has no impact on end users of the component. (#26113)
- `dockerstatsreceiver`: Removes duplicate `Timeout` field. This change has no impact on end users of the component. (#26114)
- `elasticsearchexporter`: Do not export the function `DurationAsMicroseconds` (#26304)
- `jaegerexporter`: Do not export the function `MetricViews` (#26304)
- `k8sobjectsreceiver`: Do not export the function `NewTicker` (#26304)
- `pkg/stanza`: Rename 'pkg/stanza/decoder' to 'pkg/stanza/decode' (#26340)
- `pkg/stanza`: Move tokenize.SplitterConfig.Encoding to fileconsumer.Config.Encoding (#26511)
- `pkg/stanza`: Remove Flusher from tokenize.SplitterConfig (#26517)
  Removes the following in favor of flush.WithPeriod - tokenize.DefaultFlushPeriod - tokenize.FlusherConfig - tokenize.NewFlusherConfig
- `pkg/stanza`: Remove tokenize.SplitterConfig (#26537)
- `pkg/stanza`: Rename "tokenize" package to "split" (#26540)
  - Remove 'Multiline' struct
  - Remove 'NewMultilineConfig' struct
  - Rename 'MultilineConfig' to 'split.Config'
  
- `pkg/stanza`: Extract whitespace trim configuration into trim.Config (#26511)
  - PreserveLeading and PreserveTrailing removed from tokenize.SplitterConfig.
  - PreserveLeadingWhitespaces and PreserveTrailingWhitespaces removed from tcp.BaseConfig and udp.BaseConfig.
  

### 💡 Enhancements 💡

- `oauth2clientauthextension`: Enable dynamically reading ClientID and ClientSecret from files (#26117)
  - Read the client ID and/or secret from a file by specifying the file path to the ClientIDFile (`client_id_file`) and ClientSecretFile (`client_secret_file`) fields respectively.
  - The file is read every time the client issues a new token. This means that the corresponding value can change dynamically during the execution by modifying the file contents.
  

## v0.84.0

### 🛑 Breaking changes 🛑

- `memcachedreceiver`: Removes duplicate `Timeout` field. This change has no impact on end users of the component. (#26084)
- `podmanreceiver`: Removes duplicate `Timeout` field. This change has no impact on end users of the component. (#26083)
- `zookeeperreceiver`: Removes duplicate `Timeout` field. This change has no impact on end users of the component. (#26082)
- `jaegerreceiver`: Deprecate remote_sampling config in the jaeger receiver (#24186)
  The jaeger receiver will fail to start if remote_sampling config is specified in it.  The `receiver.jaeger.DisableRemoteSampling` feature gate can be set to let the receiver start and treat  remote_sampling config as no-op. In a future version this feature gate will be removed and the receiver will always  fail when remote_sampling config is specified.
  
- `pkg/ottl`: use IntGetter argument for Substring function (#25852)
- `pkg/stanza`: Remove deprecated 'helper.Encoding' and 'helper.EncodingConfig.Build' (#25846)
- `pkg/stanza`: Remove deprecated fileconsumer config structs (#24853)
  Includes | - MatchingCriteria - OrderingCriteria - NumericSortRule - AlphabeticalSortRule - TimestampSortRule
- `googlecloudexporter`: remove retry_on_failure from the googlecloud exporter. The exporter itself handles retries, and retrying can cause issues. (#57233)

### 🚩 Deprecations 🚩

- `pkg/stanza`: Deprecate 'helper.EncodingConfig' and 'helper.NewEncodingConfig' (#25846)
- `pkg/stanza`: Deprecate encoding related elements of helper pacakge, in favor of new decoder package (#26019)
  Includes the following deprecations | - Decoder - NewDecoder - LookupEncoding - IsNop
- `pkg/stanza`: Deprecate tokenization related elements of helper pacakge, in favor of new tokenize package (#25914)
  Includes the following deprecations | - Flusher - FlusherConfig - NewFlusherConfig - Multiline - MultilineConfig - NewMultilineConfig - NewLineStartSplitFunc - NewLineEndSplitFunc - NewNewlineSplitFunc - Splitter - SplitterConfig - NewSplitterConfig - SplitNone

### 💡 Enhancements 💡

- `googlemanagedprometheus`: Add a `add_metric_suffixes` option to the googlemanagedprometheus exporter. When set to false, metric suffixes are not added. (#26071)
- `receiver/prometheus`: translate units from prometheus to UCUM (#23208)

### 🧰 Bug fixes 🧰

- `receiver/influxdb`: add allowable inputs to line protocol precision parameter (#24974)

## v0.83.0

### 🛑 Breaking changes 🛑

- `exporter/clickhouse`: Change the type of `Config.Password` to be `configopaque.String` (#17273)
- `all`: Remove go 1.19 support, bump minimum to go 1.20 and add testing for 1.21 (#8207)
- `solacereceiver`: Move model package to the internal package (#24890)
- `receiver/statsdreceiver`: Move protocol and transport packages to internal (#24892)
- `filterprocessor`: Unexport `Strict` and `Regexp` (#24845)
- `mdatagen`: Rename the mdatagen sum field `aggregation` to `aggregation_temporality` (#16374)
- `metricstransformprocessor`: Unexport elements of the Go API of the processor (#24846)
- `mezmoexporter`: Unexport the `MezmoLogLine` and `MezmoLogBody` structs (#24842)
- `pkg/stanza`: Remove deprecated 'fileconsumer.FileAttributes' (#24688)
- `pkg/stanza`: Remove deprecated 'fileconsumer.EmitFunc' (#24688)
- `pkg/stanza`: Remove deprecated `fileconsumer.Finder` (#24688)
- `pkg/stanza`: Remove deprecated `fileconsumer.BaseSortRule` and `fileconsumer.SortRuleImpl` (#24688)
- `pkg/stanza`: Remove deprecated 'fileconsumer.Reader' (#24688)

### 🚩 Deprecations 🚩

- `pkg/stanza`: Deprecate helper.Encoding and helper.EncodingConfig.Build (#24980)
- `pkg/stanza`: Deprecate fileconsumer MatchingCriteria in favor of new matcher package (#24853)

### 💡 Enhancements 💡

- `changelog`: Generate separate changelogs for end users and package consumers (#24014)
- `splunkhecexporter`: Add heartbeat check while startup and new config param, heartbeat/startup (defaults to false). This is different than the healtcheck_startup, as the latter doesn't take token or index into account. (#24411)
- `k8sclusterreceiver`: Allows disabling metrics and resource attributes (#24568)
- `cmd/mdatagen`: Avoid reusing the same ResourceBuilder instance for multiple ResourceMetrics (#24762)

### 🧰 Bug fixes 🧰

- `splunkhecreceiver`: aligns success resp body w/ splunk enterprise (#19219)
  changes resp from plaintext "ok" to json {"text"："success", "code"：0}