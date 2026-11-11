// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
)

func newUpgradeTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(exe, []byte("agent"), 0o600))
	return &Supervisor{
		telemetrySettings: newNopTelemetrySettings(),
		config:            config.Supervisor{Agent: config.Agent{Executable: exe}, Storage: config.Storage{Directory: t.TempDir()}},
		packageManager:    &packageManager{},
		agentUpgrade:      make(chan agentUpgradeRequest),
		doneChan:          make(chan struct{}),
	}
}

func TestRecoverInterruptedPairedUpgrade(t *testing.T) {
	previous := &protobufs.AgentRemoteConfig{ConfigHash: []byte("previous")}
	target := &protobufs.AgentRemoteConfig{ConfigHash: []byte("target")}
	for _, tc := range []struct {
		name      string
		backup    bool
		committed bool
		previous  *protobufs.AgentRemoteConfig
	}{
		{name: "before binary swap", previous: previous},
		{name: "after binary swap", backup: true, previous: previous},
		{name: "after binary rollback", previous: previous},
		{name: "after commit", backup: true, committed: true, previous: previous},
		{name: "after backup cleanup", committed: true, previous: previous},
		{name: "without previous remote config", backup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUpgradeTestSupervisor(t)
			exe := s.config.Agent.Executable
			binary := "previous"
			if tc.backup || tc.committed {
				binary = "target"
			}
			require.NoError(t, os.WriteFile(exe, []byte(binary), 0o600))
			if tc.backup {
				require.NoError(t, os.WriteFile(agentBackupPath(exe), []byte("previous"), 0o600))
			}
			require.NoError(t, s.saveLastReceivedConfig(target))
			require.NoError(t, s.writeAgentUpgradeState(&agentUpgradeState{PreviousRemoteConfig: tc.previous, Committed: tc.committed}))

			// Recover using a fresh Supervisor, as after a process restart.
			restarted := &Supervisor{config: s.config, telemetrySettings: newNopTelemetrySettings()}
			require.NoError(t, restarted.recoverInterruptedUpgrade())
			content, err := os.ReadFile(exe)
			require.NoError(t, err)
			wantBinary, wantConfig := "previous", tc.previous
			if tc.committed {
				wantBinary, wantConfig = "target", target
			}
			assert.Equal(t, wantBinary, string(content))
			if wantConfig == nil {
				assert.NoFileExists(t, filepath.Join(s.config.Storage.Directory, lastRecvRemoteConfigFile))
			} else {
				restored, err := restarted.loadPersistedRemoteConfig(lastRecvRemoteConfigFile)
				require.NoError(t, err)
				assert.True(t, proto.Equal(wantConfig, restored))
			}
			assert.NoFileExists(t, agentBackupPath(exe))
			assert.NoFileExists(t, filepath.Join(s.config.Storage.Directory, agentUpgradeStateFileName))
		})
	}
}

func TestRecoverInterruptedUpgrade(t *testing.T) {
	t.Run("no upgrade in progress", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		require.NoError(t, s.recoverInterruptedUpgrade())
		exe, err := os.ReadFile(s.config.Agent.Executable)
		require.NoError(t, err)
		assert.Equal(t, "agent", string(exe))
	})

	t.Run("restores the previous binary", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		exe := s.config.Agent.Executable
		require.NoError(t, os.WriteFile(exe, []byte("unverified new agent"), 0o600))
		require.NoError(t, os.WriteFile(agentBackupPath(exe), []byte("previous agent"), 0o600))

		require.NoError(t, s.recoverInterruptedUpgrade())
		content, err := os.ReadFile(exe)
		require.NoError(t, err)
		assert.Equal(t, "previous agent", string(content))
		assert.NoFileExists(t, agentBackupPath(exe))
	})
}

func TestInstallAgentBinary(t *testing.T) {
	t.Run("returns the outcome from runAgentProcess", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		outcome := errors.New("upgrade outcome")
		go func() {
			req := <-s.agentUpgrade
			assert.Equal(t, "/staged", req.stagedPath)
			req.result <- outcome
		}()
		require.ErrorIs(t, s.installAgentBinary(t.Context(), "/staged"), outcome)
	})

	t.Run("gives up when the context is cancelled before the request is taken", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, s.installAgentBinary(ctx, "/staged"), context.Canceled)
	})

	t.Run("waits for the outcome even when the context is cancelled after the request is taken", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			req := <-s.agentUpgrade
			cancel()
			req.result <- nil
		}()
		require.NoError(t, s.installAgentBinary(ctx, "/staged"))
	})

	t.Run("returns on shutdown", func(t *testing.T) {
		s := newUpgradeTestSupervisor(t)
		go func() {
			<-s.agentUpgrade
			close(s.doneChan)
		}()
		require.ErrorIs(t, s.installAgentBinary(t.Context(), "/staged"), errSupervisorShutdown)
	})
}

func TestHandleRestartCommandRefusedDuringUpgrade(t *testing.T) {
	s := newUpgradeTestSupervisor(t)
	s.agentUpgrading.Store(true)
	// The commander is nil: a restart attempt would panic.
	require.ErrorContains(t, s.handleRestartCommand(), "being upgraded")
}
