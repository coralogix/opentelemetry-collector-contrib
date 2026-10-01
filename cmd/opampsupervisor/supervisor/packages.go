// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	conventions "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/archive"
	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/verifier"
)

const (
	// agentPackageName is the only supported package: the top-level package
	// containing the agent. The OpAMP spec allows an empty name when there is a
	// single top-level package.
	agentPackageName = ""

	packageStateFileName        = "package_state.yaml"
	packageStatusesFileName     = "last_reported_package_statuses.binpb"
	packageVerifierCacheDirName = "sigstore"

	// maxPackageBytes is the maximum size of a downloaded package. The package is
	// held in memory while it is verified and extracted.
	maxPackageBytes = 512 << 20 // 512 MiB
)

// errAgentUnhealthy is returned by the install function when the new agent binary
// did not become healthy and was rolled back. Packages failing this way are not
// installed again.
var errAgentUnhealthy = errors.New("new agent binary did not become healthy")

// installFunc swaps in the agent binary staged at stagedPath and restarts the agent.
type installFunc func(ctx context.Context, stagedPath string) error

// packageManager manages the persistent state of downloadable packages.
// Currently only allows for a single top-level package containing the agent.
// It verifies and stages new agent binaries; installing them is delegated to install.
type packageManager struct {
	logger       *zap.Logger
	statePath    string
	statusesPath string
	cacheDir     string
	executable   string
	agentBinary  string
	verifierCfg  config.Verifier
	// bootVersion is the agent version reported by the bootstrap agent. It is
	// reported when no package install has been recorded.
	bootVersion string
	install     installFunc

	// mu guards state and both state files. opamp-go may call SetLastReportedStatuses
	// from its download reporter goroutine while the sync goroutine runs.
	mu    sync.Mutex
	state packageState
}

var _ types.PackagesStateProvider = &packageManager{}

// packageState is the persistent state of the agent package.
type packageState struct {
	AllPackagesHash hexBytes `yaml:"all_packages_hash,omitempty"`
	// Hash and Version are the server provided hash and version of the installed package.
	Hash    hexBytes `yaml:"hash,omitempty"`
	Version string   `yaml:"version,omitempty"`
	// ContentHash is the SHA-256 of the downloaded package file.
	ContentHash hexBytes `yaml:"content_hash,omitempty"`
	// ExecutableSHA256 is the SHA-256 of the agent executable installed from the
	// package. It detects executables replaced outside the Supervisor.
	ExecutableSHA256 hexBytes `yaml:"executable_sha256,omitempty"`
	// FailedContentHashes are the content hashes of packages whose agent binary did
	// not become healthy. They are not installed again.
	FailedContentHashes []hexBytes `yaml:"failed_content_hashes,omitempty"`
}

// hexBytes marshals bytes as a hex string for human readability.
type hexBytes []byte

func (h hexBytes) MarshalYAML() (any, error) {
	return hex.EncodeToString(h), nil
}

func (h *hexBytes) UnmarshalYAML(value *yaml.Node) error {
	b, err := hex.DecodeString(value.Value)
	if err != nil {
		return err
	}
	*h = b
	return nil
}

