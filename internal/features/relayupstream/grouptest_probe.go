package relayupstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SPHINX998/degrade-check/internal/pkg/claude"
	"github.com/SPHINX998/degrade-check/internal/pkg/openai"
)

// The endpoint a group test sends a model to: what a customer of that model
// would use, in the shape of the official account test.
const (
	EndpointMessages  = "messages"
	EndpointResponses = "responses"
	EndpointChat      = "chat"
)

const (
	// shortTestPrompt is asked of a model no fingerprint bank knows: an answer
	// shows it serves at all, and what it says it is.
	shortTestPrompt  = "用一句话介绍你自己，并说出你的模型名称。"
	claudeCodePrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	// probeResends is how many times a request that got no HTTP answer at all
	// (a dropped connection) is sent again; any HTTP answer is the result.
	probeResends     = 2
	excerptRunes     = 200
	modelListTimeout = 30 * time.Second
)

var probeResendPause = 3 * time.Second

// TestTokens is what one request used. Input leaves out cache reads, whichever
// way the endpoint reports them.
type TestTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

func (t TestTokens) empty() bool { return t.Input+t.Output+t.CacheRead+t.CacheWrite == 0 }

func (t *TestTokens) add(other TestTokens) {
	t.Input += other.Input
	t.Output += other.Output
	t.CacheRead += other.CacheRead
	t.CacheWrite += other.CacheWrite
}

// probeAnswer is what one test request got back.
type probeAnswer struct {
	Status        int
	Error         string
	Text          string
	ResponseModel string
	Tokens        TestTokens
	Latency       time.Duration
	// TTFB is the time to the first decoded stream event; zero when the
	// request never produced one.
	TTFB time.Duration
}

// prober sends the test requests of one key.
type prober struct {
	observe   func(*http.Request, *http.Response, error)
	doer      Doer
	base      string
	key       string
	timeout   time.Duration
	maxTokens int
	headers   map[string]string
}

// endpointFor sends Claude models to messages, GPT models to responses and
// the rest to chat completions.
func endpointFor(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "claude"):
		return EndpointMessages
	case strings.HasPrefix(lower, "gpt-"), strings.HasPrefix(lower, "codex"):
		return EndpointResponses
	default:
		return EndpointChat
	}
}

// untestable names models a text prompt cannot test: images, video, audio,
// embeddings and the like.
func untestable(model string) bool {
	lower := strings.ToLower(model)
	for _, part := range []string{"image", "dall-e", "sora", "video", "veo", "seedance", "imagen", "banana", "imagine", "embedding", "tts", "whisper", "audio", "realtime", "moderation", "rerank"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// ask sends one prompt, again when the connection dropped before any answer.
func (p prober) ask(ctx context.Context, endpoint, model, prompt string) probeAnswer {
	var answer probeAnswer
	for try := 0; try <= probeResends; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return answer
			case <-time.After(probeResendPause):
			}
		}
		answer = p.once(ctx, endpoint, model, []probeTurn{{Role: "user", Text: prompt}}, "")
		if answer.Status != 0 || ctx.Err() != nil {
			break
		}
	}
	return answer
}

// probeTurn is one message of a conversation a probe replays.
type probeTurn struct {
	Role string // user or assistant
	Text string
}

// converse sends a conversation the way a coding client continues one: the
// Claude request marks its last message for the cache, the responses request
// carries cacheKey as its prompt cache key.
func (p prober) converse(ctx context.Context, endpoint, model string, turns []probeTurn, cacheKey string) probeAnswer {
	return p.once(ctx, endpoint, model, turns, cacheKey)
}

func messagesOf(turns []probeTurn, cache bool) []map[string]any {
	out := make([]map[string]any, 0, len(turns))
	for i, turn := range turns {
		if cache && i == len(turns)-1 {
			out = append(out, map[string]any{"role": turn.Role, "content": []map[string]any{{"type": "text", "text": turn.Text,
				"cache_control": map[string]string{"type": "ephemeral"}}}})
			continue
		}
		out = append(out, map[string]any{"role": turn.Role, "content": turn.Text})
	}
	return out
}

