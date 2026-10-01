// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
)

const bootstrapConfigFileName = "bootstrap_config.yaml"

var errSupervisorShutdown = errors.New("supervisor is shutting down")

// agentUpgradeRequest asks runAgentProcess, which owns the agent process and its
// executable, to install the agent binary staged at stagedPath.
type agentUpgradeRequest struct {
	stagedPath string
	// result has capacity 1 so runAgentProcess never blocks on the reply.
	result chan error
}

// installAgentBinary installs the agent binary staged at stagedPath and returns
// once the new agent is healthy, or once the previous binary has been restored.
func (s *Supervisor) installAgentBinary(ctx context.Context, stagedPath string) error {
	req := agentUpgradeRequest{stagedPath: stagedPath, result: make(chan error, 1)}
	select {
	case s.agentUpgrade <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.doneChan:
		return errSupervisorShutdown
	}
	// Once runAgentProcess has the request, the executable may be mid-swap: wait
	// for the outcome instead of giving up on ctx.
	select {
	case err := <-req.result:
		return err
	case <-s.doneChan:
		return errSupervisorShutdown
	}
}

// upgradeAgent swaps in the agent binary at stagedPath and starts it. If the new
// agent does not become healthy, the previous binary is restored and started.
// It must only be called from runAgentProcess.
func (s *Supervisor) upgradeAgent(stagedPath string) error {
	s.agentUpgrading.Store(true)
	defer s.agentUpgrading.Store(false)

	exe := s.config.Agent.Executable
	backup := agentBackupPath(exe)
	logger := s.telemetrySettings.Logger
	logger.Info("Upgrading agent binary", zap.String("executable", exe))

	if err := s.commander.Stop(s.runCtx); err != nil {
		return fmt.Errorf("stop agent: %w", err)
	}
	if err := os.Rename(exe, backup); err != nil {
		_, startErr := s.startAgent() // the previous binary is still in place
		return errors.Join(fmt.Errorf("back up agent executable: %w", err), startErr)
	}
	if err := os.Rename(stagedPath, exe); err != nil {
		return errors.Join(fmt.Errorf("install new agent executable: %w", err), s.restoreAgentBinary(backup))
	}

	startErr := s.startInstalledAgent()
	if startErr == nil {
		if err := os.Remove(backup); err != nil {
			logger.Warn("Could not remove the previous agent binary", zap.String("path", backup), zap.Error(err))
		}
		logger.Info("Agent binary upgraded")
		return nil
	}
	if errors.Is(startErr, errSupervisorShutdown) {
		// Leave the backup in place: recoverInterruptedUpgrade restores it on the next start.
		return startErr
	}

	logger.Error("New agent binary did not become healthy, restoring the previous binary", zap.Error(startErr))
	// The new agent may be running but unhealthy. Stop it first, because
	// commander.Start does nothing while an agent is running.
	if err := s.commander.Stop(s.runCtx); err != nil {
		return errors.Join(fmt.Errorf("%w: %w", errAgentUnhealthy, startErr), fmt.Errorf("stop new agent: %w", err))
	}
	return errors.Join(fmt.Errorf("%w: %w", errAgentUnhealthy, startErr), s.restoreAgentBinary(backup))
}

// restoreAgentBinary moves the backup over the agent executable and starts it.
func (s *Supervisor) restoreAgentBinary(backup string) error {
	if err := os.Rename(backup, s.config.Agent.Executable); err != nil {
		return fmt.Errorf("restore previous agent binary: %w", err)
	}
	if err := s.startInstalledAgent(); err != nil {
		return fmt.Errorf("start previous agent binary: %w", err)
	}
	s.telemetrySettings.Logger.Info("Previous agent binary restored")
	return nil
}

// startInstalledAgent bootstraps the agent binary currently at the executable
// path, recomposes the agent config for it, starts it and waits until it is healthy.
func (s *Supervisor) startInstalledAgent() error {
	// The Supervisor's OpAMP server holds s.opampServerPort, and the agent config
	// file is live, so the bootstrap agent gets its own port and config file.
	port, err := s.findRandomPort()
	if err != nil {
		return fmt.Errorf("find bootstrap port: %w", err)
	}
	if err = s.getBootstrapInfo(port, filepath.Join(s.config.Storage.Directory, bootstrapConfigFileName)); err != nil {
		return fmt.Errorf("bootstrap agent: %w", err)
	}
	if err = s.opampClient.SetAgentDescription(s.agentDescription.Load().(*protobufs.AgentDescription)); err != nil {
		return fmt.Errorf("set agent description: %w", err)
	}
	if ac, ok := s.availableComponents.Load().(*protobufs.AvailableComponents); ok && ac != nil {
		if err = s.opampClient.SetAvailableComponents(ac); err != nil {
			return fmt.Errorf("set available components: %w", err)
		}
	}

	// The agent description is part of the composed config (e.g. service.version in
	// the telemetry resource), so the config must be recomposed for this binary.
	s.configWriteMu.Lock()
	_, err = s.composeMergedConfig(s.remoteConfig.Load())
	if err == nil {
		err = s.writeAgentConfig()
	}
	s.configWriteMu.Unlock()
	if err != nil {
		return fmt.Errorf("compose agent config: %w", err)
	}

	s.resetAgentReady()
	status, err := s.startAgent()
	if err != nil {
		return err
	}
	if status == agentNotStarting {
		// Without a config the agent is not run; the bootstrap proved the binary
		// works. Drop the exit event of the agent stopped for the upgrade, so
		// runAgentProcess does not handle it as a crash.
		select {
		case <-s.commander.Exited():
		default:
		}
		return nil
	}

	timer := time.NewTimer(s.config.Agent.ConfigApplyTimeout)
	defer timer.Stop()
	select {
	case <-s.agentReadyChan: // markAgentReady has set agentReady
		return nil
	case <-s.commander.Exited():
		return fmt.Errorf("agent exited with code %d", s.commander.ExitCode())
	case <-timer.C:
		return fmt.Errorf("agent was not healthy after %s", s.config.Agent.ConfigApplyTimeout)
	case <-s.doneChan:
		return errSupervisorShutdown
	}
}

// recoverInterruptedUpgrade restores the previous agent binary if the Supervisor
// stopped while an upgrade was in progress. It must run before the agent binary is executed.
func (s *Supervisor) recoverInterruptedUpgrade() error {
	exe := s.config.Agent.Executable
	backup := agentBackupPath(exe)
	if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	s.telemetrySettings.Logger.Warn("An agent upgrade was interrupted, restoring the previous agent binary", zap.String("backup", backup))
	return os.Rename(backup, exe)
}

// agentBackupPath is where the previous agent binary is kept during an upgrade.
// It is next to the executable so renames stay on one filesystem.
func agentBackupPath(executable string) string {
	return executable + ".old"
}
