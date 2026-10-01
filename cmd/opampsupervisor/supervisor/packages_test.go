// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
)

const testAgentBinary = "otelcol-contrib"

type testPackageEnv struct {
	storageDir string
	executable string
	installs   []string
	// installErr is returned by the install func after it records the call.
	installErr error
}

func newTestPackageEnv(t *testing.T) *testPackageEnv {
	t.Helper()
	dir := t.TempDir()
	env := &testPackageEnv{
		storageDir: filepath.Join(dir, "storage"),
		executable: filepath.Join(dir, "agent"),
	}
	require.NoError(t, os.Mkdir(env.storageDir, 0o700))
	require.NoError(t, os.WriteFile(env.executable, []byte("old agent"), 0o600))
	return env
}

func (e *testPackageEnv) newManager(t *testing.T) *packageManager {
	t.Helper()
	agentCfg := config.Agent{
		Executable: e.executable,
		Package: config.AgentPackage{
			AgentBinary: testAgentBinary,
			Verifier:    config.Verifier{Type: config.VerifierTypeNone},
		},
	}
	p, err := newPackageManager(zap.NewNop(), e.storageDir, agentCfg, "0.1.0", e.install)
	require.NoError(t, err)
	return p
}

// install behaves like the Supervisor: it moves the staged binary over the executable.
func (e *testPackageEnv) install(_ context.Context, stagedPath string) error {
	e.installs = append(e.installs, stagedPath)
	if e.installErr != nil {
		return e.installErr
	}
	return os.Rename(stagedPath, e.executable)
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func sha(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestPackageManagerUpdateContent(t *testing.T) {
	pkg := tarGz(t, testAgentBinary, []byte("new agent"))
	const url = "https://example.com/otelcol-contrib_1.0.0_linux_amd64.tar.gz"

	testCases := []struct {
		name        string
		packageName string
		pkg         []byte
		contentHash []byte
		url         string
		expectedErr string
	}{
		{
			name:        "unsupported package name",
			packageName: "other",
			pkg:         pkg,
			contentHash: sha(pkg),
			url:         url,
			expectedErr: `package "other" is not supported`,
		},
		{
			name:        "content hash mismatch",
			pkg:         pkg,
			contentHash: sha([]byte("something else")),
			url:         url,
			expectedErr: "does not match the offered content hash",
		},
		{
			name:        "missing content hash",
			pkg:         pkg,
			url:         url,
			expectedErr: "does not match the offered content hash",
		},
		{
			name:        "archive without the agent binary",
			pkg:         tarGz(t, "README.md", []byte("readme")),
			contentHash: sha(tarGz(t, "README.md", []byte("readme"))),
			url:         url,
			expectedErr: `extract agent binary: read tarball looking for "otelcol-contrib"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestPackageEnv(t)
			p := env.newManager(t)

			err := p.UpdateContent(t.Context(), tc.packageName, tc.url, bytes.NewReader(tc.pkg), tc.contentHash, nil)
			require.ErrorContains(t, err, tc.expectedErr)
			assert.Empty(t, env.installs, "nothing must be installed when the package is rejected")
			exe, err := os.ReadFile(env.executable)
			require.NoError(t, err)
			assert.Equal(t, "old agent", string(exe))
			assert.NoFileExists(t, env.executable+".new")
		})
	}
}

func TestPackageManagerUpdateContentInstallsRawAndTarGz(t *testing.T) {
	testCases := []struct {
		name string
		url  string
		pkg  []byte
	}{
		{name: "tar.gz", url: "https://example.com/agent.tar.gz", pkg: tarGz(t, testAgentBinary, []byte("new agent"))},
		{name: "raw binary", url: "https://example.com/agent", pkg: []byte("new agent")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestPackageEnv(t)
			p := env.newManager(t)

			require.NoError(t, p.UpdateContent(t.Context(), agentPackageName, tc.url, bytes.NewReader(tc.pkg), sha(tc.pkg), nil))
			require.Equal(t, []string{env.executable + ".new"}, env.installs)
			exe, err := os.ReadFile(env.executable)
			require.NoError(t, err)
			assert.Equal(t, "new agent", string(exe))

			hash, err := p.FileContentHash(agentPackageName)
			require.NoError(t, err)
			assert.Equal(t, sha(tc.pkg), hash)
		})
	}
}

func TestPackageManagerUpdateContentDoesNotRetryUnhealthyPackage(t *testing.T) {
	env := newTestPackageEnv(t)
	env.installErr = errAgentUnhealthy
	p := env.newManager(t)
	pkg := tarGz(t, testAgentBinary, []byte("bad agent"))

	err := p.UpdateContent(t.Context(), agentPackageName, "https://example.com/a.tar.gz", bytes.NewReader(pkg), sha(pkg), nil)
	require.ErrorIs(t, err, errAgentUnhealthy)
	assert.NoFileExists(t, env.executable+".new", "the staged binary must be cleaned up")

	// The failure is remembered across restarts.
	p = env.newManager(t)
	err = p.UpdateContent(t.Context(), agentPackageName, "https://example.com/a.tar.gz", bytes.NewReader(pkg), sha(pkg), nil)
	require.ErrorContains(t, err, "failed to install before, not retrying")
	assert.Len(t, env.installs, 1, "a package that failed must not be installed again")
}

func TestPackageManagerUpdateContentRetriesAfterOtherErrors(t *testing.T) {
	env := newTestPackageEnv(t)
	env.installErr = errSupervisorShutdown
	p := env.newManager(t)
	pkg := tarGz(t, testAgentBinary, []byte("new agent"))

	err := p.UpdateContent(t.Context(), agentPackageName, "https://example.com/a.tar.gz", bytes.NewReader(pkg), sha(pkg), nil)
	require.ErrorIs(t, err, errSupervisorShutdown)

	env.installErr = nil
	require.NoError(t, p.UpdateContent(t.Context(), agentPackageName, "https://example.com/a.tar.gz", bytes.NewReader(pkg), sha(pkg), nil))
	assert.Len(t, env.installs, 2)
}

func TestPackageManagerState(t *testing.T) {
	env := newTestPackageEnv(t)
	p := env.newManager(t)

	state, err := p.PackageState(agentPackageName)
	require.NoError(t, err)
	assert.Equal(t, types.PackageState{Exists: true, Type: protobufs.PackageType_PackageType_TopLevel, Version: "0.1.0"}, state,
		"without a recorded install the bootstrap version is reported")

	state, err = p.PackageState("other")
	require.NoError(t, err)
	assert.False(t, state.Exists)
	require.Error(t, p.CreatePackage("other", protobufs.PackageType_PackageType_Addon))
	require.Error(t, p.DeletePackage(agentPackageName))
	require.Error(t, p.SetPackageState(agentPackageName, types.PackageState{Exists: true, Type: protobufs.PackageType_PackageType_Addon}))

	pkg := []byte("new agent")
	require.NoError(t, p.UpdateContent(t.Context(), agentPackageName, "https://example.com/agent", bytes.NewReader(pkg), sha(pkg), nil))
	installed := types.PackageState{Exists: true, Type: protobufs.PackageType_PackageType_TopLevel, Hash: []byte{1, 2}, Version: "1.0.0"}
	require.NoError(t, p.SetPackageState(agentPackageName, installed))
	require.NoError(t, p.SetAllPackagesHash([]byte{3, 4}))
	statuses := &protobufs.PackageStatuses{ServerProvidedAllPackagesHash: []byte{3, 4}}
	require.NoError(t, p.SetLastReportedStatuses(statuses))

	// A restarted Supervisor reports the installed package.
	p = env.newManager(t)
	state, err = p.PackageState(agentPackageName)
	require.NoError(t, err)
	assert.Equal(t, installed, state)
	allHash, err := p.AllPackagesHash()
	require.NoError(t, err)
	assert.Equal(t, []byte{3, 4}, allHash)
	loaded, err := p.LastReportedStatuses()
	require.NoError(t, err)
	assert.Equal(t, statuses.ServerProvidedAllPackagesHash, loaded.ServerProvidedAllPackagesHash)
}

func TestPackageManagerForgetsReplacedExecutable(t *testing.T) {
	env := newTestPackageEnv(t)
	p := env.newManager(t)
	pkg := []byte("new agent")
	require.NoError(t, p.UpdateContent(t.Context(), agentPackageName, "https://example.com/agent", bytes.NewReader(pkg), sha(pkg), nil))
	require.NoError(t, p.SetPackageState(agentPackageName, types.PackageState{Exists: true, Type: protobufs.PackageType_PackageType_TopLevel, Hash: []byte{1}, Version: "1.0.0"}))
	require.NoError(t, p.SetAllPackagesHash([]byte{2}))

	// For example, a system package manager installs another agent version.
	require.NoError(t, os.WriteFile(env.executable, []byte("agent installed by apt"), 0o600))

	p = env.newManager(t)
	state, err := p.PackageState(agentPackageName)
	require.NoError(t, err)
	assert.Nil(t, state.Hash)
	assert.Equal(t, "0.1.0", state.Version)
	allHash, err := p.AllPackagesHash()
	require.NoError(t, err)
	assert.Nil(t, allHash, "the next offer must be synced again")
	hash, err := p.FileContentHash(agentPackageName)
	require.NoError(t, err)
	assert.Nil(t, hash)
}

func TestPackageManagerLastReportedStatusesMissing(t *testing.T) {
	p := newTestPackageEnv(t).newManager(t)
	statuses, err := p.LastReportedStatuses()
	require.NoError(t, err)
	assert.Nil(t, statuses)
}
