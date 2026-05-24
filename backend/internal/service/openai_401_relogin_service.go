package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const (
	relogin401AttemptedEventKey = "relogin_401_attempted_event"
	relogin401AttemptedAtKey    = "relogin_401_attempted_at"
	relogin401LastResultKey     = "relogin_401_last_result"
	relogin401LastErrorKey      = "relogin_401_last_error"
)

type openAI401ReloginRunner interface {
	Run(ctx context.Context, account *Account, cfg config.TokenRelogin401Config) (map[string]any, error)
}

type OpenAI401ReloginService struct {
	accountRepo      AccountRepository
	settingService   *SettingService
	cacheInvalidator TokenCacheInvalidator
	tempUnschedCache TempUnschedCache
	cfg              config.TokenRelogin401Config
	runner           openAI401ReloginRunner

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewOpenAI401ReloginService(
	accountRepo AccountRepository,
	settingService *SettingService,
	cacheInvalidator TokenCacheInvalidator,
	tempUnschedCache TempUnschedCache,
	cfg *config.Config,
) *OpenAI401ReloginService {
	reloginCfg := config.TokenRelogin401Config{}
	if cfg != nil {
		reloginCfg = cfg.TokenRefresh.Relogin401
	}
	normalizeOpenAI401ReloginConfig(&reloginCfg)
	return &OpenAI401ReloginService{
		accountRepo:      accountRepo,
		settingService:   settingService,
		cacheInvalidator: cacheInvalidator,
		tempUnschedCache: tempUnschedCache,
		cfg:              reloginCfg,
		runner:           commandOpenAI401ReloginRunner{},
		stopCh:           make(chan struct{}),
	}
}

func (s *OpenAI401ReloginService) Start() {
	if s == nil {
		return
	}
	if s.accountRepo == nil {
		slog.Warn("openai_401_relogin.service_disabled", "reason", "account_repo_nil")
		return
	}

	s.wg.Add(1)
	go s.loop()
	slog.Info("openai_401_relogin.service_started",
		"check_interval_seconds", s.cfg.CheckIntervalSeconds)
}

func (s *OpenAI401ReloginService) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
}

func (s *OpenAI401ReloginService) loop() {
	defer s.wg.Done()

	interval := time.Duration(s.cfg.CheckIntervalSeconds) * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-timer.C:
			settings, err := s.loadSettings(context.Background())
			if err != nil {
				slog.Warn("openai_401_relogin.load_settings_failed", "error", err)
			} else if err := s.processOnceWithSettings(context.Background(), settings); err != nil {
				slog.Warn("openai_401_relogin.cycle_failed", "error", err)
			}
			next := s.cfg.CheckIntervalSeconds
			if settings != nil && settings.CheckIntervalSeconds > 0 {
				next = settings.CheckIntervalSeconds
			}
			if next <= 0 {
				next = 60
			}
			interval = time.Duration(next) * time.Second
			timer.Reset(interval)
		}
	}
}

func (s *OpenAI401ReloginService) ProcessOnce(ctx context.Context) error {
	settings, err := s.loadSettings(ctx)
	if err != nil {
		return err
	}
	return s.processOnceWithSettings(ctx, settings)
}

