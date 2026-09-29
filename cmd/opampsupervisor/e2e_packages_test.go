// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor"
)

const (
	// agentPackageName is the name of the top-level agent package in PackagesAvailable.
	agentPackageName = ""
	// packagedAgentBinary is the default agent.package.agent_binary.
	packagedAgentBinary = "otelcol-contrib"

	oldAgentVersion = "1.0.0-e2e"
	newAgentVersion = "2.0.0-e2e"
)

// wrapperBehavior selects what a test agent binary does when started with a
// config that is not the upgrade bootstrap config.
type wrapperBehavior int

const (
	// runCollector runs the real Collector.
	runCollector wrapperBehavior = iota
	// exitImmediately exits with an error, also during the bootstrap.
	exitImmediately
	// hangWhenRunning passes the upgrade bootstrap but then never starts the
	// Collector, so it never reports being healthy.
	hangWhenRunning
	// versionOnlyInUpgradeBootstrap sets the version only in the upgrade bootstrap,
	// like a real new binary whose build version differs. When running, the
	// Collector reports the service.version from the config the Supervisor wrote.
	versionOnlyInUpgradeBootstrap
)

// agentWrapper returns a shell script that runs the Collector built for the e2e
// tests and makes it report version as its service.version. The version shows
// which agent binary the Supervisor is running.
func agentWrapper(t *testing.T, version string, behavior wrapperBehavior) []byte {
	t.Helper()
	collectorPath, err := filepath.Abs("../../bin/otelcontribcol_" + runtime.GOOS + "_" + runtime.GOARCH)
	require.NoError(t, err)
	require.FileExists(t, collectorPath)

	// The Supervisor sets the telemetry resource from the bootstrap AgentDescription.
	// This config is merged last, so it overrides the reported version.
	versionConfig := fmt.Sprintf("yaml:service::telemetry::resource::attributes: [{name: service.version, value: %q}]", version)
	run := fmt.Sprintf(`
case "$*" in
  *--config*) exec %q "$@" --config %q ;;
esac
exec %q "$@"
`, collectorPath, versionConfig, collectorPath)

	switch behavior {
	case exitImmediately:
		return []byte("#!/bin/sh\necho 'broken agent' >&2\nexit 1\n")
	case versionOnlyInUpgradeBootstrap:
		return []byte(fmt.Sprintf(`#!/bin/sh
case "$*" in
  *bootstrap_config.yaml*) exec %q "$@" --config %q ;;
esac
exec %q "$@"
`, collectorPath, versionConfig, collectorPath))
	case hangWhenRunning:
		return []byte(fmt.Sprintf(`#!/bin/sh
case "$*" in
  *bootstrap_config.yaml*) ;;
  *) exec sleep 600 ;;
esac
%s`, run))
	default:
		return []byte("#!/bin/sh\n" + run)
	}
}

// agentPackageServer is an OpAMP server that records what the Supervisor reports
// about its agent, plus an HTTP server that serves agent packages.
type agentPackageServer struct {
	opamp *testingOpAMPServer
	files *httptest.Server

	mu sync.Mutex
	// packages maps download paths to the package offered there.
	packages      map[string][]byte
	versions      []string
	healthy       bool
	packageStatus *protobufs.PackageStatus
}

func newAgentPackageServer(t *testing.T) *agentPackageServer {
	t.Helper()
	s := &agentPackageServer{packages: map[string][]byte{}}
	s.opamp = newOpAMPServer(t, defaultConnectingHandler, types.ConnectionCallbacks{
		OnMessage: func(_ context.Context, _ types.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
			s.mu.Lock()
			defer s.mu.Unlock()
			if message.AgentDescription != nil {
				for _, attr := range message.AgentDescription.IdentifyingAttributes {
					if attr.Key == "service.version" {
						s.versions = append(s.versions, attr.Value.GetStringValue())
					}
				}
			}
			if message.Health != nil {
				s.healthy = message.Health.Healthy
			}
			if status := message.GetPackageStatuses().GetPackages()[agentPackageName]; status != nil {
				s.packageStatus = proto.Clone(status).(*protobufs.PackageStatus)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	s.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		pkg, ok := s.packages[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pkg)
	}))
	t.Cleanup(s.files.Close)
	return s
}

func (s *agentPackageServer) lastVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.versions) == 0 {
		return ""
	}
	return s.versions[len(s.versions)-1]
}

func (s *agentPackageServer) sawVersion(version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions {
		if v == version {
			return true
		}
	}
	return false
}

func (s *agentPackageServer) isHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

func (s *agentPackageServer) status() *protobufs.PackageStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packageStatus
}

