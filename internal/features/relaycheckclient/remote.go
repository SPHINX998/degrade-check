package relaycheckclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SPHINX998/degrade-check/internal/features/relayupstream"
)

const (
	// DefaultServer is check-api. Requests to it carry a model name and answer
	// text, never a key or a relay address.
	DefaultServer = "https://check-api.pinaic.com"
	clientPath    = "/api/v1/pinai-features/relay-check/client"
	remoteTimeout = 30 * time.Second
)

// Promo is the labelled promotion on the page.
type Promo struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	URL   string `json:"url"`
	CTA   string `json:"cta"`
}

// Price is a list price in USD per million tokens.
type Price struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cache_read"`
}

// ServerConfig is what check-api tells the client.
type ServerConfig struct {
	Enabled       bool             `json:"enabled"`
	Models        []string         `json:"models"`
	Promo         Promo            `json:"promo"`
	LatestVersion string           `json:"latest_version"`
	Prices        map[string]Price `json:"prices"`
	// OfficialVerdicts is off until the official CLI path is calibrated.
	OfficialVerdicts bool `json:"official_verdicts"`
}

// PendingAnswer is an answer already paid for that check-api could not judge at
// the time; it is judged again later instead of being wasted.
type PendingAnswer struct {
	Model    string `json:"model"`
	Expected int    `json:"expected"`
	Text     string `json:"text"`
}

// RemoteJudge is relayupstream.PurityJudge over check-api. One instance serves
// one test: it records what failed so the test can report it.
type RemoteJudge struct {
	Base   string
	Client *http.Client

	mu      sync.Mutex
	failed  error
	pending []PendingAnswer
}

func NewRemoteJudge(base string, client *http.Client) *RemoteJudge {
	return &RemoteJudge{Base: strings.TrimRight(base, "/"), Client: client}
}

func (r *RemoteJudge) call(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.Base+clientPath+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &refusal)
		return fmt.Errorf("判定服务返回 %d：%s", resp.StatusCode, refusal.Error)
	}
	return json.Unmarshal(data, into)
}

// Config asks check-api for its settings. A test starts only when this
// succeeds, so an unreachable judge costs the user nothing.
func (r *RemoteJudge) Config(ctx context.Context) (ServerConfig, error) {
	var out ServerConfig
	err := r.call(ctx, http.MethodGet, "/config", nil, &out)
	return out, err
}

func (r *RemoteJudge) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = err
	}
}

// Failure is the first judge error of this test, nil when there was none.
func (r *RemoteJudge) Failure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// Pending are the answers this test could not get judged.
func (r *RemoteJudge) Pending() []PendingAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PendingAnswer(nil), r.pending...)
}

func (r *RemoteJudge) Challenge(model string) (relayupstream.PurityChallenge, bool) {
	var out struct {
		Banked   bool   `json:"banked"`
		Prompt   string `json:"prompt"`
		Expected int    `json:"expected"`
	}
	if err := r.call(context.Background(), http.MethodPost, "/challenge", map[string]string{"model": model}, &out); err != nil {
		r.fail(err)
		return relayupstream.PurityChallenge{}, false
	}
	if !out.Banked {
		return relayupstream.PurityChallenge{}, false
	}
	return relayupstream.PurityChallenge{Model: model, Prompt: out.Prompt, Expected: out.Expected}, true
}

func (r *RemoteJudge) judge(ctx context.Context, answer PendingAnswer) (relayupstream.PurityResult, error) {
	var out struct {
		Verdict     string  `json:"verdict"`
		Attributed  string  `json:"attributed"`
		Probability float64 `json:"probability"`
		Note        string  `json:"note"`
		OneClass    bool    `json:"one_class"`
		Substitute  string  `json:"substitute"`
	}
	if err := r.call(ctx, http.MethodPost, "/judge", answer, &out); err != nil {
		return relayupstream.PurityResult{}, err
	}
	if out.Verdict == "" {
		return relayupstream.PurityResult{}, errors.New("判定服务没有给出结论")
	}
	return relayupstream.PurityResult{Verdict: out.Verdict, Attributed: out.Attributed, Probability: out.Probability,
		Note: out.Note, OneClass: out.OneClass, Substitute: out.Substitute}, nil
}

func (r *RemoteJudge) Judge(challenge relayupstream.PurityChallenge, answer string) relayupstream.PurityResult {
	pending := PendingAnswer{Model: challenge.Model, Expected: challenge.Expected, Text: answer}
	result, err := r.judge(context.Background(), pending)
	if err != nil {
		r.fail(err)
		r.mu.Lock()
		r.pending = append(r.pending, pending)
		r.mu.Unlock()
		return relayupstream.PurityResult{Verdict: relayupstream.PurityInvalid, Note: "判定服务暂时不可用"}
	}
	return result
}

// Rejudge judges saved answers again, combining them as a retest would.
func (r *RemoteJudge) Rejudge(ctx context.Context, answers []PendingAnswer) (relayupstream.PurityResult, error) {
	var combined relayupstream.PurityResult
	for i, answer := range answers {
		result, err := r.judge(ctx, answer)
		if err != nil {
			return combined, err
		}
		if i == 0 {
			combined = result
		} else {
			combined = relayupstream.CombineAnswers(combined, result)
		}
	}
	return combined, nil
}
