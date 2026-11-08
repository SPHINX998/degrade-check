//go:build unit

package relaycheckclient

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, vars map[string]string, cli ...string) Env {
	t.Helper()
	return Env{
		Home:   t.TempDir(),
		Getenv: func(name string) string { return vars[name] },
		LookPath: func(name string) (string, error) {
			for _, have := range cli {
				if have == name {
					return name, nil
				}
			}
			return "", exec.ErrNotFound
		},
	}
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func only(t *testing.T, targets []Target, app string) Target {
	t.Helper()
	var found []Target
	for _, target := range targets {
		if target.App == app {
			found = append(found, target)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s target, got %+v", app, found)
	}
	return found[0]
}

func TestScanCodexKeyForms(t *testing.T) {
	cases := map[string]struct {
		config, auth string
		vars         map[string]string
		wantKey      string
		wantBase     string
	}{
		"inline bearer": {
			config:  "model = \"gpt-6-sol\"\nmodel_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://r.example.com/v1\"\nwire_api = \"responses\"\nexperimental_bearer_token = \"sk-inline-1234\"\nhttp_headers = { \"x-a\" = \"b\" }\n",
			wantKey: "sk-inline-1234", wantBase: "https://r.example.com",
		},
		"env key": {
			config:  "model_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://r.example.com\"\nenv_key = \"RELAY_KEY\"\n",
			vars:    map[string]string{"RELAY_KEY": "sk-env-5678"},
			wantKey: "sk-env-5678", wantBase: "https://r.example.com",
		},
		"auth.json key with openai_base_url": {
			config:  "openai_base_url = \"https://r.example.com/v1/\"\n",
			auth:    `{"OPENAI_API_KEY":"sk-auth-9012"}`,
			wantKey: "sk-auth-9012", wantBase: "https://r.example.com",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := fixture(t, tc.vars)
			put(t, filepath.Join(env.Home, ".codex", "config.toml"), tc.config)
			if tc.auth != "" {
				put(t, filepath.Join(env.Home, ".codex", "auth.json"), tc.auth)
			}
			target := only(t, Scan(env), AppCodex)
			if target.apiKey != tc.wantKey || target.baseURL != tc.wantBase || !target.Testable() {
				t.Fatalf("got key=%q base=%q problem=%q", target.apiKey, target.baseURL, target.Problem)
			}
		})
	}
}

func TestScanCodexCarriesHeadersAndHidesKeyFromJSON(t *testing.T) {
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".codex", "config.toml"), "model_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://r.example.com\"\nexperimental_bearer_token = \"sk-secret-value-1234\"\nhttp_headers = { \"x-a\" = \"b\" }\n")
	target := only(t, Scan(env), AppCodex)
	if target.headers["x-a"] != "b" {
		t.Fatalf("headers: %+v", target.headers)
	}
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-secret-value") || !strings.Contains(string(raw), "…1234") {
		t.Fatalf("the page must only see a hint of the key: %s", raw)
	}
}

func TestScanOfficialLogin(t *testing.T) {
	env := fixture(t, nil, "codex", "claude")
	put(t, filepath.Join(env.Home, ".codex", "auth.json"), `{"auth_mode":"chatgpt","tokens":{"access_token":"at-secret"}}`)
	put(t, filepath.Join(env.Home, ".claude", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"at-secret-2"}}`)
	targets := Scan(env)
	codex, claude := only(t, targets, AppCodex), only(t, targets, AppClaude)
	if codex.Mode != ModeOfficial || !codex.Testable() || codex.Model != defaultCodexModel {
		t.Fatalf("codex: %+v", codex)
	}
	if claude.Mode != ModeOfficial || !claude.Testable() {
		t.Fatalf("claude: %+v", claude)
	}
	if codex.apiKey != "" || claude.apiKey != "" {
		t.Fatal("an official login token must never be read into a target")
	}
}

func officialRow(targets []Target, app string) *Target {
	for i := range targets {
		if targets[i].App == app && targets[i].Mode == ModeOfficial {
			return &targets[i]
		}
	}
	return nil
}

func TestOfficialLoginIsListedNextToAConfiguredKey(t *testing.T) {
	env := fixture(t, nil, "codex", "claude")
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-relay-1111","ANTHROPIC_BASE_URL":"https://a.example.com"}}`)
	put(t, filepath.Join(env.Home, ".claude", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"at-secret"}}`)
	put(t, filepath.Join(env.Home, ".codex", "config.toml"), `model_provider = "x"