func (s *OpenAI401ReloginService) processOnceWithSettings(ctx context.Context, settings *OpenAI401GuardSettings) error {
	if s == nil || settings == nil || !settings.Enabled || s.accountRepo == nil {
		return nil
	}
	if settings.ProviderType == OpenAI401ProviderExternalCommand && len(settings.SessionProviderCommand) == 0 {
		slog.Warn("openai_401_relogin.skip_cycle", "reason", "session_provider_command_empty")
		return nil
	}
	cfg := openAI401GuardToReloginConfig(settings)
	accounts, err := s.listCandidateAccounts(ctx, cfg.MaxAccountsPerCycle)
	if err != nil {
		return err
	}
	slog.Info("openai_401_relogin.scan_completed",
		"candidates", len(accounts),
		"max_accounts_per_cycle", cfg.MaxAccountsPerCycle)

	processed := 0
	for i := range accounts {
		if cfg.MaxAccountsPerCycle > 0 && processed >= cfg.MaxAccountsPerCycle {
			break
		}
		account := accounts[i]
		eventKey, ok := openAI401ReloginEventKey(&account, time.Now())
		if !ok {
			continue
		}
		if strings.TrimSpace(account.GetCredential(relogin401AttemptedEventKey)) == eventKey {
			continue
		}
		email := openAI401ReloginEmail(&account)
		if email == "" {
			slog.Warn("openai_401_relogin.skip_no_email", "account_id", account.ID)
			if err := s.markAttempt(ctx, &account, eventKey, "skipped", "missing email credential"); err != nil {
				slog.Warn("openai_401_relogin.mark_skip_failed", "account_id", account.ID, "error", err)
			}
			processed++
			continue
		}
		if !openAI401EmailDomainAllowed(email, cfg.AllowedEmailDomains) {
			slog.Info("openai_401_relogin.skip_email_domain",
				"account_id", account.ID,
				"email_domain", openAI401EmailDomain(email))
			if err := s.markAttempt(ctx, &account, eventKey, "skipped", "email domain is not in OpenAI 401 allowed domains"); err != nil {
				slog.Warn("openai_401_relogin.mark_skip_failed", "account_id", account.ID, "error", err)
			}
			processed++
			continue
		}

		processed++
		if err := s.reloginAccount(ctx, &account, eventKey, cfg, settings); err != nil {
			slog.Warn("openai_401_relogin.account_failed", "account_id", account.ID, "error", err)
		}
	}

	return nil
}

func (s *OpenAI401ReloginService) loadSettings(ctx context.Context) (*OpenAI401GuardSettings, error) {
	if s == nil {
		return nil, nil
	}
	if s.settingService != nil {
		return s.settingService.GetOpenAI401GuardSettings(ctx)
	}
	settings := DefaultOpenAI401GuardSettings()
	settings.Enabled = s.cfg.Enabled
	settings.CheckIntervalSeconds = s.cfg.CheckIntervalSeconds
	settings.ProviderType = s.cfg.ProviderType
	settings.TimeoutSeconds = s.cfg.TimeoutSeconds
	settings.MaxAccountsPerCycle = s.cfg.MaxAccountsPerCycle
	settings.DeleteOnFailure = s.cfg.DeleteOnFailure
	settings.SessionProviderCommand = append([]string(nil), s.cfg.Command...)
	settings.IncludeCredentialsEnv = s.cfg.IncludeCredentialsEnv
	settings.TempEmailBaseURL = s.cfg.TempEmailBaseURL
	settings.AllowedEmailDomains = append([]string(nil), s.cfg.AllowedEmailDomains...)
	if envName := strings.TrimSpace(s.cfg.TempEmailAdminAuthEnv); envName != "" {
		settings.TempEmailAdminAuth = strings.TrimSpace(os.Getenv(envName))
	}
	normalizeOpenAI401GuardSettings(settings)
	settings.TempEmailAdminConfigured = strings.TrimSpace(settings.TempEmailAdminAuth) != ""
	return settings, nil
}

func (s *OpenAI401ReloginService) listCandidateAccounts(ctx context.Context, limit int) ([]Account, error) {
	if limit <= 0 {
		limit = 5
	}
	params := pagination.PaginationParams{
		Page:      1,
		PageSize:  limit,
		SortBy:    "name",
		SortOrder: pagination.SortOrderAsc,
	}

	errorAccounts, _, err := s.accountRepo.ListWithFilters(ctx, params, PlatformOpenAI, AccountTypeOAuth, StatusError, "", 0, "")
	if err != nil {
		return nil, err
	}
	tempAccounts, _, err := s.accountRepo.ListWithFilters(ctx, params, PlatformOpenAI, AccountTypeOAuth, "temp_unschedulable", "", 0, "")
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]struct{}, len(errorAccounts)+len(tempAccounts))
	out := make([]Account, 0, len(errorAccounts)+len(tempAccounts))
	for _, account := range errorAccounts {
		if _, ok := seen[account.ID]; ok {
			continue
		}
		seen[account.ID] = struct{}{}
		out = append(out, account)
	}
	for _, account := range tempAccounts {
		if _, ok := seen[account.ID]; ok {
			continue
		}
		seen[account.ID] = struct{}{}
		out = append(out, account)
	}
	return out, nil
}

