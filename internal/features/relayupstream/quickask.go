package relayupstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// QuickPurity is PurityResult with stable JSON names for the public API.
type QuickPurity struct {
	Verdict     string  `json:"verdict"`
	Attributed  string  `json:"attributed,omitempty"`
	Substitute  string  `json:"substitute,omitempty"`
	Probability float64 `json:"probability,omitempty"`
	Note        string  `json:"note,omitempty"`
	OneClass    bool    `json:"one_class,omitempty"`
}

// QuickAnswer is one request the checker made, with the evidence the page
// shows.
type QuickAnswer struct {
	Endpoint    string     `json:"endpoint"`
	Status      int        `json:"status,omitempty"`
	Error       string     `json:"error,omitempty"`
	Verdict     string     `json:"verdict,omitempty"`
	Attributed  string     `json:"attributed,omitempty"`
	Substitute  string     `json:"substitute,omitempty"`
	Probability float64    `json:"probability,omitempty"`
	Note        string     `json:"note,omitempty"`
	Tokens      TestTokens `json:"tokens"`
	LatencyMs   int64      `json:"latency_ms"`
	TTFTMs      int64      `json:"ttft_ms,omitempty"`
	Answer      string     `json:"answer,omitempty"`
}

func quickEndpoints(model, preferred string) []string {
	if preferred != "" {
		return []string{preferred}
	}
	first := endpointFor(model)
	out := []string{first}
	for _, endpoint := range []string{EndpointChat, EndpointResponses, EndpointMessages} {
		if endpoint != first {
			out = append(out, endpoint)
		}
	}
	return out
}

// ask tries the preferred endpoint first and falls back only when the relay
// says the path itself is missing. A model rejection, auth failure or stream
// error is the answer, not a reason to send the same question elsewhere.
func askFirstServed(ctx context.Context, p prober, endpoints []string, model, prompt string) (probeAnswer, string) {
	var last probeAnswer
	lastEndpoint := endpoints[0]
	for _, endpoint := range endpoints {
		answer := p.ask(ctx, endpoint, model, prompt)
		last, lastEndpoint = answer, endpoint
		if answer.Status == http.StatusOK {
			return answer, endpoint
		}
		if answer.Status != http.StatusNotFound && answer.Status != http.StatusMethodNotAllowed {
			return answer, endpoint
		}
	}
	return last, lastEndpoint
}

func quickPurity(result PurityResult) QuickPurity {
	return QuickPurity{
		Verdict: result.Verdict, Attributed: result.Attributed, Substitute: result.Substitute,
		Probability: result.Probability, Note: result.Note, OneClass: result.OneClass,
	}
}

func quickAnswer(endpoint string, answer probeAnswer, result PurityResult) QuickAnswer {
	return QuickAnswer{
		Endpoint: endpoint, Status: answer.Status, Error: answer.Error,
		Verdict: result.Verdict, Attributed: result.Attributed, Substitute: result.Substitute,
		Probability: result.Probability, Note: result.Note,
		Tokens: answer.Tokens, LatencyMs: answer.Latency.Milliseconds(), TTFTMs: answer.TTFB.Milliseconds(),
		Answer: excerpt(answer.Text),
	}
}

// askedChallenges is what the questions of one check came to.
type askedChallenges struct {
	purity   PurityResult
	answers  []QuickAnswer
	tokens   TestTokens
	first    probeAnswer
	endpoint string
}

// askChallenges asks the challenge, and a second when told to or when the first
// answer was not plainly genuine.
func askChallenges(ctx context.Context, p prober, judge PurityJudge, endpoints []string, model string, confirm, retestSuspect bool) askedChallenges {
	challenge, banked := judge.Challenge(model)
	prompt := challenge.Prompt
	if !banked {
		prompt = shortTestPrompt
	}
	answer, endpoint := askFirstServed(ctx, p, endpoints, model, prompt)
	first := judgeAnswer(judge, banked, challenge, answer)
	out := askedChallenges{first: answer, endpoint: endpoint}
	out.answers = append(out.answers, quickAnswer(endpoint, answer, first))
	out.tokens.add(answer.Tokens)
	if banked && (confirm || (retestSuspect && retestable(first.Verdict))) {
		if second, ok := judge.Challenge(model); ok {
			secondAnswer, _ := askFirstServed(ctx, p, []string{endpoint}, model, second.Prompt)
			judged := judgeAnswer(judge, true, second, secondAnswer)
			out.answers = append(out.answers, quickAnswer(endpoint, secondAnswer, judged))
			out.tokens.add(secondAnswer.Tokens)
			first = combine(first, judged)
		}
	}
	out.purity = first
	return out
}

// AskRequest is one channel to put the challenge to.
type AskRequest struct {
	BaseURL   string
	APIKey    string
	Model     string
	Endpoint  string
	Headers   map[string]string
	Timeout   time.Duration
	MaxTokens int
}

// AskResult is what the channel's answers came to.
type AskResult struct {
	Purity    QuickPurity
	Answers   []QuickAnswer
	Tokens    TestTokens
	LatencyMs int64
}

// AskChallenge puts the challenge to one channel, with a second question when
// the first answer was not plainly genuine.
func AskChallenge(ctx context.Context, doer Doer, judge PurityJudge, req AskRequest) (AskResult, error) {
	model := strings.TrimSpace(req.Model)
	if doer == nil || judge == nil {
		return AskResult{}, errors.New("指纹库或探测客户端未装配")
	}
	if model == "" || strings.TrimSpace(req.BaseURL) == "" || strings.TrimSpace(req.APIKey) == "" {
		return AskResult{}, errors.New("接口地址、API Key 和模型都不能为空")
	}
	p := prober{doer: doer, base: strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"), key: strings.TrimSpace(req.APIKey),
		timeout: req.Timeout, maxTokens: req.MaxTokens, headers: req.Headers}
	asked := askChallenges(ctx, p, judge, quickEndpoints(model, req.Endpoint), model, false, true)
	return AskResult{Purity: quickPurity(asked.purity), Answers: asked.answers, Tokens: asked.tokens,
		LatencyMs: asked.first.Latency.Milliseconds()}, nil
}
