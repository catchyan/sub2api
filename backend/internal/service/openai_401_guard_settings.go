package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// OpenAI401GuardSettings controls the OpenAI OAuth 401 repair daemon.
type OpenAI401GuardSettings struct {
	Enabled                  bool     `json:"enabled"`
	CheckIntervalSeconds     int      `json:"check_interval_seconds"`
	TimeoutSeconds           int      `json:"timeout_seconds"`
	MaxAccountsPerCycle      int      `json:"max_accounts_per_cycle"`
	DeleteOnFailure          bool     `json:"delete_on_failure"`
	SessionProviderCommand   []string `json:"session_provider_command"`
	IncludeCredentialsEnv    bool     `json:"include_credentials_env"`
	TempEmailBaseURL         string   `json:"temp_email_base_url"`
	TempEmailAdminAuth       string   `json:"temp_email_admin_auth,omitempty"`
	TempEmailAdminConfigured bool     `json:"temp_email_admin_auth_configured"`
}

func DefaultOpenAI401GuardSettings() *OpenAI401GuardSettings {
	return &OpenAI401GuardSettings{
		Enabled:                false,
		CheckIntervalSeconds:   60,
		TimeoutSeconds:         300,
		MaxAccountsPerCycle:    5,
		DeleteOnFailure:        false,
		SessionProviderCommand: []string{},
		IncludeCredentialsEnv:  false,
	}
}

func (s *SettingService) GetOpenAI401GuardSettings(ctx context.Context) (*OpenAI401GuardSettings, error) {
	base := DefaultOpenAI401GuardSettings()
	if s != nil && s.cfg != nil {
		applyOpenAI401GuardConfig(base, s.cfg.TokenRefresh.Relogin401)
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAI401GuardSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			normalizeOpenAI401GuardSettings(base)
			base.TempEmailAdminConfigured = strings.TrimSpace(base.TempEmailAdminAuth) != ""
			return base, nil
		}
		return nil, fmt.Errorf("get openai 401 guard settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		normalizeOpenAI401GuardSettings(base)
		base.TempEmailAdminConfigured = strings.TrimSpace(base.TempEmailAdminAuth) != ""
		return base, nil
	}

	var stored OpenAI401GuardSettings
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		slog.Warn("failed to unmarshal openai 401 guard settings, falling back to defaults",
			"error", err,
			"key", SettingKeyOpenAI401GuardSettings)
		normalizeOpenAI401GuardSettings(base)
		base.TempEmailAdminConfigured = strings.TrimSpace(base.TempEmailAdminAuth) != ""
		return base, nil
	}
	mergeOpenAI401GuardSettings(base, &stored)
	normalizeOpenAI401GuardSettings(base)
	base.TempEmailAdminConfigured = strings.TrimSpace(base.TempEmailAdminAuth) != ""
	return base, nil
}

func (s *SettingService) SetOpenAI401GuardSettings(ctx context.Context, settings *OpenAI401GuardSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	current, err := s.GetOpenAI401GuardSettings(ctx)
	if err != nil {
		return err
	}
	next := *settings
	if strings.TrimSpace(next.TempEmailAdminAuth) == "" {
		next.TempEmailAdminAuth = current.TempEmailAdminAuth
	}
	normalizeOpenAI401GuardSettings(&next)
	data, err := json.Marshal(&next)
	if err != nil {
		return fmt.Errorf("marshal openai 401 guard settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyOpenAI401GuardSettings, string(data))
}

func applyOpenAI401GuardConfig(target *OpenAI401GuardSettings, cfg config.TokenRelogin401Config) {
	if target == nil {
		return
	}
	target.Enabled = cfg.Enabled
	target.CheckIntervalSeconds = cfg.CheckIntervalSeconds
	target.TimeoutSeconds = cfg.TimeoutSeconds
	target.MaxAccountsPerCycle = cfg.MaxAccountsPerCycle
	target.DeleteOnFailure = cfg.DeleteOnFailure
	target.SessionProviderCommand = append([]string(nil), cfg.Command...)
	target.IncludeCredentialsEnv = cfg.IncludeCredentialsEnv
	target.TempEmailBaseURL = cfg.TempEmailBaseURL
	if envName := strings.TrimSpace(cfg.TempEmailAdminAuthEnv); envName != "" {
		target.TempEmailAdminAuth = strings.TrimSpace(os.Getenv(envName))
	}
}

func mergeOpenAI401GuardSettings(target, incoming *OpenAI401GuardSettings) {
	if target == nil || incoming == nil {
		return
	}
	target.Enabled = incoming.Enabled
	target.CheckIntervalSeconds = incoming.CheckIntervalSeconds
	target.TimeoutSeconds = incoming.TimeoutSeconds
	target.MaxAccountsPerCycle = incoming.MaxAccountsPerCycle
	target.DeleteOnFailure = incoming.DeleteOnFailure
	target.SessionProviderCommand = append([]string(nil), incoming.SessionProviderCommand...)
	target.IncludeCredentialsEnv = incoming.IncludeCredentialsEnv
	target.TempEmailBaseURL = incoming.TempEmailBaseURL
	target.TempEmailAdminAuth = incoming.TempEmailAdminAuth
}

func normalizeOpenAI401GuardSettings(settings *OpenAI401GuardSettings) {
	if settings == nil {
		return
	}
	if settings.CheckIntervalSeconds < 10 {
		settings.CheckIntervalSeconds = 60
	}
	if settings.CheckIntervalSeconds > 3600 {
		settings.CheckIntervalSeconds = 3600
	}
	if settings.TimeoutSeconds < 10 {
		settings.TimeoutSeconds = 300
	}
	if settings.TimeoutSeconds > 1800 {
		settings.TimeoutSeconds = 1800
	}
	if settings.MaxAccountsPerCycle < 1 {
		settings.MaxAccountsPerCycle = 5
	}
	if settings.MaxAccountsPerCycle > 100 {
		settings.MaxAccountsPerCycle = 100
	}
	settings.TempEmailBaseURL = strings.TrimRight(strings.TrimSpace(settings.TempEmailBaseURL), "/")
	settings.TempEmailAdminAuth = strings.TrimSpace(settings.TempEmailAdminAuth)
	settings.SessionProviderCommand = normalizeOpenAI401GuardCommand(settings.SessionProviderCommand)
}

func normalizeOpenAI401GuardCommand(command []string) []string {
	out := make([]string, 0, len(command))
	for _, part := range command {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func openAI401GuardToReloginConfig(settings *OpenAI401GuardSettings) config.TokenRelogin401Config {
	cfg := config.TokenRelogin401Config{}
	if settings == nil {
		return cfg
	}
	cfg.Enabled = settings.Enabled
	cfg.CheckIntervalSeconds = settings.CheckIntervalSeconds
	cfg.Command = append([]string(nil), settings.SessionProviderCommand...)
	cfg.TimeoutSeconds = settings.TimeoutSeconds
	cfg.MaxAccountsPerCycle = settings.MaxAccountsPerCycle
	cfg.IncludeCredentialsEnv = settings.IncludeCredentialsEnv
	cfg.TempEmailBaseURL = settings.TempEmailBaseURL
	cfg.TempEmailAdminAuth = settings.TempEmailAdminAuth
	cfg.DeleteOnFailure = settings.DeleteOnFailure
	return cfg
}