[model_providers.x]
base_url = "https://b.example.com"
experimental_bearer_token = "sk-relay-2222"
`)
	put(t, filepath.Join(env.Home, ".codex", "auth.json"), `{"auth_mode":"chatgpt","tokens":{"access_token":"at-secret-2"}}`)
	targets := Scan(env)
	for _, app := range []string{AppClaude, AppCodex} {
		official := officialRow(targets, app)
		if official == nil || !official.Testable() || official.Current || official.apiKey != "" {
			t.Fatalf("%s official row: %+v in %+v", app, official, targets)
		}
	}
	if len(targets) != 4 {
		t.Fatalf("both keys and both logins: %+v", targets)
	}
	if picked := selected(targets, Settings{}); len(picked) != 2 || picked[0].Mode != ModeAPIKey || picked[1].Mode != ModeAPIKey {
		t.Fatalf("a login must not be tested by default next to the key in use: %+v", picked)
	}
}

func TestClaudeLoginInKeychainIsFoundByAccountRecord(t *testing.T) {
	env := fixture(t, nil, "claude")
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-relay-1111","ANTHROPIC_BASE_URL":"https://a.example.com"}}`)
	put(t, filepath.Join(env.Home, ".claude.json"), `{"oauthAccount":{"emailAddress":"x@example.com"}}`)
	if officialRow(Scan(env), AppClaude) == nil {
		t.Fatal("no row for a login kept in the keychain")
	}
}

func TestNoLoginNoExtraRow(t *testing.T) {
	env := fixture(t, nil, "claude", "codex")
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-relay-1111","ANTHROPIC_BASE_URL":"https://a.example.com"}}`)
	put(t, filepath.Join(env.Home, ".codex", "auth.json"), `{"OPENAI_API_KEY":"sk-relay-3333"}`)
	targets := Scan(env)
	if officialRow(targets, AppClaude) != nil || officialRow(targets, AppCodex) != nil {
		t.Fatalf("%+v", targets)
	}
}

func TestScanOfficialLoginWithoutCLIIsNotTestable(t *testing.T) {
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".codex", "auth.json"), `{"auth_mode":"chatgpt"}`)
	put(t, filepath.Join(env.Home, ".claude", ".credentials.json"), `{}`)
	targets := Scan(env)
	if only(t, targets, AppCodex).Testable() || only(t, targets, AppClaude).Testable() {
		t.Fatalf("no CLI means no test: %+v", targets)
	}
}

func TestScanClaudeSettingsWinOverEnvironment(t *testing.T) {
	env := fixture(t, map[string]string{"ANTHROPIC_AUTH_TOKEN": "sk-env-0000", "ANTHROPIC_BASE_URL": "https://env.example.com"})
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-file-1111","ANTHROPIC_BASE_URL":"https://file.example.com","ANTHROPIC_MODEL":"claude-opus-5-5[1m]"}}`)
	target := only(t, Scan(env), AppClaude)
	if target.apiKey != "sk-file-1111" || target.Host != "file.example.com" || target.Model != "claude-opus-5-5" {
		t.Fatalf("%+v", target)
	}
}

func TestScanBrokenAndMissingConfig(t *testing.T) {
	env := fixture(t, nil)
	if got := Scan(env); len(got) != 0 {
		t.Fatalf("nothing configured means no rows: %+v", got)
	}
	put(t, filepath.Join(env.Home, ".codex", "config.toml"), "model_provider = [broken")
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), "{broken")
	targets := Scan(env)
	if only(t, targets, AppCodex).Testable() || only(t, targets, AppClaude).Testable() {
		t.Fatalf("a broken file must show as a problem: %+v", targets)
	}
}

func seedCCSwitch(t *testing.T, env Env, rows ...[4]any) {
	t.Helper()
	path := filepath.Join(env.Home, ".cc-switch", "cc-switch.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, is_current BOOLEAN, sort_index INTEGER, created_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if _, err := db.Exec(`INSERT INTO providers VALUES (?,?,?,?,?,?,?)`, fmt.Sprint(i), row[0], row[1], row[2], row[3], i, i); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanCCSwitchListsAllProvidersAndMergesLive(t *testing.T) {
	env := fixture(t, nil)
	live := `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-current-1111","ANTHROPIC_BASE_URL":"https://a.example.com"}}`
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), live)
	seedCCSwitch(t, env,
		[4]any{"claude", "站 A", live, true},
		[4]any{"claude", "站 B", `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-other-2222","ANTHROPIC_BASE_URL":"https://b.example.com"}}`, false},
		[4]any{"claude", "官方", `{"env":{}}`, false},
		[4]any{"codex", "站 C", `{"auth":{"OPENAI_API_KEY":"sk-codex-3333"},"config":"model = \"gpt-6-sol\"\nmodel_provider = \"x\"\n[model_providers.x]\nbase_url = \"https://c.example.com/v1\"\nwire_api = \"responses\"\n"}`, true},
	)
	targets := Scan(env)
	if len(targets) != 3 {
		t.Fatalf("want A, B and C (official row is not a provider key): %+v", targets)
	}
	for _, target := range targets {
		switch target.Host {
		case "a.example.com":
			if target.Name != "站 A" || target.Source != "本机配置 + CC Switch" || !target.Current {
				t.Errorf("merged row: %+v", target)
			}
		case "b.example.com":
			if target.Current {
				t.Errorf("B is not in use: %+v", target)
			}
		case "c.example.com":
			if target.apiKey != "sk-codex-3333" || target.Model != "gpt-6-sol" {
				t.Errorf("C: %+v", target)
			}
		default:
			t.Errorf("unexpected: %+v", target)
		}
	}
}

