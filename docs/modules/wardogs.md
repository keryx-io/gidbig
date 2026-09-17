# wardogs

Directory `internal/wardogs`. Implements `bot.Module`.

Command `/wardogs` reports whether WARDOGS officially supports Linux/Proton. The
verdict comes from the community tracker `doeswardogshavelinux.support`, which
follows BULKHEAD's official statements.

- Endpoint: `GET https://doeswardogshavelinux.support/api/status` → `{"status":"NO"}`
- Cache: 10 min TTL for the parsed status **with single-flight grouping** — callers
  that arrive on a cold or expired cache join the running fetch instead of starting
  their own, so a burst causes one upstream request and cannot trip the host's
  `x-ratelimit-limit: 5`.
- Failures are never cached and clear the in-flight slot: the error path answers with
  a short retry hint and the next call fetches again.
- Sources in the reply: the tracker is named as the source, the Steam forum hub is
  linked for a manual check. A pinned statement is deliberately **not** cited — it
  goes stale the moment the verdict flips to YES.
- Every URL is wrapped in angle brackets (`<url>`) because Discord otherwise
  attaches a link preview, which clutters the reply.

## Reply shape

```text
❌ **No** — WARDOGS does not officially support Linux/Proton yet.

**Sources**
• Tracker: <https://doeswardogshavelinux.support/>
• Steam discussions (check manually): <https://steamcommunity.com/app/1867240/discussions/>
```

## Command flow

```mermaid
sequenceDiagram
    participant U as User
    participant D as Discord
    participant W as wardogs.Module
    participant Cache as status cache
    participant T as doeswardogshavelinux.support

    U->>D: /wardogs
    D->>W: onInteractionCreate
    W->>D: deferred response
    W->>W: go composeStatus()
    W->>Cache: statusCached()
    alt fresh cache (TTL 10m)
        Cache-->>W: cached status
    else in-flight fetch
        Cache-->>W: join running call, wait for its result
    else miss or expired
        W->>Cache: register in-flight call
        W->>T: GET /api/status
        T-->>W: {"status":"YES"|"NO"}
        W->>Cache: store status (TTL 10m), clear in-flight
    end
    W->>W: formatStatus() → verdict + source link
    W->>D: InteractionResponseEdit(verdict)
```
