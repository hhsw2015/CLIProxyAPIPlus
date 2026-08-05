package auth

import (
	"context"
	"strings"
)

// RefreshHomeSelectionAfterUnauthorized refreshes the credential snapshot bound
// to a Home dispatch selection after an upstream 401. Home dispatch auths are
// not part of manager.auths, so we call the selection's Executor.Refresh path
// directly rather than routing through refreshAuthForRequest.
func (m *Manager) RefreshHomeSelectionAfterUnauthorized(ctx context.Context, selection *HomeDispatchSelection, failedAuth *Auth) (*Auth, bool, error) {
	if m == nil || selection == nil {
		return nil, false, nil
	}
	current := selection.CloneAuth()
	if failedAuth == nil {
		failedAuth = current
	}
	if current == nil || failedAuth == nil {
		return current, false, nil
	}
	// If a concurrent refresh already installed a new token on the selection,
	// reuse it and short-circuit the network refresh.
	if current.ID == failedAuth.ID {
		currentToken := strings.TrimSpace(authAccessToken(current))
		failedToken := strings.TrimSpace(authAccessToken(failedAuth))
		if currentToken != "" && failedToken != "" && currentToken != failedToken {
			return current, true, nil
		}
	}
	if selection.Executor == nil {
		return current, false, nil
	}
	refreshed, errRefresh := selection.Executor.Refresh(ctx, failedAuth.Clone())
	if errRefresh != nil || refreshed == nil {
		return current, false, errRefresh
	}
	selection.authMu.Lock()
	preserveHomeRoutingAttributes(refreshed, selection.Auth)
	selection.Auth = refreshed
	selection.authMu.Unlock()
	return selection.CloneAuth(), true, nil
}
