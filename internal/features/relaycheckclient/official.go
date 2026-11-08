package relaycheckclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SPHINX998/degrade-check/internal/features/relayupstream"
)

// An official login is only ever used by its own CLI: this tool never reads,
// copies or refreshes the login token. A third-party caller refreshing a token
// can sign the user out, and Anthropic forbids subscription tokens outside its
// own tools.

// CommandRunner runs one CLI with the prompt on stdin in a scratch directory.
type CommandRunner func(ctx context.Context, name string, args []string, stdin string, env []string) (stdout []byte, stderr []byte, err error)

// RunCommand is the real CommandRunner.
func RunCommand(ctx context.Context, name string, args []string, stdin string, env []string) ([]byte, []byte, error) {
	dir, err := os.MkdirTemp("", "pinai-check-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// officialAnswer is what one CLI run gives back.
type officialAnswer struct {
	Text     string
	Tokens   relayupstream.TestTokens
	Latency  time.Duration
	Cost     *float64
	Quota    string
	Error    string
	Reported string // the model the CLI says answered
}

func (r *Runner) runOfficial(ctx context.Context, target Target, model string, judge *RemoteJudge, result *Result) {
	ask := func(prompt string) officialAnswer {
		if target.App == AppCodex {
			return r.askCodex(ctx, model, prompt)
		}
		return r.askClaude(ctx, model, prompt)
	}
	var combined relayupstream.PurityResult
	var cost float64
	hasCost := false
	for question := 0; question < 2; question++ {
		challenge, banked := judge.Challenge(model)
		if judge.Failure() != nil {
			return
		}
		if !banked {
			if question == 0 {
				result.Verdict = VerdictError
				result.Note = model + " 不在指纹库里，官方登录这一路不测"
			}
			return
		}
		answer := ask(challenge.Prompt)
		result.Questions++
		result.InputTokens += answer.Tokens.Input
		result.OutputTokens += answer.Tokens.Output
		result.CacheTokens += answer.Tokens.CacheRead + answer.Tokens.CacheWrite
		result.LatencyMs += answer.Latency.Milliseconds()
		if answer.Cost != nil && *answer.Cost > 0 {
			cost += *answer.Cost
			hasCost = true
		}
		if answer.Quota != "" {
			if result.QuotaBefore == "" {
				result.QuotaBefore = answer.Quota
			}
			result.QuotaAfter = answer.Quota
		}
		if answer.Error != "" {
			if question == 0 {
				result.Verdict, result.Note = VerdictError, answer.Error
				break
			}
			break
		}
		verdict := relayupstream.JudgeExternalAnswer(judge, true, challenge, answer.Text, answer.Tokens, answer.Latency)
		if question == 0 {
			combined = verdict
		} else {
			combined = relayupstream.CombineAnswers(combined, verdict)
		}
		result.Verdict, result.Attributed, result.Substitute = combined.Verdict, combined.Attributed, combined.Substitute
		result.Probability, result.OneClass, result.Note = combined.Probability, combined.OneClass, combined.Note
		if !relayupstream.Retestable(combined.Verdict) {
			break
		}
	}
	switch {
	case hasCost:
		result.Cost, result.CostBasis = &cost, CostReported
	default:
		if estimate, ok := r.estimate(model, result.InputTokens, result.OutputTokens, result.CacheTokens); ok {
			result.Cost, result.CostBasis = &estimate, CostEstimate
		}
	}
}

// cliEnv is the process environment without what would point the CLI somewhere
// other than the official login.
func cliEnv(drop ...string) []string {
	out := make([]string, 0, len(os.Environ()))
next:
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		for _, prefix := range drop {
			if strings.HasPrefix(strings.ToUpper(name), prefix) {
				continue next
			}
		}
		out = append(out, entry)
	}
	return out
}

func codexArgs(model string) []string {
	return []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules",
		"--sandbox", "read-only", "--model", model, "-"}
}

func (r *Runner) askCodex(ctx context.Context, model, prompt string) officialAnswer {
	started := time.Now()
	stdout, stderr, err := r.Exec(ctx, "codex", codexArgs(model), prompt, cliEnv("OPENAI_API_KEY", "OPENAI_BASE_URL"))
	answer := parseCodexOutput(stdout)
	answer.Latency = time.Since(started)
	if err != nil && answer.Error == "" && answer.Text == "" {
		answer.Error = "codex 没跑成功：" + tail(stderr, err)
	}
	return answer
}

