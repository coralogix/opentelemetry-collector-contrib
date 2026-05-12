package objstoreprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/confmap"
)

type testBucket struct {
	objects       map[string]string
	requestedName string
	closed        bool
	getErr        error
}

func (b *testBucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.requestedName = name
	if b.getErr != nil {
		return nil, b.getErr
	}
	content, ok := b.objects[name]
	if !ok {
		return nil, fmt.Errorf("object %q not found", name)
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (b *testBucket) Close() error {
	b.closed = true
	return nil
}

func writeFilesystemObjstoreConfig(t *testing.T, bucketDir string) string {
	configPath := filepath.Join(t.TempDir(), "objstore.yaml")
	require.NoError(t, os.WriteFile(configPath, fmt.Appendf(nil, `
type: FILESYSTEM
config:
  directory: %q
`, bucketDir), 0o600))
	return configPath
}

func TestRetrieveFromFilesystemBucket(t *testing.T) {
	bucketDir := t.TempDir()
	configDir := filepath.Join(bucketDir, "configs")
	require.NoError(t, os.MkdirAll(configDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "otel.yaml"), []byte(`
receivers:
  nop:
exporters:
  nop:
service:
  pipelines:
    metrics:
      receivers: [nop]
      exporters: [nop]
`), 0o600))

	t.Setenv(configPathEnvVar, writeFilesystemObjstoreConfig(t, bucketDir))

	fp := NewFactory().Create(confmap.ProviderSettings{})
	retrieved, err := fp.Retrieve(
		t.Context(),
		"objstore:configs/otel.yaml?type=filesystem",
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, fp.Shutdown(t.Context()))

	raw, err := retrieved.AsRaw()
	require.NoError(t, err)
	conf, ok := raw.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, conf, "receivers")
	assert.Contains(t, conf, "exporters")
	assert.Contains(t, conf, "service")
}

func TestResolverWithObjstoreURI(t *testing.T) {
	bucketDir := t.TempDir()
	configDir := filepath.Join(bucketDir, "configs")
	require.NoError(t, os.MkdirAll(configDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "otel.yaml"), []byte(`
receivers:
  nop:
exporters:
  nop:
service:
  pipelines:
    metrics:
      receivers: [nop]
      exporters: [nop]
`), 0o600))

	t.Setenv(configPathEnvVar, writeFilesystemObjstoreConfig(t, bucketDir))

	resolver, err := confmap.NewResolver(confmap.ResolverSettings{
		URIs:              []string{"objstore:configs/otel.yaml?type=filesystem"},
		ProviderFactories: []confmap.ProviderFactory{NewFactory()},
	})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resolver.Shutdown(t.Context()))
	}()

	conf, err := resolver.Resolve(t.Context())
	require.NoError(t, err)
	assert.Contains(t, conf.ToStringMap(), "service")
}

func TestRetrieveUsesConfigFileAndObjectName(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "objstore.yaml")
	configContent := []byte("directory: /tmp\n")
	require.NoError(t, os.WriteFile(configPath, configContent, 0o600))
	t.Setenv(configPathEnvVar, configPath)

	bucket := &testBucket{objects: map[string]string{
		"nested/otel.yaml": "receivers:\n  nop:\n",
	}}
	fp := &provider{newBucket: func(providerType string, config []byte) (objectStoreBucket, error) {
		assert.Equal(t, "filesystem", providerType)
		assert.Equal(t, configContent, config)
		return bucket, nil
	}}

	retrieved, err := fp.Retrieve(t.Context(), "objstore:nested/otel.yaml?type=filesystem", nil)
	require.NoError(t, err)
	require.True(t, bucket.closed)
	assert.Equal(t, "nested/otel.yaml", bucket.requestedName)

	raw, err := retrieved.AsRaw()
	require.NoError(t, err)
	assert.Contains(t, raw, "receivers")
}

