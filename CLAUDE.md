# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Working conventions

### Commit messages

Use scoped commits: `scope: description`. Scope is the subsystem, package, or area changed (e.g. `eso`, `gippity`, `audio`, `.gitignore`). No conventional-commit type prefix — the type must be inferable from the description itself. When a GitHub issue exists, include its number in parentheses after the scope: `scope (#123): description`. See https://scopedcommits.com/.

Good: `eso (#42): restore Discord mention pings in generated text`
Bad: `feat(eso): restore Discord mention pings in generated text`

### Git and GitHub
Operate git and GitHub autonomously — create branches, commit, push, open/merge PRs, comment on issues, close issues — without asking for confirmation first. The only exceptions are force-pushes to `master` and destructive operations (reset --hard, branch -D with unmerged work).

PRs target `master`. Once CI passes, squash-merge to `master` — this triggers deployment automatically. Approve the PR or comment `LGTM` / `looks good` / `/merge` / `ship it` to merge.

Prefer a linear, readable history: use `--squash` when merging PRs, rebase feature branches onto their base rather than merging, and never create merge commits.

Each PR should represent one logical, independently reversible change — don't bundle unrelated fixes or features into a single PR just because they're convenient. If a task touches multiple separable concerns, open a PR per concern.

### Versioning

`version-tag.yaml` auto-tags a new semver version **when a PR is merged into `master`**. It derives the bump level from the merged PR's labels, read straight from the `pull_request` event payload: a `breaking` or `major` label → major bump, `minor` → minor bump, otherwise → **patch** bump. The repo defines four version labels: `major`, `minor`, `breaking`, `patch`.

The workflow triggers on the PR-merge event (not on raw pushes) on purpose: this repo is **rebase-only**, so GitHub rewrites commit hashes on merge and links the new SHA to the PR only after an indexing delay. A push-triggered `commits/<sha>/pulls` lookup races that delay and silently defaults every PR to a patch bump. Reading labels from the event payload avoids the lookup entirely.

Because the bump comes from PR labels, **every PR must carry exactly one version label** — this is enforced by the `require-version-label.yaml` workflow, which fails the PR until a version label is present. Choose the label by semver intent: new feature → `minor`, bug fix / docs / chore → `patch`, backwards-incompatible change → `breaking`/`major`.

**A direct push to `master` does not auto-tag at all** (no PR-merge event fires), so it produces no version bump or release. Every version bump must go through a labeled PR.

IMPORTANT: If the user asks you to push directly to `master`, you MUST first inform them that a direct push produces no version tag or release (the bump only happens on a labeled PR merge), and confirm that is intended before pushing. For anything that should ship as a release, recommend a labeled PR instead.

### Bot commands

Prefer Discord slash commands (`/` prefix) over legacy chat commands (`!` prefix). Register slash commands via `discordgo.Session.ApplicationCommandCreate` on startup and handle them in an `InteractionCreate` handler. When implementing a new command or touching an existing legacy `!`-prefix command during any task, refactor it to a slash command. Use ephemeral responses (`discordgo.MessageFlagsEphemeral`) for owner/admin replies to avoid leaking data in public channels.

### Unit tests
After every code change, check whether the affected package has tests (`*_test.go` files). If none exist, write them before opening the PR. A fix without tests is not done.

## Commands

```bash
make build                    # Build binary to ./bin/gidbig
make test                     # go test -v ./...
make release                  # Cross-compile for linux/amd64, arm64, 386, arm and darwin/amd64
make docker                   # Build Docker image
make update                   # go get -u -t ./... && go mod tidy
```

CI runs `golangci-lint` on every push. The project uses pre-commit hooks via `.pre-commit-config.yaml` (`go fmt`, `golangci-lint`).

## Architecture

Gidbig is a Discord bot focused on soundboard playback in voice channels, with a web UI, AI chat, and a module system.

### Startup flow (`internal/core/cmd.go:StartGidbig`)

