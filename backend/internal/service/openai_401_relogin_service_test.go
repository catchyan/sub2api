//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type openAI401ReloginRepo struct {
	mockAccountRepoForGemini
	accounts               []Account
	updateCredentialsCalls int
	updateCalls            int
	clearTempCalls         int
	clearErrorCalls        int
	deleteCalls            int
	listWithFiltersCalls   int
}

func (r *openAI401ReloginRepo) ListActive(context.Context) ([]Account, error) {
	var out []Account
	for _, account := range r.accounts {
		if account.Status == "" || account.Status == StatusActive {
			out = append(out, account)
		}
	}
	return out, nil
}

func (r *openAI401ReloginRepo) ListWithFilters(_ context.Context, _ pagination.PaginationParams, platform, accountType, status, _ string, _ int64, _ string) ([]Account, *pagination.PaginationResult, error) {
	r.listWithFiltersCalls++
	now := time.Now()
	var out []Account
	for _, account := range r.accounts {
		if platform != "" && account.Platform != platform {
			continue
		}
		if accountType != "" && account.Type != accountType {
			continue
		}
		if status != "" {
			if status == "temp_unschedulable" {
				if account.Status != StatusActive || account.TempUnschedulableUntil == nil || !now.Before(*account.TempUnschedulableUntil) {
					continue
				}
			} else if account.Status != status {
				continue
			}
		}
		out = append(out, account)
	}
	return out, &pagination.PaginationResult{Total: int64(len(out))}, nil
}

func (r *openAI401ReloginRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.updateCredentialsCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts[i].Credentials = cloneCredentials(credentials)
			return nil
		}
	}
	return nil
}

func (r *openAI401ReloginRepo) Update(_ context.Context, account *Account) error {
	r.updateCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == account.ID {
			r.accounts[i] = *account
			r.accounts[i].Credentials = cloneCredentials(account.Credentials)
			r.accounts[i].Extra = cloneCredentials(account.Extra)
			return nil
		}
	}
	return nil
}

func (r *openAI401ReloginRepo) ClearError(_ context.Context, id int64) error {
	r.clearErrorCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts[i].Status = StatusActive
			r.accounts[i].ErrorMessage = ""
			return nil
		}
	}
	return nil
}

func (r *openAI401ReloginRepo) ClearTempUnschedulable(_ context.Context, id int64) error {
	r.clearTempCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts[i].TempUnschedulableUntil = nil
			r.accounts[i].TempUnschedulableReason = ""
		}
	}
	return nil
}

func (r *openAI401ReloginRepo) Delete(_ context.Context, id int64) error {
	r.deleteCalls++
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			r.accounts = append(r.accounts[:i], r.accounts[i+1:]...)
			return nil
		}
	}
	return nil
}

type openAI401ReloginRunnerStub struct {
	calls      int
	credential map[string]any
	err        error
}

func (r *openAI401ReloginRunnerStub) Run(context.Context, *Account, config.TokenRelogin401Config) (map[string]any, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return cloneCredentials(r.credential), nil
}

func TestOpenAI401ReloginService_ProcessOnceSuccess(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	reason := mustTempUnschedReason(t, 401, until)
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:                      1,
			Name:                    "acct",
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusActive,
			Credentials:             map[string]any{"email": "user@example.com", "refresh_token": "old-refresh"},
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: reason,
		}},
	}
	runner := &openAI401ReloginRunnerStub{credential: map[string]any{
		"access_token":  "new-access",
		"refresh_token": "new-refresh",
		"expires_in":    json.Number("3600"),
	}}
	invalidator := &tokenCacheInvalidatorStub{}
	cache := &tempUnschedCacheStub{}
	svc := newTestOpenAI401ReloginService(repo, runner, invalidator, cache)

	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, 1, runner.calls)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, 1, repo.clearTempCalls)
	require.Equal(t, 1, invalidator.calls)
	require.Equal(t, 1, cache.deleteCalls)
	require.Equal(t, "new-access", repo.accounts[0].GetCredential("access_token"))
	require.Equal(t, "new-refresh", repo.accounts[0].GetCredential("refresh_token"))
	require.NotEmpty(t, repo.accounts[0].GetCredential("expires_at"))
	require.Equal(t, "success", repo.accounts[0].GetCredential(relogin401LastResultKey))
	require.Nil(t, repo.accounts[0].TempUnschedulableUntil)
}

