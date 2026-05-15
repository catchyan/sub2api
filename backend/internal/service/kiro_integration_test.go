package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func buildKiroEventStreamFrame(eventType string, payload []byte) []byte {
	crc32IeeeTab := crc32.MakeTable(crc32.IEEE)
	var headersBuf bytes.Buffer
	_ = headersBuf.WriteByte(byte(len(":event-type")))
	_, _ = headersBuf.WriteString(":event-type")
	_ = headersBuf.WriteByte(7)
	_ = binary.Write(&headersBuf, binary.BigEndian, uint16(len(eventType)))
	_, _ = headersBuf.WriteString(eventType)
	_ = headersBuf.WriteByte(byte(len(":message-type")))
	_, _ = headersBuf.WriteString(":message-type")
	_ = headersBuf.WriteByte(7)
	_ = binary.Write(&headersBuf, binary.BigEndian, uint16(len("event")))
	_, _ = headersBuf.WriteString("event")

	headers := headersBuf.Bytes()
	headersLen := uint32(len(headers))
	totalLen := uint32(12 + len(headers) + len(payload) + 4)

	var preludeBuf bytes.Buffer
	_ = binary.Write(&preludeBuf, binary.BigEndian, totalLen)
	_ = binary.Write(&preludeBuf, binary.BigEndian, headersLen)
	preludeBytes := preludeBuf.Bytes()
	preludeCRC := crc32.Checksum(preludeBytes, crc32IeeeTab)

	var frame bytes.Buffer
	_, _ = frame.Write(preludeBytes)
	_ = binary.Write(&frame, binary.BigEndian, preludeCRC)
	_, _ = frame.Write(headers)
	_, _ = frame.Write(payload)
	messageCRC := crc32.Checksum(frame.Bytes(), crc32IeeeTab)
	_ = binary.Write(&frame, binary.BigEndian, messageCRC)
	return frame.Bytes()
}

type kiroHydrationCacheStub struct {
	snapshot []*Account
	accounts map[int64]*Account
}

func (c *kiroHydrationCacheStub) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return c.snapshot, true, nil
}

func (c *kiroHydrationCacheStub) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	return nil
}

func (c *kiroHydrationCacheStub) GetAccount(_ context.Context, accountID int64) (*Account, error) {
	return c.accounts[accountID], nil
}

func (c *kiroHydrationCacheStub) SetAccount(context.Context, *Account) error {
	return nil
}

func (c *kiroHydrationCacheStub) DeleteAccount(context.Context, int64) error {
	return nil
}

func (c *kiroHydrationCacheStub) UpdateLastUsed(context.Context, map[int64]time.Time) error {
	return nil
}

func (c *kiroHydrationCacheStub) TryLockBucket(context.Context, SchedulerBucket, time.Duration) (bool, error) {
	return true, nil
}

func (c *kiroHydrationCacheStub) UnlockBucket(context.Context, SchedulerBucket) error {
	return nil
}

func (c *kiroHydrationCacheStub) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	return nil, nil
}

func (c *kiroHydrationCacheStub) GetOutboxWatermark(context.Context) (int64, error) {
	return 0, nil
}

func (c *kiroHydrationCacheStub) SetOutboxWatermark(context.Context, int64) error {
	return nil
}

