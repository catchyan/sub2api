package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var kiroFallbackModels = []string{
	"auto-kiro",
	"claude-sonnet-4",
	"claude-haiku-4.5",
	"claude-sonnet-4.5",
	"claude-opus-4.5",
	"claude-sonnet-4.6",
	"claude-opus-4.6",
	"claude-opus-4.7",
	"claude-3.7-sonnet",
}

const kiroModelCacheTTL = 10 * time.Minute
const kiroMaxToolNameLength = 64
const kiroThinkingStartTag = "<thinking>"
const kiroThinkingEndTag = "</thinking>"
const kiroThinkingModeTag = "<thinking_mode>"
const kiroThinkingMaxLengthTag = "<max_thinking_length>"
const kiroThinkingEffortTag = "<thinking_effort>"
const kiroThinkingMinBudgetTokens = 1024
const kiroThinkingMaxBudgetTokens = 24576
const kiroThinkingDefaultBudgetTokens = 20000

type KiroModel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func KiroDefaultModels() []KiroModel {
	return []KiroModel{
		{ID: "auto-kiro", Type: "model", DisplayName: "Kiro Auto"},
		{ID: "claude-sonnet-4", Type: "model", DisplayName: "Claude Sonnet 4"},
		{ID: "claude-haiku-4.5", Type: "model", DisplayName: "Claude Haiku 4.5"},
		{ID: "claude-sonnet-4.5", Type: "model", DisplayName: "Claude Sonnet 4.5"},
		{ID: "claude-opus-4.5", Type: "model", DisplayName: "Claude Opus 4.5"},
		{ID: "claude-sonnet-4.6", Type: "model", DisplayName: "Claude Sonnet 4.6"},
		{ID: "claude-opus-4.6", Type: "model", DisplayName: "Claude Opus 4.6"},
		{ID: "claude-opus-4.7", Type: "model", DisplayName: "Claude Opus 4.7"},
		{ID: "claude-3.7-sonnet", Type: "model", DisplayName: "Claude Sonnet 3.7"},
	}
}

type kiroModelCacheEntry struct {
	models    []string
	expiresAt time.Time
}

type kiroToolNameMaps struct {
	aliasToOriginal map[string]string
	originalToAlias map[string]string
}

// KiroForwardResult holds the result of a Kiro forward operation for billing purposes.
type KiroForwardResult struct {
	Model        string
	Account      *Account
	InputTokens  int
	OutputTokens int
	Stream       bool
	Duration     time.Duration
	RequestID    string
}

type KiroGatewayService struct {
	accountRepo       AccountRepository
	schedulerSnapshot *SchedulerSnapshotService
	tokenProvider     *KiroTokenProvider
	httpUpstream      HTTPUpstream
	modelCacheMu      sync.Mutex
	modelCache        map[int64]kiroModelCacheEntry
}

func NewKiroGatewayService(accountRepo AccountRepository, schedulerSnapshot *SchedulerSnapshotService, tokenProvider *KiroTokenProvider, httpUpstream HTTPUpstream) *KiroGatewayService {
	return &KiroGatewayService{
		accountRepo:       accountRepo,
		schedulerSnapshot: schedulerSnapshot,
		tokenProvider:     tokenProvider,
		httpUpstream:      httpUpstream,
		modelCache:        map[int64]kiroModelCacheEntry{},
	}
}

