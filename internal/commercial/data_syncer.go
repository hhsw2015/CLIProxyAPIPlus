//go:build commercial

package commercial

import (
	"context"
	"fmt"
	"log"
	"strings"

	sub2api "github.com/Wei-Shaw/sub2api/pkg/types"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// DataSyncer synchronizes CPA config data into Sub2API's PostgreSQL database.
type DataSyncer struct {
	adminSvc sub2api.AdminService
	dryRun   bool
}

// NewDataSyncer creates a DataSyncer. When dryRun is true, no writes are performed.
func NewDataSyncer(adminSvc sub2api.AdminService, dryRun bool) *DataSyncer {
	return &DataSyncer{adminSvc: adminSvc, dryRun: dryRun}
}

// SyncReport summarizes the sync operation results.
type SyncReport struct {
	GroupsCreated    int
	GroupsUpdated    int
	GroupsSkipped    int
	AccountsCreated  int
	AccountsUpdated  int
	AccountsDisabled int
	AccountsSkipped  int
	Errors           []string
}

func (r *SyncReport) String() string {
	return fmt.Sprintf("groups(created=%d updated=%d skipped=%d) accounts(created=%d updated=%d disabled=%d skipped=%d) errors=%d",
		r.GroupsCreated, r.GroupsUpdated, r.GroupsSkipped,
		r.AccountsCreated, r.AccountsUpdated, r.AccountsDisabled, r.AccountsSkipped,
		len(r.Errors))
}

// Sync performs the full CPA -> Sub2API data synchronization.
func (s *DataSyncer) Sync(ctx context.Context, cfg *config.Config) *SyncReport {
	report := &SyncReport{}

	mappings := CollectAllMappings(cfg)
	if len(mappings) == 0 {
		log.Println("[data-sync] no syncable CPA entries found")
		return report
	}

	log.Printf("[data-sync] collected %d account mappings from CPA config", len(mappings))

	// Phase 1: Ensure groups exist
	groupSpecs := DeriveGroups(mappings)
	groupIDMap := s.syncGroups(ctx, groupSpecs, report)

	// Phase 2: Sync accounts
	s.syncAccounts(ctx, mappings, groupIDMap, report)

	log.Printf("[data-sync] %s", report)
	return report
}

// syncGroups creates or updates groups, returns name -> ID map.
func (s *DataSyncer) syncGroups(ctx context.Context, specs []GroupSpec, report *SyncReport) map[string]int64 {
	result := map[string]int64{}

	existing, err := s.adminSvc.GetAllGroups(ctx)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list groups: %v", err))
		return result
	}

	existingMap := map[string]*sub2api.Group{}
	for i := range existing {
		existingMap[existing[i].Name] = &existing[i]
	}

	for _, spec := range specs {
		if s.dryRun {
			log.Printf("[data-sync][DRY-RUN] would create/update group: %s (platform=%s)", spec.Name, spec.Platform)
			report.GroupsSkipped++
			continue
		}

		if g, ok := existingMap[spec.Name]; ok {
			result[spec.Name] = g.ID
			report.GroupsSkipped++
			continue
		}

		rm := 1.0
		g, err := s.adminSvc.CreateGroup(ctx, &sub2api.CreateGroupInput{
			Name:             spec.Name,
			Description:      fmt.Sprintf("Auto-synced from CPA (priority=%d)", spec.CPAPriority),
			Platform:         spec.Platform,
			RateMultiplier:   rm,
			SubscriptionType: "standard",
		})
		if err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "already exists") {
				// Race condition: group was created between list and create
				all, _ := s.adminSvc.GetAllGroups(ctx)
				for i := range all {
					if all[i].Name == spec.Name {
						result[spec.Name] = all[i].ID
						break
					}
				}
				report.GroupsSkipped++
			} else {
				report.Errors = append(report.Errors, fmt.Sprintf("create group %s: %v", spec.Name, err))
			}
			continue
		}
		result[spec.Name] = g.ID
		report.GroupsCreated++
		log.Printf("[data-sync] created group: %s (id=%d, platform=%s)", spec.Name, g.ID, spec.Platform)
	}

	return result
}

