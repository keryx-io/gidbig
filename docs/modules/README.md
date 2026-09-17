# Modules

One file per internal package. Each doc: what it does + sequence diagram of the key flow.

## Feature modules

| Module | Slash commands | Purpose |
| --- | --- | --- |
| [core](core.md) | `/status` `/uptime` `/list` `/play` | Composition root, soundboard, web server, Discord lifecycle |
| [coffee](coffee.md) | `/brew` `/coffeemachine` `/setbeverage` | Coffee machine economy, orders, stats |
| [gippity](gippity.md) | `/gippity` | LLM chat over Discord messages, vision, privacy |
| [leetoclock](leetoclock.md) | – | Daily reaction-time game + scoreboard |
| [wttrin](wttrin.md) | `/wttr` `/wttrf` | Weather + forecast via wttr.in + LLM outro |
| [eso](eso.md) | `/eso` | Esoteric nonsense generator (LLM, fallback) |
| [stoll](stoll.md) | `/stoll` | Random Dr. Axel Stoll quote |
| [wardogs](wardogs.md) | `/wardogs` | WARDOGS Linux/Proton support check via doeswardogshavelinux.support |
| [gamerstatus](gamerstatus.md) | – | Rotating bot game status |
| [admin](admin.md) | `/admin` | Owner-only admin dispatch, aggregates providers |

## Shared infra

| Module | Purpose |
| --- | --- |
| [bot](bot.md) | Module interface, router, background supervisor, middleware |
| [cfg](cfg.md) | YAML config load + validation |
| [llm](llm.md) | OpenAI/OpenRouter client, personalities, language detect |
| [util](util.md) | Discord helpers, AI responder, reactions, season logic |
