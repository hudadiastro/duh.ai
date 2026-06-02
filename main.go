package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/aggregator"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/calendar"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/claude"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/config"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/github"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/gmail"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/summarizer"
)

type App struct {
	cfg      *config.Config
	store    *store.Store
	agg      *aggregator.Aggregator
	gmail    *gmail.Client
	sum      *summarizer.Summarizer
	tmpl     *template.Template
	md       goldmark.Markdown
	staticFS fs.FS
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	app := &App{
		cfg:   cfg,
		store: st,
		sum:   summarizer.New(cfg.ClaudeBinary),
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
		),
	}

	app.agg = &aggregator.Aggregator{
		Store:    st,
		Claude:   &claude.Parser{Root: cfg.ClaudeProjectsDir},
		GhDays:   cfg.GitHubLookbackDays,
		MailDays: cfg.GitHubLookbackDays,
		CalDays:  cfg.GitHubLookbackDays,
	}
	ghClient := github.New(cfg.GitHubBinary, cfg.GitHubUser)
	if ghClient.Available(context.Background()) {
		app.agg.GitHub = ghClient
	}
	if cfg.GmailConfigured() {
		app.gmail = gmail.New(cfg.GmailCredsPath, cfg.GmailTokenPath, cfg.OAuthRedirectURL, cfg.JiraSenderFilter, cfg.JiraBaseURL)
		app.agg.Gmail = app.gmail
		app.agg.Calendar = calendar.New(app.gmail, cfg.CalendarExcludes)
	}

	if err := app.loadTemplates(); err != nil {
		log.Fatalf("templates: %v", err)
	}
	app.staticFS = os.DirFS("static")

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(app.staticFS))))

	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/daily", app.handleDaily)
	mux.HandleFunc("/weekly", app.handleWeekly)
	mux.HandleFunc("/query", app.handleQuery)
	mux.HandleFunc("/api/refresh", app.handleRefresh)
	mux.HandleFunc("/api/summary", app.handleSummary)
	mux.HandleFunc("/api/notes", app.handleNotes)
	mux.HandleFunc("/api/notes/delete", app.handleDeleteNote)
	mux.HandleFunc("/oauth/gmail/start", app.handleOAuthStart)
	mux.HandleFunc("/oauth/gmail/callback", app.handleOAuthCallback)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := ":" + cfg.Port
	log.Printf("personal-ai-dashboard listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, withLogging(mux)); err != nil {
		log.Fatal(err)
	}
}

// -------------------- helpers --------------------

func (a *App) loadTemplates() error {
	funcs := template.FuncMap{
		"fmtDate":     func(t time.Time) string { return t.Format("Mon, 02 Jan 2006") },
		"fmtDateISO":  func(t time.Time) string { return t.Format("2006-01-02") },
		"fmtTime":     func(t time.Time) string { return t.Format("15:04") },
		"fmtRelative": humanizeAgo,
		"addDays":     func(t time.Time, d int) time.Time { return t.AddDate(0, 0, d) },
		"sourceLabel": sourceLabel,
		"sourceIcon":  sourceIcon,
		"upper":       strings.ToUpper,
		"hasPrefix":   strings.HasPrefix,
		"percent": func(part, total int) int {
			if total <= 0 {
				return 0
			}
			p := (part * 100) / total
			if p < 4 && part > 0 {
				return 4 // floor so a 1-event bar is still visible
			}
			return p
		},
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "…"
		},
		"safeMD": func(s string) template.HTML {
			var buf strings.Builder
			if err := a.md.Convert([]byte(s), &buf); err != nil {
				return template.HTML(template.HTMLEscapeString(s))
			}
			return template.HTML(buf.String())
		},
	}
	t, err := template.New("").Funcs(funcs).ParseGlob("templates/*.html")
	if err != nil {
		return err
	}
	a.tmpl = t
	return nil
}

func (a *App) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["GmailConfigured"] = a.cfg.GmailConfigured()
	data["GmailAuthorized"] = a.gmail != nil && a.gmail.HasToken()
	data["GitHubConfigured"] = a.agg.GitHub != nil
	data["Now"] = time.Now()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, err.Error(), 500)
	}
}

func (a *App) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render partial %s: %v", name, err)
		http.Error(w, err.Error(), 500)
	}
}

// -------------------- handlers --------------------

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/daily?date="+time.Now().Format("2006-01-02"), http.StatusFound)
}

