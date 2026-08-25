package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLegacyAccountPoolAcceptsNumericCooldownUntil(t *testing.T) {
	milliseconds := int64(1767225600000)
	data := []byte(`{
		"accounts": [{
			"accountId": "account-1",
			"email": "test@example.com",
			"refreshToken": "refresh-token",
			"status": "cooldown",
			"cooldownUntil": 1767225600000,
			"lastUsed": "2026-01-01T00:00:00Z",
			"createdAt": "2026-01-01T00:00:00Z"
		}],
		"currentIdx": 0,
		"keys": ["key-1"]
	}`)

	var pool legacyAccountPool
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("unmarshal legacy account pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(pool.Accounts))
	}

	cooldownUntil, err := parseLegacyCooldownUntil(pool.Accounts[0].CooldownUntil)
	if err != nil {
		t.Fatalf("parse numeric cooldownUntil: %v", err)
	}
	if cooldownUntil.UnixMilli() != milliseconds {
		t.Fatalf("expected cooldownUntil %d, got %d", milliseconds, cooldownUntil.UnixMilli())
	}
}

func TestApplyRecommendedModelsMarksMissingModelsRemoved(t *testing.T) {
	modelsMu.Lock()
	previousCache := modelsCache
	previousLastSync := modelsLastSync
	previousSyncSucceeded := freeModelsSyncSucceeded
	modelsCache = map[string]*ModelInfo{
		"provider/removed": {ID: "provider/removed", Status: ModelActive, Source: "free"},
		"provider/active":  {ID: "provider/active", Status: ModelActive, Source: "free"},
	}
	modelsLastSync = time.Time{}
	freeModelsSyncSucceeded = false
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsCache = previousCache
		modelsLastSync = previousLastSync
		freeModelsSyncSucceeded = previousSyncSucceeded
		modelsMu.Unlock()
	})

	var payload recommendedPayload
	if err := json.Unmarshal([]byte(`{"free":[{"id":"provider/active","name":"Active"},{"id":"provider/new","name":"New"}]}`), &payload); err != nil {
		t.Fatalf("unmarshal recommended payload: %v", err)
	}
	if added := applyRecommendedModels(payload); added != 1 {
		t.Fatalf("expected 1 newly added model, got %d", added)
	}

	modelsMu.Lock()
	removedStatus := modelsCache["provider/removed"].Status
	activeStatus := modelsCache["provider/active"].Status
	newStatus := modelsCache["provider/new"].Status
	syncSucceeded := freeModelsSyncSucceeded
	modelsMu.Unlock()

	if removedStatus != ModelRemoved {
		t.Fatalf("expected missing model to be removed, got %q", removedStatus)
	}
	if activeStatus != ModelActive || newStatus != ModelActive {
		t.Fatalf("expected current models to stay active, got active=%q new=%q", activeStatus, newStatus)
	}
	if !syncSucceeded {
		t.Fatal("expected successful payload application to mark the free model cache ready")
	}
}

func TestSeedModelsDoNotMarkFreeModelsReady(t *testing.T) {
	modelsMu.Lock()
	previousCache := modelsCache
	previousSyncSucceeded := freeModelsSyncSucceeded
	modelsCache = nil
	freeModelsSyncSucceeded = false
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsCache = previousCache
		freeModelsSyncSucceeded = previousSyncSucceeded
		modelsMu.Unlock()
	})

	if freeModelsReady() {
		t.Fatal("seed model candidates must not mark the official free model sync as ready")
	}
}

func TestCollectStreamResponseAggregatesInterleavedToolCallsByIndex(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"model":"model-a","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-0","function":{"name":"first","arguments":"{\"a\":"}},{"index":1,"id":"call-1","function":{"name":"second","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	response := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}

	collected, err := collectStreamResponse(response)
	if err != nil {
		t.Fatalf("collect stream response: %v", err)
	}
	toolCalls, ok := getNested(collected, "choices", 0, "message", "tool_calls").([]any)
	if !ok {
		t.Fatalf("expected tool_calls array, got %#v", getNested(collected, "choices", 0, "message", "tool_calls"))
	}
	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(toolCalls))
	}

	assertToolCall := func(index int, expectedID, expectedName, expectedArguments string) {
		t.Helper()
		toolCall, ok := toolCalls[index].(map[string]any)
		if !ok {
			t.Fatalf("tool call %d has unexpected type %T", index, toolCalls[index])
		}
		function, ok := toolCall["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool call %d function has unexpected type %T", index, toolCall["function"])
		}
		if toolCall["id"] != expectedID || function["name"] != expectedName || function["arguments"] != expectedArguments {
			t.Fatalf("tool call %d mismatch: %#v", index, toolCall)
		}
	}

	assertToolCall(0, "call-0", "first", `{"a":1}`)
	assertToolCall(1, "call-1", "second", `{"b":2}`)
}

