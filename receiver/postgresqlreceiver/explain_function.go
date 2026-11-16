// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P8] explain_function: estimated plans from a hardened SECURITY
// DEFINER function (ADR-0004 amendment 2026-11-15, SEC-02 conditions C4, C6,
// C12 and C14) instead of the receiver's own PREPARE / EXPLAIN.

// explainFunctionName is the only function the receiver calls.
const explainFunctionName = "coralogix_dbm.explain_statement"

const (
	defaultExplainFunctionTimeout           = time.Second
	maxExplainFunctionTimeout               = 2 * time.Second
	defaultExplainFunctionMaxCallsPerMinute = 30
	maxExplainFunctionMaxCallsPerMinute     = 600
	maxSchemaNameBytes                      = 63
)

// explainFunctionOptions are the per-call settings.
type explainFunctionOptions struct {
	timeoutMS int64
	schemas   []string
}

// explainViaFunction implements client. RED STUB.
func (*postgreSQLClient) explainViaFunction(context.Context, string, string, explainFunctionOptions, *zap.Logger) (string, error) {
	return "", nil
}

// stripLeadingComments removes the comments and whitespace before the first
// token of a statement (C4). RED STUB.
func stripLeadingComments(query string) string {
	return query
}

// explainFunctionInput returns the text sent to the function. RED STUB.
func explainFunctionInput(query string) (string, bool) {
	return query, true
}

// explainFunctionOutcome classifies a call result (C14). RED STUB.
func explainFunctionOutcome(error) metadata.AttributeExplainOutcome {
	return metadata.AttributeExplainOutcomeOk
}

// explainRateLimiter is a token bucket of maxPerMinute calls. RED STUB.
type explainRateLimiter struct{}

func newExplainRateLimiter(int) *explainRateLimiter { return &explainRateLimiter{} }

func (*explainRateLimiter) allow(time.Time) bool { return true }

// explainFunctionTxOptions are the options of the call's transaction. RED STUB.
func explainFunctionTxOptions() *sql.TxOptions { return &sql.TxOptions{} }

// explainFunctionOptions returns the per-call settings. RED STUB.
func (DBMConfig) explainFunctionOptions() explainFunctionOptions { return explainFunctionOptions{} }

// explainFunctionMaxCallsPerMinute returns the rate limit. RED STUB.
func (DBMConfig) explainFunctionMaxCallsPerMinute() int { return 0 }
