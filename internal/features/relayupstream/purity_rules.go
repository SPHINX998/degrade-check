package relayupstream

import (
	"fmt"
	"strings"
	"time"
)

// challengeInputFloor is below the input tokens of any request carrying the
// integer challenge: a channel that reports less never gave the challenge to the
// model, so its answer says nothing about the model.
const challengeInputFloor = 100

// An answer of at least replayMinOutput tokens written faster than
// replayTokensPerSecond, first token included, was not written by a model then
// but replayed from a cache. Its verdict says only what the replayed text looks
// like, so it counts nowhere.
const (
	replayMinOutput       = 200
	replayTokensPerSecond = 1000
)

// replayed: output tokens written in latency, first token included, too fast
// for a model.
func replayed(output int64, latency time.Duration) bool {
	return output >= replayMinOutput && float64(output) > replayTokensPerSecond*latency.Seconds()
}

func replayNote(output int64, latency time.Duration) string {
	return fmt.Sprintf("%.1f 秒吐出 %d 个 token，不是当场生成（像回放缓存），不计纯度", latency.Seconds(), output)
}

// The purity verdict of one model in a group test.
const (
	// PurityGenuine: the bank attributes the answer to the model asked for
	// (a non-GPT model: the answer looks like it, see PurityResult.OneClass).
	PurityGenuine = "genuine"
	// PuritySwapped: to another model of the bank (a non-GPT model: two
	// answers that do not look like it, see combineOneClass).
	PuritySwapped = "swapped"
	// PurityDegraded: to a luna model when a stronger model was asked for.
	PurityDegraded = "degraded"
	// PurityInvalid: the answer is too short or degenerate to attribute.
	PurityInvalid = "invalid"
	// PurityBlocked: an answer without a verdict that the challenge never
	// reached a model for: the upstream counts fewer input tokens than the
	// challenge alone takes (see challengeInputFloor).
	PurityBlocked = "blocked"
	// PurityMixed: the retest disagreed with the first answer; no verdict.
	PurityMixed = "mixed"
	// PurityServed: no bank knows the model; it answered.
	PurityServed = "served"
	// PurityFailed: no usable answer.
	PurityFailed = "failed"
	// PuritySkipped: an image, video, audio or embedding model.
	PuritySkipped = "skipped"
)

// PurityJudge is the account model fingerprint's integer challenge.
type PurityJudge interface {
	// Challenge is the request for model; false when no bank knows it.
	Challenge(model string) (PurityChallenge, bool)
	// Judge attributes an answer: genuine, swapped, degraded or invalid.
	Judge(challenge PurityChallenge, answer string) PurityResult
}

// PurityChallenge is one integer challenge.
type PurityChallenge struct {
	Model    string
	Prompt   string
	Expected int
}

// PurityResult is what one answer says about the model behind it.
//
// OneClass marks an answer judged the way the account model fingerprint
// judges a non-GPT relay: by whether it looks like the model asked
// (modelfingerprint.Resemblance), never by the bank's pick among its models
// . Such an answer is genuine or
// swapped on its own; Attributed is then the bank's pick, and Substitute that
// pick when the answer also looks like it by the same lines. Only two answers
// make a swap, named only by a Substitute both share (see combineOneClass).
type PurityResult struct {
	Verdict     string
	Attributed  string
	Probability float64
	Note        string
	OneClass    bool
	Substitute  string
}

