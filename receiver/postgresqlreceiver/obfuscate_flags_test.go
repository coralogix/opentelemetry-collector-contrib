// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package postgresqlreceiver

import (
	"strings"
	"testing"

	"github.com/DataDog/datadog-agent/pkg/obfuscate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DBM P7e: the legacy tokenizer (no ObfuscationMode) always turns NULL, TRUE
// and FALSE into "?" (datadog-agent pkg/obfuscate sql.go:195-197 at
// e02b9359151a); KeepNull and KeepBoolean are read only on the sqllexer path.
// The flags are therefore not set, and these tests pin the behaviour, so a
// later switch to sqllexer cannot silently start keeping the values. The DBM
// edge processor (coralogix-otel-collector processor/dbmprocessor) carries the
// same configuration and the same tests.
const obfuscateFlagsStatement = "SELECT a FROM t WHERE b IS NULL AND c = TRUE AND d = FALSE AND e = NULL AND f = true"

func assertNoNullOrBoolean(t *testing.T, out string) {
	t.Helper()
	for _, w := range strings.Fields(strings.ToLower(out)) {
		assert.NotContains(t, []string{"null", "true", "false"}, strings.Trim(w, "(),;"), out)
	}
	assert.Equal(t, 5, strings.Count(out, "?"), out)
}

func TestObfuscateNullAndBooleanBecomePlaceholders(t *testing.T) {
	out, err := obfuscateSQL(obfuscateFlagsStatement)
	require.NoError(t, err)
	assertNoNullOrBoolean(t, out)
}

func TestObfuscatorNullAndBooleanFlagsNotSet(t *testing.T) {
	assert.False(t, obfuscatorSQLConfig.KeepNull, "KeepNull is a no-op in legacy mode and would keep NULL under sqllexer")
	assert.False(t, obfuscatorSQLConfig.KeepBoolean, "KeepBoolean is a no-op in legacy mode and would keep TRUE/FALSE under sqllexer")
	assert.Empty(t, obfuscatorSQLConfig.ObfuscationMode, "the legacy tokenizer")
	assert.Equal(t, "postgresql", obfuscatorSQLConfig.DBMS)
	assert.True(t, obfuscatorSQLConfig.KeepSQLAlias)
}

// A switch to sqllexer with this configuration still replaces the values.
func TestObfuscateNullAndBooleanSurviveALexerSwitch(t *testing.T) {
	for _, mode := range []obfuscate.ObfuscationMode{obfuscate.ObfuscateOnly, obfuscate.ObfuscateAndNormalize} {
		cfg := obfuscatorSQLConfig
		cfg.ObfuscationMode = mode
		o := obfuscate.NewObfuscator(obfuscate.Config{SQL: cfg})
		q, err := o.ObfuscateSQLString(obfuscateFlagsStatement)
		o.Stop()
		require.NoError(t, err, mode)
		t.Run(string(mode), func(t *testing.T) { assertNoNullOrBoolean(t, q.Query) })
	}
}