func TestNormalizeKiroJSONCredentialsArrayWithCompanion(t *testing.T) {
	refreshToken := strings.Repeat("r", 128)
	raw := `{
		"accounts": [{
			"email": "user@example.com",
			"credentials": {
				"refreshToken": "` + refreshToken + `",
				"authMethod": "idc",
				"clientIdHash": "abc123",
				"profileArn": "arn:aws:codewhisperer:eu-west-1:123456789012:profile/test",
				"region": "us-east-1",
				"machineId": "2582956e-cc88-4669-b546-07adbffcb894"
			}
		}]
	}`
	companion := `{"clientId":"client-1","clientSecret":"secret-1"}`

	result, err := NormalizeKiroJSONCredentials([]byte(raw), []byte(companion), KiroCredentialImportRequest{DefaultName: "Kiro Test"})
	require.NoError(t, err)
	require.Len(t, result, 1)

	cred := result[0]
	require.Equal(t, KiroAuthAWSSSOOIDC, cred.AuthType)
	require.Equal(t, "Kiro Test", cred.DisplayName)
	require.Equal(t, "client-1", cred.Credentials["client_id"])
	require.Equal(t, "secret-1", cred.Credentials["client_secret"])
	require.Equal(t, "us-east-1", cred.Credentials["auth_region"])
	require.Equal(t, "eu-west-1", cred.Credentials["api_region"])
	require.Equal(t, "2582956ecc884669b54607adbffcb8942582956ecc884669b54607adbffcb894", cred.Credentials["machine_id"])
	require.Equal(t, "user@example.com", cred.Credentials["email"])
}

func TestNormalizeKiroRefreshTokensRejectsTruncatedPreview(t *testing.T) {
	_, err := NormalizeKiroRefreshTokens(KiroCredentialImportRequest{
		RefreshToken: "eyJhbGciOiJIUzI1NiJ9...",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "looks truncated")
}

func TestBuildKiroPayloadAlternatesAndKeepsImages(t *testing.T) {
	imageData := "data:image/png;base64," + strings.Repeat("a", 120)
	payload := buildKiroPayload("auto-kiro", "system note", []map[string]any{
		{"role": "assistant", "content": "previous assistant"},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "first user"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageData}},
		}},
		{"role": "user", "content": "second user"},
	}, nil, nil)

	state := payload["conversationState"].(map[string]any) //nolint:errcheck
	history := state["history"].([]any)                    //nolint:errcheck
	require.Len(t, history, 4)
	require.Contains(t, history[0].(map[string]any), "userInputMessage")         //nolint:errcheck
	require.Contains(t, history[1].(map[string]any), "assistantResponseMessage") //nolint:errcheck
	require.Contains(t, history[2].(map[string]any), "userInputMessage")         //nolint:errcheck
	require.Contains(t, history[3].(map[string]any), "assistantResponseMessage") //nolint:errcheck

	firstUser := history[2].(map[string]any)["userInputMessage"].(map[string]any) //nolint:errcheck
	require.Equal(t, "auto", firstUser["modelId"])
	require.Len(t, firstUser["images"], 1)

	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any) //nolint:errcheck
	require.Equal(t, "auto", current["modelId"])
	require.Equal(t, "system note\n\nsecond user", current["content"])
}

func TestBuildKiroPayloadIncludesToolContext(t *testing.T) {
	payload := buildKiroPayload("claude-sonnet-4.5", "", []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
		}},
	}, []map[string]any{
		{
			"name":         "Bash",
			"description":  "Run a shell command.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}},
		},
	}, nil)

	state := payload["conversationState"].(map[string]any)                                   //nolint:errcheck
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any) //nolint:errcheck
	context := current["userInputMessageContext"].(map[string]any)                           //nolint:errcheck

	require.Equal(t, "Tool results provided.", current["content"])
	require.Len(t, context["toolResults"], 1)
	require.Len(t, context["tools"], 1)

	tool := context["tools"].([]map[string]any)[0]["toolSpecification"].(map[string]any) //nolint:errcheck
	require.Equal(t, "Bash", tool["name"])
}

func TestNormalizeKiroMessagesKeepsAssistantToolUsesStructured(t *testing.T) {
	payload := buildKiroPayload("claude-sonnet-4.5", "", []map[string]any{
		{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
		{"role": "user", "content": "done"},
	}, nil, nil)

	state := payload["conversationState"].(map[string]any)                                //nolint:errcheck
	history := state["history"].([]any)                                                   //nolint:errcheck
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any) //nolint:errcheck
	toolUses := assistant["toolUses"].([]map[string]any)                                  //nolint:errcheck

	require.Equal(t, "Bash", toolUses[0]["name"])
	require.Equal(t, "toolu_1", toolUses[0]["toolUseId"])
	require.NotContains(t, assistant["content"], "tool_use")
}