func judgeAnswer(judge PurityJudge, banked bool, challenge PurityChallenge, answer probeAnswer) PurityResult {
	switch {
	case answer.Status != 200 || answer.Error != "":
		return PurityResult{Verdict: PurityFailed}
	case strings.TrimSpace(answer.Text) == "":
		return PurityResult{Verdict: PurityFailed, Note: "回答为空"}
	case !banked:
		return PurityResult{Verdict: PurityServed}
	}
	result := judge.Judge(challenge, answer.Text)
	tokens := answer.Tokens
	input := tokens.Input + tokens.CacheRead + tokens.CacheWrite
	switch {
	case result.Verdict == PurityInvalid && input > 0 && input < challengeInputFloor:
		return PurityResult{Verdict: PurityBlocked, Note: fmt.Sprintf(
			"上游报的输入只有 %d token（这道题本身就要 150 以上），%.1f 秒只回了 %d 个 token：题目没交给模型，这个渠道的指纹测不出真假",
			input, answer.Latency.Seconds(), tokens.Output)}
	case result.Verdict != PurityInvalid && replayed(tokens.Output, answer.Latency):
		return PurityResult{Verdict: PurityInvalid, Note: replayNote(tokens.Output, answer.Latency)}
	}
	return result
}

func retestable(verdict string) bool {
	return verdict == PuritySwapped || verdict == PurityDegraded || verdict == PurityInvalid || verdict == PurityBlocked
}

// combine reads a retest: a retest without a verdict keeps the first, a first
// answer without one gives way, two different verdicts are mixed. Answers
// judged by resemblance are read by combineOneClass.
func combine(first, second PurityResult) PurityResult {
	if first.OneClass || second.OneClass {
		return combineOneClass(first, second)
	}
	switch {
	case second.Verdict == PurityFailed || second.Verdict == PurityInvalid || second.Verdict == PurityBlocked:
		first.Note = firstNonEmpty(first.Note, "复测没得出结论，按首测")
		return first
	case first.Verdict == PurityInvalid || first.Verdict == PurityBlocked || first.Verdict == second.Verdict:
		return second
	}
	return PurityResult{Verdict: PurityMixed, Attributed: second.Attributed, Probability: second.Probability,
		Note: fmt.Sprintf("首测判 %s（%.2f），复测判 %s（%.2f）", first.Attributed, first.Probability, second.Attributed, second.Probability)}
}

// combineOneClass reads a retest of a non-GPT model the way the account model
// fingerprint does (modelfingerprint judgePurity): a first answer that did not
// look like the model asked is a swap only when the retest does not either,
// named only when both were attributed to one other model and both look like
// it; a retest that looks
// like it is mixed, and one without a verdict, or a single answer that does
// not look like it after a first without one, makes no verdict.
func combineOneClass(first, second PurityResult) PurityResult {
	unlike := func(r PurityResult) string { return firstNonEmpty(r.Note, "不像所问模型") }
	switch {
	case first.Verdict != PuritySwapped && second.Verdict == PuritySwapped:
		return PurityResult{Verdict: PurityInvalid, OneClass: true,
			Note: "首测没得出判定，复测" + unlike(second) + "：只有一题不像，不下结论"}
	case first.Verdict != PuritySwapped:
		return second
	case second.Verdict == PurityGenuine:
		return PurityResult{Verdict: PurityMixed, OneClass: true,
			Note: fmt.Sprintf("两题不一致：首测%s，复测像所问模型（%s），不下结论", unlike(first), second.Attributed)}
	case second.Verdict != PuritySwapped:
		return PurityResult{Verdict: PurityInvalid, OneClass: true,
			Note: fmt.Sprintf("首测%s，复测没得出判定（%s），不下结论", unlike(first), firstNonEmpty(second.Note, "无效回答"))}
	case first.Substitute != "" && first.Substitute == second.Substitute:
		return PurityResult{Verdict: PuritySwapped, OneClass: true, Attributed: first.Substitute, Substitute: first.Substitute,
			Probability: min(first.Probability, second.Probability),
			Note:        fmt.Sprintf("首测%s，复测%s；两题都像 %s", unlike(first), unlike(second), first.Substitute)}
	}
	return PurityResult{Verdict: PuritySwapped, OneClass: true,
		Note: fmt.Sprintf("首测%s，复测%s；认不出换成了什么（首测最近 %s，复测最近 %s）", unlike(first), unlike(second), first.Attributed, second.Attributed)}
}
