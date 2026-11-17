// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"errors"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P7c] queryIDInvalidKey marks a query sample whose query_id was dropped;
// the scraper counts and strips it before emission.
const queryIDInvalidKey = "_dbm_query_id_invalid"

var errInvalidQueryID = errors.New("queryid is not a canonical int64")

// isCanonicalQueryID reports whether s is a signed int64 in the canonical
// decimal form PostgreSQL prints for a bigint queryid: no sign "+", no
// leading zeros, no "-0", no spaces. Any other value is forged or corrupted
// and may carry text, so the receiver drops it and never echoes it.
func isCanonicalQueryID(s string) bool {
	id, err := strconv.ParseInt(s, 10, 64)
	return err == nil && strconv.FormatInt(id, 10) == s
}

// queryIDField logs a queryid only when it is canonical (or empty: no
// query_id, as with compute_query_id off); otherwise only that it was
// invalid.
func queryIDField(queryID string) zap.Field {
	if queryID == "" || isCanonicalQueryID(queryID) {
		return zap.String("queryID", queryID)
	}
	return zap.Bool("queryIDInvalid", true)
}

// recordInvalidQueryIDs counts the rows marked with queryIDInvalidKey and
// strips the marker from every row.
func (p *postgreSQLScraper) recordInvalidQueryIDs(ctx context.Context, rows []map[string]any, event metadata.AttributeEvent) {
	var n int64
	for _, row := range rows {
		if invalid, _ := row[queryIDInvalidKey].(bool); invalid {
			n++
		}
		delete(row, queryIDInvalidKey)
	}
	p.countInvalidQueryIDs(ctx, n, event)
}

// dropInvalidTopQueryIDs drops every top query row whose queryid is present
// but not a canonical int64 string, before it reaches the delta cache,
// EXPLAIN or the emitted records. A nil queryid is left to the caller.
func (p *postgreSQLScraper) dropInvalidTopQueryIDs(ctx context.Context, rows []map[string]any, logger *zap.Logger) []map[string]any {
	kept := make([]map[string]any, 0, len(rows))
	var n int64
	for _, row := range rows {
		value := row[dbAttributePrefix+queryidColumnName]
		if value == nil {
			kept = append(kept, row)
			continue
		}
		queryID, _ := value.(string)
		if isCanonicalQueryID(queryID) {
			kept = append(kept, row)
			continue
		}
		logger.Error("queryid is not a canonical int64; top query row dropped",
			zap.String("event", metadata.AttributeEventTopQuery.String()),
			zap.Int("queryIDLength", len(queryID)))
		n++
	}
	p.countInvalidQueryIDs(ctx, n, metadata.AttributeEventTopQuery)
	return kept
}

func (p *postgreSQLScraper) countInvalidQueryIDs(ctx context.Context, n int64, event metadata.AttributeEvent) {
	if n > 0 {
		p.telemetry.PostgresqlQueryIDInvalid.Add(ctx, n, metric.WithAttributes(attribute.String("event", event.String())))
	}
}