func TestKiroToolEventsBecomeParsedToolCalls(t *testing.T) {
	parser := &kiroStreamParser{}
	acc := &kiroToolAccumulator{}

	for _, event := range parser.feedPayloadEvents([]byte(`{"name":"Bash","toolUseId":"toolu_1","input":"{\"command\":\"ls\"}","stop":true}`)) {
		acc.handle(event)
	}
	acc.finish()

	require.Len(t, acc.calls, 1)
	require.Equal(t, "toolu_1", acc.calls[0].ID)
	require.Equal(t, "Bash", acc.calls[0].Name)
	require.Equal(t, "ls", acc.calls[0].Input.(map[string]any)["command"]) //nolint:errcheck
}

func TestCleanKiroToolSyntaxTextParsesXMLFallback(t *testing.T) {
	content, calls := cleanKiroToolSyntaxText(`before <tool_use><name>Bash</name><input>{"command":"pwd"}</input></tool_use> after`, nil)

	require.Equal(t, "before  after", content)
	require.Len(t, calls, 1)
	require.Equal(t, "Bash", calls[0].Name)
	require.Equal(t, "pwd", calls[0].Input.(map[string]any)["command"]) //nolint:errcheck
}

func TestKiroToolNameMapsShortenAndRestore(t *testing.T) {
	longName := "mcp__very_long_namespace__very_long_server_name__tool_with_a_name_far_beyond_sixty_four_characters"
	maps := buildKiroToolNameMaps([]map[string]any{{
		"name":         longName,
		"description":  "desc",
		"input_schema": map[string]any{"type": "object"},
	}})

	alias := kiroToolNameToKiro(longName, maps)
	require.NotEqual(t, longName, alias)
	require.LessOrEqual(t, len(alias), kiroMaxToolNameLength)
	require.Equal(t, longName, restoreKiroToolName(alias, maps))
}

func TestBuildKiroPayloadShortensToolNamesForKiro(t *testing.T) {
	longName := "mcp__very_long_namespace__very_long_server_name__tool_with_a_name_far_beyond_sixty_four_characters"
	payload := buildKiroPayload("claude-sonnet-4.5", "", []map[string]any{
		{"role": "user", "content": "run it"},
	}, []map[string]any{{
		"name":         longName,
		"description":  "Run something",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}}, nil)

	state := payload["conversationState"].(map[string]any)                                   //nolint:errcheck
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any) //nolint:errcheck
	context := current["userInputMessageContext"].(map[string]any)                           //nolint:errcheck
	tool := context["tools"].([]map[string]any)[0]["toolSpecification"].(map[string]any)     //nolint:errcheck
	alias := tool["name"].(string)                                                           //nolint:errcheck

	require.LessOrEqual(t, len(alias), kiroMaxToolNameLength)
	require.NotEqual(t, longName, alias)
}

func TestBuildKiroPayloadAppliesAnthropicThinkingPrefixAndHistoryThinking(t *testing.T) {
	payload := buildKiroPayloadWithThinking("claude-sonnet-4.5", "system note", []map[string]any{
		{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "private plan"},
			map[string]any{"type": "text", "text": "visible answer"},
		}},
		{"role": "user", "content": "continue"},
	}, nil, &anthropicThinkingInput{Type: "adaptive", Effort: "medium"}, nil)

	state := payload["conversationState"].(map[string]any)                                   //nolint:errcheck
	history := state["history"].([]any)                                                      //nolint:errcheck
	assistant := history[1].(map[string]any)["assistantResponseMessage"].(map[string]any)    //nolint:errcheck
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any) //nolint:errcheck

	require.Equal(t, "<thinking>private plan</thinking>\n\nvisible answer", assistant["content"])
	require.Contains(t, current["content"], "<thinking_mode>adaptive</thinking_mode><thinking_effort>medium</thinking_effort>")
	require.Contains(t, current["content"], "continue")
}