func TestScanCCSwitchRouterModeUsesRealKey(t *testing.T) {
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"PROXY_MANAGED","ANTHROPIC_BASE_URL":"http://127.0.0.1:15721"}}`)
	seedCCSwitch(t, env, [4]any{"claude", "真实站", `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-real-4444","ANTHROPIC_BASE_URL":"https://real.example.com"}}`, true})
	target := only(t, Scan(env), AppClaude)
	if target.apiKey != "sk-real-4444" || target.Host != "real.example.com" || !target.Testable() {
		t.Fatalf("%+v", target)
	}
}

func TestScanRouterModeWithoutCCSwitchRowSaysSo(t *testing.T) {
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"PROXY_MANAGED","ANTHROPIC_BASE_URL":"http://127.0.0.1:15721"}}`)
	if target := only(t, Scan(env), AppClaude); target.Testable() || !strings.Contains(target.Problem, "CC Switch") {
		t.Fatalf("%+v", target)
	}
}

func TestScanCorruptCCSwitchFallsBackToLiveFiles(t *testing.T) {
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".cc-switch", "cc-switch.db"), "not a database")
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-file-1111","ANTHROPIC_BASE_URL":"https://f.example.com"}}`)
	if target := only(t, Scan(env), AppClaude); !target.Testable() {
		t.Fatalf("%+v", target)
	}
}

func TestSelfAndInsecureFlags(t *testing.T) {
	self := Target{App: AppClaude, Mode: ModeAPIKey, apiKey: "sk-12345678", baseURL: "https://api.pinaic.com/"}.finish()
	if !self.Self || self.Insecure {
		t.Fatalf("%+v", self)
	}
	other := Target{App: AppClaude, Mode: ModeAPIKey, apiKey: "sk-12345678", baseURL: "http://notpinaic.com"}.finish()
	if other.Self || !other.Insecure {
		t.Fatalf("%+v", other)
	}
}

// judgeServer is a fake check-api; it records everything it receives.
type judgeServer struct {
	*httptest.Server
	mu           sync.Mutex
	bodies       []string
	verdicts     []string
	failJudge    bool
	banked       bool
	uncalibrated bool
}

func newJudgeServer(t *testing.T, verdicts ...string) *judgeServer {
	t.Helper()
	j := &judgeServer{verdicts: verdicts, banked: true}
	j.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		j.mu.Lock()
		defer j.mu.Unlock()
		j.bodies = append(j.bodies, r.URL.Path+" "+body.String())
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			fmt.Fprintf(w, `{"enabled":true,"official_verdicts":%v,"prices":{"gpt-6-astra":{"input":10,"output":50}},"promo":{"title":"P","url":"https://p.example.com"}}`, !j.uncalibrated)
		case strings.HasSuffix(r.URL.Path, "/challenge"):
			fmt.Fprintf(w, `{"banked":%v,"prompt":"题目","expected":300}`, j.banked)
		default:
			if j.failJudge {
				http.Error(w, `{"error":"down"}`, http.StatusBadGateway)
				return
			}
			verdict := j.verdicts[0]
			if len(j.verdicts) > 1 {
				j.verdicts = j.verdicts[1:]
			}
			fmt.Fprintf(w, `{"verdict":%q,"attributed":"gpt-5.6-luna","probability":0.9}`, verdict)
		}
	}))
	t.Cleanup(j.Close)
	return j
}

