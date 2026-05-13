package service

import (
	"bufio"
	"bytes"
	"context"
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

func (s *KiroGatewayService) ForwardOpenAIChat(ctx context.Context, c *gin.Context, body []byte) error {
	var req openAIChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
	}
	if strings.TrimSpace(req.Model) == "" {
		return writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	resp, err := s.callGenerateAcrossAccounts(ctx, groupIDFromContext(c), func(account *Account) (map[string]any, error) {
		return buildKiroPayloadFromOpenAI(req, account)
	})
	if err != nil {
		return writeOpenAIError(c, http.StatusBadGateway, "api_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return writeOpenAIError(c, mapKiroStatus(resp.StatusCode), "api_error", upstreamErrorMessage(respBody))
	}
	if req.Stream {
		streamKiroToOpenAI(c, resp.Body, resp.Header.Get("Content-Type"), req.Model)
		return nil
	}
	content, toolCalls := collectKiroResult(resp.Body, resp.Header.Get("Content-Type"))
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
		message["tool_calls"] = kiroToolCallsToOpenAI(toolCalls)
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
		"usage": gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
	return nil
}

func (s *KiroGatewayService) ForwardAnthropicMessages(ctx context.Context, c *gin.Context, body []byte) error {
	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "invalid request body")
	}
	if strings.TrimSpace(req.Model) == "" {
		return writeKiroAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	resp, err := s.callGenerateAcrossAccounts(ctx, groupIDFromContext(c), func(account *Account) (map[string]any, error) {
		return buildKiroPayloadFromAnthropic(req, account)
	})
	if err != nil {
		return writeKiroAnthropicError(c, http.StatusBadGateway, "api_error", err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return writeKiroAnthropicError(c, mapKiroStatus(resp.StatusCode), "api_error", upstreamErrorMessage(respBody))
	}
	if req.Stream {
		streamKiroToAnthropic(c, resp.Body, resp.Header.Get("Content-Type"), req.Model)
		return nil
	}
	content, toolCalls := collectKiroResult(resp.Body, resp.Header.Get("Content-Type"))
	contentBlocks := []gin.H{}
	if strings.TrimSpace(content) != "" || len(toolCalls) == 0 {
		contentBlocks = append(contentBlocks, gin.H{"type": "text", "text": content})
	}
	contentBlocks = append(contentBlocks, kiroToolCallsToAnthropicBlocks(toolCalls)...)
	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       contentBlocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         gin.H{"input_tokens": 0, "output_tokens": 0},
	})
	return nil
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
	defer resp.Body.Close()
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

func (s *KiroGatewayService) callGenerateAcrossAccounts(ctx context.Context, groupID *int64, buildPayload kiroPayloadBuilder) (*http.Response, error) {
	accounts, err := s.listAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("no schedulable kiro accounts")
	}

	var lastErr error
	for i := range accounts {
		account := &accounts[i]
		payload, err := buildPayload(account)
		if err != nil {
			return nil, err
		}
		resp, err := s.callGenerate(ctx, account, payload)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusForbidden {
			_ = s.tokenProvider.Refresh(ctx, account)
			resp.Body.Close()
			resp, err = s.callGenerate(ctx, account, payload)
			if err != nil {
				lastErr = err
				continue
			}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		if isKiroRecoverableStatus(resp.StatusCode) && i < len(accounts)-1 {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no schedulable kiro accounts")
}

func (s *KiroGatewayService) selectAccount(ctx context.Context, groupID *int64) (*Account, error) {
	accounts, err := s.listAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, errors.New("no schedulable kiro accounts")
	}
	return &accounts[0], nil
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
	Model    string           `json:"model"`
	Messages []map[string]any `json:"messages"`
	System   any              `json:"system"`
	Stream   bool             `json:"stream"`
	Tools    []map[string]any `json:"tools"`
}

func buildKiroPayloadFromOpenAI(req openAIChatRequest, account *Account) (map[string]any, error) {
	systemPrompt, messages := splitOpenAIMessages(req.Messages)
	return buildKiroPayload(req.Model, systemPrompt, messages, req.Tools, account), nil
}

func buildKiroPayloadFromAnthropic(req anthropicMessagesRequest, account *Account) (map[string]any, error) {
	systemPrompt := extractText(req.System)
	return buildKiroPayload(req.Model, systemPrompt, req.Messages, req.Tools, account), nil
}

func buildKiroPayload(model, systemPrompt string, messages []map[string]any, tools []map[string]any, account *Account) map[string]any {
	modelID := kiroResolveModel(model)
	normalized := normalizeKiroMessages(messages)
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
	context := kiroUserInputMessageContext(current, tools)
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

func normalizeKiroMessages(messages []map[string]any) []kiroChatMessage {
	out := make([]kiroChatMessage, 0, len(messages))
	for _, msg := range messages {
		role := normalizeKiroRole(kiroString(msg["role"]))
		content, images := extractKiroContentAndImages(msg["content"])
		toolUses, toolResults := extractKiroToolBlocks(msg)
		if role == "tool" || role == "function" {
			role = "user"
			name := kiroFirstNonEmpty(kiroString(msg["name"]), kiroString(msg["tool_call_id"]))
			if strings.TrimSpace(content) == "" {
				content = "(empty)"
			}
			toolResults = append(toolResults, buildKiroToolResult(name, content))
		}
		if role == "assistant" {
			toolUses = append(toolUses, extractOpenAIToolUses(msg["tool_calls"])...)
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
	context := kiroUserInputMessageContext(msg, nil)
	if len(context) > 0 {
		out["userInputMessageContext"] = context
	}
	return out
}

func kiroUserInputMessageContext(msg kiroChatMessage, tools []map[string]any) map[string]any {
	context := map[string]any{}
	if len(msg.toolResults) > 0 {
		context["toolResults"] = dedupeKiroToolResults(msg.toolResults)
	}
	if tools != nil {
		context["tools"] = normalizeKiroTools(tools)
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
	switch x := v.(type) {
	case string:
		return x, nil
	case []any:
		var parts []string
		var images []any
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				text, itemImages := extractKiroContentBlock(m)
				if strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
				images = append(images, itemImages...)
			}
		}
		return strings.Join(parts, "\n"), images
	case []map[string]any:
		var parts []string
		var images []any
		for _, item := range x {
			text, itemImages := extractKiroContentBlock(item)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
			images = append(images, itemImages...)
		}
		return strings.Join(parts, "\n"), images
	default:
		return "", nil
	}
}

func extractKiroContentBlock(block map[string]any) (string, []any) {
	blockType := strings.ToLower(kiroString(block["type"]))
	switch blockType {
	case "text", "":
		return kiroString(block["text"]), nil
	case "image", "image_url":
		if image := kiroImageFromBlock(block); image != nil {
			return "", []any{image}
		}
	case "tool_result":
		return "", nil
	case "tool_use":
		return "", nil
	}
	if text := kiroString(block["text"]); text != "" {
		return text, nil
	}
	return "", nil
}

func extractKiroToolBlocks(msg map[string]any) ([]map[string]any, []map[string]any) {
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
				if toolUse := buildKiroToolUseFromBlock(block); toolUse != nil {
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
				if toolUse := buildKiroToolUseFromBlock(block); toolUse != nil {
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

func buildKiroToolUseFromBlock(block map[string]any) map[string]any {
	name := strings.TrimSpace(kiroString(block["name"]))
	toolUseID := strings.TrimSpace(kiroString(block["id"]))
	if name == "" || toolUseID == "" {
		return nil
	}
	return map[string]any{
		"input":     sanitizeKiroToolInput(block["input"]),
		"name":      name,
		"toolUseId": toolUseID,
	}
}

func extractOpenAIToolUses(v any) []map[string]any {
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
			"name":      name,
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

func normalizeKiroTools(tools []map[string]any) []map[string]any {
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
				"name":        name,
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

func extractOpenAIToolCalls(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	var parts []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := m["function"].(map[string]any); ok {
			name := kiroFirstNonEmpty(kiroString(fn["name"]), "tool")
			args := kiroString(fn["arguments"])
			parts = append(parts, fmt.Sprintf("Tool request %s: %s", name, args))
		}
	}
	return strings.Join(parts, "\n")
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

func (p *kiroStreamParser) feedPayload(payload []byte) []string {
	events := p.feedPayloadEvents(payload)
	out := make([]string, 0, len(events))
	for _, event := range events {
		if event.Type == "content" {
			out = append(out, event.Content)
		}
	}
	return out
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
			a.current.input.WriteString(event.Input)
		}
		if event.Stop {
			a.finish()
		}
	case "tool_use_input":
		if a.current != nil && event.Input != "" {
			a.current.input.WriteString(event.Input)
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

func kiroToolCallsToAnthropicBlocks(calls []kiroToolCall) []gin.H {
	blocks := make([]gin.H, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Name) == "" {
			continue
		}
		blocks = append(blocks, gin.H{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": call.Input,
		})
	}
	return blocks
}

func kiroToolCallsToOpenAI(calls []kiroToolCall) []gin.H {
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
				"name":      call.Name,
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
			cleaned.WriteString(text[pos:])
			break
		}
		start += pos
		end := findMatchingBracket(text, start, '[', ']')
		if end < 0 {
			cleaned.WriteString(text[pos:])
			break
		}
		segment := text[start : end+1]
		if call, ok := parseBracketKiroToolCall(segment); ok {
			cleaned.WriteString(text[pos:start])
			calls = append(calls, call)
			pos = end + 1
			continue
		}
		cleaned.WriteString(text[pos : end+1])
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
		if ch == open {
			depth++
		} else if ch == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *kiroStreamParser) feedPayloadContentFields(payload []byte) []string {
	var data any
	if err := json.Unmarshal(payload, &data); err != nil {
		return p.feed(payload)
	}
	var out []string
	for _, content := range extractKiroContentFields(data) {
		if delta, ok := p.normalizeContentDelta(content); ok {
			out = append(out, delta)
		}
	}
	return out
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

func extractKiroContentFields(v any) []string {
	switch x := v.(type) {
	case map[string]any:
		if x["followupPrompt"] != nil {
			return nil
		}
		var out []string
		if content := kiroString(x["content"]); content != "" {
			out = append(out, content)
		}
		for key, value := range x {
			if key == "content" {
				continue
			}
			out = append(out, extractKiroContentFields(value)...)
		}
		return out
	case []any:
		var out []string
		for _, item := range x {
			out = append(out, extractKiroContentFields(item)...)
		}
		return out
	default:
		return nil
	}
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
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func collectKiroContent(r io.Reader, contentType string) string {
	content, _ := collectKiroResult(r, contentType)
	return content
}

func collectKiroResult(r io.Reader, contentType string) (string, []kiroToolCall) {
	parser := &kiroStreamParser{}
	if isKiroEventStream(contentType) {
		return collectKiroEventStreamResult(r, parser)
	}
	reader := bufio.NewReader(r)
	var b strings.Builder
	acc := &kiroToolAccumulator{}
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			for _, event := range parser.feedEvents(buf[:n]) {
				if event.Type == "content" {
					b.WriteString(event.Content)
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
	return cleanKiroToolSyntaxText(b.String(), acc.calls)
}

func collectKiroEventStreamContent(r io.Reader, parser *kiroStreamParser) string {
	content, _ := collectKiroEventStreamResult(r, parser)
	return content
}

func collectKiroEventStreamResult(r io.Reader, parser *kiroStreamParser) (string, []kiroToolCall) {
	decoder := newBedrockEventStreamDecoder(r)
	var b strings.Builder
	acc := &kiroToolAccumulator{}
	for {
		payload, err := decoder.Decode()
		if err != nil {
			break
		}
		for _, event := range parser.feedPayloadEvents(payload) {
			if event.Type == "content" {
				b.WriteString(event.Content)
			} else {
				acc.handle(event)
			}
		}
	}
	acc.finish()
	return cleanKiroToolSyntaxText(b.String(), acc.calls)
}

func isKiroEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "application/vnd.amazon.eventstream")
}

func streamKiroToOpenAI(c *gin.Context, r io.Reader, contentType string, model string) {
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
					"name":      event.Name,
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
	if isKiroEventStream(contentType) {
		streamKiroEventStreamEvents(c, r, parser, func(event kiroResponseEvent) {
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
		n, err := r.Read(buf)
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

func streamKiroToAnthropic(c *gin.Context, r io.Reader, contentType string, model string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	writeAnthropicEvent(c, "message_start", gin.H{
		"type":    "message_start",
		"message": gin.H{"id": msgID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": gin.H{"input_tokens": 0, "output_tokens": 0}},
	})
	parser := &kiroStreamParser{}
	acc := &kiroToolAccumulator{}
	nextIndex := 0
	textIndex := -1
	textOpen := false
	currentToolIndex := -1
	stopReason := "end_turn"
	ensureTextBlock := func() {
		if textOpen {
			return
		}
		textIndex = nextIndex
		nextIndex++
		textOpen = true
		writeAnthropicEvent(c, "content_block_start", gin.H{"type": "content_block_start", "index": textIndex, "content_block": gin.H{"type": "text", "text": ""}})
	}
	stopTextBlock := func() {
		if !textOpen {
			return
		}
		writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": textIndex})
		textOpen = false
		textIndex = -1
	}
	emitEvent := func(event kiroResponseEvent) {
		switch event.Type {
		case "content":
			ensureTextBlock()
			writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": textIndex, "delta": gin.H{"type": "text_delta", "text": event.Content}})
		case "tool_use":
			stopReason = "tool_use"
			stopTextBlock()
			acc.handle(event)
			currentToolIndex = nextIndex
			nextIndex++
			writeAnthropicEvent(c, "content_block_start", gin.H{
				"type":  "content_block_start",
				"index": currentToolIndex,
				"content_block": gin.H{
					"type":  "tool_use",
					"id":    event.ToolUseID,
					"name":  event.Name,
					"input": gin.H{},
				},
			})
			if event.Input != "" {
				writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": currentToolIndex, "delta": gin.H{"type": "input_json_delta", "partial_json": event.Input}})
			}
			if event.Stop {
				writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": currentToolIndex})
				currentToolIndex = -1
			}
		case "tool_use_input":
			stopReason = "tool_use"
			acc.handle(event)
			if currentToolIndex >= 0 && event.Input != "" {
				writeAnthropicEvent(c, "content_block_delta", gin.H{"type": "content_block_delta", "index": currentToolIndex, "delta": gin.H{"type": "input_json_delta", "partial_json": event.Input}})
			}
		case "tool_use_stop":
			stopReason = "tool_use"
			acc.handle(event)
			if event.Stop && currentToolIndex >= 0 {
				writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": currentToolIndex})
				currentToolIndex = -1
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if isKiroEventStream(contentType) {
		streamKiroEventStreamEvents(c, r, parser, func(event kiroResponseEvent) {
			emitEvent(event)
		})
		stopTextBlock()
		if currentToolIndex >= 0 {
			writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": currentToolIndex})
		}
		writeAnthropicEvent(c, "message_delta", gin.H{"type": "message_delta", "delta": gin.H{"stop_reason": stopReason, "stop_sequence": nil}, "usage": gin.H{"output_tokens": 0}})
		writeAnthropicEvent(c, "message_stop", gin.H{"type": "message_stop"})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	buf := make([]byte, 16*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, event := range parser.feedEvents(buf[:n]) {
				emitEvent(event)
			}
		}
		if err != nil {
			break
		}
	}
	stopTextBlock()
	if currentToolIndex >= 0 {
		writeAnthropicEvent(c, "content_block_stop", gin.H{"type": "content_block_stop", "index": currentToolIndex})
	}
	writeAnthropicEvent(c, "message_delta", gin.H{"type": "message_delta", "delta": gin.H{"stop_reason": stopReason, "stop_sequence": nil}, "usage": gin.H{"output_tokens": 0}})
	writeAnthropicEvent(c, "message_stop", gin.H{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
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
		payload, err := decoder.Decode()
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