func TestKiroTextToAnthropicBlocksParsesThinkingTags(t *testing.T) {
	blocks := kiroTextToAnthropicBlocks("<thinking>\nprivate plan</thinking>\n\nvisible answer")

	require.Len(t, blocks, 2)
	require.Equal(t, "thinking", blocks[0]["type"])
	require.Equal(t, "private plan", blocks[0]["thinking"])
	require.Equal(t, "text", blocks[1]["type"])
	require.Equal(t, "visible answer", blocks[1]["text"])
}

func TestRestoreKiroToolCallsRestoresOriginalNames(t *testing.T) {
	longName := "mcp__very_long_namespace__very_long_server_name__tool_with_a_name_far_beyond_sixty_four_characters"
	maps := buildKiroToolNameMaps([]map[string]any{{
		"name":         longName,
		"description":  "desc",
		"input_schema": map[string]any{"type": "object"},
	}})

	alias := kiroToolNameToKiro(longName, maps)
	restored := restoreKiroToolCalls([]kiroToolCall{{
		ID:    "toolu_1",
		Name:  alias,
		Input: map[string]any{"command": "ls"},
	}}, maps)

	require.Equal(t, longName, restored[0].Name)
}

func TestStreamKiroToAnthropicNormalizesThinkingAndToolUse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	streamKiroToAnthropic(
		c,
		strings.NewReader("<thinking>private plan</thinking>\n\n<tool_use><name>Bash</name><input>{\"command\":\"pwd\"}</input></tool_use>"),
		"text/plain",
		"claude-sonnet-4-5",
		nil,
		&anthropicThinkingInput{Type: "enabled", BudgetTokens: 4096},
	)

	body := rec.Body.String()
	require.Contains(t, body, "event: message_start")
	require.Contains(t, body, `"type":"thinking"`)
	require.Contains(t, body, `"type":"thinking_delta"`)
	require.Contains(t, body, `"type":"tool_use"`)
	require.Contains(t, body, `"partial_json"`)
	require.Contains(t, body, "pwd")
	require.Contains(t, body, `"stop_reason":"tool_use"`)
	require.NotContains(t, body, "<tool_use>")
}

func TestCollectKiroResultSniffsEventStreamWithoutContentType(t *testing.T) {
	frame := buildKiroEventStreamFrame("chunk", []byte(`{"content":"hello"}`))

	content, calls := collectKiroResult(bytes.NewReader(frame), "application/octet-stream", nil)

	require.Equal(t, "hello", content)
	require.Empty(t, calls)
}

func TestCollectKiroResultParsesKiroEventTypes(t *testing.T) {
	var stream bytes.Buffer
	_, _ = stream.Write(buildKiroEventStreamFrame("contentEvent", []byte(`{"content":"before "}`)))
	_, _ = stream.Write(buildKiroEventStreamFrame("toolUseEvent", []byte(`{"name":"Glob","toolUseId":"toolu_1","input":"{\"pattern\":\"*\"}","stop":true}`)))
	_, _ = stream.Write(buildKiroEventStreamFrame("contextUsageEvent", []byte(`{"contextUsagePercentage":3.03}`)))

	content, calls := collectKiroResult(bytes.NewReader(stream.Bytes()), "application/octet-stream", nil)

	require.Equal(t, "before", content)
	require.Len(t, calls, 1)
	require.Equal(t, "toolu_1", calls[0].ID)
	require.Equal(t, "Glob", calls[0].Name)
	require.Equal(t, "*", calls[0].Input.(map[string]any)["pattern"]) //nolint:errcheck
}

func TestStreamKiroToAnthropicParsesKiroToolUseEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var stream bytes.Buffer
	_, _ = stream.Write(buildKiroEventStreamFrame("contentEvent", []byte(`{"content":"I will inspect it."}`)))
	_, _ = stream.Write(buildKiroEventStreamFrame("toolUseEvent", []byte(`{"name":"Glob","toolUseId":"toolu_1","input":"{\"pattern\":\"*\"}","stop":true}`)))

	streamKiroToAnthropic(c, bytes.NewReader(stream.Bytes()), "application/octet-stream", "claude-sonnet-4-5", nil, nil)

	body := rec.Body.String()
	require.Contains(t, body, `"type":"tool_use"`)
	require.Contains(t, body, `"name":"Glob"`)
	require.Contains(t, body, `"partial_json":"{\"pattern\":\"*\"}"`)
	require.NotContains(t, body, ":event-type")
	require.NotContains(t, body, "toolUseEvent")
}

