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
	"strings"
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
  *bootstrap_config.yaml*|validate*) ;;
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
	packages       map[string][]byte
	versions       []string
	healthy        bool
	packageStatus  *protobufs.PackageStatus
	configStatus   *protobufs.RemoteConfigStatus
	beforeDownload func()
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
			if message.RemoteConfigStatus != nil {
				s.configStatus = proto.Clone(message.RemoteConfigStatus).(*protobufs.RemoteConfigStatus)
			}
			return &protobufs.ServerToAgent{}
		},
	})
	s.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		pkg, ok := s.packages[r.URL.Path]
		beforeDownload := s.beforeDownload
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if beforeDownload != nil {
			beforeDownload()
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
func (s *agentPackageServer) offer(version string, pkg []byte, remoteConfig ...*protobufs.AgentRemoteConfig) {
	contentHash := sha256.Sum256(pkg)
	downloadPath := "/otelcol-contrib_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	s.mu.Lock()
	s.packages[downloadPath] = pkg
	s.mu.Unlock()
	message := &protobufs.ServerToAgent{
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
	}
	if len(remoteConfig) != 0 {
		message.RemoteConfig = remoteConfig[0]
	}
	s.opamp.sendToSupervisor(message)
}

func upgradeConfig(t *testing.T, port int, marker string) *protobufs.AgentRemoteConfig {
	t.Helper()
	cfg, _ := createHealthCheckCollectorConfWithPort(t, strconv.Itoa(port))
	body := []byte("service:\n  telemetry:\n    resource:\n      attributes:\n        - name: upgrade_test\n          value: " + marker + "\n")
	hash := sha256.Sum256(body)
	return &protobufs.AgentRemoteConfig{
		Config:     &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: cfg.Bytes()}, "marker.yaml": {Body: body}}},
		ConfigHash: hash[:],
	}
}