func relayServer(t *testing.T, balances ...float64) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []*http.Request
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/usage":
			mu.Lock()
			defer mu.Unlock()
			if len(balances) == 0 {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"remaining":%v,"unit":"USD"}`, balances[min(reads, len(balances)-1)])
			reads++
		case r.URL.Path == "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"1 2 3\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1000,\"output_tokens\":100}}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func relayTarget(base string) Target {
	return Target{App: AppCodex, Mode: ModeAPIKey, Name: "站", Model: "gpt-6-astra", Endpoint: "responses", apiKey: "sk-secret-1234", baseURL: base,
		headers: map[string]string{"x-a": "b"}}.finish()
}

func TestRelayTestUsesBalanceDeltaAndKeepsKeyFromJudge(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	relay, seen := relayServer(t, 10, 9.99)
	runner := &Runner{Server: judge.URL, Client: &http.Client{}}
	result := runner.Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != "genuine" || result.Questions != 1 {
		t.Fatalf("a plainly genuine answer needs one question: %+v", result)
	}
	if result.CostBasis != CostBalance || result.Cost == nil || *result.Cost < 0.0099 || *result.Cost > 0.0101 {
		t.Fatalf("balance fell by 0.01: %+v", result)
	}
	var carried bool
	for _, req := range *seen {
		if req.URL.Path == "/v1/responses" && req.Header.Get("x-a") == "b" && req.Header.Get("Authorization") == "Bearer sk-secret-1234" {
			carried = true
		}
		if req.URL.Path == "/v1/v1/responses" {
			t.Fatal("/v1 was doubled")
		}
	}
	if !carried {
		t.Fatal("the relay must get the configured key and headers")
	}
	for _, body := range judge.bodies {
		if strings.Contains(body, "sk-secret") || strings.Contains(body, relay.URL) || strings.Contains(body, "127.0.0.1") {
			t.Fatalf("check-api must see neither key nor relay address: %s", body)
		}
	}
}

// A busy account's balance falls far more than one test costs while it runs.
func TestBalanceFallFromOtherRequestsIsNotTheTestsCost(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	relay, _ := relayServer(t, 5279.79, 5278.66)
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.CostBasis != CostEstimate || result.Cost == nil || *result.Cost > 0.0151 || !strings.Contains(result.CostNote, "其他请求") {
		t.Fatalf("list price, and say why: %+v", result)
	}
	if result.BalanceBefore == nil || result.BalanceAfter == nil {
		t.Fatalf("the readings stay on record: %+v", result)
	}
}

func TestBalanceFallWithoutAPriceIsNotClaimedAsCost(t *testing.T) {
	spent := 1.133
	result := Result{Mode: ModeAPIKey, Model: "unpriced", InputTokens: 10, OutputTokens: 10, Cost: &spent, CostBasis: CostBalance}
	settleResultCost(&result, map[string]Price{})
	if result.Cost != nil || result.CostBasis != CostUnknown || !strings.Contains(result.CostNote, "1.133") {
		t.Fatalf("%+v", result)
	}
}

func TestSavedRecordIsCorrectedAndStopsEatingTheBudget(t *testing.T) {
	now := time.Now()
	spent := 4.99
	history := []Result{{Mode: ModeAPIKey, Model: "gpt-6-astra", Questions: 1, InputTokens: 335, OutputTokens: 978, CacheTokens: 1280,
		Cost: &spent, CostBasis: CostBalance, At: now.Format(time.RFC3339)}}
	settleHistoryCosts(history, map[string]Price{"gpt-6-astra": {Input: 10, Output: 50, CacheRead: 1}})
	if history[0].CostBasis != CostEstimate || *history[0].Cost > 0.06 {
		t.Fatalf("%+v", history[0])
	}
	if ok, why := allowScheduled(Target{Mode: ModeAPIKey}, Data{Settings: DefaultSettings(), History: history}, now); !ok {
		t.Fatalf("a corrected record must not stop the schedule: %s", why)
	}
}

func TestPlausibleBalanceFallIsKept(t *testing.T) {
	spent := 0.013
	result := Result{Mode: ModeAPIKey, Model: "gpt-6-astra", InputTokens: 335, OutputTokens: 978, CacheTokens: 1280, Cost: &spent, CostBasis: CostBalance}
	settleResultCost(&result, map[string]Price{"gpt-6-astra": {Input: 10, Output: 50, CacheRead: 1}})
	if result.CostBasis != CostBalance || *result.Cost != 0.013 || result.CostNote != "" {
		t.Fatalf("%+v", result)
	}
}

func TestOfficialCostIsLeftAlone(t *testing.T) {
	reported := 0.3
	result := Result{Mode: ModeOfficial, Model: "gpt-6-astra", Cost: &reported, CostBasis: CostReported}
	settleResultCost(&result, map[string]Price{"gpt-6-astra": {Input: 10, Output: 50}})
	if result.CostBasis != CostReported || *result.Cost != 0.3 {
		t.Fatalf("%+v", result)
	}
}

func TestRelayFallsBackToEstimateWhenBalanceUnreadable(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	relay, _ := relayServer(t)
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.CostBasis != CostEstimate || result.Cost == nil || *result.Cost < 0.0149 || *result.Cost > 0.0151 {
		t.Fatalf("1000 in and 100 out at 10/50 is $0.015: %+v", result)
	}
}

func TestRelaySuspectGetsRetest(t *testing.T) {
	judge := newJudgeServer(t, "degraded", "degraded")
	relay, _ := relayServer(t)
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != "degraded" || result.Questions != 2 {
		t.Fatalf("%+v", result)
	}
}

func TestJudgeUnreachableSpendsNothing(t *testing.T) {
	relay, seen := relayServer(t, 10)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	result := (&Runner{Server: down.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != VerdictError || result.Questions != 0 || len(*seen) != 0 {
		t.Fatalf("no judge, no request: %+v seen=%d", result, len(*seen))
	}
}

func TestJudgeFailingAfterAnswerKeepsAnswerAndRejudges(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	judge.failJudge = true
	relay, _ := relayServer(t)
	runner := &Runner{Server: judge.URL, Client: &http.Client{}}
	result := runner.Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != VerdictPending || len(result.Pending) == 0 || result.Pending[0].Text != "1 2 3" {
		t.Fatalf("a paid answer must be kept: %+v", result)
	}
	judge.mu.Lock()
	judge.failJudge = false
	judge.mu.Unlock()
	if err := runner.RejudgePending(context.Background(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "genuine" || len(result.Pending) != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestUnbankedModelOnlySaysServed(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	judge.banked = false
	relay, _ := relayServer(t)
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != "served" {
		t.Fatalf("%+v", result)
	}
}

func TestProblemTargetNeverRuns(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	target := Target{App: AppCodex, Mode: ModeAPIKey, Problem: "没配置"}
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), target, "", false)
	if result.Verdict != VerdictError || len(judge.bodies) != 0 {
		t.Fatalf("%+v %v", result, judge.bodies)
	}
}

func codexEvents(text string) string {
	return fmt.Sprintf(`{"type":"thread.started"}
{"type":"item.completed","item":{"type":"agent_message","text":%q}}
{"type":"event","msg":{"rate_limits":{"primary":{"used_percent":12},"secondary":{"used_percent":40}}}}
{"type":"turn.completed","usage":{"input_tokens":1600,"cached_input_tokens":100,"output_tokens":150}}
`, text)
}

func TestOfficialCodexUsesItsCLIAndNoToken(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	var name string
	var args, env []string
	var prompt string
	runner := &Runner{Server: judge.URL, Client: &http.Client{}, Exec: func(_ context.Context, n string, a []string, stdin string, e []string) ([]byte, []byte, error) {
		name, args, prompt, env = n, a, stdin, e
		return []byte(codexEvents("1 2 3")), nil, nil
	}}
	t.Setenv("OPENAI_API_KEY", "sk-should-be-dropped")
	target := Target{App: AppCodex, Mode: ModeOfficial, Name: "官方", Model: defaultCodexModel}.finish()
	result := runner.Run(context.Background(), target, "", false)
	if name != "codex" || prompt != "题目" || !strings.Contains(strings.Join(args, " "), "--ignore-user-config") {
		t.Fatalf("%s %v %q", name, args, prompt)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "OPENAI_API_KEY=") {
			t.Fatal("the official login run must not inherit an API key")
		}
	}
	if result.Verdict != "genuine" || result.InputTokens != 1500 || result.CacheTokens != 100 || result.OutputTokens != 150 {
		t.Fatalf("%+v", result)
	}
	if result.QuotaAfter != "5 小时已用 12%，每周已用 40%" {
		t.Fatalf("quota: %q", result.QuotaAfter)
	}
	if result.CostBasis != CostEstimate {
		t.Fatalf("cost: %+v", result)
	}
}

func TestOfficialClaudeReadsJSONAndStripsEnv(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	var env, args []string
	runner := &Runner{Server: judge.URL, Client: &http.Client{}, Exec: func(_ context.Context, _ string, a []string, _ string, e []string) ([]byte, []byte, error) {
		args, env = a, e
		return []byte(`{"result":"1 2 3","is_error":false,"total_cost_usd":0.03,"usage":{"input_tokens":400,"output_tokens":150}}`), nil, nil
	}}
	t.Setenv("ANTHROPIC_BASE_URL", "https://elsewhere.example.com")
	target := Target{App: AppClaude, Mode: ModeOfficial, Name: "官方", Model: defaultClaudeModel}.finish()
	result := runner.Run(context.Background(), target, "", false)
	if result.Verdict != "genuine" || result.CostBasis != CostReported || result.Cost == nil || *result.Cost != 0.03 {
		t.Fatalf("%+v", result)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "ANTHROPIC_") {
			t.Fatalf("must not inherit %s", entry)
		}
	}
	if !strings.Contains(strings.Join(args, " "), "--system-prompt") {
		t.Fatalf("%v", args)
	}
}

func TestOfficialVerdictWithheldUntilCalibrated(t *testing.T) {
	judge := newJudgeServer(t, "degraded")
	judge.uncalibrated = true
	runner := &Runner{Server: judge.URL, Client: &http.Client{}, Exec: func(context.Context, string, []string, string, []string) ([]byte, []byte, error) {
		return []byte(codexEvents("1 2 3")), nil, nil
	}}
	target := Target{App: AppCodex, Mode: ModeOfficial, Name: "官方", Model: defaultCodexModel}.finish()
	result := runner.Run(context.Background(), target, "", false)
	if result.Verdict != "served" || result.Attributed != "" || !strings.Contains(result.Note, "校准") {
		t.Fatalf("an uncalibrated path must not claim a verdict: %+v", result)
	}
	if result.InputTokens == 0 {
		t.Fatalf("what it spent still shows: %+v", result)
	}
}

func TestUncalibratedOnlyAffectsOfficial(t *testing.T) {
	judge := newJudgeServer(t, "degraded", "degraded")
	judge.uncalibrated = true
	relay, _ := relayServer(t)
	result := (&Runner{Server: judge.URL, Client: &http.Client{}}).Run(context.Background(), relayTarget(relay.URL), "", false)
	if result.Verdict != "degraded" {
		t.Fatalf("%+v", result)
	}
}

func TestExpiredClaudeLoginSaysWhatToDoAndCostsNothing(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	runner := &Runner{Server: judge.URL, Client: &http.Client{}, Exec: func(context.Context, string, []string, string, []string) ([]byte, []byte, error) {
		return []byte(`{"is_error":true,"result":"Failed to authenticate: OAuth session expired","total_cost_usd":0}`), nil, fmt.Errorf("exit 1")
	}}
	target := Target{App: AppClaude, Mode: ModeOfficial, Name: "官方", Model: defaultClaudeModel}.finish()
	result := runner.Run(context.Background(), target, "", false)
	if result.Verdict != VerdictError || !strings.Contains(result.Note, "claude auth login") || result.Cost != nil {
		t.Fatalf("%+v", result)
	}
}

func TestOfficialCLIFailureIsAnError(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	runner := &Runner{Server: judge.URL, Client: &http.Client{}, Exec: func(context.Context, string, []string, string, []string) ([]byte, []byte, error) {
		return nil, []byte("not logged in"), fmt.Errorf("exit 1")
	}}
	target := Target{App: AppCodex, Mode: ModeOfficial, Name: "官方", Model: defaultCodexModel}.finish()
	result := runner.Run(context.Background(), target, "", false)
	if result.Verdict != VerdictError || !strings.Contains(result.Note, "not logged in") {
		t.Fatalf("%+v", result)
	}
}

type plainSealer struct{ fail bool }

func (s plainSealer) Seal(key []byte) ([]byte, error)      { return key, nil }
func (s plainSealer) Unseal(sealed []byte) ([]byte, error) { return sealed, nil }
func (s plainSealer) Level() string                        { return "test" }

func TestStoreEncryptsAtRest(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir, plainSealer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func(d *Data) error {
		d.History = append(d.History, Result{TargetName: "很好认的渠道名", Host: "marker.example.com"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, dataFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("marker.example.com")) || bytes.Contains(raw, []byte("很好认")) {
		t.Fatal("data must be ciphertext on disk")
	}
	loaded, err := store.Load()
	if err != nil || len(loaded.History) != 1 || loaded.History[0].Host != "marker.example.com" {
		t.Fatalf("%v %+v", err, loaded)
	}
}

func TestStoreUnreadableFileIsSetAsideNotDeleted(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(dir, plainSealer{})
	if _, err := store.Update(func(d *Data) error { d.Settings.IntervalHours = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(dir, dataFileName), "garbage that is long enough to not be short")
	data, err := store.Load()
	if err != nil || data.Settings.IntervalHours != DefaultSettings().IntervalHours {
		t.Fatalf("%v %+v", err, data)
	}
	aside, _ := filepath.Glob(filepath.Join(dir, "data.unreadable-*"))
	if len(aside) != 1 {
		t.Fatalf("the old file must stay: %v", aside)
	}
}

func TestStoreConcurrentUpdatesAllLand(t *testing.T) {
	store, _ := openStore(t.TempDir(), plainSealer{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Update(func(d *Data) error { d.History = append(d.History, Result{}); return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, _ := store.Load()
	if len(data.History) != 8 {
		t.Fatalf("lost updates: %d", len(data.History))
	}
}

func TestStoreHistoryIsBounded(t *testing.T) {
	store, _ := openStore(t.TempDir(), plainSealer{})
	_, err := store.Update(func(d *Data) error { d.History = make([]Result, maxHistory+50); return nil })
	if err != nil {
		t.Fatal(err)
	}
	data, _ := store.Load()
	if len(data.History) != maxHistory {
		t.Fatalf("%d", len(data.History))
	}
}

func TestPlatformSealerRoundTrip(t *testing.T) {
	sealer := newPlatformSealer(t.TempDir())
	key := bytes.Repeat([]byte{7}, sealedKeySize)
	sealed, err := sealer.Seal(key)
	if err != nil {
		t.Skipf("no keystore here: %v", err)
	}
	opened, err := sealer.Unseal(sealed)
	if err != nil || !bytes.Equal(opened, key) {
		t.Fatalf("%v %x", err, opened)
	}
}

func TestBudgetStopsScheduledRelayTests(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	spent := 4.95
	data := Data{Settings: DefaultSettings(), History: []Result{{Mode: ModeAPIKey, Questions: 1, Cost: &spent, At: now.Format(time.RFC3339)}}}
	target := Target{App: AppCodex, Mode: ModeAPIKey, Name: "站"}
	if ok, why := allowScheduled(target, data, now); ok || !strings.Contains(why, "预算") {
		t.Fatalf("%v %q", ok, why)
	}
	data.History[0].At = now.AddDate(0, -1, 0).Format(time.RFC3339)
	if ok, _ := allowScheduled(target, data, now); !ok {
		t.Fatal("last month does not count")
	}
}

func TestUnpricedTestsCountAgainstBudget(t *testing.T) {
	now := time.Now()
	data := Data{Settings: DefaultSettings()}
	for i := 0; i < 50; i++ {
		data.History = append(data.History, Result{Mode: ModeAPIKey, Questions: 1, At: now.Format(time.RFC3339)})
	}
	if ok, _ := allowScheduled(Target{Mode: ModeAPIKey}, data, now); ok {
		t.Fatal("an unpriced relay must not run past the budget")
	}
}

func TestOfficialDailyLimit(t *testing.T) {
	now := time.Now()
	data := Data{Settings: DefaultSettings()}
	target := Target{Mode: ModeOfficial, ID: "x", Name: "官方"}
	for i := 0; i < 4; i++ {
		data.History = append(data.History, Result{TargetID: "x", Questions: 1, At: now.Format(time.RFC3339)})
	}
	if ok, _ := allowScheduled(target, data, now); ok {
		t.Fatal("the daily limit must stop it")
	}
	data.Settings.OfficialDailyLimit = 5
	if ok, _ := allowScheduled(target, data, now); !ok {
		t.Fatal("the user can raise the limit")
	}
}

func TestSelectedDefaultsToChannelsInUse(t *testing.T) {
	targets := []Target{{ID: "a", Current: true}, {ID: "b"}, {ID: "c", Current: true, Problem: "x"}}
	got := selected(targets, Settings{})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("%+v", got)
	}
	got = selected(targets, Settings{Selected: []string{"b"}})
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("%+v", got)
	}
}

type fakeScheduler struct {
	installed int
	removed   int
	err       error
}

func (f *fakeScheduler) Install(string, int) error { f.installed++; return f.err }
func (f *fakeScheduler) Remove() error             { f.removed++; return f.err }

func newTestApp(t *testing.T) (*App, *httptest.Server, *fakeScheduler) {
	t.Helper()
	judge := newJudgeServer(t, "genuine")
	store, _ := openStore(t.TempDir(), plainSealer{})
	env := fixture(t, nil)
	put(t, filepath.Join(env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-page-secret-1234","ANTHROPIC_BASE_URL":"https://a.example.com"}}`)
	scheduler := &fakeScheduler{}
	app := &App{Env: env, Store: store, Runner: &Runner{Server: judge.URL, Client: &http.Client{}}, Scheduler: scheduler, Version: "t"}
	listener, _, err := app.Listen()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(app.Handler())
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return app, server, scheduler
}

