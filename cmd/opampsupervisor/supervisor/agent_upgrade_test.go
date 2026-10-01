// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
)

func newUpgradeTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(exe, []byte("agent"), 0o600))
	return &Supervisor{
		telemetrySettings: newNopTelemetrySettings(),
		config:            config.Supervisor{Agent: config.Agent{Executable: exe}},
		agentUpgrade:      make(chan agentUpgradeRequest),
		doneChan:          make(chan struct{}),
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
