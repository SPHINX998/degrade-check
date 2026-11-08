// Package relaycheckclient is the portable checker: it reads the Codex, Claude
// and CC Switch configuration on this machine, asks each configured channel the
// fingerprint challenge with the user's own credentials, and shows the verdict
// and what the test spent. It holds no fingerprint bank; check-api judges the
// answers. A credential only ever goes to the address it is configured for.
package relaycheckclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Apps, modes and the marker CC Switch writes into live config while its local
// router holds the real key.
const (
	AppCodex  = "codex"
	AppClaude = "claude"

	ModeAPIKey   = "apikey"
	ModeOfficial = "official"

	proxyManagedKey = "PROXY_MANAGED"
	ccSwitchPort    = "15721"

	defaultCodexModel  = "gpt-6-astra"
	defaultClaudeModel = "claude-opus-5-5"
)

// Target is one channel a test can run against. Its JSON is what the page sees,
// so the key and headers stay unexported.
type Target struct {
	ID       string `json:"id"`
	App      string `json:"app"`
	Mode     string `json:"mode"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Host     string `json:"host,omitempty"`
	Model    string `json:"model"`
	Endpoint string `json:"endpoint,omitempty"`
	KeyHint  string `json:"key_hint,omitempty"`
	Current  bool   `json:"current"`
	Insecure bool   `json:"insecure,omitempty"`
	Self     bool   `json:"self,omitempty"`
	Problem  string `json:"problem,omitempty"`

	baseURL string
	apiKey  string
	headers map[string]string
}

// Testable says a test can start: no problem was found with its configuration.
func (t Target) Testable() bool { return t.Problem == "" }

// Env is the machine the scan reads, injected so tests use a fixture home.
type Env struct {
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

func (e Env) codexHome() string {
	if dir := strings.TrimSpace(e.Getenv("CODEX_HOME")); dir != "" {
		return dir
	}
	return filepath.Join(e.Home, ".codex")
}

func (e Env) hasCLI(name string) bool {
	if e.LookPath == nil {
		return false
	}
	_, err := e.LookPath(name)
	return err == nil
}

// Scan lists every channel the machine is configured for. CC Switch entries come
// on top of the live files; a channel found in both is one row.
func Scan(env Env) []Target {
	live := []Target{scanCodex(env), scanClaude(env)}
	live = append(live, officialAlongsideKeys(env, live)...)
	switched := scanCCSwitch(env)

	byID := map[string]int{}
	out := make([]Target, 0, len(switched)+2)
	currentFromSwitch := map[string]bool{}
	for _, target := range switched {
		if target.Current {
			currentFromSwitch[target.App] = true
		}
		byID[target.ID] = len(out)
		out = append(out, target)
	}
	for _, target := range live {
		if target.ID == "" {
			continue
		}
		if index, seen := byID[target.ID]; seen {
			out[index].Source = "本机配置 + CC Switch"
			out[index].Current = true
			// The live file is what the client is really using right now.
			if target.Model != "" {
				out[index].Model = target.Model
			}
			continue
		}
		// While CC Switch's router holds the key, the live file only names the
		// router; the row from its database is the real channel.
		if isProxied(target) && currentFromSwitch[target.App] {
			continue
		}
		out = append(out, target)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Current && !out[j].Current
	})
	return out
}

func isProxied(target Target) bool {
	if target.apiKey == proxyManagedKey {
		return true
	}
	parsed, err := url.Parse(target.baseURL)
	return err == nil && parsed.Port() == ccSwitchPort && isLoopbackHost(parsed.Hostname())
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// finish derives everything a target's fields imply: its stable ID, its address
// and whether the address belongs to PinAI itself.
func (t Target) finish() Target {
	t.baseURL = normalizeBaseURL(t.baseURL)
	if parsed, err := url.Parse(t.baseURL); err == nil && parsed.Host != "" {
		t.Host = parsed.Host
		t.Insecure = parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname())
		host := strings.ToLower(parsed.Hostname())
		t.Self = host == "pinaic.com" || strings.HasSuffix(host, ".pinaic.com")
	}
	t.KeyHint = keyHint(t.apiKey)
	sum := sha256.Sum256([]byte(t.App + "\x00" + t.Mode + "\x00" + t.baseURL + "\x00" + t.apiKey))
	t.ID = t.App + "-" + hex.EncodeToString(sum[:6])
	if t.Mode == ModeAPIKey && t.Problem == "" {
		switch {
		case isProxied(t):
			t.Problem = "CC Switch 本地路由接管中，这里读不到真实密钥"
		case t.apiKey == "":
			t.Problem = "配置里没有找到 API Key"
		case t.baseURL == "":
			t.Problem = "配置里没有找到接口地址"
		}
	}
	return t
}

// normalizeBaseURL drops a trailing /v1: Codex configs carry it and the prober
// adds its own, which would make /v1/v1/responses.
func normalizeBaseURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	return strings.TrimRight(strings.TrimSuffix(base, "/v1"), "/")
}

func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if len(key) < 8 || key == proxyManagedKey {
		return ""
	}
	return "…" + key[len(key)-4:]
}

func readJSONMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func text(doc map[string]any, key string) string {
	value, _ := doc[key].(string)
	return strings.TrimSpace(value)
}

func child(doc map[string]any, key string) map[string]any {
	value, _ := doc[key].(map[string]any)
	return value
}

// officialAlongsideKeys finds a login sitting next to a configured key. One
// machine can have both, and the login is what the user may want to check, so
// it gets its own row; it is not what the client uses now, so it is not ticked
// by default. Whether a login exists is all this reads: never the token.
func officialAlongsideKeys(env Env, live []Target) []Target {
	hasOfficial := func(app string) bool {
		for _, target := range live {
			if target.App == app && target.Mode == ModeOfficial {
				return true
			}
		}
		return false
	}
	var out []Target
	add := func(app, name, model, cli string) {
		target := Target{App: app, Mode: ModeOfficial, Name: name, Source: "本机配置", Model: model}
		if !env.hasCLI(cli) {
			target.Problem = "没装 " + cli + " 命令行，测不了官方登录"
		}
		out = append(out, target.finish())
	}
	if !hasOfficial(AppCodex) {
		if auth, err := readJSONMap(filepath.Join(env.codexHome(), "auth.json")); err == nil && isChatGPTAuth(auth) {
			add(AppCodex, "Codex 官方登录", defaultCodexModel, "codex")
		}
	}
	if !hasOfficial(AppClaude) && claudeLoggedIn(env) {
		add(AppClaude, "Claude 官方登录", defaultClaudeModel, "claude")
	}
	return out
}

func claudeConfigDir(env Env) string {
	return firstText(env.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(env.Home, ".claude"))
}

// claudeLoggedIn looks for the login's traces: the credentials file, or on macOS,
// where the token is in the keychain, the account record in .claude.json.
func claudeLoggedIn(env Env) bool {
	if _, err := os.Stat(filepath.Join(claudeConfigDir(env), ".credentials.json")); err == nil {
		return true
	}
	doc, err := readJSONMap(filepath.Join(env.Home, ".claude.json"))
	return err == nil && child(doc, "oauthAccount") != nil
}

// ---- Codex ----

func scanCodex(env Env) Target {
	home := env.codexHome()
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	configMissing := os.IsNotExist(err)
	if err != nil && !configMissing {
		return Target{App: AppCodex, Mode: ModeAPIKey, Name: "Codex", Source: "本机配置", Problem: "读不了 config.toml"}.finish()
	}
	auth, authErr := readJSONMap(filepath.Join(home, "auth.json"))
	if authErr != nil {
		auth = nil
	}
	if configMissing && auth == nil {
		if !env.hasCLI("codex") {
			return Target{}
		}
		return Target{App: AppCodex, Mode: ModeOfficial, Name: "Codex 官方登录", Source: "本机配置", Model: defaultCodexModel,
			Problem: "Codex 还没登录"}.finish()
	}
	target := codexTarget(string(raw), auth, env)
	target.Source, target.Current = "本机配置", true
	return target.finish()
}

// codexTarget reads one Codex configuration: the live files, or the same two
// documents as CC Switch stores them.
func codexTarget(configTOML string, auth map[string]any, env Env) Target {
	config := map[string]any{}
	if strings.TrimSpace(configTOML) != "" {
		if err := toml.Unmarshal([]byte(configTOML), &config); err != nil {
			return Target{App: AppCodex, Mode: ModeAPIKey, Name: "Codex", Problem: "config.toml 格式有误"}
		}
	}
	target := Target{App: AppCodex, Mode: ModeAPIKey, Name: "Codex", Model: text(config, "model"), Endpoint: "responses"}
	authKey := text(auth, "OPENAI_API_KEY")
	providerName := text(config, "model_provider")

	if providerName == "" || providerName == "openai" {
		target.baseURL = text(config, "openai_base_url")
		switch {
		case authKey != "":
			target.apiKey = authKey
			if target.baseURL == "" {
				target.baseURL = "https://api.openai.com"
			}
		case isChatGPTAuth(auth) && target.baseURL == "":
			target.Mode, target.Name = ModeOfficial, "Codex 官方登录"
			target.Model = defaultCodexModel
			if !env.hasCLI("codex") {
				target.Problem = "没装 codex 命令行，测不了官方登录"
			}
		default:
			target.Problem = "Codex 还没配置 API Key，也没有官方登录"
		}
		return target
	}

	provider := child(child(config, "model_providers"), providerName)
	if provider == nil {
		target.Problem = "config.toml 里没有 " + providerName + " 这个供应商"
		return target
	}
	target.Name = firstText(text(provider, "name"), providerName)
	target.baseURL = firstText(text(provider, "base_url"), text(config, "openai_base_url"))
	target.apiKey = firstText(text(provider, "experimental_bearer_token"), env.Getenv(text(provider, "env_key")), authKey)
	if wire := text(provider, "wire_api"); wire == "chat" {
		target.Endpoint = "chat"
	}
	target.headers = codexHeaders(provider, env)
	if target.apiKey == "" && isChatGPTAuth(auth) {
		target.Problem = "这个供应商用官方登录鉴权，测不了"
	}
	return target
}

func isChatGPTAuth(auth map[string]any) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(text(auth, "auth_mode"), "chatgpt") {
		return true
	}
	return child(auth, "tokens") != nil
}

func codexHeaders(provider map[string]any, env Env) map[string]string {
	headers := map[string]string{}
	for key, value := range child(provider, "http_headers") {
		if s, ok := value.(string); ok {
			headers[key] = s
		}
	}
	for key, value := range child(provider, "env_http_headers") {
		if name, ok := value.(string); ok {
			if resolved := env.Getenv(name); resolved != "" {
				headers[key] = resolved
			}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func firstText(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// ---- Claude ----

func scanClaude(env Env) Target {
	dir := claudeConfigDir(env)
	settings, err := readJSONMap(filepath.Join(dir, "settings.json"))
	if err != nil && !os.IsNotExist(err) {
		return Target{App: AppClaude, Mode: ModeAPIKey, Name: "Claude", Source: "本机配置", Problem: "settings.json 格式有误"}.finish()
	}
	target := claudeTarget(settings, env.Getenv, env)
	target.Source, target.Current = "本机配置", true
	if target.Mode == ModeOfficial {
		hasCLI := env.hasCLI("claude")
		if !claudeLoggedIn(env) && !hasCLI {
			return Target{}
		}
		if !hasCLI {
			target.Problem = "没装 claude 命令行，测不了官方登录"
		}
	}
	return target.finish()
}

// claudeTarget reads one Claude configuration. The settings file's env wins
// over the process environment, as it does inside Claude Code; CC Switch's
// stored documents have no process environment behind them.
func claudeTarget(settings map[string]any, getenv func(string) string, env Env) Target {
	block := child(settings, "env")
	read := func(name string) string {
		if block != nil {
			if value := text(block, name); value != "" {
				return value
			}
		}
		if getenv != nil {
			return strings.TrimSpace(getenv(name))
		}
		return ""
	}
	target := Target{App: AppClaude, Mode: ModeAPIKey, Name: "Claude", Endpoint: "messages"}
	target.Model = claudeModel(firstText(read("ANTHROPIC_MODEL"), text(settings, "model"), read("ANTHROPIC_DEFAULT_OPUS_MODEL")))
	target.apiKey = firstText(read("ANTHROPIC_AUTH_TOKEN"), read("ANTHROPIC_API_KEY"))
	target.baseURL = read("ANTHROPIC_BASE_URL")
	target.headers = claudeHeaders(read("ANTHROPIC_CUSTOM_HEADERS"))
	switch {
	case target.apiKey != "":
		if target.baseURL == "" {
			target.baseURL = "https://api.anthropic.com"
		}
	case text(settings, "apiKeyHelper") != "":
		target.Problem = "密钥由 apiKeyHelper 脚本生成，暂不支持"
	default:
		target.Mode, target.Name = ModeOfficial, "Claude 官方登录"
		target.baseURL, target.headers = "", nil
	}
	return target
}

// claudeModel turns a client-side alias into a model name: a suffix like [1m]
// is the client's, and a bare alias names no model the bank could judge.
func claudeModel(model string) string {
	if index := strings.IndexByte(model, '['); index >= 0 {
		model = model[:index]
	}
	model = strings.TrimSpace(model)
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		return defaultClaudeModel
	}
	return model
}

func claudeHeaders(raw string) map[string]string {
	headers := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok {
			if name = strings.TrimSpace(name); name != "" {
				headers[name] = strings.TrimSpace(value)
			}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}
