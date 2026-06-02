package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port               string
	DBPath             string
	GitHubBinary       string
	GitHubUser         string // optional override; auto-detected from gh
	GitHubLookbackDays int
	ClaudeProjectsDir  string
	ClaudeBinary       string
	GmailCredsPath     string
	GmailTokenPath     string
	JiraSenderFilter   string
	JiraBaseURL        string
	OAuthRedirectURL   string
	CalendarExcludes   []string // case-insensitive substrings; events whose title matches any are skipped
}

func Load() (*Config, error) {
	_ = godotenv.Load(".env")

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Port:               getenv("PORT", "8090"),
		DBPath:             getenv("DB_PATH", "data/activity.db"),
		GitHubBinary:       getenv("GH_BINARY", "gh"),
		GitHubUser:         os.Getenv("GITHUB_USER"),
		GitHubLookbackDays: atoiDefault(os.Getenv("GITHUB_LOOKBACK_DAYS"), 30),
		ClaudeProjectsDir:  getenv("CLAUDE_PROJECTS_DIR", filepath.Join(home, ".claude", "projects")),
		ClaudeBinary:       getenv("CLAUDE_BINARY", "claude"),
		GmailCredsPath:     getenv("GMAIL_CREDENTIALS_JSON", "data/gmail_credentials.json"),
		GmailTokenPath:     getenv("GMAIL_TOKEN_JSON", "data/gmail_token.json"),
		JiraSenderFilter:   getenv("JIRA_SENDER_FILTER", "atlassian.net"),
		JiraBaseURL:        strings.TrimRight(getenv("JIRA_BASE_URL", "https://astronauts-id.atlassian.net"), "/"),
		OAuthRedirectURL:   getenv("OAUTH_REDIRECT_URL", "http://localhost:8090/oauth/gmail/callback"),
		CalendarExcludes: splitList(getenv("CALENDAR_EXCLUDE",
			"Lunch,Focus time,Daily Standup,Home,Out of office,OOO")),
	}

	return cfg, nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) GmailConfigured() bool {
	if _, err := os.Stat(c.GmailCredsPath); err != nil {
		return false
	}
	return true
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