func (s *OpenAI401ReloginService) reloginAccount(ctx context.Context, account *Account, eventKey string, cfg config.TokenRelogin401Config, settings *OpenAI401GuardSettings) error {
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	start := time.Now()
	credentials, extra, err := s.runSessionRepair(commandCtx, account, cfg, settings)
	if err != nil {
		if markErr := s.markAttempt(ctx, account, eventKey, "failed", err.Error()); markErr != nil {
			return errors.Join(err, markErr)
		}
		if cfg.DeleteOnFailure && account.Status == StatusError {
			if deleteErr := s.accountRepo.Delete(ctx, account.ID); deleteErr != nil {
				return errors.Join(err, fmt.Errorf("delete failed account: %w", deleteErr))
			}
			slog.Warn("openai_401_relogin.account_deleted_after_failure",
				"account_id", account.ID,
				"account_name", account.Name,
				"duration_ms", time.Since(start).Milliseconds())
		}
		return err
	}

	merged := MergeCredentials(account.Credentials, credentials)
	merged["_token_version"] = time.Now().UnixNano()
	merged[relogin401AttemptedEventKey] = eventKey
	merged[relogin401AttemptedAtKey] = time.Now().Format(time.RFC3339)
	merged[relogin401LastResultKey] = "success"
	delete(merged, relogin401LastErrorKey)

	mergedExtra := mergeOpenAI401ReloginExtra(account.Extra, extra)
	if err := persistOpenAI401ReloginAccount(ctx, s.accountRepo, account, merged, mergedExtra); err != nil {
		return fmt.Errorf("persist relogin credentials: %w", err)
	}
	if account.Status == StatusError {
		if err := s.accountRepo.ClearError(ctx, account.ID); err != nil {
			return fmt.Errorf("clear account error: %w", err)
		}
	}
	if err := s.accountRepo.ClearTempUnschedulable(ctx, account.ID); err != nil {
		return fmt.Errorf("clear temp unschedulable: %w", err)
	}
	if s.tempUnschedCache != nil {
		if err := s.tempUnschedCache.DeleteTempUnsched(ctx, account.ID); err != nil {
			slog.Warn("openai_401_relogin.clear_temp_unsched_cache_failed", "account_id", account.ID, "error", err)
		}
	}
	if s.cacheInvalidator != nil {
		account.Credentials = merged
		if err := s.cacheInvalidator.InvalidateToken(ctx, account); err != nil {
			slog.Warn("openai_401_relogin.invalidate_token_cache_failed", "account_id", account.ID, "error", err)
		}
	}

	slog.Info("openai_401_relogin.account_succeeded",
		"account_id", account.ID,
		"account_name", account.Name,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (s *OpenAI401ReloginService) runSessionRepair(ctx context.Context, account *Account, cfg config.TokenRelogin401Config, settings *OpenAI401GuardSettings) (map[string]any, map[string]any, error) {
	runner := s.runner
	if settings != nil && settings.ProviderType == OpenAI401ProviderBuiltinCloudflareTempEmail {
		runner = builtinOpenAI401ReloginRunner{}
	}
	if runner == nil {
		return nil, nil, errors.New("relogin runner is not configured")
	}
	payload, err := runner.Run(ctx, account, cfg)
	if err != nil {
		return nil, nil, err
	}
	credentials, extra, err := parseOpenAI401ReloginPayload(payload)
	if err != nil {
		return nil, nil, err
	}
	credentials, err = normalizeReloginCredentials(credentials)
	if err != nil {
		return nil, nil, err
	}
	return credentials, extra, nil
}

func (s *OpenAI401ReloginService) markAttempt(ctx context.Context, account *Account, eventKey, result, errText string) error {
	creds := cloneCredentials(account.Credentials)
	creds[relogin401AttemptedEventKey] = eventKey
	creds[relogin401AttemptedAtKey] = time.Now().Format(time.RFC3339)
	creds[relogin401LastResultKey] = result
	if errText == "" {
		delete(creds, relogin401LastErrorKey)
	} else {
		creds[relogin401LastErrorKey] = truncateRelogin401Error(errText)
	}
	return persistAccountCredentials(ctx, s.accountRepo, account, creds)
}

func persistOpenAI401ReloginAccount(ctx context.Context, repo AccountRepository, account *Account, credentials, extra map[string]any) error {
	if repo == nil || account == nil {
		return nil
	}
	account.Credentials = cloneCredentials(credentials)
	account.Extra = cloneCredentials(extra)
	if account.Status == StatusError {
		account.Status = StatusActive
		account.ErrorMessage = ""
	}
	return repo.Update(ctx, account)
}

type commandOpenAI401ReloginRunner struct{}

func (commandOpenAI401ReloginRunner) Run(ctx context.Context, account *Account, cfg config.TokenRelogin401Config) (map[string]any, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("relogin command is empty")
	}
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = openAI401ReloginEnv(account, cfg)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("relogin command failed: %w: %s", err, truncateRelogin401Error(msg))
		}
		return nil, fmt.Errorf("relogin command failed: %w", err)
	}
	return parseReloginCommandOutput(stdout.Bytes())
}

