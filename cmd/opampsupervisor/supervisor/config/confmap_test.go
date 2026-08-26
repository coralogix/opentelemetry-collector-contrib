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
	"go.opentelemetry.io/collector/confmap"
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
			name:         "env URI keeps env scheme",
			uri:          "env:OTELCOL_CONFIG",
			wantURI:      "env:OTELCOL_CONFIG",
			wantProvider: "env",
		},
		{
			name:         "s3 URI keeps s3 scheme",
			uri:          "s3://bucket.s3.us-east-1.amazonaws.com/config.yaml",
			wantURI:      "s3://bucket.s3.us-east-1.amazonaws.com/config.yaml",
			wantProvider: "s3",
		},
		{
			name:         "objstore URI keeps objstore scheme",
			uri:          "objstore:configs/otel.yaml",
			wantURI:      "objstore:configs/otel.yaml",
			wantProvider: "objstore",
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
		_ = os.Unsetenv(envVar)

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

	configPath := filepath.Join(t.TempDir(), "config:prod.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("extensions:\n  health_check:\n    endpoint: localhost:13133\n"), 0o600))

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

func TestRetrieveURIAsConf_ObjstoreURI_NoConfigType(t *testing.T) {
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
config:
  directory: %q
`, bucketDir), 0o600))
	t.Setenv("OBJSTORE_CONFIG_PATH", objstoreConfigPath)

	configWithQuery, err := RetrieveURIAsConf("objstore:configs/otel.yaml?type=filesystem", nil)
	require.NoError(t, err)
	require.Equal(t, "localhost:13134", configWithQuery.Get("extensions::health_check/objstore::endpoint"))

	_, err = RetrieveURIAsConf("objstore:configs/otel.yaml", nil)
	require.Error(t, err)
}

func TestRetrieveURIAsConf_FileNotFound(t *testing.T) {
	_, err := RetrieveURIAsConf(filepath.Join(t.TempDir(), "missing.yaml"), nil)
	require.ErrorIs(t, err, ErrConfigFileNotFound)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestResolveURIs_ExpandsEnvByDefaultScheme(t *testing.T) {
	const envVar = "OTELCOLCONTRIB_OPAMPSUPERVISOR_TEST_ENDPOINT"
	const envValue = "ws://localhost/v1/opamp"

	t.Setenv(envVar, envValue)

	tempDir := t.TempDir()
	// Do a chdir into a temp dir on Windows so [filepath.Rel], used under the
	// hood by confmap, does not cross volumes when the repository and temporary
	// directory use different drives. In the Windows CI, for some reason, the
	// repository is being cloned on the `D:` drive, but `t.TempDir()` is on `C:`.
	if runtime.GOOS == "windows" {
		t.Chdir(tempDir)
	}

	configPath := filepath.Join(tempDir, "supervisor.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  endpoint: ${"+envVar+"}\n"), 0o600))
	workingDir, err := os.Getwd()
	require.NoError(t, err)
	relativeConfigPath, err := filepath.Rel(workingDir, configPath)
	require.NoError(t, err)

	conf, err := ResolveURI(relativeConfigPath)
	require.NoError(t, err)
	require.Equal(t, envValue, conf.Get("server::endpoint"))
}

func TestMergeConf_PreservesServiceExtensions(t *testing.T) {
	base := confmap.NewFromStringMap(map[string]any{
		"service": map[string]any{
			"extensions": []any{"health_check", "opamp"},
		},
	})
	incoming := confmap.NewFromStringMap(map[string]any{
		"service": map[string]any{
			"extensions": []any{"opamp", "pprof"},
		},
	})

	require.NoError(t, MergeConf(base, incoming))
	require.Equal(t, []any{"health_check", "opamp", "pprof"}, base.Get("service::extensions"))
}

func TestNewConfFromYAML_Empty(t *testing.T) {
	conf, err := NewConfFromYAML(nil)
	require.NoError(t, err)
	require.Empty(t, conf.ToStringMap())
}