func (s *KiroGatewayService) ListModels(ctx context.Context, groupID *int64) ([]string, error) {
	accounts, err := s.listAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, fallback := range kiroFallbackModels {
		seen[fallback] = struct{}{}
	}
	for i := range accounts {
		models, err := s.fetchAccountModels(ctx, &accounts[i])
		if err != nil {
			continue
		}
		for _, model := range models {
			if strings.TrimSpace(model) != "" {
				seen[strings.TrimSpace(model)] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}
	sort.Strings(out)
	return out, nil
}

func (s *KiroGatewayService) ForwardOpenAIChat(ctx context.Context, c *gin.Context, body []byte) (*KiroForwardResult, error) {
	startTime := time.Now()
	var req openAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	toolNameMaps := buildKiroToolNameMaps(req.Tools)
	resp, account, err := s.callGenerateAcrossAccounts(ctx, groupIDFromContext(c), func(account *Account) (map[string]any, error) {
		return buildKiroPayloadFromOpenAI(req, account)
	})
	if err != nil {
		return nil, writeOpenAIError(c, http.StatusBadGateway, "api_error", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, writeOpenAIError(c, mapKiroStatus(resp.StatusCode), "api_error", upstreamErrorMessage(respBody))
	}

	inputTokens := estimateKiroInputTokens(body)
	var outputTokens int

	if req.Stream {
		outputTokens = streamKiroToOpenAIWithCount(c, resp.Body, resp.Header.Get("Content-Type"), req.Model, toolNameMaps)
	} else {
		content, toolCalls := collectKiroResult(resp.Body, resp.Header.Get("Content-Type"), toolNameMaps)
		outputTokens = estimateKiroOutputTokens(content, toolCalls)
		message := gin.H{
			"role":    "assistant",
			"content": content,
		}
		finishReason := "stop"
		if len(toolCalls) > 0 {
			message["content"] = nil
			if strings.TrimSpace(content) != "" {
				message["content"] = content
			}
			message["tool_calls"] = kiroToolCallsToOpenAI(toolCalls, toolNameMaps)
			finishReason = "tool_calls"
		}
		c.JSON(http.StatusOK, gin.H{
			"id":      "chatcmpl-" + uuid.NewString(),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []gin.H{{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			}},
			"usage": gin.H{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
		})
	}

	return &KiroForwardResult{
		Model:        req.Model,
		Account:      account,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Stream:       req.Stream,
		Duration:     time.Since(startTime),
		RequestID:    resp.Header.Get("X-Amzn-Requestid"),
	}, nil
}

func (s *KiroGatewayService) ForwardAnthropicMessages(ctx context.Context, c *gin.Context, body []byte) (*KiroForwardResult, error) {
	startTime := time.Now()
	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	toolNameMaps := buildKiroToolNameMaps(req.Tools)
	resp, account, err := s.callGenerateAcrossAccounts(ctx, groupIDFromContext(c), func(account *Account) (map[string]any, error) {
		return buildKiroPayloadFromAnthropic(req, account)
	})
	if err != nil {
		return nil, writeKiroAnthropicError(c, http.StatusBadGateway, "api_error", err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, writeKiroAnthropicError(c, mapKiroStatus(resp.StatusCode), "api_error", upstreamErrorMessage(respBody))
	}

	inputTokens := estimateKiroInputTokens(body)
	var outputTokens int

	if req.Stream {
		outputTokens = streamKiroToAnthropicWithCount(c, resp.Body, resp.Header.Get("Content-Type"), req.Model, toolNameMaps, req.Thinking)
	} else {
		content, toolCalls := collectKiroResult(resp.Body, resp.Header.Get("Content-Type"), toolNameMaps)
		outputTokens = estimateKiroOutputTokens(content, toolCalls)
		contentBlocks, stopReason := kiroAnthropicContentBlocks(content, toolCalls, kiroThinkingRequested(req.Thinking))
		c.JSON(http.StatusOK, gin.H{
			"id":            "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
			"type":          "message",
			"role":          "assistant",
			"model":         req.Model,
			"content":       contentBlocks,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         gin.H{"input_tokens": inputTokens, "output_tokens": outputTokens},
		})
	}

	return &KiroForwardResult{
		Model:        req.Model,
		Account:      account,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Stream:       req.Stream,
		Duration:     time.Since(startTime),
		RequestID:    resp.Header.Get("X-Amzn-Requestid"),
	}, nil
}

func (s *KiroGatewayService) CountTokens(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "Failed to read request body"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"input_tokens": estimateKiroInputTokens(body)})
}

func (s *KiroGatewayService) fetchAccountModels(ctx context.Context, account *Account) ([]string, error) {
	if cached, ok := s.getCachedModels(account.ID); ok {
		return cached, nil
	}
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	apiRegion := kiroAPIRegion(account)
	values := url.Values{}
	values.Set("origin", "AI_EDITOR")
	if account.GetCredential("auth_type") == KiroAuthDesktop {
		if profileARN := account.GetCredential("profile_arn"); profileARN != "" {
			values.Set("profileArn", profileARN)
		}
	}
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/ListAvailableModels?%s", apiRegion, values.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	decorateKiroAPIHeaders(req, account)
	resp, err := s.do(ctx, account, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro list models failed: status=%d", resp.StatusCode)
	}
	var data struct {
		Models []struct {
			ModelID string `json:"modelId"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(data.Models))
	for _, model := range data.Models {
		models = append(models, model.ModelID)
	}
	s.setCachedModels(account.ID, models)
	return models, nil
}

func (s *KiroGatewayService) getCachedModels(accountID int64) ([]string, bool) {
	s.modelCacheMu.Lock()
	defer s.modelCacheMu.Unlock()
	entry, ok := s.modelCache[accountID]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return append([]string(nil), entry.models...), true
}

func (s *KiroGatewayService) setCachedModels(accountID int64, models []string) {
	s.modelCacheMu.Lock()
	defer s.modelCacheMu.Unlock()
	s.modelCache[accountID] = kiroModelCacheEntry{
		models:    append([]string(nil), models...),
		expiresAt: time.Now().Add(kiroModelCacheTTL),
	}
}

func (s *KiroGatewayService) callGenerate(ctx context.Context, account *Account, payload map[string]any) (*http.Response, error) {
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", kiroAPIRegion(account))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	decorateKiroAPIHeaders(req, account)
	return s.do(ctx, account, req)
}

type kiroPayloadBuilder func(account *Account) (map[string]any, error)

func (s *KiroGatewayService) callGenerateAcrossAccounts(ctx context.Context, groupID *int64, buildPayload kiroPayloadBuilder) (*http.Response, *Account, error) {
	accounts, err := s.listAccounts(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	if len(accounts) == 0 {
		return nil, nil, errors.New("no schedulable kiro accounts")
	}

	var lastErr error
	for i := range accounts {
		account := &accounts[i]
		payload, err := buildPayload(account)
		if err != nil {
			return nil, nil, err
		}
		resp, err := s.callGenerate(ctx, account, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusForbidden {
			_ = s.tokenProvider.Refresh(ctx, account)
			_ = resp.Body.Close()
			resp, err = s.callGenerate(ctx, account, payload)
			if err != nil {
				lastErr = err
				continue
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, account, nil
		}
		if isKiroRecoverableStatus(resp.StatusCode) && i < len(accounts)-1 {
			_ = resp.Body.Close()
			continue
		}
		return resp, account, nil
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, errors.New("no schedulable kiro accounts")
}

func (s *KiroGatewayService) listAccounts(ctx context.Context, groupID *int64) ([]Account, error) {
	if forcePlatform, ok := ctx.Value(ctxkey.ForcePlatform).(string); ok && forcePlatform != "" && forcePlatform != PlatformKiro {
		return nil, fmt.Errorf("forced platform %s is not kiro", forcePlatform)
	}
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformKiro, true)
		if err != nil {
			return nil, err
		}
		return s.hydrateAccounts(ctx, accounts)
	}
	if groupID != nil {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformKiro)
	}
	return s.accountRepo.ListSchedulableByPlatform(ctx, PlatformKiro)
}

func (s *KiroGatewayService) hydrateAccounts(ctx context.Context, accounts []Account) ([]Account, error) {
	if s.schedulerSnapshot == nil || len(accounts) == 0 {
		return accounts, nil
	}
	hydrated := make([]Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		full, err := s.schedulerSnapshot.GetAccount(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		if full == nil {
			return nil, fmt.Errorf("selected kiro account %d not found during hydration", account.ID)
		}
		hydrated = append(hydrated, *full)
	}
	return hydrated, nil
}

func (s *KiroGatewayService) do(ctx context.Context, account *Account, req *http.Request) (*http.Response, error) {
	proxyURL := ""
	if account != nil && account.Proxy != nil && account.Proxy.IsActive() {
		proxyURL = account.Proxy.URL()
	}
	if s.httpUpstream != nil {
		return s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	return http.DefaultClient.Do(req)
}

type openAIChatRequest struct {
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []map[string]any `json:"tools"`
}

type anthropicMessagesRequest struct {
	Model    string                  `json:"model"`
	Messages []map[string]any        `json:"messages"`
	System   any                     `json:"system"`
	Stream   bool                    `json:"stream"`
	Tools    []map[string]any        `json:"tools"`
	Thinking *anthropicThinkingInput `json:"thinking,omitempty"`
}

func buildKiroPayloadFromOpenAI(req openAIChatRequest, account *Account) (map[string]any, error) {
	systemPrompt, messages := splitOpenAIMessages(req.Messages)
	return buildKiroPayload(req.Model, systemPrompt, messages, req.Tools, account), nil
}

func buildKiroPayloadFromAnthropic(req anthropicMessagesRequest, account *Account) (map[string]any, error) {
	systemPrompt := extractText(req.System)
	return buildKiroPayloadWithThinking(req.Model, systemPrompt, req.Messages, req.Tools, req.Thinking, account), nil
}

func buildKiroPayload(model, systemPrompt string, messages []map[string]any, tools []map[string]any, account *Account) map[string]any {
	return buildKiroPayloadWithThinking(model, systemPrompt, messages, tools, nil, account)
}

type anthropicThinkingInput struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Effort       string `json:"effort,omitempty"`
}

func buildKiroPayloadWithThinking(model, systemPrompt string, messages []map[string]any, tools []map[string]any, thinking *anthropicThinkingInput, account *Account) map[string]any {
	modelID := kiroResolveModel(model)
	systemPrompt = applyKiroThinkingPrefix(systemPrompt, thinking)
	toolNameMaps := buildKiroToolNameMaps(tools)
	normalized := normalizeKiroMessages(messages, toolNameMaps)
	if len(normalized) == 0 {
		normalized = []kiroChatMessage{{role: "user", content: "Continue"}}
	}
	normalized = enforceKiroAlternation(normalized)
	if normalized[len(normalized)-1].role != "user" {
		normalized = append(normalized, kiroChatMessage{role: "user", content: "Continue"})
	}

	history := make([]any, 0, len(normalized)-1)
	for _, msg := range normalized[:len(normalized)-1] {
		if msg.role == "assistant" {
			assistant := map[string]any{"content": msg.nonEmptyContent("(empty)")}
			if len(msg.toolUses) > 0 {
				assistant["toolUses"] = msg.toolUses
			}
			history = append(history, map[string]any{"assistantResponseMessage": assistant})
			continue
		}
		history = append(history, map[string]any{"userInputMessage": buildKiroUserMessage(msg, modelID)})
	}
	current := normalized[len(normalized)-1]
	currentContent := current.nonEmptyContent("Continue")
	if strings.TrimSpace(systemPrompt) != "" {
		currentContent = strings.TrimSpace(systemPrompt) + "\n\n" + currentContent
	}
	current.content = currentContent
	userInput := buildKiroUserMessage(current, modelID)
	context := kiroUserInputMessageContext(current, tools, toolNameMaps)
	if len(context) > 0 {
		userInput["userInputMessageContext"] = context
	}
	state := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  uuid.NewString(),
		"currentMessage": map[string]any{
			"userInputMessage": userInput,
		},
	}
	if len(history) > 0 {
		state["history"] = history
	}
	payload := map[string]any{"conversationState": state}
	if account != nil && account.GetCredential("auth_type") == KiroAuthDesktop {
		if profileARN := account.GetCredential("profile_arn"); profileARN != "" {
			payload["profileArn"] = profileARN
		}
	}
	return payload
}

type kiroChatMessage struct {
	role        string
	content     string
	images      []any
	toolUses    []map[string]any
	toolResults []map[string]any
}

func (m kiroChatMessage) nonEmptyContent(fallback string) string {
	if strings.TrimSpace(m.content) != "" {
		return m.content
	}
	return fallback
}

func normalizeKiroMessages(messages []map[string]any, toolNameMaps *kiroToolNameMaps) []kiroChatMessage {
	out := make([]kiroChatMessage, 0, len(messages))
	for _, msg := range messages {
		role := normalizeKiroRole(kiroString(msg["role"]))
		content, images, thinking := extractKiroContentImagesAndThinking(msg["content"])
		toolUses, toolResults := extractKiroToolBlocks(msg, toolNameMaps)
		if role == "tool" || role == "function" {
			role = "user"
			name := kiroFirstNonEmpty(kiroString(msg["name"]), kiroString(msg["tool_call_id"]))
			if strings.TrimSpace(content) == "" {
				content = "(empty)"
			}
			toolResults = append(toolResults, buildKiroToolResult(name, content))
		}
		if role == "assistant" {
			if strings.TrimSpace(thinking) != "" {
				content = wrapKiroThinkingContent(thinking, content)
			}
			toolUses = append(toolUses, extractOpenAIToolUses(msg["tool_calls"], toolNameMaps)...)
		}
		if strings.TrimSpace(content) == "" && len(toolResults) > 0 {
			content = "Tool results provided."
		}
		if strings.TrimSpace(content) == "" && len(images) == 0 {
			content = "(empty)"
		}
		out = append(out, kiroChatMessage{role: role, content: content, images: images, toolUses: toolUses, toolResults: toolResults})
	}
	return out
}

func normalizeKiroRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return "assistant"
	case "tool", "function":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return "user"
	}
}

func applyKiroThinkingPrefix(systemPrompt string, thinking *anthropicThinkingInput) string {
	prefix := buildKiroThinkingPrefix(thinking)
	if prefix == "" {
		return systemPrompt
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return prefix
	}
	if hasKiroThinkingPrefix(systemPrompt) {
		return systemPrompt
	}
	return prefix + "\n" + systemPrompt
}

func buildKiroThinkingPrefix(thinking *anthropicThinkingInput) string {
	if !kiroThinkingRequested(thinking) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "enabled":
		return fmt.Sprintf("%senabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", kiroThinkingModeTag, normalizeKiroThinkingBudget(thinking.BudgetTokens))
	case "adaptive":
		effort := strings.ToLower(strings.TrimSpace(thinking.Effort))
		if effort != "low" && effort != "medium" && effort != "high" {
			effort = "high"
		}
		return fmt.Sprintf("%sadaptive</thinking_mode><thinking_effort>%s</thinking_effort>", kiroThinkingModeTag, effort)
	default:
		return ""
	}
}

func kiroThinkingRequested(thinking *anthropicThinkingInput) bool {
	if thinking == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(thinking.Type)) {
	case "enabled", "adaptive":
		return true
	default:
		return false
	}
}

func hasKiroThinkingPrefix(systemPrompt string) bool {
	return strings.Contains(systemPrompt, kiroThinkingModeTag) ||
		strings.Contains(systemPrompt, kiroThinkingMaxLengthTag) ||
		strings.Contains(systemPrompt, kiroThinkingEffortTag)
}

func normalizeKiroThinkingBudget(value int) int {
	if value <= 0 {
		value = kiroThinkingDefaultBudgetTokens
	}
	if value < kiroThinkingMinBudgetTokens {
		value = kiroThinkingMinBudgetTokens
	}
	if value > kiroThinkingMaxBudgetTokens {
		value = kiroThinkingMaxBudgetTokens
	}
	return value
}

func wrapKiroThinkingContent(thinking, content string) string {
	thinking = strings.TrimSpace(thinking)
	if thinking == "" {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return kiroThinkingStartTag + thinking + kiroThinkingEndTag
	}
	return kiroThinkingStartTag + thinking + kiroThinkingEndTag + "\n\n" + content
}

func enforceKiroAlternation(messages []kiroChatMessage) []kiroChatMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]kiroChatMessage, 0, len(messages)+2)
	if messages[0].role == "assistant" {
		out = append(out, kiroChatMessage{role: "user", content: "(empty)"})
	}
	for _, msg := range messages {
		if len(out) > 0 && out[len(out)-1].role == msg.role {
			fillerRole := "assistant"
			if msg.role == "assistant" {
				fillerRole = "user"
			}
			out = append(out, kiroChatMessage{role: fillerRole, content: "(empty)"})
		}
		out = append(out, msg)
	}
	return out
}

func buildKiroUserMessage(msg kiroChatMessage, modelID string) map[string]any {
	out := map[string]any{
		"content": msg.nonEmptyContent("Continue"),
		"modelId": modelID,
		"origin":  "AI_EDITOR",
	}
	if len(msg.images) > 0 {
		out["images"] = msg.images
	}
	context := kiroUserInputMessageContext(msg, nil, nil)
	if len(context) > 0 {
		out["userInputMessageContext"] = context
	}
	return out
}

func kiroUserInputMessageContext(msg kiroChatMessage, tools []map[string]any, toolNameMaps *kiroToolNameMaps) map[string]any {
	context := map[string]any{}
	if len(msg.toolResults) > 0 {
		context["toolResults"] = dedupeKiroToolResults(msg.toolResults)
	}
	if tools != nil {
		context["tools"] = normalizeKiroTools(tools, toolNameMaps)
	}
	return context
}

func splitOpenAIMessages(messages []map[string]any) (string, []map[string]any) {
	var system []string
	var out []map[string]any
	for _, msg := range messages {
		if strings.EqualFold(kiroString(msg["role"]), "system") || strings.EqualFold(kiroString(msg["role"]), "developer") {
			if txt := extractText(msg["content"]); txt != "" {
				system = append(system, txt)
			}
			continue
		}
		out = append(out, msg)
	}
	return strings.Join(system, "\n\n"), out
}

func extractText(v any) string {
	text, _ := extractKiroContentAndImages(v)
	return text
}

func extractKiroContentAndImages(v any) (string, []any) {
	text, images, _ := extractKiroContentImagesAndThinking(v)
	return text, images
}

func extractKiroContentImagesAndThinking(v any) (string, []any, string) {
	switch x := v.(type) {
	case string:
		return x, nil, ""
	case []any:
		var parts []string
		var images []any
		var thinking []string
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				text, itemImages, itemThinking := extractKiroContentBlockDetailed(m)
				if strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
				images = append(images, itemImages...)
				if strings.TrimSpace(itemThinking) != "" {
					thinking = append(thinking, itemThinking)
				}
			}
		}
		return strings.Join(parts, "\n"), images, strings.Join(thinking, "")
	case []map[string]any:
		var parts []string
		var images []any
		var thinking []string
		for _, item := range x {
			text, itemImages, itemThinking := extractKiroContentBlockDetailed(item)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
			images = append(images, itemImages...)
			if strings.TrimSpace(itemThinking) != "" {
				thinking = append(thinking, itemThinking)
			}
		}
		return strings.Join(parts, "\n"), images, strings.Join(thinking, "")
	default:
		return "", nil, ""
	}
}

func extractKiroContentBlockDetailed(block map[string]any) (string, []any, string) {
	blockType := strings.ToLower(kiroString(block["type"]))
	switch blockType {
	case "text", "":
		return kiroString(block["text"]), nil, ""
	case "image", "image_url":
		if image := kiroImageFromBlock(block); image != nil {
			return "", []any{image}, ""
		}
	case "tool_result":
		return "", nil, ""
	case "tool_use":
		return "", nil, ""
	case "thinking":
		return "", nil, kiroFirstNonEmpty(kiroString(block["thinking"]), kiroString(block["text"]))
	case "redacted_thinking":
		return "", nil, ""
	}
	if text := kiroString(block["text"]); text != "" {
		return text, nil, ""
	}
	return "", nil, ""
}

func extractKiroToolBlocks(msg map[string]any, toolNameMaps *kiroToolNameMaps) ([]map[string]any, []map[string]any) {
	var toolUses []map[string]any
	var toolResults []map[string]any
	switch content := msg["content"].(type) {
	case []any:
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(kiroString(block["type"])) {
			case "tool_use":
				if toolUse := buildKiroToolUseFromBlock(block, toolNameMaps); toolUse != nil {
					toolUses = append(toolUses, toolUse)
				}
			case "tool_result":
				toolUseID := kiroString(block["tool_use_id"])
				toolResults = append(toolResults, buildKiroToolResult(toolUseID, extractText(block["content"])))
			}
		}
	case []map[string]any:
		for _, block := range content {
			switch strings.ToLower(kiroString(block["type"])) {
			case "tool_use":
				if toolUse := buildKiroToolUseFromBlock(block, toolNameMaps); toolUse != nil {
					toolUses = append(toolUses, toolUse)
				}
			case "tool_result":
				toolUseID := kiroString(block["tool_use_id"])
				toolResults = append(toolResults, buildKiroToolResult(toolUseID, extractText(block["content"])))
			}
		}
	}
	return toolUses, toolResults
}

func buildKiroToolUseFromBlock(block map[string]any, toolNameMaps *kiroToolNameMaps) map[string]any {
	name := strings.TrimSpace(kiroString(block["name"]))
	toolUseID := strings.TrimSpace(kiroString(block["id"]))
	if name == "" || toolUseID == "" {
		return nil
	}
	return map[string]any{
		"input":     sanitizeKiroToolInput(block["input"]),
		"name":      kiroToolNameToKiro(name, toolNameMaps),
		"toolUseId": toolUseID,
	}
}

func extractOpenAIToolUses(v any, toolNameMaps *kiroToolNameMaps) []map[string]any {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	var out []map[string]any
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(kiroString(fn["name"]))
		if name == "" {
			continue
		}
		toolUseID := strings.TrimSpace(kiroString(m["id"]))
		if toolUseID == "" {
			toolUseID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		}
		out = append(out, map[string]any{
			"input":     sanitizeKiroToolInput(parseKiroToolArguments(fn["arguments"])),
			"name":      kiroToolNameToKiro(name, toolNameMaps),
			"toolUseId": toolUseID,
		})
	}
	return out
}

func buildKiroToolResult(toolUseID, content string) map[string]any {
	toolUseID = strings.TrimSpace(toolUseID)
	if toolUseID == "" {
		toolUseID = "tool"
	}
	if strings.TrimSpace(content) == "" {
		content = "(empty)"
	}
	return map[string]any{
		"content":   []map[string]string{{"text": content}},
		"status":    "success",
		"toolUseId": toolUseID,
	}
}

func dedupeKiroToolResults(results []map[string]any) []map[string]any {
	seen := map[string]struct{}{}
	out := make([]map[string]any, 0, len(results))
	for _, result := range results {
		id := kiroString(result["toolUseId"])
		if id == "" {
			out = append(out, result)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, result)
	}
	return out
}

func buildKiroToolNameMaps(tools []map[string]any) *kiroToolNameMaps {
	maps := &kiroToolNameMaps{
		aliasToOriginal: map[string]string{},
		originalToAlias: map[string]string{},
	}
	if len(tools) == 0 {
		return maps
	}
	for _, tool := range tools {
		name, _, _ := extractKiroToolDefinition(tool)
		if name == "" {
			continue
		}
		alias := shortenKiroToolName(name)
		maps.originalToAlias[name] = alias
		if alias != name {
			maps.aliasToOriginal[alias] = name
		}
	}
	return maps
}

func shortenKiroToolName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) <= kiroMaxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:12]
	prefixLength := kiroMaxToolNameLength - len(hash) - 1
	if prefixLength < 0 {
		prefixLength = 0
	}
	return name[:prefixLength] + "_" + hash
}

func kiroToolNameToKiro(name string, maps *kiroToolNameMaps) string {
	name = strings.TrimSpace(name)
	if maps == nil {
		return shortenKiroToolName(name)
	}
	if alias, ok := maps.originalToAlias[name]; ok {
		return alias
	}
	return shortenKiroToolName(name)
}

func restoreKiroToolName(name string, maps *kiroToolNameMaps) string {
	name = strings.TrimSpace(name)
	if maps == nil {
		return name
	}
	if original, ok := maps.aliasToOriginal[name]; ok {
		return original
	}
	return name
}

func normalizeKiroTools(tools []map[string]any, toolNameMaps *kiroToolNameMaps) []map[string]any {
	const maxDescriptionLength = 9216
	if len(tools) == 0 {
		return []map[string]any{kiroPlaceholderTool()}
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name, description, schema := extractKiroToolDefinition(tool)
		if name == "" {
			continue
		}
		lowerName := strings.ToLower(name)
		if lowerName == "web_search" || lowerName == "websearch" {
			continue
		}
		if strings.TrimSpace(description) == "" {
			continue
		}
		if len(description) > maxDescriptionLength {
			description = description[:maxDescriptionLength] + "..."
		}
		out = append(out, map[string]any{
			"toolSpecification": map[string]any{
				"name":        kiroToolNameToKiro(name, toolNameMaps),
				"description": description,
				"inputSchema": map[string]any{
					"json": normalizeKiroToolSchema(schema),
				},
			},
		})
	}
	if len(out) == 0 {
		return []map[string]any{kiroPlaceholderTool()}
	}
	return out
}

func extractKiroToolDefinition(tool map[string]any) (string, string, any) {
	if fn, ok := tool["function"].(map[string]any); ok {
		return strings.TrimSpace(kiroString(fn["name"])), kiroString(fn["description"]), firstKiroMapValue(fn, "parameters", "input_schema")
	}
	return strings.TrimSpace(kiroString(tool["name"])), kiroString(tool["description"]), firstKiroMapValue(tool, "input_schema", "parameters")
}

func normalizeKiroToolSchema(schema any) any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	if m["type"] == "object" {
		if _, ok := m["properties"]; !ok {
			m["properties"] = map[string]any{}
		}
	}
	return m
}

func kiroPlaceholderTool() map[string]any {
	return map[string]any{
		"toolSpecification": map[string]any{
			"name":        "no_tool_available",
			"description": "This is a placeholder tool when no other tools are available. It does nothing.",
			"inputSchema": map[string]any{
				"json": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
}

func sanitizeKiroToolInput(input any) any {
	switch x := input.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range x {
			if key == "" {
				continue
			}
			out[key] = value
		}
		return out
	default:
		return x
	}
}

func parseKiroToolArguments(v any) any {
	switch x := v.(type) {
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(x), &parsed); err == nil {
			return parsed
		}
		return map[string]any{"raw_arguments": x}
	default:
		return x
	}
}

func kiroImageFromBlock(block map[string]any) any {
	if imageURL, ok := block["image_url"].(map[string]any); ok {
		if image := parseKiroDataURLImage(kiroString(imageURL["url"])); image != nil {
			return image
		}
	}
	if imageURL := kiroString(block["image_url"]); imageURL != "" {
		return parseKiroDataURLImage(imageURL)
	}
	if source, ok := block["source"].(map[string]any); ok {
		data := kiroString(source["data"])
		mediaType := kiroFirstNonEmpty(kiroString(source["media_type"]), kiroString(source["mediaType"]))
		if data != "" {
			return map[string]any{
				"format": kiroImageFormat(mediaType),
				"source": map[string]any{"bytes": data},
			}
		}
	}
	return nil
}

func parseKiroDataURLImage(raw string) any {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:image/") {
		return nil
	}
	parts := strings.SplitN(raw, ",", 2)
	if len(parts) != 2 {
		return nil
	}
	return map[string]any{
		"format": kiroImageFormat(parts[0]),
		"source": map[string]any{"bytes": parts[1]},
	}
}

func kiroImageFormat(mediaType string) string {
	mediaType = strings.ToLower(mediaType)
	switch {
	case strings.Contains(mediaType, "png"):
		return "png"
	case strings.Contains(mediaType, "gif"):
		return "gif"
	case strings.Contains(mediaType, "webp"):
		return "webp"
	default:
		return "jpeg"
	}
}

func estimateKiroOutputTokens(content string, toolCalls []kiroToolCall) int {
	runes := len([]rune(content))
	for _, tc := range toolCalls {
		runes += len([]rune(tc.Name))
		if input, ok := tc.Input.(string); ok {
			runes += len([]rune(input))
		} else {
			b, _ := json.Marshal(tc.Input)
			runes += len([]rune(string(b)))
		}
	}
	if runes == 0 {
		return 1
	}
	estimated := runes / 4
	if runes%4 != 0 {
		estimated++
	}
	if estimated < 1 {
		return 1
	}
	return estimated
}

func estimateKiroInputTokens(body []byte) int {
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return 1
	}
	text := strings.Join(extractKiroPromptText(data), "\n")
	runes := len([]rune(text))
	if runes == 0 {
		return 1
	}
	estimated := runes / 4
	if runes%4 != 0 {
		estimated++
	}
	if estimated < 1 {
		return 1
	}
	return estimated
}

func extractKiroPromptText(v any) []string {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	case map[string]any:
		var out []string
		for key, value := range x {
			switch key {
			case "content", "text", "system", "prompt":
				out = append(out, extractKiroPromptText(value)...)
			case "messages":
				out = append(out, extractKiroPromptText(value)...)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range x {
			out = append(out, extractKiroPromptText(item)...)
		}
		return out
	default:
		return nil
	}
}

type kiroStreamParser struct {
	buffer      string
	lastContent string
}

type kiroResponseEvent struct {
	Type      string
	Content   string
	ToolUseID string
	Name      string
	Input     string
	Stop      bool
}

type kiroToolCall struct {
	ID    string
	Name  string
	Input any
}

type kiroToolAccumulator struct {
	current *kiroToolCallBuffer
	calls   []kiroToolCall
}

type kiroToolCallBuffer struct {
	id    string
	name  string
	input strings.Builder
}

func (p *kiroStreamParser) feedPayloadEvents(payload []byte) []kiroResponseEvent {
	var data any
	if err := json.Unmarshal(payload, &data); err != nil {
		return p.feedEvents(payload)
	}
	return p.extractEvents(data)
}

func (p *kiroStreamParser) feedEvents(chunk []byte) []kiroResponseEvent {
	contents := p.feed(chunk)
	out := make([]kiroResponseEvent, 0, len(contents))
	for _, content := range contents {
		out = append(out, kiroResponseEvent{Type: "content", Content: content})
	}
	return out
}

func (p *kiroStreamParser) extractEvents(v any) []kiroResponseEvent {
	var out []kiroResponseEvent
	for _, event := range extractKiroResponseEvents(v) {
		if event.Type == "content" {
			if delta, ok := p.normalizeContentDelta(event.Content); ok {
				event.Content = delta
				out = append(out, event)
			}
			continue
		}
		out = append(out, event)
	}
	return out
}

func extractKiroResponseEvents(v any) []kiroResponseEvent {
	switch x := v.(type) {
	case map[string]any:
		if x["followupPrompt"] != nil {
			return nil
		}
		name := strings.TrimSpace(kiroString(x["name"]))
		toolUseID := strings.TrimSpace(kiroString(x["toolUseId"]))
		if name != "" && toolUseID != "" {
			return []kiroResponseEvent{{
				Type:      "tool_use",
				ToolUseID: toolUseID,
				Name:      name,
				Input:     normalizeKiroToolInputString(x["input"]),
				Stop:      kiroEventBool(x["stop"]),
			}}
		}
		if _, ok := x["input"]; ok {
			return []kiroResponseEvent{{
				Type:      "tool_use_input",
				ToolUseID: toolUseID,
				Input:     normalizeKiroToolInputString(x["input"]),
			}}
		}
		if _, ok := x["stop"]; ok && x["contextUsagePercentage"] == nil {
			return []kiroResponseEvent{{Type: "tool_use_stop", Stop: kiroEventBool(x["stop"])}}
		}
		var out []kiroResponseEvent
		if content := kiroString(x["content"]); content != "" {
			out = append(out, kiroResponseEvent{Type: "content", Content: content})
		}
		for key, value := range x {
			if key == "content" {
				continue
			}
			out = append(out, extractKiroResponseEvents(value)...)
		}
		return out
	case []any:
		var out []kiroResponseEvent
		for _, item := range x {
			out = append(out, extractKiroResponseEvents(item)...)
		}
		return out
	default:
		return nil
	}
}

func (a *kiroToolAccumulator) handle(event kiroResponseEvent) {
	switch event.Type {
	case "tool_use":
		if a.current != nil && a.current.id != event.ToolUseID {
			a.finish()
		}
		if a.current == nil {
			a.current = &kiroToolCallBuffer{id: event.ToolUseID, name: event.Name}
		}
		if event.Name != "" {
			a.current.name = event.Name
		}
		if event.Input != "" {
			_, _ = a.current.input.WriteString(event.Input)
		}
		if event.Stop {
			a.finish()
		}
	case "tool_use_input":
		if a.current != nil && event.Input != "" {
			_, _ = a.current.input.WriteString(event.Input)
		}
	case "tool_use_stop":
		if event.Stop {
			a.finish()
		}
	}
}

func (a *kiroToolAccumulator) finish() {
	if a.current == nil {
		return
	}
	id := a.current.id
	if id == "" {
		id = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	}
	input := parseKiroToolInputString(a.current.input.String())
	a.calls = append(a.calls, kiroToolCall{ID: id, Name: a.current.name, Input: input})
	a.current = nil
}

func normalizeKiroToolInputString(input any) string {
	if input == nil {
		return ""
	}
	switch x := input.(type) {
	case string:
		return x
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func parseKiroToolInputString(input string) any {
	input = strings.TrimSpace(input)
	if input == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(input), &parsed); err == nil {
		if parsed == nil {
			return map[string]any{}
		}
		return parsed
	}
	return map[string]any{"raw_arguments": input}
}

func kiroEventBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true")
	default:
		return false
	}
}

func kiroToolCallsToAnthropicBlocks(calls []kiroToolCall, toolNameMaps *kiroToolNameMaps) []gin.H {
	blocks := make([]gin.H, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Name) == "" {
			continue
		}
		blocks = append(blocks, gin.H{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  restoreKiroToolName(call.Name, toolNameMaps),
			"input": call.Input,
		})
	}
	return blocks
}

func kiroToolCallsToOpenAI(calls []kiroToolCall, toolNameMaps *kiroToolNameMaps) []gin.H {
	out := make([]gin.H, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Name) == "" {
			continue
		}
		args, err := json.Marshal(call.Input)
		if err != nil {
			args = []byte(`{}`)
		}
		out = append(out, gin.H{
			"id":   call.ID,
			"type": "function",
			"function": gin.H{
				"name":      restoreKiroToolName(call.Name, toolNameMaps),
				"arguments": string(args),
			},
		})
	}
	return out
}

func cleanKiroToolSyntaxText(text string, parsed []kiroToolCall) (string, []kiroToolCall) {
	calls := append([]kiroToolCall(nil), parsed...)
	bracketCleaned, bracketCalls := parseBracketKiroToolCalls(text)
	xmlCleaned, xmlCalls := parseXMLKiroToolCalls(bracketCleaned)
	calls = dedupeKiroParsedToolCalls(append(calls, append(bracketCalls, xmlCalls...)...))
	return strings.TrimSpace(xmlCleaned), calls
}

func parseBracketKiroToolCalls(text string) (string, []kiroToolCall) {
	var calls []kiroToolCall
	var cleaned strings.Builder
	pos := 0
	for {
		start := strings.Index(text[pos:], "[Called")
		if start < 0 {
			_, _ = cleaned.WriteString(text[pos:])
			break
		}
		start += pos
		end := findMatchingBracket(text, start, '[', ']')
		if end < 0 {
			_, _ = cleaned.WriteString(text[pos:])
			break
		}
		segment := text[start : end+1]
		if call, ok := parseBracketKiroToolCall(segment); ok {
			_, _ = cleaned.WriteString(text[pos:start])
			calls = append(calls, call)
			pos = end + 1
			continue
		}
		_, _ = cleaned.WriteString(text[pos : end+1])
		pos = end + 1
	}
	return cleaned.String(), calls
}

func parseBracketKiroToolCall(segment string) (kiroToolCall, bool) {
	re := regexp.MustCompile(`(?is)^\[Called\s+([A-Za-z0-9_.-]+)\s+with\s+args:\s*(.*)\]$`)
	match := re.FindStringSubmatch(strings.TrimSpace(segment))
	if len(match) != 3 {
		return kiroToolCall{}, false
	}
	input := parseKiroToolInputString(match[2])
	return kiroToolCall{ID: "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24], Name: match[1], Input: input}, true
}

func parseXMLKiroToolCalls(text string) (string, []kiroToolCall) {
	re := regexp.MustCompile(`(?is)<tool_use>(.*?)</tool_use>`)
	var calls []kiroToolCall
	cleaned := re.ReplaceAllStringFunc(text, func(segment string) string {
		bodyMatch := re.FindStringSubmatch(segment)
		if len(bodyMatch) != 2 {
			return segment
		}
		if call, ok := parseXMLKiroToolCall(bodyMatch[1]); ok {
			calls = append(calls, call)
			return ""
		}
		return segment
	})
	return cleaned, calls
}

func parseXMLKiroToolCall(body string) (kiroToolCall, bool) {
	body = strings.TrimSpace(body)
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err == nil {
		name := strings.TrimSpace(kiroFirstNonEmpty(kiroString(data["name"]), kiroString(data["tool"])))
		if name == "" {
			return kiroToolCall{}, false
		}
		return kiroToolCall{ID: "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24], Name: name, Input: parseKiroToolArguments(data["input"])}, true
	}
	name := regexp.MustCompile(`(?is)<name>(.*?)</name>`).FindStringSubmatch(body)
	input := regexp.MustCompile(`(?is)<input>(.*?)</input>`).FindStringSubmatch(body)
	if len(name) < 2 {
		return kiroToolCall{}, false
	}
	rawInput := "{}"
	if len(input) >= 2 {
		rawInput = input[1]
	}
	return kiroToolCall{ID: "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24], Name: strings.TrimSpace(name[1]), Input: parseKiroToolInputString(rawInput)}, true
}

func dedupeKiroParsedToolCalls(calls []kiroToolCall) []kiroToolCall {
	seen := map[string]struct{}{}
	out := make([]kiroToolCall, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Name) == "" {
			continue
		}
		args, _ := json.Marshal(call.Input)
		key := call.Name + "\x00" + string(args)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, call)
	}
	return out
}

func restoreKiroToolCalls(calls []kiroToolCall, toolNameMaps *kiroToolNameMaps) []kiroToolCall {
	if toolNameMaps == nil || len(calls) == 0 {
		return calls
	}
	out := make([]kiroToolCall, 0, len(calls))
	for _, call := range calls {
		call.Name = restoreKiroToolName(call.Name, toolNameMaps)
		out = append(out, call)
	}
	return out
}

func kiroAnthropicContentBlocks(content string, toolCalls []kiroToolCall, thinkingRequested bool) ([]gin.H, string) {
	var blocks []gin.H
	if thinkingRequested {
		blocks = append(blocks, kiroTextToAnthropicBlocks(content)...)
	} else if strings.TrimSpace(content) != "" || len(toolCalls) == 0 {
		blocks = append(blocks, gin.H{"type": "text", "text": content})
	}
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	if thinkingRequested && len(toolCalls) == 0 && kiroBlocksContainOnlyThinking(blocks) {
		blocks = append(blocks, gin.H{"type": "text", "text": " "})
		stopReason = "max_tokens"
	}
	blocks = append(blocks, kiroToolCallsToAnthropicBlocks(toolCalls, nil)...)
	return blocks, stopReason
}

func kiroBlocksContainOnlyThinking(blocks []gin.H) bool {
	if len(blocks) == 0 {
		return false
	}
	hasThinking := false
	for _, block := range blocks {
		switch strings.ToLower(kiroString(block["type"])) {
		case "thinking":
			if strings.TrimSpace(kiroString(block["thinking"])) != "" {
				hasThinking = true
			}
		case "text":
			if !kiroIsWhitespaceOnly(kiroString(block["text"])) {
				return false
			}
		}
	}
	return hasThinking
}

func kiroTextToAnthropicBlocks(raw string) []gin.H {
	if raw == "" {
		return nil
	}
	startPos := kiroFindRealTag(raw, kiroThinkingStartTag, 0)
	if startPos == -1 {
		return []gin.H{{"type": "text", "text": raw}}
	}
	before := raw[:startPos]
	rest := raw[startPos+len(kiroThinkingStartTag):]
	if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	}
	endPos := kiroFindRealThinkingEndTag(rest, 0)
	if endPos == -1 {
		endPos = kiroFindRealThinkingEndTagAtBufferEnd(rest, 0)
	}
	thinking := ""
	after := ""
	if endPos == -1 {
		thinking = rest
	} else {
		thinking = rest[:endPos]
		after = rest[endPos+len(kiroThinkingEndTag):]
	}
	if strings.HasPrefix(after, "\r\n\r\n") {
		after = after[4:]
	} else if strings.HasPrefix(after, "\n\n") {
		after = after[2:]
	}
	var blocks []gin.H
	if !kiroIsWhitespaceOnly(before) {
		blocks = append(blocks, gin.H{"type": "text", "text": before})
	}
	blocks = append(blocks, gin.H{"type": "thinking", "thinking": thinking})
	if !kiroIsWhitespaceOnly(after) {
		blocks = append(blocks, gin.H{"type": "text", "text": after})
	}
	if len(blocks) == 0 {
		return []gin.H{{"type": "text", "text": raw}}
	}
	return blocks
}

func kiroIsWhitespaceOnly(text string) bool {
	return strings.TrimSpace(text) == ""
}

func kiroFindRealTag(text, tag string, startIndex int) int {
	searchStart := startIndex
	if searchStart < 0 {
		searchStart = 0
	}
	for {
		pos := strings.Index(text[searchStart:], tag)
		if pos < 0 {
			return -1
		}
		pos += searchStart
		hasQuoteBefore := kiroIsQuoteCharAt(text, pos-1)
		hasQuoteAfter := kiroIsQuoteCharAt(text, pos+len(tag))
		if !hasQuoteBefore && !hasQuoteAfter {
			return pos
		}
		searchStart = pos + 1
	}
}

func kiroFindRealThinkingEndTag(buffer string, startIndex int) int {
	searchStart := startIndex
	if searchStart < 0 {
		searchStart = 0
	}
	for {
		pos := kiroFindRealTag(buffer, kiroThinkingEndTag, searchStart)
		if pos == -1 {
			return -1
		}
		after := buffer[pos+len(kiroThinkingEndTag):]
		if strings.HasPrefix(after, "\n\n") || strings.HasPrefix(after, "\r\n\r\n") {
			return pos
		}
		searchStart = pos + 1
	}
}

func kiroFindRealThinkingEndTagAtBufferEnd(buffer string, startIndex int) int {
	searchStart := startIndex
	if searchStart < 0 {
		searchStart = 0
	}
	for {
		pos := kiroFindRealTag(buffer, kiroThinkingEndTag, searchStart)
		if pos == -1 {
			return -1
		}
		after := buffer[pos+len(kiroThinkingEndTag):]
		if kiroIsWhitespaceOnly(after) {
			return pos
		}
		searchStart = pos + 1
	}
}

func kiroIsQuoteCharAt(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return false
	}
	switch text[index] {
	case '"', '\'', '`':
		return true
	default:
		return false
	}
}

func findMatchingBracket(text string, start int, open, close byte) int {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if escape {
			escape = false
			continue
		}
		if inString && ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *kiroStreamParser) feed(chunk []byte) []string {
	p.buffer += string(chunk)
	var out []string
	for {
		pos := strings.Index(p.buffer, `{"content":`)
		if pos < 0 {
			if len(p.buffer) > 4096 {
				p.buffer = p.buffer[len(p.buffer)-4096:]
			}
			return out
		}
		end := findMatchingJSONBrace(p.buffer, pos)
		if end < 0 {
			return out
		}
		raw := p.buffer[pos : end+1]
		p.buffer = p.buffer[end+1:]
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err == nil {
			content := kiroString(data["content"])
			if content != "" && data["followupPrompt"] == nil {
				if delta, ok := p.normalizeContentDelta(content); ok {
					out = append(out, delta)
				}
			}
		}
	}
}

func (p *kiroStreamParser) normalizeContentDelta(content string) (string, bool) {
	if content == "" || content == p.lastContent {
		return "", false
	}
	delta := content
	if p.lastContent != "" && strings.HasPrefix(content, p.lastContent) {
		delta = strings.TrimPrefix(content, p.lastContent)
	}
	p.lastContent = content
	if delta == "" {
		return "", false
	}
	return delta, true
}

func findMatchingJSONBrace(s string, start int) int {
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if escape {
			escape = false
			continue
		}
		if inString && ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func decodeKiroEventStreamPayload(decoder *bedrockEventStreamDecoder) ([]byte, error) {
	for {
		msg, err := decoder.DecodeMessage()
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(msg.Payload)) == 0 {
			continue
		}
		if decoded := extractBedrockChunkData(msg.Payload); decoded != nil {
			return decoded, nil
		}
		return msg.Payload, nil
	}
}

func collectKiroResult(r io.Reader, contentType string, toolNameMaps *kiroToolNameMaps) (string, []kiroToolCall) {
	parser := &kiroStreamParser{}
	reader := bufio.NewReader(r)
	if isLikelyKiroEventStream(contentType, reader) {
		return collectKiroEventStreamResult(reader, parser, toolNameMaps)
	}
	var b strings.Builder
	acc := &kiroToolAccumulator{}
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			events := parser.feedEvents(buf[:n])
			if len(events) == 0 && !looksLikeKiroEventStreamBytes(buf[:n]) {
				_, _ = b.Write(buf[:n])
			}
			for _, event := range events {
				if event.Type == "content" {
					_, _ = b.WriteString(event.Content)
				} else {
					acc.handle(event)
				}
			}
		}
		if err != nil {
			break
		}
	}
	acc.finish()
	content, calls := cleanKiroToolSyntaxText(b.String(), acc.calls)
	return content, restoreKiroToolCalls(calls, toolNameMaps)
}

func collectKiroEventStreamResult(r io.Reader, parser *kiroStreamParser, toolNameMaps *kiroToolNameMaps) (string, []kiroToolCall) {
	decoder := newBedrockEventStreamDecoder(r)
	var b strings.Builder
	acc := &kiroToolAccumulator{}
	for {
		payload, err := decodeKiroEventStreamPayload(decoder)
		if err != nil {
			break
		}
		for _, event := range parser.feedPayloadEvents(payload) {
			if event.Type == "content" {
				_, _ = b.WriteString(event.Content)
			} else {
				acc.handle(event)
			}
		}
	}
	acc.finish()
	content, calls := cleanKiroToolSyntaxText(b.String(), acc.calls)
	return content, restoreKiroToolCalls(calls, toolNameMaps)
}

func isKiroEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "application/vnd.amazon.eventstream")
}

