package calendar

import (
	"context"
	"fmt"
	"strings"
	"time"

	calapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/gmail"
	"github.com/rahmadnurhuda/personal-ai-dashboard/internal/store"
)

// Client reuses the Gmail OAuth flow (same Google project + token) to pull
// calendar events the user attended or was invited to.
type Client struct {
	Auth     *gmail.Client // shared OAuth token source
	Excludes []string      // case-insensitive substrings; titles matching any are skipped
}

func New(auth *gmail.Client, excludes []string) *Client {
	return &Client{Auth: auth, Excludes: excludes}
}

// HasToken delegates to the underlying gmail client.
func (c *Client) HasToken() bool { return c.Auth != nil && c.Auth.HasToken() }

// Ingest pulls events from the user's primary calendar between (now - lookback)
// and (now + 1 day). Includes today's upcoming meetings so the dashboard
// answers "what do I have left today?" too.
func (c *Client) Ingest(ctx context.Context, s *store.Store, lookbackDays int) (int, error) {
	if c.Auth == nil {
		return 0, fmt.Errorf("calendar: gmail client not configured")
	}
	src, err := c.Auth.TokenSource(ctx)
	if err != nil {
		return 0, err
	}
	svc, err := calapi.NewService(ctx, option.WithTokenSource(src))
	if err != nil {
		return 0, fmt.Errorf("calendar service: %w", err)
	}

	from := time.Now().AddDate(0, 0, -lookbackDays)
	to := time.Now().AddDate(0, 0, 1)

	written := 0
	pageToken := ""
	for {
		req := svc.Events.List("primary").
			TimeMin(from.Format(time.RFC3339)).
			TimeMax(to.Format(time.RFC3339)).
			SingleEvents(true).
			OrderBy("startTime").
			MaxResults(250).
			ShowDeleted(false)
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}
		resp, err := req.Context(ctx).Do()
		if err != nil {
			return written, fmt.Errorf("calendar events.list: %w", err)
		}
		for _, ev := range resp.Items {
			start, allDay := eventStart(ev)
			if start.IsZero() {
				continue
			}
			title := ev.Summary
			if title == "" {
				title = "(no title)"
			}
			if c.titleExcluded(title) {
				continue
			}
			status := rsvpStatus(ev)

			meta := map[string]any{
				"organizer":  organizer(ev),
				"attendees":  len(ev.Attendees),
				"all_day":    allDay,
				"hangout":    ev.HangoutLink,
				"event_type": ev.EventType,
			}
			if ev.Location != "" {
				meta["location"] = ev.Location
			}

			e := store.Event{
				ID:         "calendar:" + ev.Id,
				Source:     "calendar",
				Kind:       "meeting",
				OccurredAt: start,
				Title:      title,
				URL:        ev.HtmlLink,
				Project:    organizer(ev),
				Status:     status,
				Meta:       meta,
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

func (c *Client) titleExcluded(title string) bool {
	if len(c.Excludes) == 0 {
		return false
	}
	low := strings.ToLower(title)
	for _, pat := range c.Excludes {
		if strings.Contains(low, strings.ToLower(pat)) {
			return true
		}
	}
	return false
}

func eventStart(ev *calapi.Event) (time.Time, bool) {
	if ev == nil || ev.Start == nil {
		return time.Time{}, false
	}
	if ev.Start.DateTime != "" {
		t, err := time.Parse(time.RFC3339, ev.Start.DateTime)
		if err == nil {
			return t.In(time.Local), false
		}
	}
	if ev.Start.Date != "" {
		t, err := time.ParseInLocation("2006-01-02", ev.Start.Date, time.Local)
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func rsvpStatus(ev *calapi.Event) string {
	// Find the current user's attendance status (self == true).
	for _, a := range ev.Attendees {
		if a.Self {
			switch a.ResponseStatus {
			case "accepted":
				return "Accepted"
			case "declined":
				return "Declined"
			case "tentative":
				return "Tentative"
			case "needsAction":
				return "Pending"
			}
		}
	}
	if ev.Status == "cancelled" {
		return "Cancelled"
	}
	return ""
}

func organizer(ev *calapi.Event) string {
	if ev.Organizer == nil {
		return ""
	}
	if ev.Organizer.DisplayName != "" {
		return ev.Organizer.DisplayName
	}
	return ev.Organizer.Email
}