var errInterruptedStream = errors.New("interrupted stream")

type interruptedStreamReader struct {
	data string
	sent bool
}

func (reader *interruptedStreamReader) Read(buffer []byte) (int, error) {
	if !reader.sent {
		reader.sent = true
		return copy(buffer, reader.data), nil
	}
	return 0, errInterruptedStream
}

type readerCloser struct {
	io.Reader
}

func (readerCloser) Close() error { return nil }

func TestCollectStreamResponseReturnsReadError(t *testing.T) {
	response := &http.Response{Body: readerCloser{Reader: &interruptedStreamReader{
		data: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n",
	}}}

	_, err := collectStreamResponse(response)
	if !errors.Is(err, errInterruptedStream) {
		t.Fatalf("expected interrupted stream error, got %v", err)
	}
}

func TestLoadPoolResetsAndPersistsStaleDailyUsage(t *testing.T) {
	previousDB := statsDB
	databasePath := filepath.Join(t.TempDir(), "stats.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	statsDB = database
	t.Cleanup(func() {
		statsDB = previousDB
		database.Close()
	})

	schema := []string{
		`CREATE TABLE accounts (
			account_id TEXT PRIMARY KEY, email TEXT, refresh_token TEXT, access_token TEXT,
			expires_at INTEGER, status TEXT, cooldown_until INTEGER, fail_count INTEGER,
			usage_count INTEGER, usage_count_today INTEGER, usage_date TEXT,
			last_used INTEGER, created_at INTEGER, last_reason TEXT,
			tokens_total INTEGER NOT NULL DEFAULT 0,
			tokens_today INTEGER NOT NULL DEFAULT 0,
			tokens_date TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE api_keys (key TEXT PRIMARY KEY, created_at INTEGER)`,
		`CREATE TABLE proxy_state (id INTEGER PRIMARY KEY, current_idx INTEGER)`,
		`INSERT INTO proxy_state(id, current_idx) VALUES(1, 0)`,
	}
	for _, statement := range schema {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create test schema: %v", err)
		}
	}

	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	if _, err := database.Exec(`INSERT INTO accounts
		(account_id, email, refresh_token, access_token, expires_at, status, cooldown_until,
		 fail_count, usage_count, usage_count_today, usage_date, last_used, created_at, last_reason,
		 tokens_total, tokens_today, tokens_date)
		VALUES ('account-1', 'test@example.com', '', '', 0, 'active', 0, 0, 12, 7, ?, 0, 0, '', 0, 0, ?)`, yesterday, yesterday); err != nil {
		t.Fatalf("insert test account: %v", err)
	}

	pool := loadPool()
	if len(pool.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(pool.Accounts))
	}
	today := time.Now().Format("2006-01-02")
	if pool.Accounts[0].UsageCountToday != 0 || pool.Accounts[0].UsageDate != today {
		t.Fatalf("daily usage was not reset in memory: count=%d date=%q",
			pool.Accounts[0].UsageCountToday, pool.Accounts[0].UsageDate)
	}

	var persistedCount int64
	var persistedDate string
	if err := database.QueryRow(`SELECT usage_count_today, usage_date FROM accounts WHERE account_id='account-1'`).Scan(
		&persistedCount, &persistedDate); err != nil {
		t.Fatalf("read persisted daily usage: %v", err)
	}
	if persistedCount != 0 || persistedDate != today {
		t.Fatalf("daily usage was not persisted: count=%d date=%q", persistedCount, persistedDate)
	}
}

func TestDefaultModelConcurrentReadWrite(t *testing.T) {
	modelsMu.Lock()
	previousCache := modelsCache
	previousDefaultModel := defaultModel
	modelsCache = map[string]*ModelInfo{
		"provider/a": {ID: "provider/a", Status: ModelActive},
		"provider/b": {ID: "provider/b", Status: ModelActive},
	}
	defaultModel = "provider/a"
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsCache = previousCache
		defaultModel = previousDefaultModel
		modelsMu.Unlock()
	})

	var waitGroup sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		waitGroup.Add(1)
		go func(worker int) {
			defer waitGroup.Done()
			for iteration := 0; iteration < 1000; iteration++ {
				if (worker+iteration)%2 == 0 {
					setDefaultModel("provider/a")
				} else {
					setDefaultModel("provider/b")
				}
				_ = getDefaultModel()
			}
		}(worker)
	}
	waitGroup.Wait()
}