func responsesInput(turns []probeTurn) []map[string]any {
	out := make([]map[string]any, 0, len(turns))
	for _, turn := range turns {
		kind := "input_text"
		if turn.Role == "assistant" {
			kind = "output_text"
		}
		out = append(out, map[string]any{"role": turn.Role, "content": []map[string]any{{"type": kind, "text": turn.Text}}})
	}
	return out
}

func (p prober) once(ctx context.Context, endpoint, model string, turns []probeTurn, cacheKey string) (answer probeAnswer) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	started := time.Now()
	defer func() { answer.Latency = time.Since(started) }()
	var path string
	var body map[string]any
	headers := map[string]string{"Authorization": "Bearer " + p.key}
	switch endpoint {
	case EndpointMessages:
		path, body = "/v1/messages?beta=true", map[string]any{
			"model":    model,
			"messages": messagesOf(turns, cacheKey != ""),
			"system": []map[string]any{{"type": "text", "text": claudeCodePrompt,
				"cache_control": map[string]string{"type": "ephemeral"}}},
			"metadata":    map[string]string{"user_id": claudeUserID()},
			"max_tokens":  p.maxTokens,
			"temperature": 1,
			"stream":      true,
		}
		for key, value := range claude.DefaultHeaders() {
			headers[key] = value
		}
		headers["x-api-key"] = p.key
		headers["anthropic-version"] = "2023-06-01"
		headers["anthropic-beta"] = claude.APIKeyBetaHeader
	case EndpointResponses:
		path, body = "/v1/responses", map[string]any{
			"model":        model,
			"instructions": openai.DefaultInstructions,
			"input":        responsesInput(turns),
			"stream":       true,
		}
		if cacheKey != "" {
			body["prompt_cache_key"] = cacheKey
		}
	default:
		path, body = "/v1/chat/completions", map[string]any{
			"model":          model,
			"messages":       messagesOf(turns, false),
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"max_tokens":     p.maxTokens,
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		answer.Error = err.Error()
		return answer
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+path, bytes.NewReader(raw))
	if err != nil {
		answer.Error = err.Error()
		return answer
	}
	req.Header.Set("Content-Type", "application/json")
	if p.observe != nil {
		req.Header.Set("X-Client-Request-Id", randomKey())
	}
	for key, value := range p.headers {
		req.Header.Set(key, value)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if p.doer == nil {
		answer.Error = "http client unavailable"
		return answer
	}
	resp, err := p.doer.Do(req)
	if p.observe != nil {
		p.observe(req, resp, err)
	}
	if err != nil {
		answer.Error = "请求上游失败：" + err.Error()
		return answer
	}
	defer func() { _ = resp.Body.Close() }()
	answer.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		answer.Error = snippet(data)
		return answer
	}
	read := readMessages
	switch endpoint {
	case EndpointResponses:
		read = readResponses
	case EndpointChat:
		read = readChat
	}
	var text strings.Builder
	firstEvent := true
	err = events(resp, func(event map[string]any) {
		if firstEvent {
			firstEvent = false
			answer.TTFB = time.Since(started)
		}
		read(event, &text, &answer)
	})
	answer.Text = text.String()
	if err != nil && answer.Error == "" {
		answer.Error = "读流中断：" + err.Error()
		if ctx.Err() != nil {
			answer.Error = fmt.Sprintf("超过 %d 秒没答完", int(p.timeout/time.Second))
		}
	}
	return answer
}

// events feeds every SSE data payload to fn; a plain JSON body is fed as one
// payload marked whole.
func events(resp *http.Response, fn func(map[string]any)) error {
	body := io.LimitReader(resp.Body, maxResponseBody)
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		data, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		var doc map[string]any
		if json.Unmarshal(data, &doc) != nil {
			return errors.New("回包不是 JSON 也不是流：" + snippet(data))
		}
		doc["__whole"] = true
		fn(doc)
		return nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBody)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var doc map[string]any
		if json.Unmarshal([]byte(payload), &doc) == nil {
			fn(doc)
		}
	}
	return scanner.Err()
}