func isLikelyKiroEventStream(contentType string, reader *bufio.Reader) bool {
	if isKiroEventStream(contentType) {
		return true
	}
	if reader == nil {
		return false
	}
	peek, err := reader.Peek(512)
	if len(peek) == 0 {
		return false
	}
	if looksLikeKiroEventStreamBytes(peek) {
		return true
	}
	if bytes.Contains(peek, []byte(":event-type")) ||
		bytes.Contains(peek, []byte(":message-type")) ||
		bytes.Contains(peek, []byte("contextUsageEvent")) ||
		bytes.Contains(peek, []byte("meteringEvent")) {
		return true
	}
	if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
		return false
	}
	return false
}

func looksLikeKiroEventStreamBytes(data []byte) bool {
	if len(data) >= 12 {
		totalLength := bedrockReadUint32(data[0:4])
		headersLength := bedrockReadUint32(data[4:8])
		if totalLength >= 16 && totalLength <= 64*1024*1024 && headersLength <= totalLength-16 {
			return true
		}
	}
	return bytes.Contains(data, []byte(":event-type")) ||
		bytes.Contains(data, []byte(":message-type")) ||
		bytes.Contains(data, []byte("toolUseEvent")) ||
		bytes.Contains(data, []byte("contextUsageEvent")) ||
		bytes.Contains(data, []byte("meteringEvent"))
}

