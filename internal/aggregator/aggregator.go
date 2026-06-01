package aggregator

import (
	"context"
	"fmt"
	"sync"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/calendar"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/claude"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/github"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/gmail"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

type Aggregator struct {
	Store    *store.Store
	Claude   *claude.Parser
	GitHub   *github.Client   // may be nil
	Gmail    *gmail.Client    // may be nil
	Calendar *calendar.Client // may be nil
	GhDays   int
	MailDays int
	CalDays  int
}

type SourceResult struct {
	Source string
	Count  int
	Err    error
}

// RefreshAll runs all configured sources concurrently and returns per-source results.
func (a *Aggregator) RefreshAll(ctx context.Context) []SourceResult {
	var wg sync.WaitGroup
	out := make([]SourceResult, 0, 3)
	var mu sync.Mutex
	record := func(r SourceResult) {
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
		status := "ok"
		msg := fmt.Sprintf("%d events", r.Count)
		if r.Err != nil {
			status = "error"
			msg = r.Err.Error()
		}
		_ = a.Store.RecordRefresh(ctx, r.Source, status, msg)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := a.Claude.Ingest(ctx, a.Store)
		record(SourceResult{Source: "claude_code", Count: n, Err: err})
	}()

	if a.GitHub != nil && a.GitHub.Available(ctx) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := a.GitHub.Ingest(ctx, a.Store, a.GhDays)
			record(SourceResult{Source: "github", Count: n, Err: err})
		}()
	}

	if a.Gmail != nil && a.Gmail.HasToken() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := a.Gmail.Ingest(ctx, a.Store, a.MailDays)
			record(SourceResult{Source: "jira", Count: n, Err: err})
		}()
	}

	if a.Calendar != nil && a.Calendar.HasToken() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := a.Calendar.Ingest(ctx, a.Store, a.CalDays)
			record(SourceResult{Source: "calendar", Count: n, Err: err})
		}()
	}

	wg.Wait()
	return out
}