// syncAccounts performs upsert of Account records.
func (s *DataSyncer) syncAccounts(ctx context.Context, mappings []AccountMapping, groupIDMap map[string]int64, report *SyncReport) {
	// Load all CPA-sourced accounts from DB
	existingMap := s.loadExistingCPAAccounts(ctx, report)

	newStableIDs := map[string]bool{}

	for _, m := range mappings {
		newStableIDs[m.StableID] = true
		groupID, ok := groupIDMap[m.GroupKey]
		if !ok && !s.dryRun {
			report.Errors = append(report.Errors, fmt.Sprintf("group %s not found for account %s", m.GroupKey, m.StableID))
			continue
		}

		if s.dryRun {
			log.Printf("[data-sync][DRY-RUN] would create/update account: %s (platform=%s, type=%s, group=%s, priority=%d)",
				m.CreateInput.Name, m.CreateInput.Platform, m.CreateInput.Type, m.GroupKey, m.CreateInput.Priority)
			report.AccountsSkipped++
			continue
		}

		m.CreateInput.GroupIDs = []int64{groupID}

		if existing, ok := existingMap[m.StableID]; ok {
			if accountNeedsUpdate(existing, &m) {
				updateInput := &sub2api.UpdateAccountInput{
					Name:                  m.CreateInput.Name,
					Type:                  m.CreateInput.Type,
					Credentials:           m.CreateInput.Credentials,
					Extra:                 m.CreateInput.Extra,
					Priority:              &m.CreateInput.Priority,
					GroupIDs:              &m.CreateInput.GroupIDs,
					SkipMixedChannelCheck: true,
				}
				if existing.Status == "disabled" {
					updateInput.Status = "active"
				}
				_, err := s.adminSvc.UpdateAccount(ctx, existing.ID, updateInput)
				if err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("update account %s: %v", m.StableID, err))
				} else {
					report.AccountsUpdated++
				}
			} else {
				report.AccountsSkipped++
			}
		} else {
			_, err := s.adminSvc.CreateAccount(ctx, &m.CreateInput)
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("create account %s: %v", m.StableID, err))
			} else {
				report.AccountsCreated++
			}
		}
	}

	// Disable accounts that were removed from CPA config
	for stableID, acc := range existingMap {
		if newStableIDs[stableID] {
			continue
		}
		if acc.Status == "disabled" {
			continue
		}
		if s.dryRun {
			log.Printf("[data-sync][DRY-RUN] would disable removed account: %s (id=%d)", stableID, acc.ID)
			continue
		}
		_, err := s.adminSvc.UpdateAccount(ctx, acc.ID, &sub2api.UpdateAccountInput{
			Status: "disabled",
		})
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("disable account %d: %v", acc.ID, err))
		} else {
			report.AccountsDisabled++
			log.Printf("[data-sync] disabled removed account: id=%d stable_id=%s", acc.ID, stableID)
		}
	}
}

// loadExistingCPAAccounts fetches all accounts with cpa_source=true from the DB.
func (s *DataSyncer) loadExistingCPAAccounts(ctx context.Context, report *SyncReport) map[string]*sub2api.Account {
	result := map[string]*sub2api.Account{}

	// Fetch accounts page by page, filter by cpa_source in extra
	page := 1
	pageSize := 200
	for {
		accounts, total, err := s.adminSvc.ListAccounts(ctx, page, pageSize, "", "", "", "", 0, "", "", "")
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("list accounts page %d: %v", page, err))
			break
		}

		for i := range accounts {
			acc := &accounts[i]
			if acc.Extra == nil {
				continue
			}
			if src, ok := acc.Extra[extraKeyCPASource]; ok {
				if srcBool, ok := src.(bool); ok && srcBool {
					if sid, ok := acc.Extra[extraKeyCPAStableID].(string); ok {
						result[sid] = acc
					}
				}
			}
		}

		if int64(page*pageSize) >= total || len(accounts) == 0 {
			break
		}
		page++
	}

	return result
}

