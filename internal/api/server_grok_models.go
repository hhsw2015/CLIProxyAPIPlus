package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/client/grokbuild"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// grokModelsFromHomeEntries builds a Grok models payload from the entries the
// Home model store returned. Ported from upstream so Home-backed catalogs can
// power grok-shell's /v1/models response.
func grokModelsFromHomeEntries(entries []homeModelEntry) []grokbuild.ModelInfo {
	models := make([]grokbuild.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(models, grokbuild.ModelInfo{
			ID:            entry.id,
			DisplayName:   entry.displayName,
			ContextLength: entry.contextLength,
		})
	}
	return models
}

// grokModelsFromRegistryInfos mirrors the Home path with local registry data.
func grokModelsFromRegistryInfos(infos []*registry.ModelInfo) []grokbuild.ModelInfo {
	models := make([]grokbuild.ModelInfo, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		model := grokbuild.ModelInfo{
			ID:            info.ID,
			DisplayName:   info.DisplayName,
			ContextLength: info.ContextLength,
		}
		if info.Thinking != nil {
			model.ReasoningLevels = append([]string(nil), info.Thinking.Levels...)
		}
		models = append(models, model)
	}
	return models
}

// handleGrokModels serves /v1/models when the caller identifies as grok-shell.
func (s *Server) handleGrokModels(c *gin.Context) {
	var models []grokbuild.ModelInfo
	if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
		entries, ok := s.loadHomeModelEntries(c)
		if !ok {
			return
		}
		models = grokModelsFromHomeEntries(entries)
	} else {
		models = grokModelsFromRegistryInfos(registry.GetGlobalRegistry().GetAvailableModelInfos())
	}
	c.JSON(http.StatusOK, grokbuild.BuildResponse(models))
}
