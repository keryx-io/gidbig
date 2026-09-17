# Gidbig 🤖

Gidbig is a Discord bot written in Go — soundboard playback in voice channels, AI chat, weather, time-based games, and more.

> **⚠️ Fair warning:** This project has evolved into an AI agent adventure park and LLM testing ground. Features have been written, reviewed, refactored, and occasionally broken by a rotating cast of AI models. The fact that it's still running is either a testament to Go's resilience or proof that LLMs are, at the very least, okayish at writing code. Possibly both. 🎢🦾

## ✨ Features

### Core

- 🏓 **Ping/Pong** — send exact lowercase `ping` or `pong`; the bot replies `Pong!` or `Ping!` and updates its game status
- 🔊 **Soundboard** — plays pre-encoded `.dca` audio files in your voice channel
  - `!<prefix>` — play a random sound from that collection
  - `!<prefix> <soundname>` — play a specific sound
  - `!list` — list all available sound collections
  - `!uptime` — owner-only uptime command
  - Files live in `audio/` as `{prefix}_{soundname}.dca`; an optional matching `.txt` file supplies the Web UI description
- 🌐 **Web UI** — browser interface to trigger sounds; requires Discord OAuth2 credentials in config
- 📊 **`/status`** — owner-only ephemeral command showing versions, uptime, memory/runtime statistics, and guild/user counts
- 🛡️ **`/admin`** — owner-only administrative commands

### 🔌 Modules

| Plugin | What it does |
|---|---|
| ☕ **coffee** | Greets users with their preferred morning beverage when they say "moin", "morgen", etc. `/setbeverage` configures it, `/brew` serves a selected drink, and `/coffeemachine` manages and reports machine state |
| 🔮 **eso** | `/eso [thema]` generates esoteric pseudoscience nonsense through the LLM, with a local fallback |
| 🎮 **gamerstatus** | Rotates the bot's Discord game/activity status every 5–15 minutes after an initial 5-minute delay |
| 🤖 **gippity** | Responds through an LLM when mentioned in an allowed guild, stores conversation history in SQLite, and provides `/gippity privacy set:on\|off` |
| 🕐 **leetoclock** | Daily 13:37 game — messages around 13:37 score by time offset; the top three at or after 13:37 rank alongside early/late categories |
| 🧌 **stoll** | `/stoll` — Stoll-related commands |
| 🐧 **wardogs** | `/wardogs` — reports whether WARDOGS officially supports Linux/Proton, read from the community tracker doeswardogshavelinux.support (10 min cache) |
| 🌤️ **wttrin** | `!wttr <location>` / `!wttrf <location>` — current weather / forecast with an LLM-generated outro |

## 🚀 Quickstart

### 1. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`:

```yaml
discord:
    token: "YOUR_DISCORD_BOT_TOKEN"
    owner_id: "YOUR_DISCORD_USER_ID"
    shard_id: 0
    shard_count: 0
web:
    oauth:
        client_id: "YOUR_OAUTH_CLIENT_ID"
        client_secret: "YOUR_OAUTH_CLIENT_SECRET"
        redirect_uri: "YOUR_REDIRECT_URI"
    session_secret: "A_STRONG_RANDOM_SESSION_SECRET"
    port: 8080
database:
    path: "gidbig.db"
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

Set `OPENAI_API_KEY` in the bot environment for the default OpenAI provider. Keep API credentials out of `config.yaml`.

OpenRouter is also supported through its OpenAI-compatible API:

```yaml
llm:
    provider: "openrouter"
    model: "anthropic/claude-sonnet-4"
    vision_model: "google/gemini-2.5-flash" # optional; defaults to model
    http_referer: "https://your-deployment.example" # optional attribution
    title: "Gidbig" # optional attribution
```

Set `OPENROUTER_API_KEY` when using OpenRouter. `llm.provider` defaults to `openai`, the OpenAI model defaults to `gpt-4o-mini`, and `llm.vision_model` defaults to `llm.model`. `llm.base_url` can optionally override either provider's API endpoint for an OpenAI-compatible gateway.

The web server starts only when `web.port`, `web.session_secret`, `web.oauth.client_id`, `web.oauth.client_secret`, and `web.oauth.redirect_uri` are set. `gippity.allowed_guilds` restricts guilds where mention-driven AI chat runs.

### 2. Add audio files 🎵

Drop `.dca` files into `./audio/` following the naming scheme `{prefix}_{soundname}.dca`. Prefix and sound name must be nonempty and cannot contain underscores.
Example: `airhorn_default.dca` → `!airhorn default`

### 3. Build and run 🔨

Local builds require Go 1.25 and a CGO-capable C compiler for SQLite.

```bash
make build
./bin/gidbig
```

Run the binary from a working directory containing `config.yaml`, `audio/`, `web/`, and a writable `plugins/` directory.

## 🛠️ Build

```bash
make build                    # Build binary → ./bin/gidbig
make test                     # go test -v ./...
make lint                     # golangci-lint run ./...
make release                  # Cross-compile linux/{amd64,arm64,386,arm} and darwin/amd64 into ./bin/release
make docker                   # Build Docker image
make update                   # go get -u -t ./... && go mod tidy
```

## 🐳 Docker

```bash
make docker

# Prepare writable persistent storage
mkdir -p data/plugins
touch data/gippity.db data/gidbig.db

# Run with Web UI on port 8080
docker run -it \
  -p 8080:8080 \
  --mount type=bind,source=$(pwd)/config.yaml,target=/gidbig/config.yaml \
  --mount type=bind,source=$(pwd)/audio,target=/gidbig/audio \
  --mount type=bind,source=$(pwd)/data/plugins,target=/gidbig/plugins \
  --mount type=bind,source=$(pwd)/data/gippity.db,target=/gidbig/gippity.db \
  --mount type=bind,source=$(pwd)/data/gidbig.db,target=/gidbig/gidbig.db \
  gidbig:$(git describe --tags)
```

The database mount must match `database.path`. `docker-compose.yml` is also available, but add equivalent writable persistence mounts before using it in production.

## 🗺️ Roadmap

- 🔀 **Finish migrating `!`-prefix commands to Discord slash commands** — soundboard and wttrin remain legacy commands
- 🏗️ **Finish module migration** — move gippity and leetoclock to `bot.Module` and centralize routing and command registration

## 📄 License

See [LICENSE](LICENSE).
