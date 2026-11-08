package relaycheckclient

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxPageBody      = 1 << 20
	idleExitAfter    = 3 * time.Minute
	configCacheFor   = 5 * time.Minute
	historyShownRows = 200
)

// App is the page's backend. It listens on loopback only and answers only a
// caller holding the token from the URL it opened itself, so another website in
// the same browser cannot start a paid test or read the results.
type App struct {
	Env       Env
	Store     *Store
	Runner    *Runner
	Scheduler Scheduler
	Exe       string
	Version   string
	// KeepAlive is for a tray program: closing the window is not leaving.
	KeepAlive bool
	// OpenURL opens a link in the user's browser.
	OpenURL func(string)
	// NetLog is the record of what this run sent out.
	NetLog *NetLog

	token string
	host  string

	mu       sync.Mutex
	lastSeen time.Time
	job      jobState
	config   *ServerConfig
	configAt time.Time
	cancel   context.CancelFunc

	modelCache map[string]cachedModels
}

type jobState struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
}

// Listen prepares the page on a free loopback port and returns its URL.
func (a *App) Listen() (net.Listener, string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, "", err
	}
	a.token = hex.EncodeToString(raw[:])
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	a.host = listener.Addr().String()
	return listener, "http://" + a.host + "/?t=" + a.token, nil
}

// Serve runs until the page has been closed for a while and no test is running.
func (a *App) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.lastSeen = time.Now()
	a.cancel = cancel
	a.mu.Unlock()
	server := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = server.Close()
				return
			case <-ticker.C:
				a.mu.Lock()
				idle := !a.KeepAlive && !a.job.Running && time.Since(a.lastSeen) > idleExitAfter
				a.mu.Unlock()
				if idle {
					_ = server.Close()
					return
				}
			}
		}
	}()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Handler is the page and its API behind the local guard.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page)
	api := func(pattern string, handler http.HandlerFunc) { mux.HandleFunc(pattern, a.guardAPI(handler)) }
	api("GET /api/state", a.state)
	api("GET /api/job", a.jobStatus)
	api("POST /api/test", a.startTest)
	api("POST /api/settings", a.saveSettings)
	api("GET /api/models", a.models)
	api("GET /api/net", a.netRecord)
	api("POST /api/open", a.openLink)
	return a.guardHost(mux)
}

// guardHost refuses a request naming any other host: a website can rebind its
// own name to 127.0.0.1, but its Host header still gives it away.
func (a *App) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.host != "" && r.Host != a.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *App) tokenOK(given string) bool {
	return a.token != "" && subtle.ConstantTimeCompare([]byte(given), []byte(a.token)) == 1
}

func (a *App) guardAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.tokenOK(r.Header.Get("X-Check-Token")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+a.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		a.mu.Lock()
		a.lastSeen = time.Now()
		a.mu.Unlock()
		next(w, r)
	}
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	if !a.tokenOK(r.URL.Query().Get("t")) {
		http.Error(w, "请从程序打开的页面进入", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
	_, _ = w.Write(pageHTML)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// serverConfig is check-api's config, remembered briefly. It is only ever a
// convenience: failing it falls back to the built-in promotion.
func (a *App) serverConfig(ctx context.Context) (ServerConfig, bool) {
	a.mu.Lock()
	cached, at := a.config, a.configAt
	a.mu.Unlock()
	if cached != nil && time.Since(at) < configCacheFor {
		return *cached, true
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	config, err := NewRemoteJudge(a.Runner.Server, a.Runner.Client).Config(ctx)
	if err != nil {
		return ServerConfig{}, false
	}
	a.mu.Lock()
	a.config, a.configAt = &config, time.Now()
	a.mu.Unlock()
	return config, true
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	data, err := a.Store.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	targets := Scan(a.Env)
	config, online := a.serverConfig(r.Context())
	promo := config.Promo
	if promo.URL == "" {
		promo = builtInPromo()
	}
	if online && a.Runner.Prices == nil {
		a.Runner.Prices = config.Prices
	}
	settleHistoryCosts(data.History, a.Runner.Prices)
	history := data.History
	if len(history) > historyShownRows {
		history = history[len(history)-historyShownRows:]
	}
	shown := make([]Result, len(history))
	copy(shown, history)
	for i := range shown {
		shown[i].Pending = nil // the answers stay on disk, not on the page
	}
	a.mu.Lock()
	job := a.job
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"version": a.Version, "latest_version": config.LatestVersion, "online": online,
		"targets": targets, "settings": data.Settings, "history": shown, "promo": promo,
		"month_spend": monthSpend(data.History, data.Settings, time.Now()), "seal": a.Store.SealLevel(), "job": job,
	})
}

func (a *App) jobStatus(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	job := a.job
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, job)
}