// accountNeedsUpdate checks if an existing account differs from the new mapping.
func accountNeedsUpdate(existing *sub2api.Account, mapping *AccountMapping) bool {
	if existing.Name != mapping.CreateInput.Name {
		return true
	}
	if existing.Type != mapping.CreateInput.Type {
		return true
	}
	if existing.Priority != mapping.CreateInput.Priority {
		return true
	}
	if existing.Status == "disabled" {
		return true
	}
	if credentialsChanged(existing.Credentials, mapping.CreateInput.Credentials) {
		return true
	}
	// Check if group binding changed (handles priority saturation edge case)
	if existing.Extra != nil {
		if oldGroupKey, ok := existing.Extra["cpa_group_key"].(string); ok {
			if oldGroupKey != mapping.GroupKey {
				return true
			}
		}
	}
	return false
}

// credentialsChanged compares key credential fields (ignores transient metadata).
func credentialsChanged(old, new map[string]any) bool {
	keys := []string{"api_key", "base_url", "aws_access_key_id", "aws_secret_access_key", "aws_region", "auth_mode"}
	for _, k := range keys {
		oldVal, _ := old[k].(string)
		newVal, _ := new[k].(string)
		if oldVal != newVal {
			return true
		}
	}
	return false
}

// SyncAuthStatus updates Sub2API Account status from CPA auth runtime state.
func (s *DataSyncer) SyncAuthStatus(ctx context.Context, auths []*coreauth.Auth) {
	if s.dryRun {
		return
	}

	existing := s.loadExistingCPAAccounts(ctx, &SyncReport{})
	if len(existing) == 0 {
		return
	}

	updated := 0
	for _, auth := range auths {
		sid := authToStableID(auth)
		if sid == "" {
			continue
		}
		acc, ok := existing[sid]
		if !ok {
			continue
		}

		newStatus := mapCPAAuthStatus(auth)
		if acc.Status == newStatus {
			continue
		}

		_, err := s.adminSvc.UpdateAccount(ctx, acc.ID, &sub2api.UpdateAccountInput{
			Status: newStatus,
		})
		if err == nil {
			updated++
		}
	}

	if updated > 0 {
		log.Printf("[data-sync] status sync: updated %d accounts", updated)
	}
}

func authToStableID(auth *coreauth.Auth) string {
	provider := auth.Provider
	switch {
	case provider == "claude" && auth.Attributes["aws_access_key_id"] != "":
		return stableID("claude-key", auth.Attributes["aws_access_key_id"]+":"+auth.Attributes["aws_region"])
	case provider == "claude":
		if key := auth.Attributes["api_key"]; key != "" {
			return stableID("claude-key", key)
		}
	case provider == "gemini":
		if key := auth.Attributes["api_key"]; key != "" {
			return stableID("gemini-key", key)
		}
	case provider == "codex":
		if key := auth.Attributes["api_key"]; key != "" {
			return stableID("codex-key", key)
		}
	default:
		// OpenAI Compat: provider is lowercase(compat.Name), e.g. "taijiai", "cookiepool"
		// Identify by presence of compat_name attribute
		if name := auth.Attributes["compat_name"]; name != "" {
			baseURL := auth.Attributes["base_url"]
			apiKey := auth.Attributes["api_key"]
			if baseURL != "" && apiKey != "" {
				return stableID("openai-compat", name+":"+baseURL+":"+apiKey)
			}
		}
	}
	return ""
}

func mapCPAAuthStatus(auth *coreauth.Auth) string {
	if auth.Disabled {
		return "disabled"
	}
	switch auth.Status {
	case coreauth.StatusActive:
		if auth.Unavailable || auth.Quota.Exceeded {
			return "error"
		}
		return "active"
	case coreauth.StatusError:
		return "error"
	case coreauth.StatusDisabled:
		return "disabled"
	default:
		return "active"
	}
}
