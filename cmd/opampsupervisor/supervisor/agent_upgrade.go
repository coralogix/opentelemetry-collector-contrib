// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const (
	bootstrapConfigFileName   = "bootstrap_config.yaml"
	agentUpgradeStateFileName = "agent_upgrade.json"
)

// The binary backup and this record form a single update. Before commit,
// recovery restores the old pair. After commit, it only removes the backup.
type agentUpgradeState struct {
	PreviousRemoteConfig *protobufs.AgentRemoteConfig `json:"previous_remote_config"`
	Committed            bool                         `json:"committed"`
}

var errSupervisorShutdown = errors.New("supervisor is shutting down")

// agentUpgradeRequest asks runAgentProcess, which owns the agent process and its
// executable, to install the agent binary staged at stagedPath.
type agentUpgradeRequest struct {
	stagedPath   string
	remoteConfig *protobufs.AgentRemoteConfig
	// result has capacity 1 so runAgentProcess never blocks on the reply.
	result chan error
}

func (s *Supervisor) syncPackages(msg *types.MessageData) {
	remoteConfig := msg.RemoteConfig
	if remoteConfig != nil {
		remoteConfig = proto.Clone(remoteConfig).(*protobufs.AgentRemoteConfig)
		s.reportRemoteConfigStatus(remoteConfig, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, "")
	}
	s.packageManager.pendingRemoteConfig.Store(remoteConfig)
	if err := msg.PackageSyncer.Sync(s.runCtx); err != nil {
		s.telemetrySettings.Logger.Error("Could not sync offered packages", zap.Error(err))
		if remoteConfig != nil {
			s.reportRemoteConfigStatus(remoteConfig, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		}
		return
	}
	if remoteConfig == nil {
		return
	}
	s.packageSyncWG.Go(func() {
		select {
		case <-msg.PackageSyncer.Done():
		case <-s.doneChan:
			return
		}
		installed, err := s.packageManager.PackageState(agentPackageName)
		offered := msg.PackagesAvailable.GetPackages()[agentPackageName]
		if err != nil || offered == nil || !bytes.Equal(installed.Hash, offered.Hash) {
			s.reportRemoteConfigStatus(remoteConfig, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "The paired agent package was not installed")
			return
		}
		// This also applies the config when the offered package was already installed.
		s.onMessage(s.runCtx, &types.MessageData{RemoteConfig: remoteConfig})
	})
}

// installAgentBinary installs the agent binary staged at stagedPath and returns
// once the new agent is healthy, or once the previous binary has been restored.
func (s *Supervisor) installAgentBinary(ctx context.Context, stagedPath string) error {
	req := agentUpgradeRequest{stagedPath: stagedPath, remoteConfig: s.packageManager.pendingRemoteConfig.Load(), result: make(chan error, 1)}
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
// agent does not become healthy, the previous binary/config pair is restored.
// It must only be called from runAgentProcess.
func (s *Supervisor) upgradeAgent(stagedPath string, remoteConfig *protobufs.AgentRemoteConfig) error {
	s.agentUpgrading.Store(true)
	defer s.agentUpgrading.Store(false)

	exe := s.config.Agent.Executable
	backup := agentBackupPath(exe)
	logger := s.telemetrySettings.Logger
	logger.Info("Upgrading agent binary", zap.String("executable", exe))
	if err := s.commander.Stop(s.runCtx); err != nil {
		return fmt.Errorf("stop agent: %w", err)
	}
	var state *agentUpgradeState
	if remoteConfig != nil {
		state = &agentUpgradeState{PreviousRemoteConfig: s.remoteConfig.Load()}
		if err := s.writeAgentUpgradeState(state); err != nil {
			_, startErr := s.startAgent()
			return errors.Join(err, startErr)
		}
	}
	if err := os.Rename(exe, backup); err != nil {
		return errors.Join(fmt.Errorf("back up agent executable: %w", err), s.restoreAgentBinary())
	}
	if err := os.Rename(stagedPath, exe); err != nil {
		return errors.Join(fmt.Errorf("install new agent executable: %w", err), s.restoreAgentBinary())
	}

	var startErr error
	if remoteConfig != nil {
		startErr = s.setUpgradeRemoteConfig(remoteConfig)
	}
	if startErr == nil {
		startErr = s.startInstalledAgent()
	}
	if startErr == nil && state != nil {
		state.Committed = true
		startErr = s.writeAgentUpgradeState(state)
	}
	if startErr == nil {
		if err := os.Remove(backup); err != nil {
			logger.Warn("Could not remove the previous agent binary", zap.String("path", backup), zap.Error(err))
		} else if state != nil {
			if err := os.Remove(filepath.Join(s.config.Storage.Directory, agentUpgradeStateFileName)); err != nil {
				logger.Warn("Could not remove the committed agent upgrade record", zap.Error(err))
			}
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
	return errors.Join(fmt.Errorf("%w: %w", errAgentUnhealthy, startErr), s.restoreAgentBinary())
}

// restoreAgentBinary restores the previous binary/config pair and starts it.
func (s *Supervisor) restoreAgentBinary() error {
	if err := s.recoverInterruptedUpgrade(); err != nil {
		return err
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

// recoverInterruptedUpgrade restores an uncommitted binary/config pair or
// finishes cleanup after a committed update. It runs before executing the agent.
func (s *Supervisor) recoverInterruptedUpgrade() error {
	statePath := filepath.Join(s.config.Storage.Directory, agentUpgradeStateFileName)
	data, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return s.restoreAgentBackup()
	}
	if err != nil {
		return err
	}
	var state agentUpgradeState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("read agent upgrade record: %w", err)
	}
	if state.Committed {
		if err := os.Remove(agentBackupPath(s.config.Agent.Executable)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Remove(statePath)
	}
	if err := s.restoreAgentBackup(); err != nil {
		return err
	}
	if err := s.setUpgradeRemoteConfig(state.PreviousRemoteConfig); err != nil {
		return err
	}
	return os.Remove(statePath)
}

func (s *Supervisor) restoreAgentBackup() error {
	exe := s.config.Agent.Executable
	backup := agentBackupPath(exe)
	err := os.Rename(backup, exe)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s.telemetrySettings.Logger.Warn("Restored the previous agent binary", zap.String("backup", backup))
	return nil
}

func (s *Supervisor) writeAgentUpgradeState(state *agentUpgradeState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := filepath.Join(s.config.Storage.Directory, agentUpgradeStateFileName)
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *Supervisor) setUpgradeRemoteConfig(remoteConfig *protobufs.AgentRemoteConfig) error {
	var err error
	if remoteConfig == nil {
		err = os.Remove(filepath.Join(s.config.Storage.Directory, lastRecvRemoteConfigFile))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = s.saveLastReceivedConfig(remoteConfig)
	}
	if err != nil {
		return err
	}
	s.remoteConfig.Store(remoteConfig)
	s.usingLastWorkingRemoteConfig.Store(false)
	return nil
}

// agentBackupPath is where the previous agent binary is kept during an upgrade.
// It is next to the executable so renames stay on one filesystem.
func agentBackupPath(executable string) string {
	return executable + ".old"
}
