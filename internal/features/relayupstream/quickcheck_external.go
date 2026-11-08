package relayupstream

import (
	"context"
	"strings"
	"time"
)

// ClaudeCodeSystemPrompt is the system prompt the purity bank's Claude answers
// were collected under; a caller asking through another path sends the same.
const ClaudeCodeSystemPrompt = claudeCodePrompt

// JudgeExternalAnswer reads an answer that came through something other than
// this package's prober (an official CLI) by the same rules as JudgeAnswer's
// HTTP path. An empty answer is a failure; a model no bank knows only served.
func JudgeExternalAnswer(judge PurityJudge, banked bool, challenge PurityChallenge, text string, tokens TestTokens, latency time.Duration) PurityResult {
	if strings.TrimSpace(text) == "" {
		return PurityResult{Verdict: PurityFailed, Note: "回答为空"}
	}
	return judgeAnswer(judge, banked, challenge, probeAnswer{Status: 200, Text: text, Tokens: tokens, Latency: latency})
}

// CombineAnswers reads a retest the way QuickCheck does.
func CombineAnswers(first, second PurityResult) PurityResult { return combine(first, second) }

// Retestable says whether a verdict is worth a second question.
func Retestable(verdict string) bool { return retestable(verdict) }

// ModelUntestable says a text challenge cannot test this model.
func ModelUntestable(model string) bool { return untestable(model) }

// ListModels reads the models a key can ask for, with the caller's own client.
func ListModels(ctx context.Context, doer Doer, base, key string) ([]string, error) {
	return listModels(ctx, doer, base, key)
}