1. Load `config.yaml` → setup structured logging (text in dev, JSON in prod)
2. Scan `./audio/` for `{prefix}_{soundname}.dca` files → build `COLLECTIONS`
3. Start optional web server (requires OAuth credentials in config)
4. Pre-load all `.dca` audio into memory as Opus frame buffers
5. Open Discord WebSocket, register `onMessageCreate` and interaction handlers
6. Initialize LLM client and resolve personality
7. Initialize each module (anticheat, coffee, eso, gamerstatus, gippity, leetoclock, stoll, wttrin) via its `Init()` or `Start()` call
8. Register all slash commands via `ApplicationCommandBulkOverwrite`

### Module system (built-in)

All modules live in `internal/`, compile into the binary, and implement `bot.Module` (or use `Start(*discordgo.Session)` for legacy modules). Each registers its own event handlers and slash commands during init.

### Soundboard / audio

- Audio files: `./audio/{prefix}_{soundname}.dca` (DCA = pre-encoded Opus frames)
- `soundCollection` groups sounds under a command prefix; `soundClip` holds a weight and pre-loaded `[][]byte` buffer of Opus frames
- `onMessageCreate` matches `!{prefix} [soundname]` → `enqueuePlay()` → per-guild queue (max 6 items, mutex-protected) → `playSound()` sends Opus packets to voice channel
- Weighted random selection when no specific sound is named

### Web server (`internal/core/webserver.go`)

Standard library `net/http` (`ServeMux`) + AES-GCM encrypted cookie session store. Discord OAuth2 (identify + guilds). Session key is derived from the Web SessionSecret. Routes: `/`, `/discordLogin`, `/discordCallback`, `/playsound` (POST), `/logout`, `/api/queue`, `/health`. IP addresses are anonymized to /16 (IPv4) or /64 (IPv6).

### Key packages

| Package | Role |
|---|---|---|
| `internal/core` | Discord session, soundboard, web server |
| `internal/cfg` | YAML config loading |
| `internal/bot` | Module interface, deps, router, middleware |
| `internal/admin` | `/admin` slash command, admin subcommand registry |
| `internal/anticheat` | GamingOnLinux anti-cheat compatibility lookup with a cached CSV dataset |
| `internal/llm` | Shared OpenAI/LLM client |
| `internal/gippity` | OpenAI integration with GORM/SQLite conversation history |
| `internal/leetoclock` | Time-based joke module with SQLite datastore |
| `internal/coffee` | Greeting-reaction module |
| `internal/eso` | Conspiracy-text generator |
| `internal/stoll` | Quote/Stoll module |
| `internal/wttrin` | Weather module |
| `internal/gamerstatus` | Rotating custom status |
| `internal/util` | Shared Discord helpers and random utilities |

### Configuration

`config.yaml` (required, copy from `config.example.yaml`):
```yaml
discord:
    token: "BOT_TOKEN"
    owner_id: "USER_ID"
    shard_id: 0
    shard_count: 0
web:
    oauth:
        client_id: "OAUTH_ID"
        client_secret: "OAUTH_SECRET"
        redirect_uri: "REDIRECT_URI"
    session_secret: "base64-encoded-32-random-bytes"
    port: 8080
gippity:
    allowed_guilds:
        - "YOUR_DISCORD_GUILD_ID"
    ignored_users: []
llm:
    provider: "openai"
    model: "gpt-4o-mini"
    vision_model: ""
    personality: ""
    personality_preset: ""
dev_mode: true
```

Web server only starts when `web.port`, `web.oauth.client_id`, and `web.oauth.client_secret` are all set.

### Deployment

When a PR is merged to `master`, `pipeline.yaml` dispatches a deploy event to the `deploy-gidbig` repository and deploys the new `master` HEAD.

### Pinned dependencies

`go.mod` pins `bwmarrin/discordgo` to fork commit `yeongaori/discordgo-fork@930441e7` (2026-03-07) via a `replace` directive. Don't bump it without verifying voice playback in a DAVE-enabled Discord channel: every fork release after `c77a807b` (2026-03-08) removed the immediate `HandleExecuteTransition` call from the DAVE Welcome handler, so DAVE never activates, frames are sent without DAVE encryption, and Discord clients silently drop them — see #113. Reasoning is also in the `go.mod` comment above the `replace` line.
