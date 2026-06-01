package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KiroUsageLimitsResponse Kiro getUsageLimits API 返回结构
type KiroUsageLimitsResponse struct {
	UsageBreakdownList []KiroUsageBreakdown  `json:"usageBreakdownList"`
	SubscriptionInfo   *KiroSubscriptionInfo `json:"subscriptionInfo"`
	UserInfo           *KiroUserInfo         `json:"userInfo"`
}

type KiroUsageBreakdown struct {
	ResourceType              string         `json:"resourceType"`
	DisplayName               string         `json:"displayName"`
	CurrentUsage              *float64       `json:"currentUsage"`
	CurrentUsageWithPrecision *float64       `json:"currentUsageWithPrecision"`
	UsageLimit                *float64       `json:"usageLimit"`
	UsageLimitWithPrecision   *float64       `json:"usageLimitWithPrecision"`
	NextDateReset             *int64         `json:"nextDateReset"`
	Bonuses                   []KiroBonus    `json:"bonuses"`
	FreeTrialInfo             *KiroFreeTrial `json:"freeTrialInfo"`
}

func (b *KiroUsageBreakdown) GetCurrentUsage() float64 {
	if b.CurrentUsageWithPrecision != nil {
		return *b.CurrentUsageWithPrecision
	}
	if b.CurrentUsage != nil {
		return *b.CurrentUsage
	}
	return 0
}

func (b *KiroUsageBreakdown) GetUsageLimit() float64 {
	if b.UsageLimitWithPrecision != nil {
		return *b.UsageLimitWithPrecision
	}
	if b.UsageLimit != nil {
		return *b.UsageLimit
	}
	return 0
}

type KiroBonus struct {
	Amount    *float64 `json:"amount"`
	ExpiresAt *int64   `json:"expiresAt"`
}

type KiroFreeTrial struct {
	Remaining *float64 `json:"remaining"`
	Total     *float64 `json:"total"`
}

type KiroSubscriptionInfo struct {
	SubscriptionTitle string `json:"subscriptionTitle"`
	Title             string `json:"title"`
}

func (s *KiroSubscriptionInfo) GetTitle() string {
	if s == nil {
		return ""
	}
	if s.SubscriptionTitle != "" {
		return s.SubscriptionTitle
	}
	return s.Title
}

type KiroUserInfo struct {
	Email string `json:"email"`
}

