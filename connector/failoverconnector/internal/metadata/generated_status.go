// Code generated by mdatagen. DO NOT EDIT.

package metadata

import (
	"go.opentelemetry.io/collector/component"
)

var (
	Type = component.MustNewType("failover")
)

const (
	TracesToTracesStability   = component.StabilityLevelAlpha
	MetricsToMetricsStability = component.StabilityLevelAlpha
	LogsToLogsStability       = component.StabilityLevelAlpha
)