// offer sends pkg to the Supervisor as version of the top-level agent package.
func (s *agentPackageServer) offer(version string, pkg []byte) {
	contentHash := sha256.Sum256(pkg)
	downloadPath := "/otelcol-contrib_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	s.mu.Lock()
	s.packages[downloadPath] = pkg
	s.mu.Unlock()
	s.opamp.sendToSupervisor(&protobufs.ServerToAgent{
		PackagesAvailable: &protobufs.PackagesAvailable{
			Packages: map[string]*protobufs.PackageAvailable{
				agentPackageName: {
					Type:    protobufs.PackageType_PackageType_TopLevel,
					Version: version,
					Hash:    contentHash[:],
					File: &protobufs.DownloadableFile{
						DownloadUrl: s.files.URL + downloadPath,
						ContentHash: contentHash[:],
					},
				},
			},
			AllPackagesHash: contentHash[:],
		},
	})
}

func agentTarGz(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Mirror the layout of official releases: a README next to the binary.
	for name, content := range map[string][]byte{"README.md": []byte("readme"), packagedAgentBinary: binary} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

type agentUpgradeEnv struct {
	supervisor      *supervisor.Supervisor
	server          *agentPackageServer
	executable      string
	storageDir      string
	healthCheckPort int
}

// startSupervisorWithAgent starts a Supervisor whose agent executable is written
// by prepare, gives the agent a config with a health check and waits until it is
// healthy. The caller must shut the Supervisor down.
func startSupervisorWithAgent(t *testing.T, prepare func(executable string)) *agentUpgradeEnv {
	t.Helper()
	env := &agentUpgradeEnv{
		server:     newAgentPackageServer(t),
		executable: filepath.Join(t.TempDir(), "otelcol"),
		storageDir: t.TempDir(),
	}
	prepare(env.executable)

	s, _ := newSupervisor(t, "packages", map[string]string{
		"url":         env.server.opamp.addr,
		"storage_dir": env.storageDir,
		"executable":  env.executable,
	})
	require.NoError(t, s.Start(t.Context()))
	env.supervisor = s
	waitForSupervisorConnection(env.server.opamp.supervisorConnected, true)

	port, err := findRandomPort()
	require.NoError(t, err)
	env.healthCheckPort = port
	cfg, hash := createHealthCheckCollectorConfWithPort(t, strconv.Itoa(port))
	env.server.opamp.sendToSupervisor(&protobufs.ServerToAgent{
		RemoteConfig: &protobufs.AgentRemoteConfig{
			Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: cfg.Bytes()}}},
			ConfigHash: hash,
		},
	})

	require.Eventually(t, func() bool {
		return env.server.lastVersion() == oldAgentVersion && env.server.isHealthy() && env.collectorServesHealthCheck()
	}, 30*time.Second, 250*time.Millisecond, "the initial agent never became healthy")
	return env
}

func (e *agentUpgradeEnv) collectorServesHealthCheck() bool {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d", e.healthCheckPort))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (e *agentUpgradeEnv) waitForPackageStatus(t *testing.T, want protobufs.PackageStatusEnum, errorContains string) *protobufs.PackageStatus {
	t.Helper()
	var status *protobufs.PackageStatus
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status = e.server.status()
		require.NotNil(c, status)
		require.Equal(c, want, status.Status, "error message: %s", status.ErrorMessage)
		require.Contains(c, status.ErrorMessage, errorContains)
	}, 90*time.Second, 250*time.Millisecond)
	return status
}

// waitForInstalled waits until the Supervisor reports pkg as the installed version.
func (e *agentUpgradeEnv) waitForInstalled(t *testing.T, version string, pkg []byte) {
	t.Helper()
	contentHash := sha256.Sum256(pkg)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status := e.server.status()
		require.NotNil(c, status)
		require.Equal(c, protobufs.PackageStatusEnum_PackageStatusEnum_Installed, status.Status, "error message: %s", status.ErrorMessage)
		require.Equal(c, version, status.AgentHasVersion)
		require.Equal(c, contentHash[:], status.AgentHasHash)
	}, 90*time.Second, 250*time.Millisecond)
}

func (e *agentUpgradeEnv) requireExecutable(t *testing.T, content []byte) {
	t.Helper()
	actual, err := os.ReadFile(e.executable)
	require.NoError(t, err)
	require.Equal(t, string(content), string(actual))
	require.NoFileExists(t, e.executable+".old", "the previous binary must not be left behind")
	require.NoFileExists(t, e.executable+".new", "the staged binary must not be left behind")
}

func skipAgentUpgradeTestOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("The test agent binaries are shell scripts.")
	}
}

