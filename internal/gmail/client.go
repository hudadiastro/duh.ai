package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

type Client struct {
	CredsPath   string
	TokenPath   string
	RedirectURL string
	Sender      string
	JiraBaseURL string

	mu     sync.Mutex
	config *oauth2.Config
}

func New(credsPath, tokenPath, redirectURL, sender, jiraBaseURL string) *Client {
	return &Client{
		CredsPath:   credsPath,
		TokenPath:   tokenPath,
		RedirectURL: redirectURL,
		Sender:      sender,
		JiraBaseURL: jiraBaseURL,
	}
}

// HasCredentials reports whether the OAuth client secrets file exists.
func (c *Client) HasCredentials() bool {
	_, err := os.Stat(c.CredsPath)
	return err == nil
}

// HasToken reports whether the user has already authorized this app.
func (c *Client) HasToken() bool {
	_, err := os.Stat(c.TokenPath)
	return err == nil
}

func (c *Client) oauthConfig() (*oauth2.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.config != nil {
		return c.config, nil
	}
	b, err := os.ReadFile(c.CredsPath)
	if err != nil {
		return nil, fmt.Errorf("read gmail credentials: %w", err)
	}
	cfg, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope, calendar.CalendarReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse google credentials: %w", err)
	}
	// Force our callback URL so installed-app and web-app creds both work locally.
	cfg.RedirectURL = c.RedirectURL
	c.config = cfg
	return cfg, nil
}

// AuthURL returns the consent screen URL the user must visit.
func (c *Client) AuthURL(state string) (string, error) {
	cfg, err := c.oauthConfig()
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce), nil
}

// Exchange swaps the code for a token and persists it.
func (c *Client) Exchange(ctx context.Context, code string) error {
	cfg, err := c.oauthConfig()
	if err != nil {
		return err
	}
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return err
	}
	return c.saveToken(tok)
}

func (c *Client) saveToken(tok *oauth2.Token) error {
	if err := os.MkdirAll(dirOf(c.TokenPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(c.TokenPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

func (c *Client) loadToken() (*oauth2.Token, error) {
	f, err := os.Open(c.TokenPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (c *Client) service(ctx context.Context) (*gmail.Service, error) {
	src, err := c.TokenSource(ctx)
	if err != nil {
		return nil, err
	}
	return gmail.NewService(ctx, option.WithTokenSource(src))
}

// TokenSource returns an oauth2 TokenSource for reuse by sibling clients
// (e.g. Google Calendar) that share this OAuth client/token.
func (c *Client) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	cfg, err := c.oauthConfig()
	if err != nil {
		return nil, err
	}
	tok, err := c.loadToken()
	if err != nil {
		return nil, errors.New("google not authorized — visit /oauth/gmail/start")
	}
	return cfg.TokenSource(ctx, tok), nil
}

var jiraKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+-\d+)\b`)
var statusHints = []string{
	"To Do", "In Progress", "In Review", "Done", "Closed",
	"Blocked", "Code Review", "Ready for QA", "QA", "Resolved",
	"Reopened", "Cancelled", "Backlog", "Selected for Development",
}

// Ingest pulls recent Jira notification emails and turns each into a ticket event.
func (c *Client) Ingest(ctx context.Context, s *store.Store, lookbackDays int) (int, error) {
	svc, err := c.service(ctx)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf("from:%s newer_than:%dd", c.Sender, lookbackDays)
	written := 0
	pageToken := ""
	for {
		req := svc.Users.Messages.List("me").Q(query).MaxResults(100)
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}
		resp, err := req.Context(ctx).Do()
		if err != nil {
			return written, err
		}
		for _, m := range resp.Messages {
			msg, err := svc.Users.Messages.Get("me", m.Id).Format("metadata").MetadataHeaders("Subject", "From", "Date").Context(ctx).Do()
			if err != nil {
				continue
			}
			subj := header(msg, "Subject")
			from := header(msg, "From")
			ts := time.Unix(msg.InternalDate/1000, 0)
			key := jiraKey.FindString(subj)
			if key == "" {
				continue
			}
			status := detectStatus(subj)
			title := strings.TrimSpace(subj)
			ticketURL := ""
			if c.JiraBaseURL != "" {
				ticketURL = c.JiraBaseURL + "/browse/" + key
			}
			e := store.Event{
				ID:         "jira:" + m.Id,
				Source:     "jira",
				Kind:       "ticket",
				OccurredAt: ts,
				Title:      title,
				URL:        ticketURL,
				Project:    key,
				Status:     status,
				Meta: map[string]any{
					"message_id": m.Id,
					"from":       from,
					"subject":    subj,
				},
			}
			if err := s.UpsertEvent(ctx, e); err != nil {
				return written, err
			}
			written++
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return written, nil
}

func header(m *gmail.Message, name string) string {
	if m == nil || m.Payload == nil {
		return ""
	}
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func detectStatus(s string) string {
	lower := strings.ToLower(s)
	for _, st := range statusHints {
		if strings.Contains(lower, strings.ToLower(st)) {
			return st
		}
	}
	// Heuristics for common Jira notification verbs
	switch {
	case strings.Contains(lower, "created"):
		return "Created"
	case strings.Contains(lower, "commented"), strings.Contains(lower, "comment"):
		return "Commented"
	case strings.Contains(lower, "assigned"):
		return "Assigned"
	case strings.Contains(lower, "updated"):
		return "Updated"
	case strings.Contains(lower, "mentioned"):
		return "Mentioned"
	}
	return ""
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