func call(t *testing.T, server *httptest.Server, method, path, token, origin, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Check-Token", token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data := new(bytes.Buffer)
	_, _ = data.ReadFrom(resp.Body)
	return resp.StatusCode, data.String()
}

func TestPageGuards(t *testing.T) {
	app, server, _ := newTestApp(t)
	if code, _ := call(t, server, http.MethodGet, "/api/state", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := call(t, server, http.MethodGet, "/api/state", "wrong", "", ""); code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := call(t, server, http.MethodGet, "/api/state", app.token, "https://evil.example.com", ""); code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", code)
	}
	if code, _ := call(t, server, http.MethodGet, "/", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("page without token: %d", code)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/?t="+app.token, nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound host: %d", resp.StatusCode)
	}
}

func TestPageNeverSeesKey(t *testing.T) {
	app, server, _ := newTestApp(t)
	code, body := call(t, server, http.MethodGet, "/api/state", app.token, "", "")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(body, "sk-page-secret") || !strings.Contains(body, "…1234") {
		t.Fatalf("%s", body)
	}
	if !strings.Contains(body, `"promo"`) {
		t.Fatalf("the promo must reach the page: %s", body)
	}
}

func TestPromoFallsBackWhenServerIsDown(t *testing.T) {
	app, server, _ := newTestApp(t)
	app.Runner.Server = "http://127.0.0.1:1"
	_, body := call(t, server, http.MethodGet, "/api/state", app.token, "", "")
	if !strings.Contains(body, "app.pinaic.com") || !strings.Contains(body, `"online":false`) {
		t.Fatalf("%s", body)
	}
}