func TestNewObjstoreBucketValidatesConfigType(t *testing.T) {
	bucketDir := t.TempDir()

	bucket, err := newObjstoreBucket("filesystem", fmt.Appendf(nil, `
type: FILESYSTEM
config:
  directory: %q
`, bucketDir))
	require.NoError(t, err)
	require.NoError(t, bucket.Close())

	_, err = newObjstoreBucket("s3", fmt.Appendf(nil, `
type: FILESYSTEM
config:
  directory: %q
`, bucketDir))
	require.ErrorContains(t, err, `objstore config type "FILESYSTEM" does not match type "s3" in URI`)
}

func TestRetrieveErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "objstore.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("directory: /tmp\n"), 0o600))

	tests := []struct {
		name      string
		uri       string
		newBucket newBucketFunc
	}{
		{
			name: "unsupported scheme",
			uri:  "file:otel.yaml",
		},
		{
			name: "missing type",
			uri:  "objstore:otel.yaml",
		},
		{
			name: "empty type",
			uri:  "objstore:otel.yaml?type=",
		},
		{
			name: "missing object",
			uri:  "objstore:?type=filesystem",
		},
		{
			name: "missing env var",
			uri:  "objstore:otel.yaml?type=filesystem",
		},
		{
			name: "missing config file",
			uri:  "objstore:otel.yaml?type=filesystem",
		},
		{
			name: "bucket factory error",
			uri:  "objstore:otel.yaml?type=filesystem",
			newBucket: func(string, []byte) (objectStoreBucket, error) {
				return nil, errors.New("factory failed")
			},
		},
		{
			name: "object get error",
			uri:  "objstore:otel.yaml?type=filesystem",
			newBucket: func(string, []byte) (objectStoreBucket, error) {
				return &testBucket{getErr: errors.New("get failed")}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.name {
			case "missing env var":
			case "missing config file":
				t.Setenv(configPathEnvVar, filepath.Join(t.TempDir(), "missing.yaml"))
			default:
				t.Setenv(configPathEnvVar, configPath)
			}

			fp := &provider{newBucket: tt.newBucket}
			if fp.newBucket == nil {
				fp.newBucket = func(string, []byte) (objectStoreBucket, error) {
					return &testBucket{objects: map[string]string{}}, nil
				}
			}

			_, err := fp.Retrieve(t.Context(), tt.uri, nil)
			require.Error(t, err)
			require.NoError(t, fp.Shutdown(t.Context()))
		})
	}
}

func TestParseURI(t *testing.T) {
	tests := []struct {
		name       string
		uri        string
		provider   string
		objectName string
	}{
		{
			name:       "confmap-compatible uri",
			uri:        "objstore:configs/otel.yaml?type=filesystem",
			provider:   "filesystem",
			objectName: "configs/otel.yaml",
		},
		{
			name:       "absolute path style object name",
			uri:        "objstore:/configs/otel.yaml?type=filesystem",
			provider:   "filesystem",
			objectName: "configs/otel.yaml",
		},
		{
			name:       "object name with colon",
			uri:        "objstore:configs/otel:prod.yaml?type=s3",
			provider:   "s3",
			objectName: "configs/otel:prod.yaml",
		},
		{
			name:       "escaped object name",
			uri:        "objstore:" + url.PathEscape("configs/nested otel.yaml") + "?type=filesystem",
			provider:   "filesystem",
			objectName: "configs/nested otel.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProvider, gotObjectName, err := parseURI(tt.uri)
			require.NoError(t, err)
			assert.Equal(t, tt.provider, gotProvider)
			assert.Equal(t, tt.objectName, gotObjectName)
		})
	}
}

func TestScheme(t *testing.T) {
	fp := NewFactory().Create(confmap.ProviderSettings{})
	assert.Equal(t, "objstore", fp.Scheme())
	require.NoError(t, fp.Shutdown(t.Context()))
}

func TestFactory(t *testing.T) {
	p := NewFactory().Create(confmap.ProviderSettings{})
	_, ok := p.(*provider)
	require.True(t, ok)
}