func openAI401ReloginEnv(account *Account, cfg config.TokenRelogin401Config) []string {
	env := os.Environ()
	env = append(env,
		"SUB2API_ACCOUNT_ID="+strconv.FormatInt(account.ID, 10),
		"SUB2API_ACCOUNT_NAME="+account.Name,
		"SUB2API_ACCOUNT_EMAIL="+openAI401ReloginEmail(account),
		"SUB2API_ACCOUNT_PLATFORM="+account.Platform,
		"SUB2API_ACCOUNT_TYPE="+account.Type,
		"SUB2API_SESSION_IMPORT_FORMAT=codex_session_json",
	)
	if proxy := strings.TrimSpace(account.GetCredential("proxy_url")); proxy != "" {
		env = append(env, "SUB2API_PROXY_URL="+proxy)
	}
	if baseURL := strings.TrimSpace(cfg.TempEmailBaseURL); baseURL != "" {
		env = append(env, "SUB2API_TEMP_EMAIL_BASE_URL="+baseURL)
	}
	if authEnv := strings.TrimSpace(cfg.TempEmailAdminAuthEnv); authEnv != "" {
		env = append(env, "SUB2API_TEMP_EMAIL_ADMIN_AUTH_ENV="+authEnv)
		if auth := strings.TrimSpace(os.Getenv(authEnv)); auth != "" {
			env = append(env, "SUB2API_TEMP_EMAIL_ADMIN_AUTH="+auth)
		}
	}
	if auth := strings.TrimSpace(cfg.TempEmailAdminAuth); auth != "" {
		env = append(env, "SUB2API_TEMP_EMAIL_ADMIN_AUTH="+auth)
		env = append(env, "SUB2API_TEMP_EMAIL_ADMIN_AUTH_CONFIGURED=true")
	}
	if cfg.IncludeCredentialsEnv {
		if raw, err := json.Marshal(account.Credentials); err == nil {
			env = append(env, "SUB2API_ACCOUNT_CREDENTIALS_JSON="+string(raw))
		}
	}
	return env
}

func parseReloginCommandOutput(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("relogin command produced empty stdout")
	}

	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse relogin stdout JSON: %w", err)
	}
	return payload, nil
}

