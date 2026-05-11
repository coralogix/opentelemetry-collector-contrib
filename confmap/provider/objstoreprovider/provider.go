//go:generate make mdatagen

package objstoreprovider // import "github.com/open-telemetry/opentelemetry-collector-contrib/confmap/provider/objstoreprovider"

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/go-kit/log"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/client"
	"go.opentelemetry.io/collector/confmap"
	"gopkg.in/yaml.v2"
)

const (
	schemeName       = "objstore"
	typeQueryName    = "type"
	configPathEnvVar = "OBJSTORE_CONFIG_PATH"
	componentName    = "confmap/objstoreprovider"
)

type objectStoreBucket interface {
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	Close() error
}

type newBucketFunc func(providerType string, config []byte) (objectStoreBucket, error)

type provider struct {
	newBucket newBucketFunc
}

// NewFactory returns a new confmap.ProviderFactory that creates a confmap.Provider
// which reads configuration from an object store supported by github.com/thanos-io/objstore.
//
// This Provider supports "objstore" scheme, and can be called with a URI that follows:
//
//	objstore-uri = "objstore:" object-name "?type=" provider-type
//
// The OBJSTORE_CONFIG_PATH environment variable must point to a Thanos objstore YAML
// configuration file. The type query parameter in the URI selects the object storage
// provider used with that config file. The object-name is the object key containing Collector YAML.
//
// Examples:
// `objstore:configs/otel.yaml?type=s3`
func NewFactory() confmap.ProviderFactory {
	return confmap.NewProviderFactory(newProvider)
}

func newProvider(confmap.ProviderSettings) confmap.Provider {
	return &provider{newBucket: newObjstoreBucket}
}

func newObjstoreBucket(providerType string, config []byte) (objectStoreBucket, error) {
	var providerConfig any
	if err := yaml.Unmarshal(config, &providerConfig); err != nil {
		return nil, fmt.Errorf("failed to parse objstore config: %w", err)
	}

	return client.NewBucketFromConfig(log.NewNopLogger(), &client.BucketConfig{
		Type:   objstore.ObjProvider(providerType),
		Config: providerConfig,
	}, componentName, nil)
}

func (p *provider) Retrieve(ctx context.Context, uri string, _ confmap.WatcherFunc) (*confmap.Retrieved, error) {
	providerType, objectName, err := parseURI(uri)
	if err != nil {
		return nil, err
	}

	configPath, ok := os.LookupEnv(configPathEnvVar)
	if !ok || configPath == "" {
		return nil, fmt.Errorf("env var %q must be set for %q provider", configPathEnvVar, schemeName)
	}

	config, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read objstore config file %q: %w", configPath, err)
	}

	bucket, err := p.newBucket(providerType, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create objstore bucket from config %q: %w", configPath, err)
	}
	defer bucket.Close()

	reader, err := bucket.Get(ctx, objectName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch objstore object %q: %w", objectName, err)
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read objstore object %q: %w", objectName, err)
	}

	return confmap.NewRetrievedFromYAML(content)
}

func (*provider) Scheme() string {
	return schemeName
}

func (*provider) Shutdown(context.Context) error {
	return nil
}

func parseURI(uri string) (string, string, error) {
	if !strings.HasPrefix(uri, schemeName+":") {
		return "", "", fmt.Errorf("%q uri is not supported by %q provider", uri, schemeName)
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse objstore uri %q: %w", uri, err)
	}
	if parsed.Scheme != schemeName {
		return "", "", fmt.Errorf("%q uri is not supported by %q provider", uri, schemeName)
	}

	providerType := parsed.Query().Get(typeQueryName)
	if providerType == "" {
		return "", "", fmt.Errorf("%q uri must include a non-empty %q query parameter", uri, typeQueryName)
	}

	objectName := objectNameFromURL(parsed)
	objectName, err = url.PathUnescape(objectName)
	if err != nil {
		return "", "", fmt.Errorf("failed to unescape objstore object name in uri %q: %w", uri, err)
	}
	if objectName == "" {
		return "", "", fmt.Errorf("%q uri must include a non-empty object name", uri)
	}

	return providerType, objectName, nil
}

func objectNameFromURL(parsed *url.URL) string {
	if parsed.Opaque != "" {
		return parsed.Opaque
	}
	if parsed.Host != "" {
		return strings.TrimPrefix(parsed.Host+parsed.Path, "/")
	}
	return strings.TrimPrefix(parsed.Path, "/")
}