func (a *App) startTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPageBody)).Decode(&req); err != nil || len(req.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有选要测的渠道"})
		return
	}
	wanted := map[string]bool{}
	for _, id := range req.IDs {
		wanted[id] = true
	}
	data, err := a.Store.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var targets []testJob
	for _, target := range Scan(a.Env) {
		if wanted[target.ID] && target.Testable() {
			for _, model := range modelsFor(target, data.Settings) {
				targets = append(targets, testJob{target: target, model: model})
			}
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "选中的渠道都测不了"})
		return
	}
	a.mu.Lock()
	if a.job.Running {
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "上一轮测试还没结束"})
		return
	}
	a.job = jobState{Running: true, Total: len(targets)}
	a.mu.Unlock()
	go a.runTests(targets)
	writeJSON(w, http.StatusAccepted, map[string]int{"total": len(targets)})
}

func (a *App) runTests(jobs []testJob) {
	defer func() {
		a.mu.Lock()
		a.job.Running, a.job.Current = false, ""
		a.mu.Unlock()
	}()
	ctx := context.Background()
	for _, job := range jobs {
		a.mu.Lock()
		a.job.Current = job.target.Name + " · " + job.model
		a.mu.Unlock()
		result := a.Runner.Run(ctx, job.target, job.model, false)
		_, _ = a.Store.Update(func(d *Data) error { d.History = append(d.History, result); return nil })
		a.mu.Lock()
		a.job.Done++
		a.mu.Unlock()
	}
	_, _ = a.Store.Update(func(d *Data) error { rejudgeAll(ctx, a.Runner, d); return nil })
}

type testJob struct {
	target Target
	model  string
}

type cachedModels struct {
	list ModelList
	at   time.Time
}

// models answers one channel's model list. It costs nothing, but a slow relay
// must not be asked again on every page refresh.
func (a *App) models(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a.mu.Lock()
	cached, ok := a.modelCache[id]
	a.mu.Unlock()
	if ok && time.Since(cached.at) < modelListCacheFor {
		writeJSON(w, http.StatusOK, cached.list)
		return
	}
	for _, target := range Scan(a.Env) {
		if target.ID != id {
			continue
		}
		config, _ := a.serverConfig(r.Context())
		list := a.Runner.ListModels(r.Context(), target, config)
		if list.Error == "" {
			a.mu.Lock()
			if a.modelCache == nil {
				a.modelCache = map[string]cachedModels{}
			}
			a.modelCache[id] = cachedModels{list: list, at: time.Now()}
			a.mu.Unlock()
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "没有这个渠道"})
}

// netRecord answers the network record.
func (a *App) netRecord(w http.ResponseWriter, _ *http.Request) {
	events := []NetEvent{}
	if a.NetLog != nil {
		events = a.NetLog.Events()
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// openLink sends a link to the user's browser; the app window stays the app.
func (a *App) openLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPageBody)).Decode(&req); err != nil || !strings.HasPrefix(req.URL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "链接无效"})
		return
	}
	if a.OpenURL != nil {
		a.OpenURL(req.URL)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var settings Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPageBody)).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "设置格式不对"})
		return
	}
	settings.normalize()
	for id, model := range settings.Models {
		if model = strings.TrimSpace(model); model == "" {
			delete(settings.Models, id)
		} else {
			settings.Models[id] = model
		}
	}
	var scheduleErr error
	if settings.ScheduleEnabled {
		scheduleErr = a.Scheduler.Install(a.Exe, settings.IntervalHours)
	} else {
		scheduleErr = a.Scheduler.Remove()
	}
	if scheduleErr != nil {
		// A schedule the OS refused must not read as on.
		settings.ScheduleEnabled = false
	}
	data, err := a.Store.Update(func(d *Data) error { d.Settings = settings; return nil })
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if scheduleErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": scheduleErr.Error(), "settings": data.Settings})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": data.Settings})
}

// ReinstallSchedule keeps the task pointing at this file after the portable
// program was moved.
func (a *App) ReinstallSchedule() {
	data, err := a.Store.Load()
	if err == nil && data.Settings.ScheduleEnabled {
		_ = a.Scheduler.Install(a.Exe, data.Settings.IntervalHours)
	}
}