func parseOpenAI401ReloginPayload(payload map[string]any) (map[string]any, map[string]any, error) {
	if len(payload) == 0 {
		return nil, nil, errors.New("relogin output is empty")
	}

	if nested, ok := mapValue(payload, "payload"); ok {
		payload = nested
	}
	if nested, ok := mapValue(payload, "data"); ok {
		payload = nested
	}

	if content := firstReloginString(payload, "content", "session_json", "sessionJSON"); content != "" {
		session, err := parseOpenAI401SessionContent(content)
		if err != nil {
			return nil, nil, err
		}
		if extra, ok := mapValue(payload, "extra"); ok {
			session.extra = mergeOpenAI401ReloginExtra(extra, session.extra)
		}
		return session.credentials, session.extra, nil
	}

	sessionInput := payload
	if session, ok := mapValue(payload, "session"); ok {
		sessionInput = cloneCredentials(session)
		if accessToken := firstReloginString(payload, "accessToken", "access_token"); accessToken != "" {
			sessionInput["accessToken"] = accessToken
		}
		session, err := convertOpenAI401SessionJSON(sessionInput)
		if err != nil {
			return nil, nil, err
		}
		if extra, ok := mapValue(payload, "extra"); ok {
			session.extra = mergeOpenAI401ReloginExtra(extra, session.extra)
		}
		return session.credentials, session.extra, nil
	}
	if looksLikeOpenAI401SessionJSON(sessionInput) {
		session, err := convertOpenAI401SessionJSON(sessionInput)
		if err != nil {
			return nil, nil, err
		}
		if extra, ok := mapValue(payload, "extra"); ok {
			session.extra = mergeOpenAI401ReloginExtra(extra, session.extra)
		}
		return session.credentials, session.extra, nil
	}

	if nested, ok := mapValue(payload, "credentials"); ok {
		extra, _ := mapValue(payload, "extra")
		return cloneCredentials(nested), cloneCredentials(extra), nil
	}

	return cloneCredentials(payload), nil, nil
}

type openAI401SessionDocument struct {
	credentials map[string]any
	extra       map[string]any
}

func parseOpenAI401SessionContent(content string) (*openAI401SessionDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("session content is empty")
	}
	if !looksLikeJSON(content) {
		return convertOpenAI401SessionJSON(map[string]any{"accessToken": content})
	}

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	values := make([]any, 0, 1)
	for {
		var value any
		err := decoder.Decode(&value)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse session content JSON: %w", err)
		}
		values = append(values, value)
	}
	flattened := flattenOpenAI401SessionValues(values)
	if len(flattened) != 1 {
		return nil, fmt.Errorf("session content must contain exactly one account, got %d", len(flattened))
	}
	switch value := flattened[0].(type) {
	case string:
		return convertOpenAI401SessionJSON(map[string]any{"accessToken": value})
	case map[string]any:
		return convertOpenAI401SessionJSON(value)
	default:
		return nil, errors.New("session content account must be an object or access token string")
	}
}

func flattenOpenAI401SessionValues(values []any) []any {
	out := make([]any, 0, len(values))
	var appendValue func(any)
	appendValue = func(value any) {
		if arr, ok := value.([]any); ok {
			for _, item := range arr {
				appendValue(item)
			}
			return
		}
		out = append(out, value)
	}
	for _, value := range values {
		appendValue(value)
	}
	return out
}

