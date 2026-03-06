// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package config

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestResolveURIs_FileAndS3Sources(t *testing.T) {
	rustFS := StartRustFS(t)

	s3Bucket := fmt.Sprintf("opampsupervisor-%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	s3Key := "configs/s3.yaml"

	s3Config := []byte(`
extensions:
  health_check/s3:
    endpoint: "localhost:13132"
`)
	rustFS.UploadObject(t, s3Bucket, s3Key, s3Config)
	rustFS.SetTestEnv(t)

	localConfigFile, err := os.CreateTemp(t.TempDir(), "local-config-*.yaml")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = localConfigFile.Close()
	})

	localConfig := []byte(`
extensions:
  health_check/file:
    endpoint: "localhost:13131"
`)
	_, err = localConfigFile.Write(localConfig)
	require.NoError(t, err)

	s3URI := rustFS.S3URI(s3Bucket, s3Key)
	conf, err := ResolveURIs([]string{localConfigFile.Name(), s3URI})
	require.NoError(t, err)

	require.Equal(t, "localhost:13131", conf.Get("extensions::health_check/file::endpoint"))
	require.Equal(t, "localhost:13132", conf.Get("extensions::health_check/s3::endpoint"))
}