// KiroUsageItem 单个资源类型的额度信息
type KiroUsageItem struct {
	ResourceType     string     `json:"resource_type"`
	DisplayName      string     `json:"display_name"`
	CurrentUsage     float64    `json:"current_usage"`
	UsageLimit       float64    `json:"usage_limit"`
	Utilization      float64    `json:"utilization"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	RemainingSeconds int        `json:"remaining_seconds,omitempty"`
}

// KiroQuotaFetcher 从 Kiro API 获取账号额度信息
type KiroQuotaFetcher struct {
	tokenProvider *KiroTokenProvider
	httpUpstream  HTTPUpstream
}

// NewKiroQuotaFetcher 创建 KiroQuotaFetcher
func NewKiroQuotaFetcher(tokenProvider *KiroTokenProvider, httpUpstream HTTPUpstream) *KiroQuotaFetcher {
	return &KiroQuotaFetcher{
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
	}
}

// CanFetch 检查是否可以获取此账户的额度
func (f *KiroQuotaFetcher) CanFetch(account *Account) bool {
	if account == nil {
		return false
	}
	return account.Platform == PlatformKiro && account.Type == AccountTypeKiro
}

// FetchQuota 获取 Kiro 账户额度信息
func (f *KiroQuotaFetcher) FetchQuota(ctx context.Context, account *Account) (*QuotaResult, error) {
	accessToken, err := f.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get kiro access token: %w", err)
	}

	resp, err := f.callGetUsageLimits(ctx, account, accessToken)
	if err != nil {
		return nil, err
	}

	usageInfo := f.buildUsageInfo(resp)

	// 转换为 map[string]any 以匹配 QuotaResult.Raw 类型
	rawBytes, _ := json.Marshal(resp)
	var raw map[string]any
	_ = json.Unmarshal(rawBytes, &raw)

	return &QuotaResult{
		UsageInfo: usageInfo,
		Raw:       raw,
	}, nil
}

func (f *KiroQuotaFetcher) callGetUsageLimits(ctx context.Context, account *Account, accessToken string) (*KiroUsageLimitsResponse, error) {
	region := kiroAPIRegion(account)
	baseURL := fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/getUsageLimits", region)

	params := url.Values{}
	params.Set("isEmailRequired", "true")
	params.Set("origin", "AI_EDITOR")
	params.Set("resourceType", "AGENTIC_REQUEST")
	if profileARN := strings.TrimSpace(account.GetCredential("profile_arn")); profileARN != "" {
		params.Set("profileArn", profileARN)
	}

	fullURL := baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create kiro usage request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	decorateKiroRuntimeHeaders(req, account)

	resp, err := f.do(ctx, account, req)
	if err != nil {
		return nil, fmt.Errorf("kiro usage request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("kiro usage: 401 unauthorized (token expired or invalid)")
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("kiro usage: 403 forbidden")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro usage: status=%d body=%s", resp.StatusCode, truncateForError(body))
	}

	var result KiroUsageLimitsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode kiro usage response: %w", err)
	}
	return &result, nil
}

func (f *KiroQuotaFetcher) do(ctx context.Context, account *Account, req *http.Request) (*http.Response, error) {
	proxyURL := ""
	if account != nil && account.Proxy != nil && account.Proxy.IsActive() {
		proxyURL = account.Proxy.URL()
	}
	if f.httpUpstream != nil {
		return f.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	return http.DefaultClient.Do(req)
}

func (f *KiroQuotaFetcher) buildUsageInfo(resp *KiroUsageLimitsResponse) *UsageInfo {
	now := time.Now()
	info := &UsageInfo{
		UpdatedAt: &now,
	}

	// 订阅信息
	if resp.SubscriptionInfo != nil {
		title := resp.SubscriptionInfo.GetTitle()
		info.SubscriptionTierRaw = title
		info.SubscriptionTier = normalizeKiroTier(title)
	}

	// 用户邮箱
	if resp.UserInfo != nil && resp.UserInfo.Email != "" {
		info.KiroEmail = resp.UserInfo.Email
	}

	// 额度明细
	info.KiroUsageBreakdown = make([]KiroUsageItem, 0, len(resp.UsageBreakdownList))
	for _, item := range resp.UsageBreakdownList {
		used := item.GetCurrentUsage()
		limit := item.GetUsageLimit()

		usageItem := KiroUsageItem{
			ResourceType: item.ResourceType,
			DisplayName:  item.DisplayName,
			CurrentUsage: used,
			UsageLimit:   limit,
		}

		if limit > 0 {
			usageItem.Utilization = (used / limit) * 100
		}

		if item.NextDateReset != nil {
			resetTime := time.UnixMilli(*item.NextDateReset)
			usageItem.ResetsAt = &resetTime
			usageItem.RemainingSeconds = int(time.Until(resetTime).Seconds())
		}

		info.KiroUsageBreakdown = append(info.KiroUsageBreakdown, usageItem)
	}

	// 用主要资源类型填充 FiveHour 兼容字段
	for _, item := range info.KiroUsageBreakdown {
		if item.ResourceType == "AGENTIC_REQUEST" && item.UsageLimit > 0 {
			progress := &UsageProgress{
				Utilization: item.Utilization,
				ResetsAt:    item.ResetsAt,
			}
			if item.ResetsAt != nil {
				progress.RemainingSeconds = int(time.Until(*item.ResetsAt).Seconds())
			}
			info.FiveHour = progress
			break
		}
	}

	return info
}

func normalizeKiroTier(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "pro"):
		return "PRO"
	case strings.Contains(lower, "free"):
		return "FREE"
	default:
		return "UNKNOWN"
	}
}
