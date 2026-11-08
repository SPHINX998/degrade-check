package relaycheckclient

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CC Switch keeps its providers in SQLite.
)

// scanCCSwitch reads CC Switch's providers read-only; this tool writes nothing
// of CC Switch's. An unreadable or reshaped database only means no extra rows:
// the live config files still work.
func scanCCSwitch(env Env) []Target {
	path := filepath.Join(env.Home, ".cc-switch", "cc-switch.db")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT name, app_type, settings_config, is_current FROM providers
		WHERE app_type IN ('codex', 'claude') ORDER BY app_type, sort_index, created_at`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []Target
	seen := map[string]bool{}
	for rows.Next() {
		var name, app, settings string
		var current bool
		if rows.Scan(&name, &app, &settings, &current) != nil {
			continue
		}
		target, ok := ccSwitchTarget(app, settings, env)
		if !ok {
			continue
		}
		target.Name, target.Source, target.Current = name, "CC Switch", current
		target = target.finish()
		if !seen[target.ID] {
			seen[target.ID] = true
			out = append(out, target)
		}
	}
	return out
}

// ccSwitchTarget reads a stored provider. One with no key of its own means
// "the official login", which the live scan already reports as its own row.
func ccSwitchTarget(app, settings string, env Env) (Target, bool) {
	doc := map[string]any{}
	if err := json.Unmarshal([]byte(settings), &doc); err != nil {
		return Target{}, false
	}
	var target Target
	switch app {
	case AppCodex:
		config, _ := doc["config"].(string)
		target = codexTarget(config, child(doc, "auth"), env)
	case AppClaude:
		target = claudeTarget(doc, nil, env)
	default:
		return Target{}, false
	}
	if target.Mode != ModeAPIKey || strings.TrimSpace(target.apiKey) == "" {
		return Target{}, false
	}
	return target, true
}