func streamKiroToOpenAI(c *gin.Context, r io.Reader, contentType string, model string, toolNameMaps *kiroToolNameMaps) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	parser := &kiroStreamParser{}
	first := true
	toolIndex := -1
	acc := &kiroToolAccumulator{}
	finishReason := "stop"
	reader := bufio.NewReader(r)
	emitEvent := func(event kiroResponseEvent) {
		switch event.Type {
		case "content":
			delta := gin.H{"content": event.Content}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": nil}},
			})
		case "tool_use":
			finishReason = "tool_calls"
			acc.handle(event)
			toolIndex++
			toolDelta := gin.H{
				"index": toolIndex,
				"id":    event.ToolUseID,
				"type":  "function",
				"function": gin.H{
					"name":      restoreKiroToolName(event.Name, toolNameMaps),
					"arguments": event.Input,
				},
			}
			delta := gin.H{"tool_calls": []gin.H{toolDelta}}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": nil}},
			})
		case "tool_use_input":
			finishReason = "tool_calls"
			acc.handle(event)
			if toolIndex < 0 {
				toolIndex = 0
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": gin.H{"tool_calls": []gin.H{{
					"index": toolIndex,
					"function": gin.H{
						"arguments": event.Input,
					},
				}}}, "finish_reason": nil}},
			})
		case "tool_use_stop":
			finishReason = "tool_calls"
			acc.handle(event)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if isLikelyKiroEventStream(contentType, reader) {
		streamKiroEventStreamEvents(c, reader, parser, func(event kiroResponseEvent) {
			emitEvent(event)
		})
		writeSSEData(c, gin.H{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []gin.H{{"index": 0, "delta": gin.H{}, "finish_reason": finishReason}},
		})
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	buf := make([]byte, 16*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			for _, event := range parser.feedEvents(buf[:n]) {
				emitEvent(event)
			}
		}
		if err != nil {
			break
		}
	}
	writeSSEData(c, gin.H{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []gin.H{{"index": 0, "delta": gin.H{}, "finish_reason": finishReason}},
	})
	_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func streamKiroToOpenAIWithCount(c *gin.Context, r io.Reader, contentType string, model string, toolNameMaps *kiroToolNameMaps) int {
	var outputRunes int
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	parser := &kiroStreamParser{}
	first := true
	toolIndex := -1
	acc := &kiroToolAccumulator{}
	finishReason := "stop"
	reader := bufio.NewReader(r)
	emitEvent := func(event kiroResponseEvent) {
		switch event.Type {
		case "content":
			outputRunes += len([]rune(event.Content))
			delta := gin.H{"content": event.Content}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": nil}},
			})
		case "tool_use":
			finishReason = "tool_calls"
			acc.handle(event)
			outputRunes += len([]rune(event.Name)) + len([]rune(event.Input))
			toolIndex++
			toolDelta := gin.H{
				"index": toolIndex,
				"id":    event.ToolUseID,
				"type":  "function",
				"function": gin.H{
					"name":      restoreKiroToolName(event.Name, toolNameMaps),
					"arguments": event.Input,
				},
			}
			delta := gin.H{"tool_calls": []gin.H{toolDelta}}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": delta, "finish_reason": nil}},
			})
		case "tool_use_input":
			finishReason = "tool_calls"
			acc.handle(event)
			outputRunes += len([]rune(event.Input))
			if toolIndex < 0 {
				toolIndex = 0
			}
			writeSSEData(c, gin.H{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []gin.H{{"index": 0, "delta": gin.H{"tool_calls": []gin.H{{
					"index": toolIndex,
					"function": gin.H{
						"arguments": event.Input,
					},
				}}}, "finish_reason": nil}},
			})
		case "tool_use_stop":
			finishReason = "tool_calls"
			acc.handle(event)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if isLikelyKiroEventStream(contentType, reader) {
		streamKiroEventStreamEvents(c, reader, parser, func(event kiroResponseEvent) {
			emitEvent(event)
		})
		writeSSEData(c, gin.H{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []gin.H{{"index": 0, "delta": gin.H{}, "finish_reason": finishReason}},
		})
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	} else {
		buf := make([]byte, 16*1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				for _, event := range parser.feedEvents(buf[:n]) {
					emitEvent(event)
				}
			}
			if err != nil {
				break
			}
		}
		writeSSEData(c, gin.H{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []gin.H{{"index": 0, "delta": gin.H{}, "finish_reason": finishReason}},
		})
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	estimated := outputRunes / 4
	if outputRunes%4 != 0 {
		estimated++
	}
	if estimated < 1 {
		return 1
	}
	return estimated
}