func TestOpenAI401ReloginService_ProcessOnceFailureOnlyOncePerEvent(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	reason := mustTempUnschedReason(t, 401, until)
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:                      1,
			Name:                    "user@example.com",
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusActive,
			Credentials:             map[string]any{"email": "user@example.com"},
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: reason,
		}},
	}
	runner := &openAI401ReloginRunnerStub{err: errors.New("login failed")}
	svc := newTestOpenAI401ReloginService(repo, runner, nil, nil)

	require.NoError(t, svc.ProcessOnce(context.Background()))
	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, 1, runner.calls)
	require.Equal(t, 1, repo.updateCredentialsCalls)
	require.Equal(t, 0, repo.updateCalls)
	require.Equal(t, 0, repo.clearTempCalls)
	require.Equal(t, "failed", repo.accounts[0].GetCredential(relogin401LastResultKey))
	require.Contains(t, repo.accounts[0].GetCredential(relogin401LastErrorKey), "login failed")
}

func TestOpenAI401ReloginService_ProcessOnceDeletesAfterFailureWhenEnabled(t *testing.T) {
	updatedAt := time.Now().Add(-time.Minute)
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:           45,
			Name:         "user@example.com",
			Platform:     PlatformOpenAI,
			Type:         AccountTypeOAuth,
			Status:       StatusError,
			ErrorMessage: "Token revoked (401): Your authentication token has been invalidated.",
			UpdatedAt:    updatedAt,
			Credentials:  map[string]any{"email": "user@example.com"},
		}},
	}
	runner := &openAI401ReloginRunnerStub{err: errors.New("login failed")}
	svc := newTestOpenAI401ReloginService(repo, runner, nil, nil)
	svc.cfg.DeleteOnFailure = true

	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, 1, runner.calls)
	require.Equal(t, 1, repo.updateCredentialsCalls)
	require.Equal(t, 1, repo.deleteCalls)
	require.Empty(t, repo.accounts)
}

func TestOpenAI401ReloginService_ProcessOnceSkipsNon401(t *testing.T) {
	until := time.Now().Add(10 * time.Minute)
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:                      1,
			Name:                    "user@example.com",
			Platform:                PlatformOpenAI,
			Type:                    AccountTypeOAuth,
			Status:                  StatusActive,
			Credentials:             map[string]any{"email": "user@example.com"},
			TempUnschedulableUntil:  &until,
			TempUnschedulableReason: mustTempUnschedReason(t, 429, until),
		}},
	}
	runner := &openAI401ReloginRunnerStub{credential: map[string]any{"access_token": "unused"}}
	svc := newTestOpenAI401ReloginService(repo, runner, nil, nil)

	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, 0, runner.calls)
	require.Equal(t, 0, repo.updateCredentialsCalls)
	require.Equal(t, 0, repo.updateCalls)
	require.Equal(t, 0, repo.clearTempCalls)
}

func TestOpenAI401ReloginService_ProcessOnceHandlesErrorStatus401(t *testing.T) {
	updatedAt := time.Now().Add(-time.Minute)
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:           45,
			Name:         "user@example.com",
			Platform:     PlatformOpenAI,
			Type:         AccountTypeOAuth,
			Status:       StatusError,
			ErrorMessage: "Token revoked (401): Your authentication token has been invalidated. Please try signing in again.",
			UpdatedAt:    updatedAt,
			Credentials:  map[string]any{"email": "user@example.com"},
		}},
	}
	runner := &openAI401ReloginRunnerStub{credential: map[string]any{
		"access_token": "new-access",
		"expires_in":   3600,
	}}
	invalidator := &tokenCacheInvalidatorStub{}
	svc := newTestOpenAI401ReloginService(repo, runner, invalidator, nil)

	require.NoError(t, svc.ProcessOnce(context.Background()))
	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, 1, runner.calls)
	require.Equal(t, 4, repo.listWithFiltersCalls)
	require.Equal(t, 1, repo.updateCalls)
	require.Equal(t, 1, repo.clearTempCalls)
	require.Equal(t, 0, repo.clearErrorCalls)
	require.Equal(t, 1, invalidator.calls)
	require.Equal(t, "new-access", repo.accounts[0].GetCredential("access_token"))
	require.Equal(t, "success", repo.accounts[0].GetCredential(relogin401LastResultKey))
	require.Equal(t, StatusActive, repo.accounts[0].Status)
}

func TestParseReloginCommandOutputSupportsNestedCredentials(t *testing.T) {
	payload, err := parseReloginCommandOutput([]byte(`{"credentials":{"access_token":"abc","expires_in":3600}}`))
	require.NoError(t, err)
	creds, _, err := parseOpenAI401ReloginPayload(payload)
	require.NoError(t, err)
	normalized, err := normalizeReloginCredentials(creds)
	require.NoError(t, err)
	require.Equal(t, "abc", credentialValueAsString(normalized["access_token"]))
	require.NotEmpty(t, credentialValueAsString(normalized["expires_at"]))
}

