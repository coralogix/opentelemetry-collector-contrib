// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver"

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/postgresqlreceiver/internal/metadata"
)

// [DBM P7c] queryIDInvalidKey marks a query sample whose query_id was dropped;
// the scraper counts and strips it before emission.
const queryIDInvalidKey = "_dbm_query_id_invalid"

var errInvalidQueryID = errors.New("queryid is not a canonical int64")

// isCanonicalQueryID is a red-phase stub.
func isCanonicalQueryID(string) bool {
	return true
}

// queryIDField is a red-phase stub.
func queryIDField(queryID string) zap.Field {
	return zap.String("queryID", queryID)
}

// recordInvalidQueryIDs is a red-phase stub.
func (*postgreSQLScraper) recordInvalidQueryIDs(context.Context, []map[string]any, metadata.AttributeEvent) {
}

// dropInvalidTopQueryIDs is a red-phase stub.
func (*postgreSQLScraper) dropInvalidTopQueryIDs(_ context.Context, rows []map[string]any, _ *zap.Logger) []map[string]any {
	return rows
}