// parseCodexOutput reads `codex exec --json`, one event per line.
func parseCodexOutput(stdout []byte) officialAnswer {
	var answer officialAnswer
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		event := map[string]any{}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if limits := findKey(event, "rate_limits"); limits != nil {
			if quota := describeCodexQuota(limits); quota != "" {
				answer.Quota = quota
			}
		}
		switch text(event, "type") {
		case "item.completed":
			item := child(event, "item")
			if kind := text(item, "type"); kind == "agent_message" || kind == "assistant_message" {
				answer.Text = text(item, "text")
			}
		case "turn.completed":
			usage := child(event, "usage")
			cached := intField(usage, "cached_input_tokens")
			answer.Tokens = relayupstream.TestTokens{Input: intField(usage, "input_tokens") - cached,
				Output: intField(usage, "output_tokens"), CacheRead: cached}
		case "turn.failed":
			answer.Error = firstText(text(child(event, "error"), "message"), "codex 这一轮失败")
		case "error":
			answer.Error = firstText(text(event, "message"), answer.Error)
		}
	}
	return answer
}

// describeCodexQuota says how much of the subscription windows are used.
func describeCodexQuota(limits any) string {
	doc, ok := limits.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, window := range []struct{ key, label string }{{"primary", "5 小时"}, {"secondary", "每周"}} {
		if entry := child(doc, window.key); entry != nil {
			if used, ok := entry["used_percent"].(float64); ok {
				parts = append(parts, fmt.Sprintf("%s已用 %.0f%%", window.label, used))
			}
		}
	}
	return strings.Join(parts, "，")
}

func findKey(doc map[string]any, key string) any {
	if value, ok := doc[key]; ok {
		return value
	}
	for _, value := range doc {
		if nested, ok := value.(map[string]any); ok {
			if found := findKey(nested, key); found != nil {
				return found
			}
		}
	}
	return nil
}

func intField(doc map[string]any, key string) int64 {
	value, _ := doc[key].(float64)
	return int64(value)
}

func claudeArgs(model string) []string {
	return []string{"-p", "--output-format", "json", "--no-session-persistence", "--model", model,
		"--tools", "", "--strict-mcp-config", "--setting-sources", "local",
		"--system-prompt", relayupstream.ClaudeCodeSystemPrompt}
}

func (r *Runner) askClaude(ctx context.Context, model, prompt string) officialAnswer {
	started := time.Now()
	env := append(cliEnv("ANTHROPIC_", "CLAUDE_CODE_MAX_OUTPUT"), fmt.Sprintf("CLAUDE_CODE_MAX_OUTPUT_TOKENS=%d", probeMaxTokens))
	stdout, stderr, err := r.Exec(ctx, "claude", claudeArgs(model), prompt, env)
	answer := parseClaudeOutput(stdout)
	answer.Latency = time.Since(started)
	if err != nil && answer.Error == "" && answer.Text == "" {
		answer.Error = "claude 没跑成功：" + tail(stderr, err)
	}
	return answer
}

// parseClaudeOutput reads `claude -p --output-format json`.
func parseClaudeOutput(stdout []byte) officialAnswer {
	var doc struct {
		Result       string   `json:"result"`
		IsError      bool     `json:"is_error"`
		TotalCostUSD *float64 `json:"total_cost_usd"`
		Usage        struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	var answer officialAnswer
	if json.Unmarshal(bytes.TrimSpace(stdout), &doc) != nil {
		return answer
	}
	answer.Tokens = relayupstream.TestTokens{Input: doc.Usage.Input, Output: doc.Usage.Output,
		CacheRead: doc.Usage.CacheRead, CacheWrite: doc.Usage.CacheWrite}
	answer.Cost = doc.TotalCostUSD
	if doc.IsError {
		answer.Error = firstText(doc.Result, "claude 返回错误")
		if strings.Contains(answer.Error, "authenticate") {
			answer.Error = "Claude 官方登录已失效，请先运行 claude auth login 重新登录"
		}
		return answer
	}
	answer.Text = doc.Result
	return answer
}

func tail(stderr []byte, err error) string {
	message := strings.TrimSpace(string(stderr))
	if len(message) > 300 {
		message = message[len(message)-300:]
	}
	return firstText(message, err.Error())
}
