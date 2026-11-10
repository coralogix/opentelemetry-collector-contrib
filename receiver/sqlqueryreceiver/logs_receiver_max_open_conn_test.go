// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package sqlqueryreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/sqlqueryreceiver"

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/sqlquery"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/sqlqueryreceiver/internal/metadata"
)

// connCounter is a fake database/sql driver that counts the connections it
// has open and the most it ever had open at once. Every query takes 30 ms and
// returns no rows.
type connCounter struct {
	mu            sync.Mutex
	live, maxLive int
	queries       atomic.Int64
}

func (c *connCounter) Open(string) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live++
	c.maxLive = max(c.maxLive, c.live)
	return &countedConn{c: c}, nil
}

func (c *connCounter) state() (live, maxLive int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live, c.maxLive
}

type countedConn struct{ c *connCounter }

func (*countedConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*countedConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (cc *countedConn) Close() error {
	cc.c.mu.Lock()
	defer cc.c.mu.Unlock()
	cc.c.live--
	return nil
}

func (cc *countedConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	cc.c.queries.Add(1)
	select {
	case <-time.After(30 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return emptyRows{}, nil
}

type emptyRows struct{}

func (emptyRows) Columns() []string         { return []string{"col1"} }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

var (
	registerCounter sync.Once
	counters        sync.Map // dsn -> *connCounter
)

type counterDriver struct{}

func (counterDriver) Open(dsn string) (driver.Conn, error) {
	c, _ := counters.Load(dsn)
	return c.(*connCounter).Open(dsn)
}

// startCountedLogsReceiver starts a logs receiver with three queries on a
// fresh connCounter and returns the receiver, the counter and the number of
// sql.Open calls.
func startCountedLogsReceiver(t *testing.T, maxOpenConn int, maxIdleConn *int) (*logsReceiver, *connCounter, *atomic.Int64) {
	t.Helper()
	registerCounter.Do(func() { sql.Register("dbmp6counter", counterDriver{}) })
	c := &connCounter{}
	counters.Store(t.Name(), c)
	opens := &atomic.Int64{}
	opener := func(driverName, dsn string) (*sql.DB, error) {
		opens.Add(1)
		return sql.Open(driverName, dsn)
	}
	queries := make([]sqlquery.Query, 3)
	for i := range queries {
		queries[i] = sqlquery.Query{SQL: "select 1", Logs: []sqlquery.LogsCfg{{BodyColumn: "col1"}}}
	}
	r, err := createLogsReceiverFunc(opener, sqlquery.NewDbClient)(
		t.Context(),
		receivertest.NewNopSettings(metadata.Type),
		&Config{
			Config: sqlquery.Config{
				ControllerConfig: scraperhelper.ControllerConfig{CollectionInterval: time.Hour, InitialDelay: time.Hour},
				Driver:           "dbmp6counter",
				DataSource:       t.Name(),
				Queries:          queries,
			},
			MaxOpenConn: maxOpenConn,
			MaxIdleConn: maxIdleConn,
		},
		&consumertest.LogsSink{},
	)
	require.NoError(t, err)
	require.NoError(t, r.Start(t.Context(), componenttest.NewNopHost()))
	t.Cleanup(func() { _ = r.Shutdown(context.WithoutCancel(t.Context())) })
	return r.(*logsReceiver), c, opens
}

// TestLogsReceiver_MaxOpenConnBoundsTheReceiver (DBM P6, bug fix): with
// max_open_conn set, the queries of a logs receiver share one pool of at most
// that many connections. Idle connections keep the database/sql default.
func TestLogsReceiver_MaxOpenConnBoundsTheReceiver(t *testing.T) {
	r, c, opens := startCountedLogsReceiver(t, 1, nil)
	r.collect()
	live, maxLive := c.state()
	assert.Equal(t, int64(3), c.queries.Load(), "every query ran")
	assert.Equal(t, int64(1), opens.Load(), "one pool per receiver")
	assert.Equal(t, 1, maxLive, "at most max_open_conn connections at once")
	assert.Equal(t, 1, live, "the idle connection is kept (database/sql default)")
	r.collect()
	_, maxLive = c.state()
	assert.Equal(t, int64(6), c.queries.Load())
	assert.Equal(t, int64(1), opens.Load())
	assert.Equal(t, 1, maxLive)
}

// TestLogsReceiver_MaxIdleConnZeroKeepsNoIdle (DBM P6, opt-in): with
// max_idle_conn 0 the pool keeps no connection between collections.
func TestLogsReceiver_MaxIdleConnZeroKeepsNoIdle(t *testing.T) {
	zero := 0
	r, c, opens := startCountedLogsReceiver(t, 1, &zero)
	r.collect()
	live, maxLive := c.state()
	assert.Equal(t, int64(3), c.queries.Load())
	assert.Equal(t, int64(1), opens.Load())
	assert.Equal(t, 1, maxLive)
	assert.Equal(t, 0, live, "no idle connection kept between collections")
}

// TestLogsReceiver_MaxIdleConnAloneSharesAnUnboundedPool: max_idle_conn
// without max_open_conn shares one pool without a connection cap.
func TestLogsReceiver_MaxIdleConnAloneSharesAnUnboundedPool(t *testing.T) {
	zero := 0
	r, c, opens := startCountedLogsReceiver(t, 0, &zero)
	r.collect()
	live, maxLive := c.state()
	assert.Equal(t, int64(1), opens.Load(), "one pool per receiver")
	assert.Equal(t, 3, maxLive, "no connection cap")
	assert.Equal(t, 0, live)
}

// TestLogsReceiver_NoMaxOpenConnKeepsUpstreamBehaviour: without
// max_open_conn and max_idle_conn every query keeps its own pool, as upstream.
func TestLogsReceiver_NoMaxOpenConnKeepsUpstreamBehaviour(t *testing.T) {
	r, c, opens := startCountedLogsReceiver(t, 0, nil)
	r.collect()
	live, maxLive := c.state()
	assert.Equal(t, int64(3), c.queries.Load())
	assert.Equal(t, int64(3), opens.Load(), "one pool per query")
	assert.Equal(t, 3, maxLive, "the queries run concurrently")
	assert.Equal(t, 3, live, "each pool keeps its idle connection")
}