func (a *App) handleDaily(w http.ResponseWriter, r *http.Request) {
	date := parseDate(r.URL.Query().Get("date"), time.Now())
	from := startOfDay(date)
	to := from.AddDate(0, 0, 1)

	sources := r.URL.Query()["source"]
	events, err := a.store.ListEvents(r.Context(), from, to, sources)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	kpis := summarizeKPIs(events)
	stored, _ := a.store.RefreshStatuses(r.Context())

	cachedSummary, summaryTS, _ := a.store.GetSummary(r.Context(), "daily:"+from.Format("2006-01-02"))

	a.render(w, "daily.html", map[string]any{
		"Title":           "Daily · " + from.Format("Mon, 02 Jan 2006"),
		"Date":            from,
		"Prev":            from.AddDate(0, 0, -1),
		"Next":            from.AddDate(0, 0, 1),
		"Today":           startOfDay(time.Now()),
		"Events":          events,
		"KPIs":            kpis,
		"RefreshStatuses": stored,
		"SummaryKey":      "daily:" + from.Format("2006-01-02"),
		"Summary":         cachedSummary,
		"SummaryAt":       summaryTS,
		"AllSources":      []string{"claude_code", "github", "jira", "calendar", "note"},
		"Sources":         sources,
		"SourceSelected":  len(sources) > 0,
	})
}

func (a *App) handleWeekly(w http.ResponseWriter, r *http.Request) {
	startStr := r.URL.Query().Get("start")
	var start time.Time
	if startStr == "" {
		start = startOfWeek(time.Now())
	} else {
		start = startOfWeek(parseDate(startStr, time.Now()))
	}
	end := start.AddDate(0, 0, 7)

	events, err := a.store.ListEvents(r.Context(), start, end, nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	kpis := summarizeKPIs(events)
	buckets, _ := a.store.DailyCounts(r.Context(), start, end)
	chart := buildChart(buckets, start, 7)

	cachedSummary, summaryTS, _ := a.store.GetSummary(r.Context(), "weekly:"+start.Format("2006-01-02"))

	a.render(w, "weekly.html", map[string]any{
		"Title":      "Weekly · " + start.Format("02 Jan") + "–" + end.AddDate(0, 0, -1).Format("02 Jan 2006"),
		"Start":      start,
		"End":        end.AddDate(0, 0, -1),
		"Prev":       start.AddDate(0, 0, -7),
		"Next":       start.AddDate(0, 0, 7),
		"Events":     events,
		"KPIs":       kpis,
		"Chart":      chart,
		"SummaryKey": "weekly:" + start.Format("2006-01-02"),
		"Summary":    cachedSummary,
		"SummaryAt":  summaryTS,
	})
}

func (a *App) handleQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := parseDate(q.Get("from"), time.Now().AddDate(0, 0, -6))
	to := parseDate(q.Get("to"), time.Now())
	from = startOfDay(from)
	toEnd := startOfDay(to).AddDate(0, 0, 1)

	sources := q["source"]
	statusFilter := strings.ToLower(strings.TrimSpace(q.Get("status")))
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))

	events, err := a.store.ListEvents(r.Context(), from, toEnd, sources)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if statusFilter != "" || search != "" {
		filtered := events[:0]
		for _, e := range events {
			if statusFilter != "" && !strings.Contains(strings.ToLower(e.Status), statusFilter) {
				continue
			}
			if search != "" && !strings.Contains(strings.ToLower(e.Title+" "+e.Project), search) {
				continue
			}
			filtered = append(filtered, e)
		}
		events = filtered
	}

	a.render(w, "query.html", map[string]any{
		"Title":      fmt.Sprintf("Query · %s → %s", from.Format("02 Jan"), to.Format("02 Jan 2006")),
		"From":       from,
		"To":         to,
		"Sources":    sources,
		"Status":     q.Get("status"),
		"Q":          q.Get("q"),
		"Events":     events,
		"KPIs":       summarizeKPIs(events),
		"AllSources": []string{"claude_code", "github", "jira", "calendar", "note"},
	})
}

func (a *App) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	results := a.agg.RefreshAll(ctx)
	a.renderPartial(w, "refresh_results.html", results)
}

func (a *App) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key", 400)
		return
	}
	from, to, label, err := parseSummaryKey(key)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	events, err := a.store.ListEvents(r.Context(), from, to, nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	md, err := a.sum.Summarize(r.Context(), label, events)
	if err != nil {
		http.Error(w, "summarizer failed: "+err.Error(), 500)
		return
	}
	_ = a.store.SaveSummary(r.Context(), key, md)
	a.renderPartial(w, "summary_block.html", map[string]any{
		"Summary":    md,
		"SummaryAt":  time.Now(),
		"SummaryKey": key,
	})
}

func (a *App) handleNotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		http.Error(w, "title is required", 400)
		return
	}
	date := parseDate(r.FormValue("date"), time.Now())
	project := strings.TrimSpace(r.FormValue("project"))

	// Stamp with viewed date + current clock time. So a note added now
	// while viewing yesterday becomes "yesterday at 17:42".
	now := time.Now().In(time.Local)
	ts := time.Date(date.Year(), date.Month(), date.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.Local)

	id := fmt.Sprintf("note:%d-%s", ts.UnixNano(), randomHex(3))
	e := store.Event{
		ID:         id,
		Source:     "note",
		Kind:       "note",
		OccurredAt: ts,
		Title:      title,
		Project:    project,
	}
	if err := a.store.UpsertEvent(r.Context(), e); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.renderPartial(w, "event_row", e)
}

