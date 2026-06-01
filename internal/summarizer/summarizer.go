package summarizer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

type Summarizer struct {
	Binary string // e.g. "claude"
}

func New(binary string) *Summarizer { return &Summarizer{Binary: binary} }

// Summarize sends the activity as a prompt to Claude Code CLI and returns markdown.
// Uses `claude -p` (print mode), which is a one-shot non-interactive call.
func (s *Summarizer) Summarize(ctx context.Context, label string, events []store.Event) (string, error) {
	if len(events) == 0 {
		return "_No activity recorded for this period._", nil
	}

	prompt := buildPrompt(label, events)

	// 90s timeout — generous for a few hundred events
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.Binary, "-p", prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude cli: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("claude cli returned empty output (stderr: %s)", strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func buildPrompt(label string, events []store.Event) string {
	var b strings.Builder
	b.WriteString("You are a concise engineering stand-up writer. Below is my raw activity for ")
	b.WriteString(label)
	b.WriteString(`. Produce a short Markdown stand-up note with three sections:

### Highlights
- 3-6 bullets describing what I actually shipped or moved forward. Group by project where useful.

### Focus areas
- The themes or repos that took the most attention.

### Suggested next steps
- 2-4 follow-ups I should consider for tomorrow.

Keep it under 250 words. Do not invent activity that is not in the data. Refer to commits by short SHA or title only — never full diffs.

`)
	b.WriteString("RAW ACTIVITY:\n\n")
	for _, e := range events {
		ts := e.OccurredAt.Format("2006-01-02 15:04")
		line := fmt.Sprintf("- [%s] %s · %s", ts, e.Source, e.Title)
		if e.Project != "" {
			line += " · " + e.Project
		}
		if e.Status != "" {
			line += " · status=" + e.Status
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