func convertOpenAI401SessionJSON(raw map[string]any) (*openAI401SessionDocument, error) {
	accessToken := firstReloginString(raw,
		"accessToken", "access_token", "token",
		"tokens.accessToken", "tokens.access_token",
		"credentials.accessToken", "credentials.access_token",
	)
	if accessToken == "" {
		return nil, errors.New("session JSON missing accessToken/access_token")
	}

	claims := parseOpenAI401JWTClaims(accessToken)
	auth := map[string]any{}
	if claims != nil {
		if nested, ok := mapValue(claims, "https://api.openai.com/auth"); ok {
			auth = nested
		}
	}
	expiresAt := firstReloginTime(
		claimsValue(claims, "exp"),
		pathAny(raw, "tokens.expires_at"),
		pathAny(raw, "tokens.expiresAt"),
		pathAny(raw, "expires_at"),
		pathAny(raw, "expiresAt"),
		pathAny(raw, "expires"),
	)
	if !expiresAt.IsZero() && time.Now().UTC().After(expiresAt.Add(120*time.Second)) {
		return nil, fmt.Errorf("session access_token expired at %s", expiresAt.Format(time.RFC3339))
	}

	email := firstReloginString(raw,
		"user.email", "email", "credentials.email", "providerSpecificData.email",
	)
	if email == "" {
		email = openAI401StringFromAny(claimsValue(claims, "email"))
	}
	accountID := firstReloginString(raw,
		"account.id", "account.account_id", "account.chatgpt_account_id",
		"account_id", "accountId", "chatgptAccountId", "chatgpt_account_id",
		"providerSpecificData.chatgptAccountId", "providerSpecificData.chatgpt_account_id",
		"credentials.chatgpt_account_id",
	)
	if accountID == "" {
		accountID = firstReloginString(auth, "chatgpt_account_id")
	}
	userID := firstReloginString(raw,
		"user.id", "user_id", "userId", "chatgptUserId", "chatgpt_user_id",
		"providerSpecificData.chatgptUserId", "providerSpecificData.chatgpt_user_id",
	)
	if userID == "" {
		userID = firstReloginString(auth, "chatgpt_user_id", "user_id")
	}
	if userID == "" {
		userID = openAI401StringFromAny(claimsValue(claims, "sub"))
	}
	planType := firstReloginString(raw,
		"account.planType", "account.plan_type", "planType", "plan_type",
		"providerSpecificData.chatgptPlanType", "providerSpecificData.chatgpt_plan_type",
		"credentials.plan_type",
	)
	if planType == "" {
		planType = firstReloginString(auth, "chatgpt_plan_type")
	}

	credentials := map[string]any{"access_token": accessToken}
	setReloginIfNotEmpty(credentials, "email", email)
	setReloginIfNotEmpty(credentials, "chatgpt_account_id", accountID)
	setReloginIfNotEmpty(credentials, "chatgpt_user_id", userID)
	setReloginIfNotEmpty(credentials, "plan_type", planType)
	if refreshToken := firstReloginString(raw, "refreshToken", "refresh_token", "tokens.refreshToken", "tokens.refresh_token", "credentials.refresh_token"); refreshToken != "" {
		credentials["refresh_token"] = refreshToken
	}
	if idToken := firstReloginString(raw, "idToken", "id_token", "tokens.idToken", "tokens.id_token", "credentials.id_token"); idToken != "" {
		credentials["id_token"] = idToken
	}
	if !expiresAt.IsZero() {
		credentials["expires_at"] = expiresAt.Format(time.RFC3339)
		expiresIn := int64(time.Until(expiresAt).Seconds())
		if expiresIn < 0 {
			expiresIn = 0
		}
		credentials["expires_in"] = expiresIn
	}

	now := time.Now().UTC().Format(time.RFC3339)
	extra := map[string]any{
		"source":              "chatgpt_web_session",
		"import_source":       "codex_session",
		"last_refresh":        now,
		"relogin_401_source":  "chatgpt_web_session",
		"access_token_sha256": codexTokenFingerprint(accessToken),
	}
	setReloginIfNotEmpty(extra, "email", email)
	if sessionToken := firstReloginString(raw, "sessionToken", "session_token", "tokens.sessionToken", "tokens.session_token", "credentials.session_token"); sessionToken != "" {
		extra["session_token_present"] = true
	}
	if sessionExpiresAt := firstReloginTime(pathAny(raw, "expires")); !sessionExpiresAt.IsZero() {
		extra["session_expires_at"] = sessionExpiresAt.Format(time.RFC3339)
	}
	copyReloginExtraString(raw, extra, "user_image", "user.image")
	copyReloginExtraString(raw, extra, "user_picture", "user.picture")
	copyReloginExtraString(raw, extra, "account_structure", "account.structure")
	copyReloginExtraString(raw, extra, "account_residency_region", "account.residencyRegion")
	copyReloginExtraString(raw, extra, "compute_residency", "account.computeResidency")

	return &openAI401SessionDocument{credentials: credentials, extra: extra}, nil
}