func (a *App) handleDeleteNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := r.URL.Query().Get("id")
	if !strings.HasPrefix(id, "note:") {
		http.Error(w, "can only delete notes", 400)
		return
	}
	if err := a.store.DeleteEvent(r.Context(), id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
}

func (a *App) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if a.gmail == nil {
		http.Error(w, "gmail not configured — drop credentials JSON at data/gmail_credentials.json", 400)
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", Value: state, Path: "/", HttpOnly: true, MaxAge: 600})
	u, err := a.gmail.AuthURL(state)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (a *App) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if a.gmail == nil {
		http.Error(w, "gmail not configured", 400)
		return
	}
	c, err := r.Cookie("oauth_state")
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "oauth state mismatch", 400)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", 400)
		return
	}
	if err := a.gmail.Exchange(r.Context(), code); err != nil {
		http.Error(w, "exchange failed: "+err.Error(), 500)
		return
	}
	http.Redirect(w, r, "/daily", http.StatusFound)
}

// -------------------- utilities --------------------

func parseDate(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return def
	}
	return t
}

func startOfDay(t time.Time) time.Time {
	t = t.In(time.Local)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

func startOfWeek(t time.Time) time.Time {
	t = startOfDay(t)
	// ISO week: Monday start
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

func parseSummaryKey(key string) (from, to time.Time, label string, err error) {
	parts := strings.SplitN(key, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, "", errors.New("bad key")
	}
	d, e := time.ParseInLocation("2006-01-02", parts[1], time.Local)
	if e != nil {
		return time.Time{}, time.Time{}, "", e
	}
	switch parts[0] {
	case "daily":
		return d, d.AddDate(0, 0, 1), d.Format("Mon, 02 Jan 2006"), nil
	case "weekly":
		return d, d.AddDate(0, 0, 7), fmt.Sprintf("the week of %s", d.Format("02 Jan")), nil
	}
	return time.Time{}, time.Time{}, "", errors.New("unknown key prefix")
}

type KPIs struct {
	Total             int
	BySource          []SourceCount
	UniqueRepos       int
	UniqueTickets     int
	Meetings          int
	Notes             int
	AssistantMessages int
	InputTokens       int
	OutputTokens      int
}

type SourceCount struct {
	Source string
	Label  string
	Icon   string
	Count  int
}

func summarizeKPIs(events []store.Event) KPIs {
	out := KPIs{Total: len(events)}
	bySrc := map[string]int{}
	repos := map[string]struct{}{}
	tickets := map[string]struct{}{}
	for _, e := range events {
		bySrc[e.Source]++
		switch e.Source {
		case "github":
			if e.Project != "" {
				repos[e.Project] = struct{}{}
			}
		case "jira":
			if e.Project != "" {
				tickets[e.Project] = struct{}{}
			}
		case "calendar":
			out.Meetings++
		case "note":
			out.Notes++
		case "claude_code":
			if m, ok := e.Meta["assistant_msgs"].(float64); ok {
				out.AssistantMessages += int(m)
			}
			if m, ok := e.Meta["input_tokens"].(float64); ok {
				out.InputTokens += int(m)
			}
			if m, ok := e.Meta["output_tokens"].(float64); ok {
				out.OutputTokens += int(m)
			}
		}
	}
	out.UniqueRepos = len(repos)
	out.UniqueTickets = len(tickets)
	for src, c := range bySrc {
		out.BySource = append(out.BySource, SourceCount{Source: src, Label: sourceLabel(src), Icon: sourceIcon(src), Count: c})
	}
	sort.Slice(out.BySource, func(i, j int) bool { return out.BySource[i].Source < out.BySource[j].Source })
	return out
}

type DayBar struct {
	Day    time.Time
	Counts map[string]int
	Total  int
}

func buildChart(buckets []store.DayBucket, start time.Time, days int) []DayBar {
	idx := map[string]int{}
	out := make([]DayBar, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		out[i] = DayBar{Day: d, Counts: map[string]int{}}
		idx[d.Format("2006-01-02")] = i
	}
	for _, b := range buckets {
		i, ok := idx[b.Date.Format("2006-01-02")]
		if !ok {
			continue
		}
		out[i].Counts[b.Source] += b.Count
		out[i].Total += b.Count
	}
	return out
}

func sourceLabel(s string) string {
	switch s {
	case "claude_code":
		return "Claude Code"
	case "github":
		return "GitHub"
	case "jira":
		return "Jira"
	case "calendar":
		return "Calendar"
	case "note":
		return "Note"
	}
	return s
}

func sourceIcon(s string) string {
	switch s {
	case "claude_code":
		return "✦"
	case "github":
		return "⎇"
	case "jira":
		return "◈"
	case "calendar":
		return "▣"
	case "note":
		return "✎"
	}
	return "•"
}

func humanizeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(t).Truncate(time.Millisecond))
	})
}
