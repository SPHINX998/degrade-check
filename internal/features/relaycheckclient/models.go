package relaycheckclient

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/SPHINX998/degrade-check/internal/features/relayupstream"
)

const (
	modelListTimeout  = 30 * time.Second
	modelListCacheFor = 10 * time.Minute
)

// headerDoer puts a relay's configured headers on the model-list request.
type headerDoer struct {
	client  *http.Client
	headers map[string]string
}

func (d headerDoer) Do(req *http.Request) (*http.Response, error) {
	for key, value := range d.headers {
		req.Header.Set(key, value)
	}
	return d.client.Do(req)
}

// ModelList is what one channel can be asked about. Judgeable are the models the
// fingerprint bank knows; anything else only shows whether the channel serves it.
type ModelList struct {
	Models    []string `json:"models"`
	Judgeable []string `json:"judgeable"`
	Error     string   `json:"error,omitempty"`
}

// ListModels reads a channel's models. A key can ask its relay, free of charge;
// an official login has no such list through its CLI, so it offers the bank's
// models of that vendor.
func (r *Runner) ListModels(ctx context.Context, target Target, config ServerConfig) ModelList {
	out := ModelList{Judgeable: config.Models}
	if target.Mode == ModeOfficial {
		prefix := "gpt-"
		if target.App == AppClaude {
			prefix = "claude-"
		}
		for _, model := range config.Models {
			if strings.HasPrefix(strings.ToLower(model), prefix) {
				out.Models = append(out.Models, model)
			}
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	models, err := relayupstream.ListModels(ctx, headerDoer{client: r.Client, headers: target.headers}, target.baseURL, target.apiKey)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, model := range models {
		if !relayupstream.ModelUntestable(model) {
			out.Models = append(out.Models, model)
		}
	}
	return out
}

// modelsFor says which models a test of this target covers: what the user ticked,
// else the one the client is configured with.
func modelsFor(target Target, settings Settings) []string {
	if picked := settings.ModelSets[target.ID]; len(picked) > 0 {
		return picked
	}
	return []string{firstText(settings.Models[target.ID], target.Model, defaultModelFor(target.App))}
}
