package cliproxy

import (
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type poolCandidateRef struct {
	Provider         string
	Path             string
	RemainingPercent int
	RemainingKnown   bool
}

func newPoolCandidateRef(auth *coreauth.Auth) *poolCandidateRef {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil
	}
	ref := &poolCandidateRef{
		Provider: strings.TrimSpace(auth.Provider),
	}
	if auth.Attributes != nil {
		ref.Path = strings.TrimSpace(auth.Attributes["path"])
		if ref.Path == "" {
			ref.Path = strings.TrimSpace(auth.Attributes["source"])
		}
	}
	if ref.Path == "" {
		ref.Path = strings.TrimSpace(auth.FileName)
	}
	if remaining, known := authWeeklyRemainingPercent(auth); known {
		ref.RemainingPercent = remaining
		ref.RemainingKnown = true
	}
	return ref
}

func poolCandidatePath(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return path
		}
		if path := strings.TrimSpace(auth.Attributes["source"]); path != "" {
			return path
		}
	}
	return strings.TrimSpace(auth.FileName)
}

func (s *Service) indexPoolCandidate(auth *coreauth.Auth) {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	id := strings.TrimSpace(auth.ID)
	ref := newPoolCandidateRef(auth)

	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	if s.poolCandidateIndex == nil {
		s.poolCandidateIndex = make(map[string]*poolCandidateRef)
	}
	if ref != nil {
		s.poolCandidateIndex[id] = ref
	}
	for _, existing := range s.poolCandidateOrder {
		if existing == id {
			return
		}
	}
	s.poolCandidateOrder = append(s.poolCandidateOrder, id)
}

func (s *Service) poolCandidateRef(authID string) *poolCandidateRef {
	if s == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	s.poolCandidateMu.RLock()
	defer s.poolCandidateMu.RUnlock()
	if ref := s.poolCandidateIndex[authID]; ref != nil {
		copyRef := *ref
		return &copyRef
	}
	if auth := s.poolCandidates[authID]; auth != nil {
		return newPoolCandidateRef(auth)
	}
	return nil
}

func (s *Service) loadPoolCandidateByRef(authID string, ref *poolCandidateRef) *coreauth.Auth {
	if s == nil || s.cfg == nil || ref == nil || strings.TrimSpace(ref.Path) == "" {
		return nil
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil || len(data) == 0 {
		return nil
	}
	auths := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{
		Config:      s.cfg,
		AuthDir:     s.cfg.AuthDir,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	}, ref.Path, data)
	for _, auth := range auths {
		if auth != nil && strings.TrimSpace(auth.ID) == strings.TrimSpace(authID) {
			return auth
		}
	}
	return nil
}

func (s *Service) poolCandidateIndexLen() int {
	if s == nil {
		return 0
	}
	s.poolCandidateMu.RLock()
	defer s.poolCandidateMu.RUnlock()
	if len(s.poolCandidateIndex) == 0 {
		return len(s.poolCandidates)
	}
	count := len(s.poolCandidateIndex)
	for id := range s.poolCandidates {
		if _, ok := s.poolCandidateIndex[id]; !ok {
			count++
		}
	}
	return count
}

func (s *Service) evictPoolCandidateIfIndexed(authID string) {
	if s == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.poolCandidateMu.Lock()
	defer s.poolCandidateMu.Unlock()
	ref := s.poolCandidateIndex[authID]
	if ref == nil || strings.TrimSpace(ref.Path) == "" {
		return
	}
	delete(s.poolCandidates, authID)
}