func streamKiroToAnthropic(c *gin.Context, r io.Reader, contentType string, model string, toolNameMaps *kiroToolNameMaps, thinking *anthropicThinkingInput) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	writeAnthropicEvent(c, "message_start", gin.H{
		"type":    "message_start",
		"message": gin.H{"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": gin.H{"input_tokens": 0, "output_tokens": 0}},
	})
	content, toolCalls := collectKiroResult(r, contentType, toolNameMaps)
	blocks, stopReason := kiroAnthropicContentBlocks(content, toolCalls, kiroThinkingRequested(thinking))
	nextIndex := 0
	for _, block := range blocks {
		blockType := strings.ToLower(kiroString(block["type"]))
		switch blockType {
		case "text":
			text := kiroString(block["text"])
			if text == "" {
				continue
			}
			index := nextIndex
			nextIndex++
			writeAnthropicEvent(c, "content_block_start", gin.H{"type": "content_block_start", "index": index, "content_block": gin.H{"type": "text", "text": ""}})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "text_delta", "text": text}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		case "thinking":
			thinkingText := kiroString(block["thinking"])
			if thinkingText == "" {
				continue
			}
			index := nextIndex
			nextIndex++
			writeAnthropicEvent(c, "content_block_start", gin.H{"type": "content_block_start", "index": index, "content_block": gin.H{"type": "thinking", "thinking": ""}})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "thinking_delta", "thinking": thinkingText}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		case "tool_use":
			index := nextIndex
			nextIndex++
			input := block["input"]
			inputJSON, err := json.Marshal(input)
			if err != nil {
				inputJSON = []byte(`{}`)
			}
			writeAnthropicEvent(c, "content_block_start", gin.H{
				"type":  "content_block_start",
				"index": index,
				"content_block": gin.H{
					"type":  "tool_use",
					"id":    kiroString(block["id"]),
					"name":  kiroString(block["name"]),
					"input": gin.H{},
				},
			})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "input_json_delta", "partial_json": string(inputJSON)}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		}
	}
	writeAnthropicEvent(c, "message_delta", gin.H{"type": "message_delta", "delta": gin.H{"stop_reason": stopReason, "stop_sequence": nil}, "usage": gin.H{"output_tokens": 0}})
	writeAnthropicEvent(c, "message_stop", gin.H{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

func streamKiroToAnthropicWithCount(c *gin.Context, r io.Reader, contentType string, model string, toolNameMaps *kiroToolNameMaps, thinking *anthropicThinkingInput) int {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	writeAnthropicEvent(c, "message_start", gin.H{
		"type":    "message_start",
		"message": gin.H{"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": gin.H{"input_tokens": 0, "output_tokens": 0}},
	})
	content, toolCalls := collectKiroResult(r, contentType, toolNameMaps)
	blocks, stopReason := kiroAnthropicContentBlocks(content, toolCalls, kiroThinkingRequested(thinking))
	nextIndex := 0
	for _, block := range blocks {
		blockType := strings.ToLower(kiroString(block["type"]))
		switch blockType {
		case "text":
			text := kiroString(block["text"])
			if text == "" {
				continue
			}
			index := nextIndex
			nextIndex++
			writeAnthropicEvent(c, "content_block_start", gin.H{"type": "content_block_start", "index": index, "content_block": gin.H{"type": "text", "text": ""}})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "text_delta", "text": text}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		case "thinking":
			thinkingText := kiroString(block["thinking"])
			if thinkingText == "" {
				continue
			}
			index := nextIndex
			nextIndex++
			writeAnthropicEvent(c, "content_block_start", gin.H{"type": "content_block_start", "index": index, "content_block": gin.H{"type": "thinking", "thinking": ""}})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "thinking_delta", "thinking": thinkingText}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		case "tool_use":
			index := nextIndex
			nextIndex++
			input := block["input"]
			inputJSON, err := json.Marshal(input)
			if err != nil {
				inputJSON = []byte(`{}`)
			}
			writeAnthropicEvent(c, "content_block_start", gin.H{
				"type":  "content_block_start",
				"index": index,
				"content_block": gin.H{
					"type":  "tool_use",
					"id":    kiroString(block["id"]),
					"name":  kiroString(block["name"]),
					"input": gin.H{},
				},
			})
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": index, "delta": gin.H{"type": "input_json_delta", "partial_json": string(inputJSON)}})
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": index})
		}
	}
	outputTokens := estimateKiroOutputTokens(content, toolCalls)
	writeAnthropicEvent(c, "message_delta", gin.H{"type": "message_delta", "delta": gin.H{"stop_reason": stopReason, "stop_sequence": nil}, "usage": gin.H{"output_tokens": outputTokens}})
	writeAnthropicEvent(c, "message_stop", gin.H{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
	return outputTokens
}

func streamKiroEventStream(c *gin.Context, r io.Reader, parser *kiroStreamParser, emit func(content string)) {
	streamKiroEventStreamEvents(c, r, parser, func(event kiroResponseEvent) {
		if event.Type == "content" {
			emit(event.Content)
		}
	})
}

func streamKiroEventStreamEvents(c *gin.Context, r io.Reader, parser *kiroStreamParser, emit func(event kiroResponseEvent)) {
	decoder := newBedrockEventStreamDecoder(r)
	for {
		payload, err := decodeKiroEventStreamPayload(decoder)
		if err != nil {
			return
		}
		for _, event := range parser.feedPayloadEvents(payload) {
			emit(event)
		}
	}
}

func writeSSEData(c *gin.Context, data any) {
	b, _ := json.Marshal(data)
	_, _ = c.Writer.Write([]byte("data: "))
	_, _ = c.Writer.Write(b)
	_, _ = c.Writer.Write([]byte("\n\n"))
}

func writeAnthropicEvent(c *gin.Context, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = c.Writer.Write([]byte("event: " + event + "\n"))
	_, _ = c.Writer.Write([]byte("data: "))
	_, _ = c.Writer.Write(b)
	_, _ = c.Writer.Write([]byte("\n\n"))
}

func writeOpenAIError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"error": gin.H{"type": typ, "message": message}})
	return nil
}

