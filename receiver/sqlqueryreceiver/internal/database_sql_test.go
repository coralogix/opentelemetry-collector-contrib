// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type poolTestDriver struct{}

func (poolTestDriver) Open(string) (driver.Conn, error) { return poolTestConn{}, nil }

type poolTestConn struct{}

func (poolTestConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (poolTestConn) Close() error                        { return nil }
func (poolTestConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (poolTestConn) Query(string, []driver.Value) (driver.Rows, error) {
	return poolTestRows{}, nil
}

type poolTestRows struct{}

func (poolTestRows) Columns() []string         { return []string{"a"} }
func (poolTestRows) Close() error              { return nil }
func (poolTestRows) Next([]driver.Value) error { return io.EOF }

var registerPoolTest sync.Once

func poolTestOpener(t *testing.T) func(string, string) (*sql.DB, error) {
	t.Helper()
	registerPoolTest.Do(func() { sql.Register("dbmp6pooltest", poolTestDriver{}) })
	return sql.Open
}

func idleAfterOneQuery(t *testing.T, maxIdle *int) int {
	t.Helper()
	db, err := NewPool(poolTestOpener(t), "dbmp6pooltest", "x", 1, maxIdle).DB()
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query("select 1")
	require.NoError(t, err)
	require.NoError(t, rows.Close())
	return db.Stats().Idle
}

// TestPoolMaxIdleConns (DBM P6): max_idle_conn 0 keeps no idle connection;
// unset keeps the database/sql default (one idle here, capped by max open 1).
func TestPoolMaxIdleConns(t *testing.T) {
	zero := 0
	assert.Equal(t, 0, idleAfterOneQuery(t, &zero))
	assert.Equal(t, 1, idleAfterOneQuery(t, nil))
}

// TestPoolDoesNotCacheAnOpenError: a failed open is retried on the next call.
func TestPoolDoesNotCacheAnOpenError(t *testing.T) {
	registerPoolTest.Do(func() { sql.Register("dbmp6pooltest", poolTestDriver{}) })
	calls := 0
	opener := func(d, dsn string) (*sql.DB, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("down")
		}
		return sql.Open(d, dsn)
	}
	p := NewPool(opener, "dbmp6pooltest", "x", 1, nil)
	_, err := p.DB()
	require.Error(t, err)
	db, err := p.DB()
	require.NoError(t, err)
	require.NotNil(t, db)
	_ = db.Close()
	assert.Equal(t, 2, calls)
}