func newPackageManager(logger *zap.Logger, storageDir string, agentCfg config.Agent, bootVersion string, install installFunc) (*packageManager, error) {
	p := &packageManager{
		logger:       logger,
		statePath:    filepath.Join(storageDir, packageStateFileName),
		statusesPath: filepath.Join(storageDir, packageStatusesFileName),
		cacheDir:     filepath.Join(storageDir, packageVerifierCacheDirName),
		executable:   agentCfg.Executable,
		agentBinary:  agentCfg.Package.AgentBinary,
		verifierCfg:  agentCfg.Package.Verifier,
		bootVersion:  bootVersion,
		install:      install,
	}

	stateBytes, err := os.ReadFile(p.statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("read package state: %w", err)
	default:
		if err = yaml.Unmarshal(stateBytes, &p.state); err != nil {
			return nil, fmt.Errorf("parse package state: %w", err)
		}
	}

	if p.state.ExecutableSHA256 == nil {
		return p, nil
	}
	exeHash, err := fileSHA256(p.executable)
	if err != nil {
		return nil, fmt.Errorf("hash agent executable: %w", err)
	}
	if !bytes.Equal(exeHash, p.state.ExecutableSHA256) {
		// The recorded package is no longer the running agent. Forget it so the
		// server's next offer is installed instead of skipped as up to date.
		logger.Info("Agent executable was replaced outside of the Supervisor, forgetting the installed package")
		p.state = packageState{FailedContentHashes: p.state.FailedContentHashes}
		if err := p.writeState(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *packageManager) AllPackagesHash() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.AllPackagesHash, nil
}

func (p *packageManager) SetAllPackagesHash(hash []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.AllPackagesHash = hash
	return p.writeState()
}

func (*packageManager) Packages() ([]string, error) {
	return []string{agentPackageName}, nil
}

func (p *packageManager) PackageState(packageName string) (types.PackageState, error) {
	if packageName != agentPackageName {
		return types.PackageState{Exists: false}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	version := p.state.Version
	if version == "" {
		version = p.bootVersion
	}
	return types.PackageState{
		Exists:  true,
		Type:    protobufs.PackageType_PackageType_TopLevel,
		Hash:    p.state.Hash,
		Version: version,
	}, nil
}

func (p *packageManager) SetPackageState(packageName string, state types.PackageState) error {
	if packageName != agentPackageName {
		return fmt.Errorf("package %q is not supported, only the top-level agent package is", packageName)
	}
	if state.Type != protobufs.PackageType_PackageType_TopLevel {
		return errors.New("agent package must be a top-level package")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Hash = state.Hash
	p.state.Version = state.Version
	return p.writeState()
}

func (*packageManager) CreatePackage(packageName string, _ protobufs.PackageType) error {
	return fmt.Errorf("cannot create package %q: only the top-level agent package (empty name) is supported", packageName)
}

func (p *packageManager) FileContentHash(packageName string) ([]byte, error) {
	if packageName != agentPackageName {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.ContentHash, nil
}

// UpdateContent verifies the downloaded package, extracts the agent binary next
// to the agent executable and installs it. It returns once the new agent is
// healthy, or once it has been rolled back.
func (p *packageManager) UpdateContent(ctx context.Context, packageName, downloadURL string, data io.Reader, contentHash, signature []byte) error {
	if packageName != agentPackageName {
		return fmt.Errorf("package %q is not supported, only the top-level agent package is", packageName)
	}
	if p.failedBefore(contentHash) {
		return fmt.Errorf("package with content hash %x failed to install before, not retrying", contentHash)
	}

	pkg, err := io.ReadAll(io.LimitReader(data, maxPackageBytes+1))
	if err != nil {
		return fmt.Errorf("read package: %w", err)
	}
	if len(pkg) > maxPackageBytes {
		return fmt.Errorf("package exceeds maximum size of %d bytes", maxPackageBytes)
	}
	if actual := sha256.Sum256(pkg); !bytes.Equal(actual[:], contentHash) {
		return fmt.Errorf("package content hash %x does not match the offered content hash %x", actual, contentHash)
	}

	v, err := verifier.NewVerifier(p.verifierCfg, p.cacheDir)
	if err != nil {
		return fmt.Errorf("create package verifier: %w", err)
	}
	if err = v.Verify(pkg, signature); err != nil {
		return fmt.Errorf("verify package signature: %w", err)
	}

	extractor, err := archive.NewExtractor(archive.FormatFromURL(downloadURL))
	if err != nil {
		return err
	}
	stagedPath := p.executable + ".new"
	defer os.Remove(stagedPath) // no-op once the staged binary is installed
	if err = extractor.Extract(ctx, pkg, p.agentBinary, stagedPath); err != nil {
		return fmt.Errorf("extract agent binary: %w", err)
	}
	exeHash, err := fileSHA256(stagedPath)
	if err != nil {
		return fmt.Errorf("hash staged agent binary: %w", err)
	}

	if err = p.install(ctx, stagedPath); err != nil {
		if errors.Is(err, errAgentUnhealthy) {
			p.mu.Lock()
			p.state.FailedContentHashes = append(p.state.FailedContentHashes, contentHash)
			if writeErr := p.writeState(); writeErr != nil {
				err = errors.Join(err, writeErr)
			}
			p.mu.Unlock()
		}
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.ContentHash = contentHash
	p.state.ExecutableSHA256 = exeHash
	return p.writeState()
}

func (*packageManager) DeletePackage(packageName string) error {
	if packageName == agentPackageName {
		return errors.New("cannot delete the top-level agent package")
	}
	// Other packages are never created, so there is nothing to delete.
	return nil
}

func (p *packageManager) LastReportedStatuses() (*protobufs.PackageStatuses, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	statusBytes, err := os.ReadFile(p.statusesPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last reported package statuses: %w", err)
	}
	statuses := &protobufs.PackageStatuses{}
	if err := proto.Unmarshal(statusBytes, statuses); err != nil {
		return nil, fmt.Errorf("parse last reported package statuses: %w", err)
	}
	return statuses, nil
}

func (p *packageManager) SetLastReportedStatuses(statuses *protobufs.PackageStatuses) error {
	statusBytes, err := proto.Marshal(statuses)
	if err != nil {
		return fmt.Errorf("marshal package statuses: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return os.WriteFile(p.statusesPath, statusBytes, 0o600)
}

func (p *packageManager) failedBefore(contentHash []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.ContainsFunc(p.state.FailedContentHashes, func(h hexBytes) bool {
		return bytes.Equal(h, contentHash)
	})
}

// writeState persists the package state. The caller must hold p.mu.
func (p *packageManager) writeState() error {
	stateBytes, err := yaml.Marshal(p.state)
	if err != nil {
		return fmt.Errorf("marshal package state: %w", err)
	}
	if err := os.WriteFile(p.statePath, stateBytes, 0o600); err != nil {
		return fmt.Errorf("write package state: %w", err)
	}
	return nil
}

// agentVersion returns the service.version identifying attribute of ad.
func agentVersion(ad *protobufs.AgentDescription) string {
	for _, attr := range ad.GetIdentifyingAttributes() {
		if attr.Key == string(conventions.ServiceVersionKey) {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
