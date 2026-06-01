package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

// Client uses the local `gh` CLI (already authenticated) to fetch commits
// authored by the user, avoiding the need for a separate PAT.
type Client struct {
	Binary   string // default "gh"
	Username string // auto-detected from `gh api user`; can be overridden via env
}

func New(binary, username string) *Client {
	if binary == "" {
		binary = "gh"
	}
	return &Client{Binary: binary, Username: username}
}

// Available reports whether the gh CLI is installed and authenticated.
func (c *Client) Available(ctx context.Context) bool {
	if _, err := exec.LookPath(c.Binary); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, "auth", "status")
	return cmd.Run() == nil
}

// DetectUsername reads the logged-in user via `gh api user --jq .login`.
func (c *Client) DetectUsername(ctx context.Context) (string, error) {
	if c.Username != "" {
		return c.Username, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Binary, "api", "user", "--jq", ".login")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh api user: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	u := strings.TrimSpace(out.String())
	if u == "" {
		return "", fmt.Errorf("gh returned empty username")
	}
	c.Username = u
	return u, nil
}

type ghCommit struct {
	SHA    string `json:"sha"`
	URL    string `json:"url"`
	Commit struct {
		Author struct {
			Date  time.Time `json:"date"`
			Email string    `json:"email"`
			Name  string    `json:"name"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Repository struct {
		FullName string `json:"fullName"`
		Name     string `json:"name"`
		URL      string `json:"url"`
	} `json:"repository"`
}

// Ingest pulls commits authored by the user since `from` and writes them into the store.
func (c *Client) Ingest(ctx context.Context, s *store.Store, lookbackDays int) (int, error) {
	user, err := c.DetectUsername(ctx)
	if err != nil {
		return 0, err
	}

	from := time.Now().AddDate(0, 0, -lookbackDays).Format("2006-01-02")
	args := []string{
		"search", "commits",
		"--author", user,
		"--author-date", ">=" + from,
		"--sort", "author-date",
		"--order", "desc",
		"--json", "sha,commit,url,repository",
		"-L", "200",
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("gh search commits: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	var commits []ghCommit
	if err := json.Unmarshal(stdout.Bytes(), &commits); err != nil {
		return 0, fmt.Errorf("parse gh json: %w", err)
	}

	written := 0
	for _, com := range commits {
		title := firstLine(com.Commit.Message)
		e := store.Event{
			ID:         "github:" + com.SHA,
			Source:     "github",
			Kind:       "commit",
			OccurredAt: com.Commit.Author.Date,
			Title:      title,
			URL:        com.URL,
			Project:    com.Repository.FullName,
			Meta: map[string]any{
				"sha":     shortSHA(com.SHA),
				"message": com.Commit.Message,
				"repo":    com.Repository.FullName,
				"author":  com.Commit.Author.Name,
			},
		}
		if err := s.UpsertEvent(ctx, e); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}

func shortSHA(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}