func looksLikeOpenAI401SessionJSON(raw map[string]any) bool {
	if len(raw) == 0 {
		return false
	}
	return firstReloginString(raw, "accessToken", "tokens.accessToken", "tokens.access_token") != "" ||
		mapHasAny(raw, "session", "user", "account", "providerSpecificData")
}

func mergeOpenAI401ReloginExtra(existing, incoming map[string]any) map[string]any {
	out := cloneCredentials(existing)
	for key, value := range incoming {
		out[key] = value
	}
	return out
}

func mapValue(raw map[string]any, key string) (map[string]any, bool) {
	value, ok := raw[key]
	if !ok {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	return obj, ok
}

func mapHasAny(raw map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := raw[key]; ok {
			return true
		}
	}
	return false
}

func firstReloginString(raw map[string]any, paths ...string) string {
	for _, path := range paths {
		if value, ok := pathValue(raw, path); ok {
			if str := openAI401StringFromAny(value); str != "" {
				return str
			}
		}
	}
	return ""
}

func pathValue(raw map[string]any, path string) (any, bool) {
	if raw == nil {
		return nil, false
	}
	parts := strings.Split(path, ".")
	var current any = raw
	for _, part := range parts {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := obj[part]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func pathAny(raw map[string]any, path string) any {
	value, _ := pathValue(raw, path)
	return value
}

func claimsValue(claims map[string]any, key string) any {
	if claims == nil {
		return nil
	}
	return claims[key]
}

func openAI401StringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return ""
	}
}

func firstReloginTime(values ...any) time.Time {
	for _, value := range values {
		if t, ok := parseReloginTimeValue(value); ok {
			return t
		}
	}
	return time.Time{}
}

func parseReloginTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return parsed.UTC(), true
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return reloginUnixTime(n), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return reloginUnixTime(n), true
		}
	case float64:
		return reloginUnixTime(int64(v)), true
	case int64:
		return reloginUnixTime(v), true
	case int:
		return reloginUnixTime(int64(v)), true
	}
	return time.Time{}, false
}

func reloginUnixTime(value int64) time.Time {
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}

func parseOpenAI401JWTClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	decoded, err := decodeReloginJWTPart(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.UseNumber()
	if err := decoder.Decode(&claims); err != nil {
		return nil
	}
	return claims
}

func decodeReloginJWTPart(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	padded := segment
	if rem := len(padded) % 4; rem > 0 {
		padded += strings.Repeat("=", 4-rem)
	}
	if decoded, err := base64.URLEncoding.DecodeString(padded); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(padded)
}

func setReloginIfNotEmpty(target map[string]any, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		target[key] = value
	}
}

func copyReloginExtraString(raw, extra map[string]any, key, path string) {
	if value := firstReloginString(raw, path); value != "" {
		extra[key] = value
	}
}

func codexTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func looksLikeJSON(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	return content[0] == '{' || content[0] == '['
}

func normalizeReloginCredentials(credentials map[string]any) (map[string]any, error) {
	if strings.TrimSpace(credentialValueAsString(credentials["access_token"])) == "" {
		return nil, errors.New("relogin output missing access_token")
	}

	out := cloneCredentials(credentials)
	if strings.TrimSpace(credentialValueAsString(out["expires_at"])) == "" {
		if expiresIn, ok := credentialValueAsInt64(out["expires_in"]); ok && expiresIn > 0 {
			out["expires_at"] = time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
		}
	}
	return out, nil
}