func TestSupervisorUpgradesAgent(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	oldAgent := agentWrapper(t, oldAgentVersion, runCollector)
	newAgent := agentWrapper(t, newAgentVersion, versionOnlyInUpgradeBootstrap)
	pkg := agentTarGz(t, newAgent)

	env := startSupervisorWithAgent(t, func(executable string) {
		require.NoError(t, os.WriteFile(executable, oldAgent, 0o700))
	})
	defer env.supervisor.Shutdown()
	env.server.offer(newAgentVersion, pkg)
	env.waitForInstalled(t, newAgentVersion, pkg)

	// Installed is reported only after the new agent reported being healthy.
	env.requireExecutable(t, newAgent)
	// The running Collector reports the service.version the Supervisor put in its
	// telemetry resource. It must be the new version, not the one cached for the
	// previous binary.
	require.Never(t, func() bool { return env.server.lastVersion() != newAgentVersion }, 3*time.Second, 100*time.Millisecond,
		"the upgraded Collector must report the new service.version")
	require.Eventually(t, env.collectorServesHealthCheck, 10*time.Second, 250*time.Millisecond,
		"the upgraded Collector must run the remote config")
	require.Eventually(t, env.server.isHealthy, 10*time.Second, 250*time.Millisecond)
}

func TestSupervisorDowngradesAgent(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	oldAgent := agentWrapper(t, oldAgentVersion, runCollector)
	newAgent := agentWrapper(t, newAgentVersion, runCollector)
	newPkg := agentTarGz(t, newAgent)
	oldPkg := agentTarGz(t, oldAgent)

	env := startSupervisorWithAgent(t, func(executable string) {
		require.NoError(t, os.WriteFile(executable, oldAgent, 0o700))
	})
	defer env.supervisor.Shutdown()

	env.server.offer(newAgentVersion, newPkg)
	env.waitForInstalled(t, newAgentVersion, newPkg)
	env.requireExecutable(t, newAgent)
	require.Eventually(t, func() bool {
		return env.server.lastVersion() == newAgentVersion && env.server.isHealthy() && env.collectorServesHealthCheck()
	}, 30*time.Second, 250*time.Millisecond, "the upgraded agent must run")

	// The new version works, but the server decides to go back to the previous one.
	env.server.offer(oldAgentVersion, oldPkg)
	env.waitForInstalled(t, oldAgentVersion, oldPkg)

	env.requireExecutable(t, oldAgent)
	assert.Equal(t, oldAgentVersion, env.server.lastVersion())
	require.Eventually(t, func() bool {
		return env.server.isHealthy() && env.collectorServesHealthCheck()
	}, 30*time.Second, 250*time.Millisecond, "the downgraded agent must run the remote config")
}

func TestSupervisorRollsBackFailedAgentUpgrade(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)

	testCases := []struct {
		name     string
		behavior wrapperBehavior
		// newAgentStarted is true when the new binary gets far enough to report its version.
		newAgentStarted bool
		errorContains   string
	}{
		{
			name:          "new agent fails to start",
			behavior:      exitImmediately,
			errorContains: "bootstrap agent",
		},
		{
			name:            "new agent runs but never becomes healthy",
			behavior:        hangWhenRunning,
			newAgentStarted: true,
			errorContains:   "agent was not healthy after 10s",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			oldAgent := agentWrapper(t, oldAgentVersion, runCollector)
			pkg := agentTarGz(t, agentWrapper(t, newAgentVersion, tc.behavior))

			env := startSupervisorWithAgent(t, func(executable string) {
				require.NoError(t, os.WriteFile(executable, oldAgent, 0o700))
			})
			defer env.supervisor.Shutdown()
			env.server.offer(newAgentVersion, pkg)

			status := env.waitForPackageStatus(t, protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed, "new agent binary did not become healthy")
			assert.Contains(t, status.ErrorMessage, tc.errorContains)
			assert.Empty(t, status.AgentHasVersion, "the failed version must not be reported as installed")

			// The previous binary is back in place and runs the remote config again.
			env.requireExecutable(t, oldAgent)
			assert.Equal(t, tc.newAgentStarted, env.server.sawVersion(newAgentVersion))
			require.Eventually(t, func() bool {
				return env.server.lastVersion() == oldAgentVersion && env.server.isHealthy() && env.collectorServesHealthCheck()
			}, 30*time.Second, 250*time.Millisecond, "the previous agent must run again after the rollback")

			// The failed package is not installed again when offered again.
			env.server.offer(newAgentVersion, pkg)
			env.waitForPackageStatus(t, protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed, "failed to install before, not retrying")
			env.requireExecutable(t, oldAgent)
			assert.True(t, env.collectorServesHealthCheck())
		})
	}
}

func TestSupervisorRestoresAgentAfterInterruptedUpgrade(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	oldAgent := agentWrapper(t, oldAgentVersion, runCollector)

	// The Supervisor stopped after swapping in a binary it had not yet verified
	// as healthy. The previous binary is still next to the executable.
	env := startSupervisorWithAgent(t, func(executable string) {
		require.NoError(t, os.WriteFile(executable, agentWrapper(t, newAgentVersion, exitImmediately), 0o700))
		require.NoError(t, os.WriteFile(executable+".old", oldAgent, 0o700))
	})
	defer env.supervisor.Shutdown()

	env.requireExecutable(t, oldAgent)
}
