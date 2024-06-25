# Splunk Enterprise Receiver

<!-- status autogenerated section -->
| Status        |           |
| ------------- |-----------|
| Stability     | [alpha]: metrics   |
| Distributions | [] |
| Issues        | [![Open issues](https://img.shields.io/github/issues-search/open-telemetry/opentelemetry-collector-contrib?query=is%3Aissue%20is%3Aopen%20label%3Areceiver%2Fsplunkenterprise%20&label=open&color=orange&logo=opentelemetry)](https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aopen+is%3Aissue+label%3Areceiver%2Fsplunkenterprise) [![Closed issues](https://img.shields.io/github/issues-search/open-telemetry/opentelemetry-collector-contrib?query=is%3Aissue%20is%3Aclosed%20label%3Areceiver%2Fsplunkenterprise%20&label=closed&color=blue&logo=opentelemetry)](https://github.com/open-telemetry/opentelemetry-collector-contrib/issues?q=is%3Aclosed+is%3Aissue+label%3Areceiver%2Fsplunkenterprise) |
| [Code Owners](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/CONTRIBUTING.md#becoming-a-code-owner)    | [@shalper2](https://www.github.com/shalper2), [@MovieStoreGuy](https://www.github.com/MovieStoreGuy), [@greatestusername](https://www.github.com/greatestusername) |

[alpha]: https://github.com/open-telemetry/opentelemetry-collector#alpha
<!-- end autogenerated section -->

The Splunk Enterprise Receiver is a pull based tool which enables the ingestion of performance metrics describing the operational status of a user's Splunk Enterprise deployment to an appropriate observability tool.
It is designed to leverage several different data sources to gather these metrics including the [introspection api endpoint](https://docs.splunk.com/Documentation/Splunk/9.1.1/RESTREF/RESTintrospect) and serializing
results from ad-hoc searches. Because of this, care must be taken by users when enabling metrics as running searches can effect your Splunk Enterprise Deployment and introspection may fail to report for Splunk
Cloud deployments. The primary purpose of this receiver is to empower those tasked with the maintenance and care of a Splunk Enterprise deployment to leverage opentelemetry and their observability toolset in their
jobs.

## Configuration

The following settings are required, omitting them will either cause your receiver to fail to compile or result in 4/5xx return codes during scraping. 

**NOTE:** These must be set for each Splunk instance type (indexer, search head, or cluster master) from which you wish to pull metrics. At present, only one of each type is accepted, per configured receiver instance. This means, for example, that if you have three different "indexer" type instances that you would like to pull metrics from you will need to configure three different `splunkenterprise` receivers for each indexer node you wish to monitor.

* `basicauth` (from [basicauthextension](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/extension/basicauthextension)): A configured stanza for the basicauthextension.
* `auth` (no default): String name referencing your auth extension.
* `endpoint` (no default): your Splunk Enterprise host's endpoint.

The following settings are optional:

* `collection_interval` (default: 10m): The time between scrape attempts.
* `timeout` (default: 60s): The time the scrape function will wait for a response before returning empty.

Example:

```yaml
extensions:
    basicauth/indexer:
        client_auth:
            username: admin
            password: securityFirst
    basicauth/cluster_master:
        client_auth:
            username: admin
            password: securityFirst

receivers:
    splunkenterprise:
        indexer:
            auth: 
              authenticator: basicauth/indexer
            endpoint: "https://localhost:8089"
            timeout: 45s
        cluster_master:
            auth: 
              authenticator: basicauth/cluster_master
            endpoint: "https://localhost:8089"
            timeout: 45s

exporters:
  logging:
    loglevel: info

service:
  extensions: [basicauth/indexer, basicauth/cluster_master]
  pipelines:
    metrics:
      receivers: [splunkenterprise]
      exporters: [logging]
```

For a full list of settings exposed by this receiver please look [here](./config.go) with a detailed configuration [here](./testdata/config.yaml).