func TestParseReloginCommandOutputSupportsGuJumpgateSessionContent(t *testing.T) {
	accessToken := buildReloginTestJWT(t, time.Now().Add(time.Hour), map[string]any{
		"email": "claim@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-claim",
			"chatgpt_user_id":    "user-claim",
			"chatgpt_plan_type":  "plus",
		},
	})
	content, err := json.Marshal(map[string]any{
		"user": map[string]any{
			"id":    "user-json",
			"email": "json@example.com",
		},
		"account": map[string]any{
			"id":       "acct-json",
			"planType": "team",
		},
		"accessToken":  accessToken,
		"sessionToken": "secret-session-token",
		"expires":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)

	payload, err := parseReloginCommandOutput([]byte(fmt.Sprintf(`{"content":%q}`, string(content))))
	require.NoError(t, err)
	creds, extra, err := parseOpenAI401ReloginPayload(payload)
	require.NoError(t, err)

	require.Equal(t, accessToken, creds["access_token"])
	require.Equal(t, "json@example.com", creds["email"])
	require.Equal(t, "acct-json", creds["chatgpt_account_id"])
	require.Equal(t, "user-json", creds["chatgpt_user_id"])
	require.Equal(t, "team", creds["plan_type"])
	require.NotContains(t, creds, "session_token")
	require.Equal(t, "chatgpt_web_session", extra["source"])
	require.Equal(t, true, extra["session_token_present"])
	require.NotEmpty(t, extra["access_token_sha256"])
}

func TestOpenAI401ReloginService_ProcessOncePersistsSessionJSONExtra(t *testing.T) {
	accessToken := buildReloginTestJWT(t, time.Now().Add(time.Hour), map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-new",
			"chatgpt_user_id":    "user-new",
		},
	})
	repo := &openAI401ReloginRepo{
		accounts: []Account{{
			ID:           45,
			Name:         "user@example.com",
			Platform:     PlatformOpenAI,
			Type:         AccountTypeOAuth,
			Status:       StatusError,
			ErrorMessage: "Token revoked (401): Your authentication token has been invalidated.",
			UpdatedAt:    time.Now().Add(-time.Minute),
			Credentials:  map[string]any{"email": "user@example.com", "refresh_token": "stale-refresh"},
			Extra:        map[string]any{"openai_passthrough": true},
		}},
	}
	runner := &openAI401ReloginRunnerStub{credential: map[string]any{
		"session": map[string]any{
			"accessToken": accessToken,
			"user": map[string]any{
				"email": "user@example.com",
			},
		},
	}}
	svc := newTestOpenAI401ReloginService(repo, runner, nil, nil)

	require.NoError(t, svc.ProcessOnce(context.Background()))

	require.Equal(t, "acct-new", repo.accounts[0].GetCredential("chatgpt_account_id"))
	require.Equal(t, "chatgpt_web_session", repo.accounts[0].Extra["source"])
	require.Equal(t, true, repo.accounts[0].Extra["openai_passthrough"])
	require.Equal(t, StatusActive, repo.accounts[0].Status)
}

func newTestOpenAI401ReloginService(repo *openAI401ReloginRepo, runner openAI401ReloginRunner, invalidator TokenCacheInvalidator, cache TempUnschedCache) *OpenAI401ReloginService {
	svc := NewOpenAI401ReloginService(repo, nil, invalidator, cache, &config.Config{
		TokenRefresh: config.TokenRefreshConfig{
			Relogin401: config.TokenRelogin401Config{
				Enabled:              true,
				Command:              []string{"unused"},
				CheckIntervalSeconds: 60,
				TimeoutSeconds:       10,
				MaxAccountsPerCycle:  5,
				DeleteOnFailure:      false,
			},
		},
	})
	svc.runner = runner
	return svc
}

func buildReloginTestJWT(t *testing.T, exp time.Time, extraClaims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "none", "typ": "JWT"}
	claims := map[string]any{
		"sub": "user-from-sub",
		"exp": exp.Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extraClaims {
		claims[k] = v
	}
	headerBytes, err := json.Marshal(header)
	require.NoError(t, err)
	claimBytes, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimBytes) + "."
}

func mustTempUnschedReason(t *testing.T, status int, until time.Time) string {
	t.Helper()
	raw, err := json.Marshal(TempUnschedState{
		UntilUnix:       until.Unix(),
		TriggeredAtUnix: time.Now().Unix(),
		StatusCode:      status,
		ErrorMessage:    "unauthorized",
	})
	require.NoError(t, err)
	return string(raw)
}