func TestStreamKiroToOpenAISniffsEventStreamWithoutContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	frame := buildKiroEventStreamFrame("chunk", []byte(`{"content":"hello"}`))

	streamKiroToOpenAI(c, bytes.NewReader(frame), "application/octet-stream", "claude-sonnet-4-5", nil)

	body := rec.Body.String()
	require.Contains(t, body, `"content":"hello"`)
	require.NotContains(t, body, ":event-type")
	require.NotContains(t, body, "contextUsageEvent")
}

func TestKiroResolveModelAliases(t *testing.T) {
	require.Equal(t, "auto", kiroResolveModel("auto-kiro"))
	require.Equal(t, "claude-haiku-4.5", kiroResolveModel("claude-haiku-4-5-latest"))
	require.Equal(t, "CLAUDE_3_7_SONNET_20250219_V1_0", kiroResolveModel("claude-3-7-sonnet-20250219"))
	require.Equal(t, "claude-opus-4.5", kiroResolveModel("claude-4.5-opus-high"))
	require.Equal(t, "claude-sonnet-4.5", kiroResolveModel("claude-sonnet-4-5-20250929"))
	require.Equal(t, "claude-opus-4.5", kiroResolveModel("us.anthropic.claude-opus-4-5-20251101-v1:0"))
	require.Equal(t, "claude-opus-4.7", kiroResolveModel("claude-opus-4-7"))
}

func TestKiroTestModelFallsBackForUnsupportedClaudeModels(t *testing.T) {
	require.Equal(t, "auto-kiro", kiroTestModel(""))
	require.Equal(t, "claude-opus-4.7", kiroTestModel("claude-opus-4-7"))
	require.Equal(t, "claude-sonnet-4.5", kiroTestModel("claude-sonnet-4-5-20250929"))
	require.Equal(t, "claude-opus-4.5", kiroTestModel("us.anthropic.claude-opus-4-5-20251101-v1:0"))
	require.Equal(t, "auto-kiro", kiroTestModel("claude-unknown-5"))
}

func TestKiroDefaultModelsUseKiroModelIDs(t *testing.T) {
	var ids []string
	for _, model := range KiroDefaultModels() {
		ids = append(ids, model.ID)
	}

	require.Contains(t, ids, "auto-kiro")
	require.Contains(t, ids, "claude-sonnet-4.5")
	require.Contains(t, ids, "claude-opus-4.7")
	require.Contains(t, ids, "claude-opus-4.6")
	require.NotContains(t, ids, "claude-opus-4-7")
	require.NotContains(t, ids, "claude-sonnet-4-6")
}

func TestKiroListAccountsHydratesSchedulerSnapshotAccount(t *testing.T) {
	groupID := int64(7)
	cache := &kiroHydrationCacheStub{
		snapshot: []*Account{{
			ID:          12,
			Platform:    PlatformKiro,
			Type:        AccountTypeKiro,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Credentials: map[string]any{
				"auth_type": KiroAuthDesktop,
			},
		}},
		accounts: map[int64]*Account{
			12: {
				ID:          12,
				Platform:    PlatformKiro,
				Type:        AccountTypeKiro,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Credentials: map[string]any{
					"auth_type":     KiroAuthDesktop,
					"refresh_token": "kiro-refresh-token",
					"access_token":  "kiro-access-token",
				},
			},
		},
	}
	snapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	gateway := NewKiroGatewayService(nil, snapshot, nil, nil)

	accounts, err := gateway.listAccounts(context.Background(), &groupID)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "kiro-refresh-token", accounts[0].GetCredential("refresh_token"))
	require.Equal(t, "kiro-access-token", accounts[0].GetCredential("access_token"))
}
