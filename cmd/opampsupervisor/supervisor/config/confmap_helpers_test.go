// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRetrieveURIForProvider(t *testing.T) {
	tests := []struct {
		name         string
		uri          string
		wantURI      string
		wantProvider string
	}{
		{
			name:         "absolute path with colon is file",
			uri:          "/etc/otel/configs:prod.yaml",
			wantURI:      "file:/etc/otel/configs:prod.yaml",
			wantProvider: "file",
		},
		{
			name:         "relative path with colon is file",
			uri:          "./config:prod.yaml",
			wantURI:      "file:./config:prod.yaml",
			wantProvider: "file",
		},
		{
			name:         "absolute path is file",
			uri:          "/etc/otel/configs.prod.yaml",
			wantURI:      "file:/etc/otel/configs.prod.yaml",
			wantProvider: "file",
		},
		{
			name:         "relative path is file",
			uri:          "./config.prod.yaml",
			wantURI:      "file:./config.prod.yaml",
			wantProvider: "file",
		},
		{
			name:         "s3 URI keeps s3 scheme",
			uri:          "s3://bucket.s3.us-east-1.amazonaws.com/config.yaml",
			wantURI:      "s3://bucket.s3.us-east-1.amazonaws.com/config.yaml",
			wantProvider: "s3",
		},
		{
			name:         "objstore URI keeps objstore scheme",
			uri:          "objstore:test-config-provider/config.yaml?type=gcs",
			wantURI:      "objstore:test-config-provider/config.yaml?type=gcs",
			wantProvider: "objstore",
		},
		{
			name:         "env URI keeps env scheme",
			uri:          "env:OTELCOL_CONFIG",
			wantURI:      "env:OTELCOL_CONFIG",
			wantProvider: "env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURI, gotProvider := retrieveURIForProvider(tt.uri)
			require.Equal(t, tt.wantURI, gotURI)
			require.Equal(t, tt.wantProvider, gotProvider)
		})
	}
}

func TestRetrieveURIAsConf_EnvURIWithoutLogger_DoesNotPanic(t *testing.T) {
	t.Run("unset environment variable", func(t *testing.T) {
		const envVar = "OTELCOLCONTRIB_OPAMPSUPERVISOR_TEST_UNSET_ENV"

		originalValue, wasSet := os.LookupEnv(envVar)
		require.NoError(t, os.Unsetenv(envVar))
		t.Cleanup(func() {
			if wasSet {
				_ = os.Setenv(envVar, originalValue)
				return
			}
			_ = os.Unsetenv(envVar)
		})

		conf, err := RetrieveURIAsConf("env:"+envVar, nil)
		require.NoError(t, err)
		require.NotNil(t, conf)
		require.Empty(t, conf.ToStringMap())
	})

	t.Run("empty environment variable", func(t *testing.T) {
		const envVar = "OTELCOLCONTRIB_OPAMPSUPERVISOR_TEST_EMPTY_ENV"
		t.Setenv(envVar, "")

		conf, err := RetrieveURIAsConf("env:"+envVar, nil)
		require.NoError(t, err)
		require.NotNil(t, conf)
		require.Empty(t, conf.ToStringMap())
	})
}

func TestRetrieveURIAsConf_FilePathWithColon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path semantics on Windows differ for ':'")
	}

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config:prod.yaml")

	err := os.WriteFile(configPath, []byte("extensions:\n  health_check:\n    endpoint: localhost:13133\n"), 0o600)
	require.NoError(t, err)

	conf, err := RetrieveURIAsConf(configPath, nil)
	require.NoError(t, err)
	require.Equal(t, "localhost:13133", conf.Get("extensions::health_check::endpoint"))
}

func TestRetrieveURIAsConf_ObjstoreURI(t *testing.T) {
	bucketDir := t.TempDir()
	configDir := filepath.Join(bucketDir, "configs")
	require.NoError(t, os.MkdirAll(configDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "otel.yaml"), []byte(`
extensions:
  health_check/objstore:
    endpoint: localhost:13134
`), 0o600))

	objstoreConfigPath := filepath.Join(t.TempDir(), "objstore.yaml")
	require.NoError(t, os.WriteFile(objstoreConfigPath, fmt.Appendf(nil, `
type: FILESYSTEM
config:
  directory: %q
`, bucketDir), 0o600))
	t.Setenv("OBJSTORE_CONFIG_PATH", objstoreConfigPath)

	configWithQuery, err := RetrieveURIAsConf("objstore:configs/otel.yaml?type=filesystem", nil)
	require.NoError(t, err)
	require.Equal(t, "localhost:13134", configWithQuery.Get("extensions::health_check/objstore::endpoint"))

	confWithoutQuery, err := RetrieveURIAsConf("objstore:configs/otel.yaml", nil)
	require.NoError(t, err)
	require.Equal(t, "localhost:13134", confWithoutQuery.Get("extensions::health_check/objstore::endpoint"))
}

func TestResolveURIs_ExpandsEnvByDefaultScheme(t *testing.T) {
	const envVar = "OTELCOLCONTRIB_OPAMPSUPERVISOR_TEST_ENDPOINT"
	const envValue = "ws://localhost/v1/opamp"

	t.Setenv(envVar, envValue)

	configPath := filepath.Join(t.TempDir(), "supervisor.yaml")
	err := os.WriteFile(configPath, []byte("server:\n  endpoint: ${"+envVar+"}\n"), 0o600)
	require.NoError(t, err)

	conf, err := ResolveURI(configPath)
	require.NoError(t, err)
	require.Equal(t, envValue, conf.Get("server::endpoint"))
}
