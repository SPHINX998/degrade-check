package relaycheckclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SPHINX998/degrade-check/internal/features/relayupstream"
)

const (
	testTimeout    = 5 * time.Minute
	requestTimeout = 3 * time.Minute
	probeMaxTokens = 8192
	// billingSettle gives the relay a moment to book the test before the
	// balance is read again.
	billingSettle = 4 * time.Second
	// balancePlausibleFactor bounds a balance difference one test could explain,
	// against that test at list price. Relays usually sell below list.
	balancePlausibleFactor = 1.5

	CostBalance  = "balance"  // the relay's balance fell by this much
	CostEstimate = "estimate" // tokens at list price
	CostReported = "reported" // the official CLI's own equivalent figure
	CostUnknown  = "unknown"

	VerdictPending = "pending" // answered and paid for, not judged yet
	VerdictError   = "error"   // no answer to judge
)

// Result is one finished test.
type Result struct {
	ID          string  `json:"id"`
	TargetID    string  `json:"target_id"`
	TargetName  string  `json:"target_name"`
	App         string  `json:"app"`
	Mode        string  `json:"mode"`
	Host        string  `json:"host,omitempty"`
	Self        bool    `json:"self,omitempty"`
	Model       string  `json:"model"`
	At          string  `json:"at"`
	Scheduled   bool    `json:"scheduled,omitempty"`
	Verdict     string  `json:"verdict"`
	Attributed  string  `json:"attributed,omitempty"`
	Substitute  string  `json:"substitute,omitempty"`
	Probability float64 `json:"probability,omitempty"`
	OneClass    bool    `json:"one_class,omitempty"`
	Note        string  `json:"note,omitempty"`
	Questions   int     `json:"questions"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheTokens  int64 `json:"cache_tokens"`
	LatencyMs    int64 `json:"latency_ms"`

	Cost          *float64 `json:"cost,omitempty"`
	CostBasis     string   `json:"cost_basis"`
	CostNote      string   `json:"cost_note,omitempty"`
	BalanceBefore *float64 `json:"balance_before,omitempty"`
	BalanceAfter  *float64 `json:"balance_after,omitempty"`
	QuotaBefore   string   `json:"quota_before,omitempty"`
	QuotaAfter    string   `json:"quota_after,omitempty"`

	Pending []PendingAnswer `json:"pending,omitempty"`
}

// Runner runs tests. Exec is the seam for the official CLIs.
type Runner struct {
	Server string
	Client *http.Client
	Exec   CommandRunner
	Now    func() time.Time
	Prices map[string]Price

	bankedMu sync.Mutex
	banked   map[string]bankedAnswer
}

func newResultID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Run tests one target. A target that cannot start returns an error result that
// spent nothing. model overrides the target's own when not empty.
func (r *Runner) Run(ctx context.Context, target Target, model string, scheduled bool) Result {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	model = firstText(model, target.Model, defaultModelFor(target.App))
	result := Result{
		ID: newResultID(), TargetID: target.ID, TargetName: target.Name, App: target.App, Mode: target.Mode,
		Host: target.Host, Self: target.Self, Model: model, At: now().Format(time.RFC3339), Scheduled: scheduled,
		Verdict: VerdictError, CostBasis: CostUnknown,
	}
	if !target.Testable() {
		result.Note = target.Problem
		return result
	}
	if relayupstream.ModelUntestable(model) {
		result.Note = model + " 是生图、视频等模型，文字题测不了"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	judge := NewRemoteJudge(r.Server, r.Client)
	config, err := judge.Config(ctx)
	if err != nil {
		result.Note = "判定服务连不上，这次没有发题，也没有花费：" + err.Error()
		return result
	}
	if !config.Enabled {
		result.Note = "判定服务维护中，这次没有发题，也没有花费"
		return result
	}
	if r.Prices == nil {
		r.Prices = config.Prices
	}
	if target.Mode == ModeOfficial {
		r.runOfficial(ctx, target, model, judge, &result)
	} else {
		r.runRelay(ctx, target, model, judge, &result)
	}
	if failure := judge.Failure(); failure != nil && len(judge.Pending()) > 0 {
		result.Verdict = VerdictPending
		result.Pending = judge.Pending()
		result.Note = "题已答完，判定服务暂时不可用，稍后自动重新判定"
	} else if failure != nil {
		// Without a challenge the prober falls back to a short question that
		// says nothing about the model; that must not read as a result.
		result.Verdict = VerdictError
		result.Note = "判定服务出错，这次没有结论：" + failure.Error()
	}
	if target.Mode == ModeOfficial && !config.OfficialVerdicts && result.Verdict != VerdictError && result.Verdict != relayupstream.PurityFailed {
		withholdOfficialVerdict(&result)
	}
	return result
}

// withholdOfficialVerdict keeps an uncalibrated path from claiming anything: the
// official CLI adds its own prompt (18,466 input tokens against 1,621 for a
// direct request, measured 2026-11-08), and the banks were not fitted on that.
func withholdOfficialVerdict(result *Result) {
	result.Verdict = relayupstream.PurityServed
	result.Attributed, result.Substitute, result.Probability, result.OneClass = "", "", 0, false
	result.Pending = nil
	result.Note = "官方登录这条路还在校准，这次只确认能用，不下结论"
}

func defaultModelFor(app string) string {
	if app == AppClaude {
		return defaultClaudeModel
	}
	return defaultCodexModel
}

// runRelay asks through the shared prober with the user's own key.
func (r *Runner) runRelay(ctx context.Context, target Target, model string, judge *RemoteJudge, result *Result) {
	before, hasBefore := r.readBalance(ctx, target)
	if hasBefore {
		result.BalanceBefore = &before
	}
	check, err := relayupstream.AskChallenge(ctx, r.Client, judge, relayupstream.AskRequest{
		BaseURL: target.baseURL, APIKey: target.apiKey, Model: model, Endpoint: target.Endpoint,
		Headers: target.headers, Timeout: requestTimeout, MaxTokens: probeMaxTokens,
	})
	if err != nil {
		result.Note = err.Error()
		return
	}
	result.Verdict = check.Purity.Verdict
	result.Attributed, result.Substitute = check.Purity.Attributed, check.Purity.Substitute
	result.Probability, result.OneClass, result.Note = check.Purity.Probability, check.Purity.OneClass, check.Purity.Note
	result.Questions = len(check.Answers)
	result.InputTokens, result.OutputTokens = check.Tokens.Input, check.Tokens.Output
	result.CacheTokens = check.Tokens.CacheRead + check.Tokens.CacheWrite
	result.LatencyMs = check.LatencyMs
	if result.Note == "" {
		for _, answer := range check.Answers {
			if answer.Error != "" {
				result.Note = answer.Error
				break
			}
		}
	}
	r.settleCost(ctx, target, result, before, hasBefore)
}

// settleCost uses what the balance fell by only when one test could explain it.
// The balance belongs to the whole account and a test takes about a minute, so
// anything else the account is doing lands in the difference.
func (r *Runner) settleCost(ctx context.Context, target Target, result *Result, before float64, hasBefore bool) {
	if hasBefore && result.InputTokens+result.OutputTokens > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(billingSettle):
		}
		if after, ok := r.readBalance(ctx, target); ok {
			result.BalanceAfter = &after
			if spent := before - after; spent > 0 {
				result.Cost, result.CostBasis = &spent, CostBalance
			}
		}
	}
	settleResultCost(result, r.Prices)
}

// settleResultCost decides what a result's cost figure may claim. It also
// corrects records saved before this rule existed.
func settleResultCost(result *Result, prices map[string]Price) {
	if result.Mode != ModeAPIKey || result.CostBasis == CostEstimate {
		return
	}
	estimate, hasEstimate := estimateCost(prices, result.Model, result.InputTokens, result.OutputTokens, result.CacheTokens)
	if result.CostBasis == CostBalance {
		if hasEstimate && *result.Cost <= estimate*balancePlausibleFactor {
			return
		}
		if hasEstimate {
			result.CostNote = "账号同时有其他请求，余额差不可用"
		} else {
			result.CostNote = fmt.Sprintf("余额减少 $%.3f，可能含账号的其他请求", *result.Cost)
		}
		result.Cost, result.CostBasis = nil, CostUnknown
	}
	if hasEstimate {
		result.Cost, result.CostBasis = &estimate, CostEstimate
	}
}

func (r *Runner) estimate(model string, input, output, cache int64) (float64, bool) {
	return estimateCost(r.Prices, model, input, output, cache)
}

func estimateCost(prices map[string]Price, model string, input, output, cache int64) (float64, bool) {
	price, ok := prices[strings.ToLower(model)]
	if !ok {
		return 0, false
	}
	return (float64(input)*price.Input + float64(output)*price.Output + float64(cache)*price.CacheRead) / 1e6, true
}

// readBalance reads /v1/usage, which Sub2API sites answer for an API key.
func (r *Runner) readBalance(ctx context.Context, target Target) (float64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.baseURL+"/v1/usage", nil)
	if err != nil {
		return 0, false
	}
	for key, value := range target.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", "Bearer "+target.apiKey)
	req.Header.Set("x-api-key", target.apiKey)
	resp, err := r.Client.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	var doc struct {
		Remaining *float64 `json:"remaining"`
		Unit      string   `json:"unit"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Remaining == nil {
		return 0, false
	}
	if doc.Unit != "" && !strings.EqualFold(doc.Unit, "USD") {
		return 0, false
	}
	return *doc.Remaining, true
}

// RejudgePending settles a result whose answers could not be judged earlier.
func (r *Runner) RejudgePending(ctx context.Context, result *Result) error {
	if len(result.Pending) == 0 {
		return nil
	}
	verdict, err := NewRemoteJudge(r.Server, r.Client).Rejudge(ctx, result.Pending)
	if err != nil {
		return err
	}
	result.Verdict, result.Attributed, result.Substitute = verdict.Verdict, verdict.Attributed, verdict.Substitute
	result.Probability, result.OneClass, result.Note = verdict.Probability, verdict.OneClass, verdict.Note
	result.Pending = nil
	return nil
}