func TestSupervisorUpgradesAgentWithPairedConfig(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	starts := filepath.Join(t.TempDir(), "starts")
	trackedWrapper := func(version string) []byte {
		wrapper := string(agentWrapper(t, version, runCollector))
		return []byte(strings.Replace(wrapper, "#!/bin/sh\n", fmt.Sprintf(`#!/bin/sh
if [ "$1" = validate ] && [ %q = %q ] && grep -q upgrade_test "$3"; then
  echo 'this config requires the new Collector' >&2
  exit 1
fi
case "$*" in
  *effective.yaml*)
    marker=$(sed -n '/name: upgrade_test/{n;s/.*value: //p;}' "$2")
    echo %q:"$marker" >> %q
    ;;
esac
`, version, oldAgentVersion, version, starts), 1))
	}
	env := startSupervisorWithAgent(t, func(executable string) {
		require.NoError(t, os.WriteFile(executable, trackedWrapper(oldAgentVersion), 0o700)) //nolint:gosec // The test wrapper must be executable.
	})
	defer env.supervisor.Shutdown()

	downloadStarted := make(chan struct{})
	releaseDownload := make(chan struct{})
	defer func() {
		select {
		case <-releaseDownload:
		default:
			close(releaseDownload)
		}
	}()
	env.server.mu.Lock()
	env.server.beforeDownload = func() {
		close(downloadStarted)
		<-releaseDownload
	}
	env.server.mu.Unlock()
	pkg := agentTarGz(t, trackedWrapper(newAgentVersion))
	paired := upgradeConfig(t, env.healthCheckPort, "paired")
	env.server.offer(newAgentVersion, pkg, paired)
	select {
	case <-downloadStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the package download did not start")
	}
	configPath := filepath.Join(env.storageDir, "effective.yaml")
	require.Never(t, func() bool {
		body, err := os.ReadFile(configPath)
		require.NoError(t, err)
		return bytes.Contains(body, []byte("upgrade_test"))
	}, time.Second, 50*time.Millisecond, "the paired config must not reach the old Collector")
	assert.True(t, env.collectorServesHealthCheck())
	close(releaseDownload)
	env.waitForInstalled(t, newAgentVersion, pkg)
	env.waitForConfigStatus(t, paired, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)

	// The server waits for both results before sending another instruction.
	env.server.opamp.sendToSupervisor(&protobufs.ServerToAgent{RemoteConfig: upgradeConfig(t, env.healthCheckPort, "later")})
	require.Eventually(t, func() bool {
		body, err := os.ReadFile(starts)
		return err == nil && strings.Contains(string(body), newAgentVersion+":later") && env.collectorServesHealthCheck()
	}, 30*time.Second, 100*time.Millisecond)
	body, err := os.ReadFile(starts)
	require.NoError(t, err)
	assert.NotContains(t, string(body), oldAgentVersion+":paired")
	assert.NotContains(t, string(body), oldAgentVersion+":later")
	assert.Contains(t, string(body), newAgentVersion+":paired\n"+newAgentVersion+":later")

	// An unchanged package must not leave its accompanying config waiting for
	// an installation that the package syncer correctly skips.
	alreadyInstalled := upgradeConfig(t, env.healthCheckPort, "already-installed")
	env.server.offer(newAgentVersion, pkg, alreadyInstalled)
	require.Eventually(t, func() bool {
		body, err := os.ReadFile(starts)
		return err == nil && strings.Contains(string(body), newAgentVersion+":already-installed") && env.collectorServesHealthCheck()
	}, 30*time.Second, 100*time.Millisecond)
	env.waitForConfigStatus(t, alreadyInstalled, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
}

func TestSupervisorKeepsPreviousConfigOnFailedPairedUpgrade(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	for _, failure := range []string{"download", "binary", "config", "archive", "executable format"} {
		t.Run(failure, func(t *testing.T) {
			oldAgent := agentWrapper(t, oldAgentVersion, runCollector)
			env := startSupervisorWithAgent(t, func(executable string) {
				require.NoError(t, os.WriteFile(executable, oldAgent, 0o700)) //nolint:gosec // The test wrapper must be executable.
			})
			defer env.supervisor.Shutdown()
			configPath := filepath.Join(env.storageDir, "last_recv_remote_config.dat")
			previousConfig, err := os.ReadFile(configPath)
			require.NoError(t, err)
			target := upgradeConfig(t, env.healthCheckPort, "paired")
			behavior := runCollector
			if failure == "binary" {
				behavior = exitImmediately
			}
			if failure == "config" {
				target.Config.ConfigMap["invalid.yaml"] = &protobufs.AgentConfigObject{Body: []byte("receivers:\n  unknown_receiver:\n")}
			}
			newAgent := agentWrapper(t, newAgentVersion, behavior)
			if failure == "executable format" {
				newAgent = []byte("not an executable")
			}
			pkg := agentTarGz(t, newAgent)
			if failure == "archive" {
				pkg = []byte("not a gzip archive")
			}
			if failure == "download" {
				// A closed endpoint makes the HTTP request fail before installation.
				env.server.files.Close()
			}
			env.server.offer(newAgentVersion, pkg, target)
			env.waitForPackageStatus(t, protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed, "")
			env.waitForConfigStatus(t, target, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
			env.requireExecutable(t, oldAgent)
			require.Eventually(t, func() bool {
				return env.server.lastVersion() == oldAgentVersion && env.collectorServesHealthCheck()
			}, 30*time.Second, 100*time.Millisecond)
			stored, err := os.ReadFile(configPath)
			require.NoError(t, err)
			assert.Equal(t, previousConfig, stored)
			effective, err := os.ReadFile(filepath.Join(env.storageDir, "effective.yaml"))
			require.NoError(t, err)
			assert.NotContains(t, string(effective), "upgrade_test")
			assert.NoFileExists(t, filepath.Join(env.storageDir, "agent_upgrade.json"))

			if failure == "archive" || failure == "executable format" {
				corrected := upgradeConfig(t, env.healthCheckPort, "corrected")
				env.server.offer(newAgentVersion, pkg, corrected)
				env.waitForPackageStatus(t, protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed, "was rejected before, not retrying")
				env.waitForConfigStatus(t, corrected, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)
				env.requireExecutable(t, oldAgent)
			}
			if failure == "config" {
				// The same package must recover when paired with a valid config.
				corrected := upgradeConfig(t, env.healthCheckPort, "corrected")
				env.server.offer(newAgentVersion, pkg, corrected)
				env.waitForInstalled(t, newAgentVersion, pkg)
				env.waitForConfigStatus(t, corrected, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)
				env.requireExecutable(t, newAgent)
				require.Eventually(t, env.collectorServesHealthCheck, 10*time.Second, 100*time.Millisecond)
			}
		})
	}
}

func TestSupervisorRetriesPackageAfterStartupFailure(t *testing.T) {
	skipAgentUpgradeTestOnWindows(t)
	oldAgent := agentWrapper(t, oldAgentVersion, runCollector)
	newAgent := agentWrapper(t, newAgentVersion, runCollector)
	env := startSupervisorWithAgent(t, func(executable string) {
		require.NoError(t, os.WriteFile(executable, oldAgent, 0o700)) //nolint:gosec // The test wrapper must be executable.
	})
	defer env.supervisor.Shutdown()
	pkg := agentTarGz(t, newAgent)
	invalid := upgradeConfig(t, env.healthCheckPort, "invalid")
	invalid.Config.ConfigMap["invalid.yaml"] = &protobufs.AgentConfigObject{Body: []byte("receivers:\n  unknown_receiver:\n")}
	env.server.offer(newAgentVersion, pkg, invalid)
	env.waitForPackageStatus(t, protobufs.PackageStatusEnum_PackageStatusEnum_InstallFailed, "unknown_receiver")
	env.waitForConfigStatus(t, invalid, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)

	// The failed config was rolled back. A later package-only offer must retry
	// the same binary with the current working config.
	env.server.offer(newAgentVersion, pkg)
	env.waitForInstalled(t, newAgentVersion, pkg)
	env.requireExecutable(t, newAgent)
	require.Eventually(t, env.collectorServesHealthCheck, 10*time.Second, 100*time.Millisecond)
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

func (e *agentUpgradeEnv) waitForConfigStatus(t *testing.T, remoteConfig *protobufs.AgentRemoteConfig, want protobufs.RemoteConfigStatuses) {
	t.Helper()
	require.Eventually(t, func() bool {
		e.server.mu.Lock()
		defer e.server.mu.Unlock()
		status := e.server.configStatus
		return status != nil && status.Status == want && bytes.Equal(status.LastRemoteConfigHash, remoteConfig.ConfigHash)
	}, 30*time.Second, 100*time.Millisecond)
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
