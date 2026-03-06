// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package config

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	rustFSImage     = "rustfs/rustfs:latest"
	rustFSRegion    = "us-east-1"
	rustFSAccessKey = "opampsupervisor-test-access"
	rustFSSecretKey = "opampsupervisor-test-secret"
)

type RustFS struct {
	endpoint string
}

func StartRustFS(t *testing.T) *RustFS {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker command is not available: %v", err)
	}

	infoCmd := exec.Command("docker", "info")
	if output, err := infoCmd.CombinedOutput(); err != nil {
		t.Skipf("docker daemon is not available: %v (output: %s)", err, strings.TrimSpace(string(output)))
	}

	containerName := fmt.Sprintf("opampsupervisor-rustfs-%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	runCmd := exec.Command(
		"docker",
		"run",
		"-d",
		"--rm",
		"--name", containerName,
		"-p", "127.0.0.1::9000",
		"-e", "RUSTFS_ACCESS_KEY="+rustFSAccessKey,
		"-e", "RUSTFS_SECRET_KEY="+rustFSSecretKey,
		rustFSImage,
	)
	output, err := runCmd.CombinedOutput()
	require.NoErrorf(t, err, "failed to start rustfs container: %s", strings.TrimSpace(string(output)))

	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	})

	const portInspectTemplate = "{{(index (index .NetworkSettings.Ports \"9000/tcp\") 0).HostPort}}"
	portCmd := exec.Command("docker", "inspect", "-f", portInspectTemplate, containerName)
	portOutput, err := portCmd.CombinedOutput()
	require.NoErrorf(t, err, "failed to inspect rustfs container port: %s", strings.TrimSpace(string(portOutput)))

	port := strings.TrimSpace(string(portOutput))
	require.NotEmpty(t, port)

	return &RustFS{endpoint: "http://127.0.0.1:" + port}
}

func (r *RustFS) SetTestEnv(t *testing.T) {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", rustFSAccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", rustFSSecretKey)
	t.Setenv("AWS_REGION", rustFSRegion)
	t.Setenv("AWS_DEFAULT_REGION", rustFSRegion)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL_S3", r.endpoint)
}

func (r *RustFS) UploadObject(t *testing.T, bucket, key string, body []byte) {
	t.Helper()

	awsCfg, err := awsconfig.LoadDefaultConfig(
		t.Context(),
		awsconfig.WithRegion(rustFSRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(rustFSAccessKey, rustFSSecretKey, "")),
	)
	require.NoError(t, err)

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(r.endpoint)
		o.UsePathStyle = true
	})

	require.Eventually(t, func() bool {
		_, createErr := s3Client.CreateBucket(t.Context(), &s3.CreateBucketInput{
			Bucket: aws.String(bucket),
		})
		if createErr != nil {
			t.Logf("retrying rustfs bucket creation: %v", createErr)
			return false
		}
		return true
	}, 3*time.Second, 100*time.Millisecond)

	require.Eventually(t, func() bool {
		_, putErr := s3Client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		})
		if putErr != nil {
			t.Logf("retrying rustfs object upload: %v", putErr)
			return false
		}
		return true
	}, 3*time.Second, 100*time.Millisecond)
}

func (r *RustFS) S3URI(bucket, key string) string {
	return fmt.Sprintf("s3://%s.s3.%s.amazonaws.com/%s", bucket, rustFSRegion, key)
}
