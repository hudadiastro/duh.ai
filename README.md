# Personal AI Dashboard

A local-first Go monolith that aggregates my day-to-day engineering activity — **Claude Code sessions**, **GitHub commits**, and **Jira tickets pulled from Gmail** — into a single browser dashboard with daily, weekly, and ad-hoc date-range views. An AI button generates a stand-up-ready summary by shelling out to the Claude Code CLI.

Built for the **Astro Personal AI Challenge** (April 2026).

---

## 1. Clear Intent — the problem

Every morning I open four things to figure out "what did I do yesterday?":

1. **Terminal**, to scroll through `git log --author=me --since=yesterday` across half a dozen repos.
2. **GitHub**, to remember which PRs I touched.
3. **Gmail**, to dig out Jira notification emails so I can list the tickets I moved.
4. **Claude Code**, to remember which sessions/prompts I worked on (now a real signal of where my time went).

This is **repetitive, slow, and error-prone**. Stand-ups become "uhh, I think I worked on X yesterday?" and weekly recaps rely on memory. I lose context between project switches.

**Goal:** one page that answers _"what did I actually do on date X?"_ — and on top of that, asks an AI to turn the raw activity into a readable narrative I can paste into stand-up or my weekly self-review.

## 2. What it does

Three pages, served by a single Go binary on `http://localhost:8090`:

| Page    | What you get                                                                                                                                                |
|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Daily**  | KPI strip (events by source, tokens, repos/tickets touched), AI stand-up summary button, full chronological activity feed. Day-step nav + jump-to-date.   |
| **Weekly** | 7-day bar chart stacked by source, AI weekly recap button, full event list across the week.                                                                |
| **Query**  | Multi-filter form: date range, source checkboxes, free-text search across title/project, Jira status filter. Useful for "what did I do on JIRA-123 last sprint?" |

A single **Refresh sources** button (top right) re-ingests from all configured sources concurrently and toasts per-source results.

The **AI summary** button pipes the raw activity for the selected period into `claude -p` with a fixed prompt that asks for: 3-6 highlight bullets, focus areas, and 2-4 suggested next steps. The markdown response is rendered with Goldmark and cached in SQLite so re-opening the page is instant.

## 3. How it's built

- **Single Go binary**, stdlib `net/http` + `html/template`, no framework.
- **Storage:** SQLite via `modernc.org/sqlite` (pure Go, no CGO), file at `data/activity.db`. One `events` table keyed by `source:external_id` so re-ingest is idempotent.
- **Sources:**
  - `internal/claude/` — walks `~/.claude/projects/*/*.jsonl`, rolls up per `(sessionId, localDate)` with prompt count, response count, and token usage.
  - `internal/github/` — shells out to the locally-authenticated `gh` CLI (`gh search commits --author USER --author-date '>=DATE' --json ...`). No PAT in `.env`; reuses whatever `gh auth login` already set up.
  - `internal/gmail/` — full OAuth2 flow (`golang.org/x/oauth2/google` + Gmail API v1, readonly scope). Lists `from:atlassian.net newer_than:30d`, parses subjects with a regex for `[A-Z]+-\d+` Jira keys, infers status from keywords.
- **AI integration:** `internal/summarizer/` shells out to the local `claude` CLI in `-p` (print) mode. Zero external secrets — uses the auth I already have configured in Claude Code.
- **Frontend:** server-rendered HTML, a single CSS file, HTMX for the Refresh and Generate-summary buttons (no SPA, no bundler).

## 4. Run it

```bash
cd personal-ai-dashboard
cp .env.example .env             # add tokens for the sources you want
go run .                         # http://localhost:8090
```

**Optional connectors:**

- **GitHub:** make sure `gh auth status` shows you logged in (`brew install gh && gh auth login` if not). No tokens in `.env`.
- **Gmail/Jira:**
  1. Google Cloud Console → create OAuth client (Web app), redirect URI `http://localhost:8090/oauth/gmail/callback`.
  2. Download the credentials JSON to `data/gmail_credentials.json`.
  3. Start the server, click `authorize →` in the footer.

Without those, the dashboard runs in Claude-Code-only mode — still useful as a session/time tracker.

## 5. Privacy & security

The challenge explicitly bans feeding employee/vendor/customer data into personal AI accounts. This tool is designed accordingly:

- **All data stays on disk.** SQLite lives in `./data/`. No telemetry, no analytics, no cloud sync.
- **No third-party AI calls.** The summary feature uses my locally-installed `claude` CLI (the same one I already use for engineering work). No data flows to a "personal" AI account.
- **OAuth tokens are file-scoped** (`data/gmail_token.json`, `chmod 0600`), git-ignored, never logged.
- **No vendor/customer data is ingested** — only my own commits, my own sessions, and Jira notifications addressed to me.

## 6. Multi-turn conversation

This README was iterated alongside Claude Code across the full build session. The conversation transcript captures:

1. Reading the challenge PDF and clarifying the deliverable.
2. Asking the user which data sources they had access to.
3. Deciding on the stack (Go stdlib + html/template + SQLite, no framework).
4. Restructuring templates after the first design hit a template-name collision (`{{define "page"}}` shared across files).
5. Adding missing template helpers (`percent`, `safeMD`) after the first build.
6. End-to-end smoke testing each route, then the AI summary path.

The transcript is included with the submission as part of the "Multi Conversation Prompt" rubric item.

## File map

```
personal-ai-dashboard/
├── main.go                       # HTTP server, routes, template helpers
├── internal/
│   ├── config/                   # .env + defaults
│   ├── store/                    # SQLite layer (events, summaries, refresh_log)
│   ├── claude/                   # JSONL session parser
│   ├── github/                   # Search Commits API client
│   ├── gmail/                    # OAuth2 + Jira email parser
│   ├── aggregator/               # Concurrent refresh across all sources
│   └── summarizer/               # Claude CLI wrapper
├── templates/                    # layout, daily, weekly, query partials
├── static/                       # one CSS file
├── data/                         # SQLite db + Gmail token (gitignored)
└── .env.example
```

---

_Built in a single afternoon with Claude Code as the pair-programmer. The fact that this dashboard tracks Claude Code sessions while being built by Claude Code is — to my mild amusement — entirely on purpose._
