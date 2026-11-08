package relaycheckclient

import (
	"context"
	"fmt"
	"time"
)

// ScheduleTask is the name the OS scheduler knows this tool by.
const ScheduleTask = "PinAICheck"

// Scheduler installs the recurring background run with the OS's own scheduler,
// so nothing of this tool stays resident between runs.
type Scheduler interface {
	Install(exe string, hours int) error
	Remove() error
}

// selected is what a scheduled run tests: the user's choice, or the channel
// each client is using right now. Testing every stored provider by default
// would spend money on channels the user is not using.
func selected(targets []Target, settings Settings) []Target {
	wanted := map[string]bool{}
	for _, id := range settings.Selected {
		wanted[id] = true
	}
	var out []Target
	for _, target := range targets {
		if !target.Testable() {
			continue
		}
		if (len(wanted) == 0 && target.Current) || wanted[target.ID] {
			out = append(out, target)
		}
	}
	return out
}

// monthSpend is what API-key tests cost this calendar month. A test whose cost
// could not be read counts at the assumed price, so an unpriced relay cannot run
// past the budget unseen. Official-login tests draw on the subscription, not on
// money; the daily limit covers those.
func monthSpend(history []Result, settings Settings, now time.Time) float64 {
	total := 0.0
	for _, result := range history {
		at, err := time.Parse(time.RFC3339, result.At)
		if err != nil || at.Year() != now.Year() || at.Month() != now.Month() || result.Mode != ModeAPIKey || result.Questions == 0 {
			continue
		}
		if result.Cost != nil {
			total += *result.Cost
		} else {
			total += settings.AssumedCostUSD * float64(result.Questions)
		}
	}
	return total
}

func officialToday(history []Result, targetID string, now time.Time) int {
	count := 0
	for _, result := range history {
		at, err := time.Parse(time.RFC3339, result.At)
		if err == nil && result.TargetID == targetID && result.Questions > 0 && at.Format(time.DateOnly) == now.Format(time.DateOnly) {
			count++
		}
	}
	return count
}

// allowScheduled says whether a scheduled test may start, and why not.
func allowScheduled(target Target, data Data, now time.Time) (bool, string) {
	if target.Mode == ModeOfficial {
		if officialToday(data.History, target.ID, now) >= data.Settings.OfficialDailyLimit {
			return false, fmt.Sprintf("%s 今天已测满 %d 次", target.Name, data.Settings.OfficialDailyLimit)
		}
		return true, ""
	}
	spent := monthSpend(data.History, data.Settings, now)
	if spent+data.Settings.AssumedCostUSD > data.Settings.MonthlyBudgetUSD {
		return false, fmt.Sprintf("本月预算 $%.2f 已用 $%.2f，定时测试已停", data.Settings.MonthlyBudgetUSD, spent)
	}
	return true, ""
}

// RunScheduled is one background run. Each result is saved before the next
// test, so a crash loses at most the one in flight.
func RunScheduled(ctx context.Context, store *Store, runner *Runner, env Env) (string, error) {
	data, err := store.Load()
	if err != nil {
		return "", err
	}
	note := ""
	// The budget check needs prices before the first test has fetched them.
	if config, err := NewRemoteJudge(runner.Server, runner.Client).Config(ctx); err == nil && runner.Prices == nil {
		runner.Prices = config.Prices
	}
	if _, err := store.Update(func(d *Data) error { rejudgeAll(ctx, runner, d); return nil }); err != nil {
		return "", err
	}
	for _, target := range selected(Scan(env), data.Settings) {
		data, err = store.Load()
		if err != nil {
			return note, err
		}
		settleHistoryCosts(data.History, runner.Prices)
		for _, model := range modelsFor(target, data.Settings) {
			if allowed, why := allowScheduled(target, data, time.Now()); !allowed {
				note = why
				break
			}
			result := runner.Run(ctx, target, model, true)
			data, err = store.Update(func(d *Data) error { d.History = append(d.History, result); return nil })
			if err != nil {
				return note, err
			}
			settleHistoryCosts(data.History, runner.Prices)
		}
	}
	return note, nil
}

// settleHistoryCosts applies today's rule to what is on record. It needs prices:
// without them old figures stay as they were rather than being guessed at.
func settleHistoryCosts(history []Result, prices map[string]Price) {
	if prices == nil {
		return
	}
	for i := range history {
		settleResultCost(&history[i], prices)
	}
}

// rejudgeAll settles answers the judge could not read earlier.
func rejudgeAll(ctx context.Context, runner *Runner, data *Data) {
	for i := range data.History {
		if len(data.History[i].Pending) > 0 {
			if runner.RejudgePending(ctx, &data.History[i]) != nil {
				return
			}
		}
	}
}