func writeKiroAnthropicError(c *gin.Context, status int, typ, message string) error {
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": message}})
	return nil
}

func mapKiroStatus(status int) int {
	switch status {
	case 402, 403, 429:
		return status
	case 400, 422:
		return status
	default:
		if status >= 500 {
			return http.StatusBadGateway
		}
		return status
	}
}

func isKiroRecoverableStatus(status int) bool {
	return status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests
}

func upstreamErrorMessage(body []byte) string {
	if len(body) == 0 {
		return "kiro upstream request failed"
	}
	return truncateForError(body)
}

func groupIDFromContext(c *gin.Context) *int64 {
	if c == nil {
		return nil
	}
	if v, ok := c.Get("api_key"); ok {
		if apiKey, ok := v.(*APIKey); ok && apiKey != nil {
			return apiKey.GroupID
		}
	}
	return nil
}

func kiroAPIRegion(account *Account) string {
	if account != nil {
		if v := strings.TrimSpace(account.GetCredential("api_region")); v != "" {
			return v
		}
		if v := detectRegionFromProfileARN(account.GetCredential("profile_arn")); v != "" {
			return v
		}
	}
	return "us-east-1"
}

func decorateKiroAPIHeaders(req *http.Request, account *Account) {
	apiRegion := kiroAPIRegion(account)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", fmt.Sprintf("q.%s.amazonaws.com", apiRegion))
	req.Header.Set("X-Amzn-Codewhisperer-Optout", "true")
	req.Header.Set("X-Amzn-Kiro-Agent-Mode", "vibe")
	req.Header.Set("X-Amz-User-Agent", kiroXAmzUserAgent(account))
	req.Header.Set("User-Agent", kiroUserAgent(account))
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	if account != nil {
		if profileARN := strings.TrimSpace(account.GetCredential("profile_arn")); profileARN != "" {
			req.Header.Set("X-Amzn-Kiro-Profile-Arn", profileARN)
		}
	}
}