func openAI401ReloginEventKey(account *Account, now time.Time) (string, bool) {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return "", false
	}

	if account.Status == StatusError && openAI401ReloginErrorMatches(account.ErrorMessage) {
		return fmt.Sprintf("error:%d:%d:%s", account.ID, account.UpdatedAt.Unix(), shortRelogin401Hash(account.ErrorMessage)), true
	}

	if account.TempUnschedulableUntil != nil &&
		now.Before(*account.TempUnschedulableUntil) &&
		openAI401ReloginReasonMatches(account.TempUnschedulableReason) {
		state, ok := parseReloginTempUnschedState(account.TempUnschedulableReason)
		if ok {
			return fmt.Sprintf("temp:%d:status:%d:triggered:%d:until:%d", account.ID, state.StatusCode, state.TriggeredAtUnix, state.UntilUnix), true
		}

		return "temp-legacy:" + shortRelogin401Hash(account.TempUnschedulableReason+"|"+account.TempUnschedulableUntil.UTC().Format(time.RFC3339Nano)), true
	}

	return "", false
}

func openAI401ReloginReasonMatches(reason string) bool {
	if state, ok := parseReloginTempUnschedState(reason); ok {
		return state.StatusCode == http.StatusUnauthorized
	}
	if wasTempUnschedByStatusCode(reason, http.StatusUnauthorized) {
		return true
	}
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "oauth 401")
}

func openAI401ReloginErrorMatches(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "401") ||
		strings.Contains(lower, "token revoked") ||
		strings.Contains(lower, "token has been invalidated") ||
		strings.Contains(lower, "authentication token has been invalidated")
}

func shortRelogin401Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func parseReloginTempUnschedState(reason string) (TempUnschedState, bool) {
	var state TempUnschedState
	if strings.TrimSpace(reason) == "" {
		return state, false
	}
	if err := json.Unmarshal([]byte(reason), &state); err != nil {
		return state, false
	}
	if state.StatusCode == 0 && state.TriggeredAtUnix == 0 && state.UntilUnix == 0 {
		return state, false
	}
	return state, true
}

func openAI401ReloginEmail(account *Account) string {
	if account == nil {
		return ""
	}
	for _, key := range []string{"email", "login_email", "account_email", "username"} {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" && strings.Contains(value, "@") {
			return value
		}
	}
	if value := strings.TrimSpace(account.Name); strings.Contains(value, "@") {
		return value
	}
	return ""
}

func openAI401EmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return strings.Trim(domain, ".")
}

func openAI401EmailDomainAllowed(email string, allowedDomains []string) bool {
	normalized := normalizeOpenAI401AllowedEmailDomains(allowedDomains)
	if len(normalized) == 0 {
		return true
	}
	domain := openAI401EmailDomain(email)
	if domain == "" {
		return false
	}
	for _, allowed := range normalized {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func normalizeOpenAI401ReloginConfig(cfg *config.TokenRelogin401Config) {
	if cfg.CheckIntervalSeconds <= 0 {
		cfg.CheckIntervalSeconds = 60
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 300
	}
	if cfg.MaxAccountsPerCycle <= 0 {
		cfg.MaxAccountsPerCycle = 5
	}
	if strings.TrimSpace(cfg.TempEmailAdminAuthEnv) == "" {
		cfg.TempEmailAdminAuthEnv = "SUB2API_TEMP_EMAIL_ADMIN_AUTH"
	}
	cfg.AllowedEmailDomains = normalizeOpenAI401AllowedEmailDomains(cfg.AllowedEmailDomains)
}

func credentialValueAsString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case json.Number:
		return val.String()
	case fmt.Stringer:
		return val.String()
	case float64:
		return strconv.FormatInt(int64(val), 10)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	default:
		return ""
	}
}

func credentialValueAsInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case json.Number:
		i, err := val.Int64()
		return i, err == nil
	case float64:
		return int64(val), true
	case int64:
		return val, true
	case int:
		return int64(val), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func truncateRelogin401Error(s string) string {
	const limit = 512
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
