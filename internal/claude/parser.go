package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

// Parser reads ~/.claude/projects/<slug>/<sessionId>.jsonl files
// and emits one event per session per day (the prompt count + token usage).
type Parser struct {
	Root string // ~/.claude/projects
}

// event line subset we care about
type line struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	Message   struct {
		Role  string `json:"role"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Cwd string `json:"cwd"`
}

type sessionAgg struct {
	SessionID    string
	Project      string
	Day          time.Time
	UserPrompts  int
	AssistantMsg int
	InputTokens  int
	OutputTokens int
	CacheReads   int
	FirstSeen    time.Time
	LastSeen     time.Time
}

func (p *Parser) Ingest(ctx context.Context, s *store.Store) (int, error) {
	if _, err := os.Stat(p.Root); err != nil {
		return 0, fmt.Errorf("claude projects dir not found: %w", err)
	}

	// We aggregate per (sessionId, localDate) to flatten multi-day sessions.
	agg := map[string]*sessionAgg{}

	err := filepath.WalkDir(p.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		return p.parseFile(path, agg)
	})
	if err != nil {
		return 0, err
	}

	written := 0
	for key, a := range agg {
		if a.UserPrompts == 0 && a.AssistantMsg == 0 {
			continue
		}
		title := fmt.Sprintf("%d prompts · %d responses · %s", a.UserPrompts, a.AssistantMsg, prettyProject(a.Project))
		e := store.Event{
			ID:         "claude:" + key,
			Source:     "claude_code",
			Kind:       "session",
			OccurredAt: a.LastSeen,
			Title:      title,
			Project:    prettyProject(a.Project),
			Meta: map[string]any{
				"session_id":     a.SessionID,
				"user_prompts":   a.UserPrompts,
				"assistant_msgs": a.AssistantMsg,
				"input_tokens":   a.InputTokens,
				"output_tokens":  a.OutputTokens,
				"cache_reads":    a.CacheReads,
				"first_seen":     a.FirstSeen,
				"last_seen":      a.LastSeen,
			},
		}
		if err := s.UpsertEvent(ctx, e); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func (p *Parser) parseFile(path string, agg map[string]*sessionAgg) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Derive project name from parent dir (the slug like "-Users-foo-Documents-bar")
	project := filepath.Base(filepath.Dir(path))

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			continue
		}
		if l.Timestamp.IsZero() || l.SessionID == "" {
			continue
		}
		local := l.Timestamp.In(time.Local)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
		key := l.SessionID + ":" + day.Format("2006-01-02")
		a, ok := agg[key]
		if !ok {
			a = &sessionAgg{
				SessionID: l.SessionID,
				Project:   project,
				Day:       day,
				FirstSeen: local,
				LastSeen:  local,
			}
			agg[key] = a
		}
		if local.Before(a.FirstSeen) {
			a.FirstSeen = local
		}
		if local.After(a.LastSeen) {
			a.LastSeen = local
		}
		switch l.Type {
		case "user":
			if l.Message.Role == "user" {
				a.UserPrompts++
			}
		case "assistant":
			a.AssistantMsg++
			a.InputTokens += l.Message.Usage.InputTokens
			a.OutputTokens += l.Message.Usage.OutputTokens
			a.CacheReads += l.Message.Usage.CacheReadInputTokens
		}
	}
	return nil
}

func prettyProject(slug string) string {
	if slug == "" {
		return "unknown"
	}
	s := strings.TrimPrefix(slug, "-")
	s = strings.ReplaceAll(s, "-", "/")
	return "/" + s
}