func kiroResolveModel(model string) string {
	normalized := normalizeKiroModelAlias(model)
	if normalized == "auto-kiro" || normalized == "" {
		return "auto"
	}
	if canonical, ok := kiroCanonicalModel(normalized); ok {
		if canonical == "auto-kiro" {
			return "auto"
		}
		if canonical == "claude-3.7-sonnet" {
			return "CLAUDE_3_7_SONNET_20250219_V1_0"
		}
		return canonical
	}
	if normalized == "claude-4.5-opus-high" || normalized == "claude-4-5-opus-high" {
		return "claude-opus-4.5"
	}
	if strings.HasPrefix(normalized, "claude-3.7-sonnet") {
		return "CLAUDE_3_7_SONNET_20250219_V1_0"
	}
	return normalized
}

func kiroTestModel(model string) string {
	if canonical, ok := kiroCanonicalModel(model); ok {
		return canonical
	}
	return "auto-kiro"
}

func kiroCanonicalModel(model string) (string, bool) {
	normalized := normalizeKiroModelAlias(model)
	switch {
	case normalized == "" || normalized == "auto" || normalized == "auto-kiro":
		return "auto-kiro", true
	case normalized == "claude-sonnet-4" || strings.HasPrefix(normalized, "claude-sonnet-4-20250514"):
		return "claude-sonnet-4", true
	case normalized == "claude-haiku-4.5" || strings.HasPrefix(normalized, "claude-haiku-4.5-20251001"):
		return "claude-haiku-4.5", true
	case normalized == "claude-sonnet-4.5" || strings.HasPrefix(normalized, "claude-sonnet-4.5-20250929"):
		return "claude-sonnet-4.5", true
	case normalized == "claude-opus-4.5" || normalized == "claude-4.5-opus-high" || normalized == "claude-4-5-opus-high" || strings.HasPrefix(normalized, "claude-opus-4.5-20251101"):
		return "claude-opus-4.5", true
	case normalized == "claude-sonnet-4.6":
		return "claude-sonnet-4.6", true
	case normalized == "claude-opus-4.6":
		return "claude-opus-4.6", true
	case normalized == "claude-opus-4.7":
		return "claude-opus-4.7", true
	case strings.HasPrefix(normalized, "claude-3.7-sonnet"):
		return "claude-3.7-sonnet", true
	default:
		return "", false
	}
}

func normalizeKiroModelAlias(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	normalized = strings.TrimPrefix(normalized, "anthropic/")
	normalized = strings.TrimPrefix(normalized, "us.anthropic.")
	normalized = strings.TrimPrefix(normalized, "global.anthropic.")
	normalized = strings.TrimSuffix(normalized, "-latest")
	normalized = strings.TrimSuffix(normalized, ":0")
	normalized = strings.ReplaceAll(normalized, "claude-3-7-sonnet", "claude-3.7-sonnet")
	normalized = strings.ReplaceAll(normalized, "claude-haiku-4-5", "claude-haiku-4.5")
	normalized = strings.ReplaceAll(normalized, "claude-sonnet-4-5", "claude-sonnet-4.5")
	normalized = strings.ReplaceAll(normalized, "claude-opus-4-5", "claude-opus-4.5")
	normalized = strings.ReplaceAll(normalized, "claude-sonnet-4-6", "claude-sonnet-4.6")
	normalized = strings.ReplaceAll(normalized, "claude-opus-4-6", "claude-opus-4.6")
	normalized = strings.ReplaceAll(normalized, "claude-opus-4-7", "claude-opus-4.7")
	return normalized
}