func TestScheduleSettingFollowsTheOS(t *testing.T) {
	app, server, scheduler := newTestApp(t)
	code, _ := call(t, server, http.MethodPost, "/api/settings", app.token, "", `{"schedule_enabled":true,"interval_hours":6,"monthly_budget_usd":5,"official_daily_limit":4}`)
	if code != http.StatusOK || scheduler.installed != 1 {
		t.Fatalf("%d %+v", code, scheduler)
	}
	scheduler.err = fmt.Errorf("refused")
	code, body := call(t, server, http.MethodPost, "/api/settings", app.token, "", `{"schedule_enabled":true,"interval_hours":6}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "refused") {
		t.Fatalf("%d %s", code, body)
	}
	data, _ := app.Store.Load()
	if data.Settings.ScheduleEnabled {
		t.Fatal("a schedule the OS refused must not read as on")
	}
}

func TestRelayModelListIsReadWithTheConfiguredKeyAndHeaders(t *testing.T) {
	var auth, custom string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, custom = r.Header.Get("Authorization"), r.Header.Get("x-a")
		fmt.Fprint(w, `{"data":[{"id":"gpt-6-astra"},{"id":"some-other-model"},{"id":"gpt-image-2"}]}`)
	}))
	defer relay.Close()
	runner := &Runner{Client: &http.Client{}}
	list := runner.ListModels(context.Background(), relayTarget(relay.URL), ServerConfig{Models: []string{"gpt-6-astra"}})
	if len(list.Models) != 2 || list.Models[1] != "some-other-model" {
		t.Fatalf("listed models, without the image one: %+v", list)
	}
	if auth != "Bearer sk-secret-1234" || custom != "b" {
		t.Fatalf("auth=%q custom=%q", auth, custom)
	}
}

func TestModelListFailureSaysWhy(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusUnauthorized) }))
	defer relay.Close()
	list := (&Runner{Client: &http.Client{}}).ListModels(context.Background(), relayTarget(relay.URL), ServerConfig{})
	if list.Error == "" {
		t.Fatalf("%+v", list)
	}
}

func TestOfficialModelListIsTheBanksModelsOfThatVendor(t *testing.T) {
	config := ServerConfig{Models: []string{"gpt-6-astra", "gpt-6-sol", "claude-opus-5-5", "kimi-k3"}}
	runner := &Runner{Client: &http.Client{}}
	claude := runner.ListModels(context.Background(), Target{App: AppClaude, Mode: ModeOfficial}, config)
	codex := runner.ListModels(context.Background(), Target{App: AppCodex, Mode: ModeOfficial}, config)
	if len(claude.Models) != 1 || claude.Models[0] != "claude-opus-5-5" || len(codex.Models) != 2 {
		t.Fatalf("%+v %+v", claude, codex)
	}
}

func TestModelsForPrefersWhatWasTicked(t *testing.T) {
	target := Target{ID: "x", App: AppCodex, Model: "gpt-6-astra"}
	if got := modelsFor(target, Settings{}); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Fatalf("%v", got)
	}
	got := modelsFor(target, Settings{ModelSets: map[string][]string{"x": {"a", "b"}}})
	if len(got) != 2 || got[1] != "b" {
		t.Fatalf("%v", got)
	}
}

func TestTestingSeveralModelsRunsEachAndCountsEach(t *testing.T) {
	app, server, _ := newTestApp(t)
	relay, _ := relayServer(t)
	put(t, filepath.Join(app.Env.Home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-page-secret-1234","ANTHROPIC_BASE_URL":"`+relay.URL+`"}}`)
	targets := Scan(app.Env)
	settings := fmt.Sprintf(`{"selected":[%q],"model_sets":{%q:["claude-opus-5-5","claude-sonnet-5-5"]}}`, targets[0].ID, targets[0].ID)
	if code, body := call(t, server, http.MethodPost, "/api/settings", app.token, "", settings); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	code, body := call(t, server, http.MethodPost, "/api/test", app.token, "", fmt.Sprintf(`{"ids":[%q]}`, targets[0].ID))
	if code != http.StatusAccepted || !strings.Contains(body, `"total":2`) {
		t.Fatalf("%d %s", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := app.Store.Load(); len(data.History) == 2 {
			if data.History[0].Model == data.History[1].Model {
				t.Fatalf("%+v", data.History)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("both models should have a result")
}

func TestOneClickCannotSpendWithoutBound(t *testing.T) {
	settings := Settings{ModelSets: map[string][]string{"x": make([]string, maxModelsPerTarget+5)}}
	settings.normalize()
	if len(settings.ModelSets["x"]) != maxModelsPerTarget {
		t.Fatalf("%d", len(settings.ModelSets["x"]))
	}
}

func TestOpenLinkOnlyTakesHTTPS(t *testing.T) {
	app, server, _ := newTestApp(t)
	var opened string
	app.OpenURL = func(url string) { opened = url }
	if code, _ := call(t, server, http.MethodPost, "/api/open", app.token, "", `{"url":"file:///c:/x.exe"}`); code != http.StatusBadRequest || opened != "" {
		t.Fatalf("%d %q", code, opened)
	}
	if code, _ := call(t, server, http.MethodPost, "/api/open", app.token, "", `{"url":"https://app.pinaic.com/models"}`); code != http.StatusOK || opened == "" {
		t.Fatalf("%d %q", code, opened)
	}
}

func TestNetLogNamesWhereAndWhatButNeverTheContent(t *testing.T) {
	judge := newJudgeServer(t, "genuine")
	relay, _ := relayServer(t)
	log := NewNetLog(judge.URL)
	runner := &Runner{Server: judge.URL, Client: &http.Client{Transport: log.Transport(http.DefaultTransport)}}
	runner.Run(context.Background(), relayTarget(relay.URL), "", false)
	raw, err := json.Marshal(log.Events())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	relayHost := strings.TrimPrefix(relay.URL, "http://")
	judgeHost := strings.TrimPrefix(judge.URL, "http://")
	if !strings.Contains(text, relayHost) || !strings.Contains(text, judgeHost) || !strings.Contains(text, "/v1/responses") {
		t.Fatalf("every destination is on record: %s", text)
	}
	if strings.Contains(text, "sk-secret") || strings.Contains(text, "1 2 3") {
		t.Fatalf("no key and no content: %s", text)
	}
	for _, event := range log.Events() {
		if event.Host == judgeHost && strings.Contains(event.Sent, "密钥") {
			t.Fatalf("check-api never gets a key: %+v", event)
		}
	}
}

func TestNetLogIsBounded(t *testing.T) {
	log := NewNetLog("https://x.example.com")
	for i := 0; i < netLogSize+50; i++ {
		log.RecordCommand("codex exec")
	}
	if len(log.Events()) != netLogSize {
		t.Fatalf("%d", len(log.Events()))
	}
}

func TestPageIsThePlainSource(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("ui", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pageHTML, source) {
		t.Fatal("the program must carry the page as it is in the source")
	}
}

func TestClientCarriesNoFingerprintBank(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../../../cmd/relaycheck-client").CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if strings.Contains(string(out), "modelfingerprint") || strings.Contains(string(out), "modeltrace") {
		t.Fatal("the client must not link the fingerprint banks")
	}
}
