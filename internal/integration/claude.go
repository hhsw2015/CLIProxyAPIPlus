package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	claudeHomeConfigBackupMetaName = "claude-home-config-metadata.json"
	claudeHomeConfigBackupBodyName = "claude-home-config-original.bin"
)

type claudeHomeConfigBackup struct {
	TargetPath string `json:"target_path"`
	Existed    bool   `json:"existed"`
}

func (m Manager) claudeTargetPath() (string, error) {
	home, err := m.resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func (m Manager) claudeHomeConfigPath() (string, error) {
	home, err := m.resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

func (m Manager) claudeStatus() (Status, error) {
	targetPath, err := m.claudeTargetPath()
	if err != nil {
		return Status{}, err
	}

	status := Status{
		Product:         ProductClaudeCode,
		State:           StateNotConfigured,
		TargetPath:      targetPath,
		BackupAvailable: m.hasLatestBackup(ProductClaudeCode),
		Warning:         "Project, local, or managed Claude Code settings may still override user settings.",
	}

	body, err := readJSONCMap(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return Status{}, fmt.Errorf("read Claude settings: %w", err)
	}

	env, _ := body["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] == m.advertisedBaseURL() {
		status.State = StateConfigured
	}
	return status, nil
}

func (m Manager) previewClaude() (Preview, error) {
	targetPath, err := m.claudeTargetPath()
	if err != nil {
		return Preview{}, err
	}

	current := ""
	body := map[string]any{}
	if raw, err := os.ReadFile(targetPath); err == nil {
		current = string(raw)
		body, err = readJSONCMap(targetPath)
		if err != nil {
			return Preview{}, fmt.Errorf("read Claude settings for preview: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return Preview{}, fmt.Errorf("read Claude settings for preview: %w", err)
	}

	env, _ := body["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = m.advertisedBaseURL()
	body["env"] = env

	plannedBytes, err := marshalJSONMap(body)
	if err != nil {
		return Preview{}, fmt.Errorf("marshal Claude preview: %w", err)
	}

	return Preview{
		Product:        ProductClaudeCode,
		CurrentContent: current,
		PlannedContent: string(plannedBytes),
	}, nil
}

func (m Manager) applyClaude() (Result, error) {
	status, err := m.claudeStatus()
	if err != nil {
		return Result{}, err
	}
	if status.State == StateConfigured {
		return Result{
			Product: ProductClaudeCode,
			Status:  status,
			Message: "already configured",
		}, nil
	}

	snap, err := m.createBackup(ProductClaudeCode, status.TargetPath)
	if err != nil {
		return Result{}, err
	}
	if err := m.backupClaudeHomeConfig(snap.BackupDir); err != nil {
		return Result{}, err
	}

	preview, err := m.previewClaude()
	if err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(filepath.Dir(status.TargetPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("prepare Claude settings path: %w", err)
	}
	if err := os.WriteFile(status.TargetPath, []byte(preview.PlannedContent), 0o600); err != nil {
		return Result{}, fmt.Errorf("write Claude settings: %w", err)
	}
	if err := m.ensureClaudeOnboardingCompleted(); err != nil {
		return Result{}, err
	}

	status, err = m.claudeStatus()
	if err != nil {
		return Result{}, err
	}
	return Result{
		Product: ProductClaudeCode,
		Status:  status,
		Message: "Claude Code configured to use CPA",
	}, nil
}

func (m Manager) rollbackClaude() (Result, error) {
	snap, err := m.loadLatestBackup(ProductClaudeCode)
	if err != nil {
		return Result{}, fmt.Errorf("load latest backup: %w", err)
	}
	if err := restoreBackupSnapshot(snap); err != nil {
		return Result{}, err
	}
	if err := m.restoreClaudeHomeConfig(snap.BackupDir); err != nil {
		return Result{}, err
	}

	status, err := m.claudeStatus()
	if err != nil {
		return Result{}, err
	}
	return Result{
		Product: ProductClaudeCode,
		Status:  status,
		Message: "rollback completed",
	}, nil
}

func restoreBackupSnapshot(snap backupSnapshot) error {
	if snap.TargetExisted {
		if err := os.MkdirAll(filepath.Dir(snap.TargetPath), 0o755); err != nil {
			return fmt.Errorf("prepare rollback target: %w", err)
		}
		if err := os.WriteFile(snap.TargetPath, snap.Original, 0o600); err != nil {
			return fmt.Errorf("restore backup: %w", err)
		}
		return nil
	}
	if err := os.Remove(snap.TargetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove target during rollback: %w", err)
	}
	return nil
}

func (m Manager) ensureClaudeOnboardingCompleted() error {
	targetPath, err := m.claudeHomeConfigPath()
	if err != nil {
		return err
	}

	body, err := readJSONCMap(targetPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read Claude home config for onboarding update: %w", err)
		}
		body = map[string]any{}
	}

	// Claude Code can ignore settings/env overrides until the home-level onboarding
	// state is complete, so keep this true when CPA takes over settings.json.
	body["hasCompletedOnboarding"] = true

	data, err := marshalJSONMap(body)
	if err != nil {
		return fmt.Errorf("marshal Claude home config: %w", err)
	}
	if err := os.WriteFile(targetPath, data, 0o600); err != nil {
		return fmt.Errorf("write Claude home config: %w", err)
	}
	return nil
}

//nolint:gosec // backupDir is created by CPA under its managed backup root.
func (m Manager) backupClaudeHomeConfig(backupDir string) error {
	targetPath, err := m.claudeHomeConfigPath()
	if err != nil {
		return err
	}

	meta := claudeHomeConfigBackup{TargetPath: targetPath}
	original, err := os.ReadFile(targetPath)
	if err == nil {
		meta.Existed = true
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read Claude home config for backup: %w", err)
	}

	metaData, err := marshalJSONMap(map[string]any{
		"target_path": meta.TargetPath,
		"existed":     meta.Existed,
	})
	if err != nil {
		return fmt.Errorf("marshal Claude home config backup metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, claudeHomeConfigBackupMetaName), metaData, 0o600); err != nil {
		return fmt.Errorf("write Claude home config backup metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, claudeHomeConfigBackupBodyName), original, 0o600); err != nil {
		return fmt.Errorf("write Claude home config backup body: %w", err)
	}
	return nil
}

//nolint:gosec // backupDir and TargetPath come from CPA-managed backup metadata.
func (m Manager) restoreClaudeHomeConfig(backupDir string) error {
	metaPath := filepath.Join(backupDir, claudeHomeConfigBackupMetaName)
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read Claude home config backup metadata: %w", err)
	}

	var meta claudeHomeConfigBackup
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("decode Claude home config backup metadata: %w", err)
	}

	original, err := os.ReadFile(filepath.Join(backupDir, claudeHomeConfigBackupBodyName))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Claude home config backup body: %w", err)
	}

	if meta.Existed {
		if err := os.WriteFile(meta.TargetPath, original, 0o600); err != nil {
			return fmt.Errorf("restore Claude home config: %w", err)
		}
		return nil
	}
	if err := os.Remove(meta.TargetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove Claude home config during rollback: %w", err)
	}
	return nil
}