func readMessages(event map[string]any, text *strings.Builder, answer *probeAnswer) {
	usage := func(u map[string]any) {
		if v := count(u["input_tokens"]); v > 0 {
			answer.Tokens.Input = v
		}
		if v := count(u["output_tokens"]); v > 0 {
			answer.Tokens.Output = v
		}
		if v := count(u["cache_read_input_tokens"]); v > 0 {
			answer.Tokens.CacheRead = v
		}
		if v := count(u["cache_creation_input_tokens"]); v > 0 {
			answer.Tokens.CacheWrite = v
		}
	}
	if event["__whole"] == true {
		answer.ResponseModel = field(event, "model")
		for _, item := range list(event["content"]) {
			if block := object(item); block["type"] == "text" {
				text.WriteString(field(block, "text"))
			}
		}
		usage(object(event["usage"]))
		if message := field(object(event["error"]), "message"); message != "" {
			answer.Error = message
		}
		return
	}
	switch event["type"] {
	case "message_start":
		message := object(event["message"])
		answer.ResponseModel = field(message, "model")
		usage(object(message["usage"]))
	case "content_block_delta":
		if delta := object(event["delta"]); delta["type"] == "text_delta" {
			text.WriteString(field(delta, "text"))
		}
	case "message_delta":
		usage(object(event["usage"]))
	case "error":
		answer.Error = firstNonEmpty(field(object(event["error"]), "message"), "上游返回 error 事件")
	}
}

func readResponses(event map[string]any, text *strings.Builder, answer *probeAnswer) {
	finish := func(response map[string]any) {
		if response == nil {
			return
		}
		answer.ResponseModel = firstNonEmpty(field(response, "model"), answer.ResponseModel)
		if u := object(response["usage"]); u != nil {
			cached := count(object(u["input_tokens_details"])["cached_tokens"])
			answer.Tokens = TestTokens{Input: count(u["input_tokens"]) - cached, Output: count(u["output_tokens"]), CacheRead: cached}
		}
		if text.Len() == 0 {
			for _, item := range list(response["output"]) {
				for _, part := range list(object(item)["content"]) {
					if block := object(part); block["type"] == "output_text" {
						text.WriteString(field(block, "text"))
					}
				}
			}
		}
		if message := field(object(response["error"]), "message"); message != "" {
			answer.Error = message
		}
	}
	if event["__whole"] == true {
		finish(event)
		return
	}
	switch field(event, "type") {
	case "response.created":
		answer.ResponseModel = field(object(event["response"]), "model")
	case "response.output_text.delta":
		text.WriteString(field(event, "delta"))
	case "response.completed", "response.incomplete":
		finish(object(event["response"]))
	case "response.failed":
		finish(object(event["response"]))
		answer.Error = firstNonEmpty(answer.Error, "response.failed")
	case "error":
		answer.Error = firstNonEmpty(field(event, "message"), field(object(event["error"]), "message"), "上游返回 error 事件")
	}
}

func readChat(event map[string]any, text *strings.Builder, answer *probeAnswer) {
	answer.ResponseModel = firstNonEmpty(field(event, "model"), answer.ResponseModel)
	if u := object(event["usage"]); u != nil {
		cached := count(object(u["prompt_tokens_details"])["cached_tokens"])
		answer.Tokens = TestTokens{Input: count(u["prompt_tokens"]) - cached, Output: count(u["completion_tokens"]), CacheRead: cached}
	}
	for _, item := range list(event["choices"]) {
		choice := object(item)
		text.WriteString(field(object(choice["delta"]), "content"))
		text.WriteString(field(object(choice["message"]), "content"))
	}
	if message := field(object(event["error"]), "message"); message != "" {
		answer.Error = message
	}
}

// listModels reads the models the key's group sells: the OpenAI list most
// sites answer, or the Gemini one.
func listModels(ctx context.Context, doer Doer, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	if doer == nil {
		return nil, errors.New("http client unavailable")
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{Status: resp.StatusCode, Message: snippet(data)}
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil, errors.New("/v1/models 不是模型列表：" + snippet(data))
	}
	seen := map[string]bool{}
	out := []string{}
	keep := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, model := range doc.Data {
		keep(model.ID)
	}
	for _, model := range doc.Models {
		keep(strings.TrimPrefix(model.Name, "models/"))
	}
	sort.Strings(out)
	return out, nil
}

func claudeUserID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return "user_" + hex.EncodeToString(buf[:8]) + "_account__session_" + hex.EncodeToString(buf[8:])
}

func excerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > excerptRunes {
		return string(runes[:excerptRunes]) + "…"
	}
	return text
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }
func field(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func count(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		value, _ := n.Int64()
		return value
	}
	return 0
}
