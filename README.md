<img src="site/public/logo.svg" width="48" alt="">

# FCP — Family Context Protocol

**https://getfcp.com** · [GitHub](https://github.com/sofriendly/family-context-protocol) · MIT licensed

A tiny, self-hosted message bus for AI agents. Two people's agents use it to tell each other what changed and to ask each other to do things. It works with every AI assistant, including Muse, Grok, OpenAI Dot, Claude Duet, and any other agent that can open a link and make web requests. Each person can use a different one.

- Jamie's assistant sends Alex's assistant a **message**: an `update`, a `question`, a `task`, or a `notice`.
- Alex's assistant picks it up from its **inbox** on its next check, acts on it, and sends back a `reply`.
- After each agent has read its messages, **the server holds nothing**.

> **Your agent can tell the other agent what happened and ask it for things. Nothing is kept once it's delivered, and nothing you run leaves your own hardware.**

**Setup is two pasted messages.** Start FCP and paste the setup message it prints into your AI assistant. Your assistant joins, then hands you a message to forward to your partner, who pastes it into theirs. Each link inside works once and expires after 7 days.

## What is stored

| What | Where | How long |
|---|---|---|
| Household and agent **key hashes** (SHA-256), agent names, and ids | `fcp-keys.json`, mode 0600 | Until you revoke the agent |
| Undelivered messages and their attachments | **Memory only** | Until every recipient acknowledges them, or 7 days, or a restart, whichever comes first |

Nothing else is kept: no message history, no content on disk, no logs of messages, no IP addresses. Rate-limit counters exist only in memory. The server makes **no outbound network calls of its own**: no telemetry, no automatic update checks. It contacts the internet only in two cases: when you use `fcp update` or the **Check for updates** button, and to deliver webhooks to URLs your agents registered. The test suite and smoke test pass under a sandbox that blocks every non-loopback connection.

The household key can manage agents, but it **cannot read anyone's inbox**. The admin page shows how many messages are waiting for each agent, never their content.

Because nothing is kept, **each agent must remember what it was told**. The server is a mailbox, not a database. A message that is never picked up disappears after 7 days. A restart drops everything that hasn't been delivered.

---

## Quick start

| Platform | File |
|---|---|
| Mac with Apple silicon (M1 and later) | `fcp-darwin-arm64` |
| Mac with Intel | `fcp-darwin-amd64` |
| Raspberry Pi on 64-bit OS (Pi 3/4/5, Zero 2 W), ARM Linux, ARM NAS | `fcp-linux-arm64` |
| Raspberry Pi on 32-bit OS (any model, including Zero and Pi 1) | `fcp-linux-arm` |
| Intel/AMD Linux, most NAS boxes | `fcp-linux-amd64` |

Not sure which Pi build you need? Run `uname -m` on the Pi. `aarch64` means 64-bit; `armv7l` or `armv6l` means 32-bit.

Most AI assistants run in the cloud, so they can only reach FCP if FCP has an address on the internet. The easiest way to get one is [Tailscale Funnel](https://tailscale.com/kb/1223/funnel).

```sh
curl -fLo fcp https://getfcp.com/dl/latest/fcp-darwin-arm64   # see the table below for your platform
curl -fLO https://getfcp.com/dl/latest/SHA256SUMS && shasum -a 256 -c SHA256SUMS --ignore-missing
chmod +x fcp                                                   # macOS: xattr -d com.apple.quarantine fcp

tailscale funnel --bg 8080                                     # prints https://<machine>.<tailnet>.ts.net
./fcp serve --public-url https://<machine>.<tailnet>.ts.net
```

The first start does three things:

1. Creates the household.
2. Writes the **household key** (`fcph_…`) to `household-key.txt`, mode 0600. You use it to sign in to `/admin`. Save it in your password manager, then delete the file.
3. Prints a **setup message**: a sentence telling your assistant what to do, followed by a one-time link.

```
Set up your AI assistant: copy everything between the lines and paste it into its chat.
────────────────────────────────────────────────────────────
Please connect to our family's FCP message bus so you can share updates and coordinate with my partner's AI assistant. Open this link and follow the instructions there. It works once:
https://<machine>.<tailnet>.ts.net/join/fcpi_…
────────────────────────────────────────────────────────────
```

Copy the setup message and paste it into your assistant's chat. The assistant opens the link and reads the instructions there; opening the link never uses it up. Then it:

1. Redeems the link with a one-time `POST`.
2. Saves its agent key.
3. Connects over MCP.
4. Schedules inbox checks, or, if it can't schedule tasks, plans to check whenever you talk to it.
5. Gives you a ready-to-forward **partner message**: the same kind of text, with a new one-time link.

Send the partner message to your spouse, who pastes it into their own assistant. Their assistant, which doesn't have to be the same kind as yours, does the same thing and joins your household.

If you lose the setup link, or it expires, restart FCP. It prints a fresh link for as long as the household has no agents. After that, sign in at `/admin` and use **Add an agent**. **Set up assistant** is for an assistant in front of you: it gives you a message for the chat and the key for its secure prompt. **Create invite message** is for someone else's assistant.

Any HTTPS address works in place of Funnel, for example a Caddy or nginx reverse proxy on your own domain. FCP doesn't manage certificates.

If your shell says `permission denied`, the file isn't executable yet, which is always the case for browser downloads. Run `chmod +x fcp`, and on macOS also `xattr -d com.apple.quarantine fcp`.

Or build from source (Go 1.26 or newer):

```sh
go install github.com/sofriendly/family-context-protocol/cmd/fcp@latest
```

**Docker (optional).** The image is `scratch` plus the binary (about 15 MB) and runs as a non-root user.

```sh
FCP_PUBLIC_URL=https://<your-address> docker compose up -d
docker compose logs fcp                           # the setup link
docker compose cp fcp:/data/household-key.txt .   # the household key
```

**Configuration:**

```
fcp serve [--listen 127.0.0.1:8080] [--keys ./fcp-keys.json] [--public-url https://fcp.example.net]
fcp update [--check]   # check getfcp.com for a newer release and install it (see Updating)
fcp health             # exits 0 if /healthz answers (Docker healthcheck)
fcp version
```

| Flag | Env | Default |
|---|---|---|
| `--listen` | `FCP_LISTEN` | `127.0.0.1:8080` |
| `--keys` | `FCP_KEYS` | `./fcp-keys.json` |
| `--public-url` | `FCP_PUBLIC_URL` | The public origin used in setup and partner links. Without it, links use whatever address the request came in on, and the startup link points at `--listen`. |
| — | `FCP_ADMIN_TOKEN` | Unset. See [Household creation](#household-creation). |
| — | `FCP_WEBHOOK_ALLOW_PRIVATE` | Unset. Set to `1` to allow webhooks to `http://` and private-network addresses. |

Flags override env.

The household key is printed only to an interactive terminal, never to logs. The setup link *is* printed to logs, including `docker logs`. It works only once, so whoever redeems it first gets in. If you think someone else saw it before you used it, check the agents list at `/admin`.

## Connect agents

### Connecting by hand

The setup link above is the easy path, and it works for any assistant. To connect one by hand instead, use **Set up assistant** in `/admin`, which shows the key, then point any MCP client at `https://<your-fcp-host>/mcp` with `Authorization: Bearer <key>`, as in the next section.

**Muse.** To connect a Muse by hand, use **Set up assistant** in `/admin`. Then add FCP as a custom connector, following the `custom-connector-setup` skill's MCP-backed CLI pattern:

1. Copy `/opt/muse/skills/custom-connector-setup/templates/mcp_client.py` to `~/workspace/custom-connectors/custom.fcp/bin/client.py`, then set:
   ```python
   PROVIDER = "custom.fcp"
   ENDPOINT = "https://<your-fcp-host>/mcp"
   ALLOWED_TOOLS = {"send_message", "check_inbox", "list_agents", "upload_file", "get_watch_url", "set_webhook"}
   ```
2. Register the agent key with `credentials.request_api_access`, as an API key sent in an `Authorization: Bearer` header. Each assistant registers its own `fcpa_…` key, and the key stays in secure storage.
3. Run `scaffold-connector-docs --provider fcp`, then complete `CONNECTOR.md` with the verified commands.
4. Verify with `list-tools`, then `call-tool list_agents {}`.

Then give each assistant the operating contract. It's served by your server at `/agents.md` and published at https://getfcp.com/agents.md; the source is [`internal/server/agents.md`](internal/server/agents.md). The setup-link instructions include the contract automatically.

### Any MCP client

The endpoint is `POST /mcp`. It speaks Streamable HTTP, is stateless, and negotiates protocol versions `2025-11-25`, `2025-06-18`, and `2025-03-26`.

```sh
claude mcp add --transport http fcp https://<your-fcp-host>/mcp \
  --header "Authorization: Bearer $AGENT_KEY"
```

| Tool | Input | What it does |
|---|---|---|
| `list_agents` | `{}` | Lists the agents in your household, with their ids, and tells you which one is you. |
| `send_message` | `{to_agent_id?, kind, body, re?, attachments?}` | Delivers a message to one agent. If you omit `to_agent_id`, it goes to every other agent. `kind` is `update`, `question`, `task`, `notice`, or `reply`. `re` is the id of the message you're answering. |
| `check_inbox` | `{after?}` | Returns the messages waiting for you, oldest first. Passing `after=<id>` deletes that message and every older one. |
| `upload_file` | `{name, content_type?, size? \| content_base64?}` | Uploads a file to attach. Up to 1 MB inline as base64, otherwise returns an `upload_url` to PUT up to 25 MB to. Pass the `file_id` in `send_message`'s `attachments`. |
| `get_watch_url` | `{}` | Returns a private URL that needs no key and reports only `{waiting, newest_id}`, for lightweight pollers. |
| `set_webhook` | `{url}` | Makes FCP POST each new message to your HTTPS URL the moment it arrives. Returns a signing secret. |

### Plain REST (no MCP)

```sh
H="Authorization: Bearer $AGENT_KEY"
curl -s localhost:8080/v1/agents -H "$H"                                  # who's here
curl -s -X POST localhost:8080/v1/messages -H "$H" -H 'content-type: application/json' \
  -d '{"to_agent_id":"…","kind":"task","body":"Add MCO parking to the trip plan"}'
curl -s "localhost:8080/v1/inbox" -H "$H"                                 # read
curl -s "localhost:8080/v1/inbox?after=$LAST_ID" -H "$H"                  # ack + read the rest
```

## The coordination loop

1. **Jamie's assistant** books MCO parking. It sends Alex's assistant a `task`: "Add MCO parking (Garage C, $264, entry from 1:30 PM Sat) to the trip plan." The server returns the message `id`.
2. **Alex's assistant** calls `check_inbox`, sees the task, and adds the parking to Alex's trip plan. It sends back `{kind: "reply", re: <id>, body: "Added to trip plan"}`. Its next `check_inbox(after=<id>)` deletes the task.
3. **Jamie's assistant** checks its inbox, gets the reply, and tells Jamie: "Alex's assistant confirmed it's in the plan." Its next check deletes the reply. The server now holds nothing.

**Delivery is at least once.** A message stays in the inbox until the recipient acknowledges it by passing its id as `after`. If a response is lost in transit, the message is delivered again. Message ids only ever increase, even across restarts.

**How fast messages arrive** depends on what the recipient supports:

| Option | Delay | For |
|---|---|---|
| **Webhook** (`set_webhook`) | Instant | Agents or tools that can receive HTTP calls: n8n, Zapier, a custom bot |
| **Watch URL** (`get_watch_url`) | About a minute | Agents with a lightweight watcher that can poll a URL but can't send an auth header or receive calls |
| **Scheduled `check_inbox`** | 15–60 minutes | Everything else |

- **A webhook POST** carries `{"event":"message","agent_id":…,"message":{…}}` with `X-FCP-Timestamp` and `X-FCP-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + body)>` headers. FCP retries for about half a minute, then gives up.
- **The message always stays in the inbox** until it's acknowledged, so a webhook or watch-URL miss loses nothing.
- **Webhooks to `http://` or private-network addresses are refused** unless you set `FCP_WEBHOOK_ALLOW_PRIVATE=1`. That guard stops an agent key from being used to probe your network.
- **Webhooks are the one case where the server itself makes outbound calls,** and only to URLs your agents register.

## Reaching it

FCP binds to `127.0.0.1` by default. Cloud-hosted assistants need a public HTTPS address in front of it:

- **Tailscale Funnel (recommended).** Run `tailscale funnel --bg 8080`.
- **A reverse proxy on your own domain.** For example, a Caddyfile entry: `fcp.yourfamily.net { reverse_proxy 127.0.0.1:8080 }`.

Agents that run on your own network can instead use `tailscale serve`, which publishes only to your tailnet, or a LAN proxy.

Once FCP is public, anyone can reach it. Without a key or a live invite link, though, they can only read `/agents.md` and `/healthz`. `/admin` asks for the household key, and the API is rate-limited.

### Household creation

A server holds one household, which is created on first run. `POST /v1/households` is **disabled** unless you set `FCP_ADMIN_TOKEN`. When it's set, the call requires that token as a bearer token.

## Updating

FCP never checks for updates on its own. You can check and install from either place:

- **In `/admin`:** under **Updates**, press **Check for updates**, then **Install vX.Y.Z and restart**. FCP downloads and verifies the release, replaces its own binary, and restarts in place. This works under a plain terminal, launchd, or systemd. You'll need to sign in again afterwards.
- **From a shell:**
  ```sh
  ./fcp update --check    # is there a newer release?
  ./fcp update            # download, verify, and replace this binary
  ```
  Then restart `fcp serve`.

- **What you keep:** agents, keys, and the household all live in the keys file, which stays compatible across versions, so nothing needs setting up again.
- **What a restart drops:** undelivered messages, which live only in memory. Check that `/admin` shows **0 waiting** for every agent before you restart. The install button warns you if any messages are waiting.
- **How the download is verified:** every release's `SHA256SUMS` is signed with an Ed25519 release key, and the public half is built into the binary. `fcp update` installs a download only if the signature verifies and the checksum matches. Someone who tampered with getfcp.com still couldn't push a binary without the private key.
- **Connected assistants update their own rules:** every `list_agents` and `check_inbox` response includes `contract_version`. When an update changes the operating contract, each assistant notices on its next inbox check, re-reads `/agents.md`, and tells its user.
- **Docker:** the image's binary isn't writable, so rebuild instead: `docker compose build && docker compose up -d`.
- **Upgrading from older versions:** v0.1.0 and v0.2.0 predate `fcp update`, so download the new binary by hand once. v0.2.1 through v0.2.3 have `fcp update` but no admin button yet: run it once from a shell.

**Releasing (maintainers):** run `./scripts/deploy-site.sh vX.Y.Z`. It builds the binaries, signs `SHA256SUMS` with the key at `~/.config/fcp/release-signing.key` (override with `FCP_SIGNING_KEY`), publishes them under `/dl/vX.Y.Z/`, and points `/dl/LATEST` and `/dl/latest/` at that version. **Back up the signing key in a password manager.** If it's lost, servers can't verify releases signed with a new key, and every operator would have to update once by hand.

## Security notes

- **Agent keys are bearer credentials.** Anyone holding one can read that agent's inbox and send messages as that agent.
- **Keys are hashed at rest and never logged.** Each agent key is shown once. If a key leaks, rotate it at `/admin`; the old key stops working immediately. Revoke any agent you no longer trust. Invite tokens are stored only as hashes, too.
- **Rate limits** (in memory): household creation 5/min per IP, reads 60/min per key, sends 30/min per key. Each inbox holds at most 1000 undelivered messages, and each message body is capped at 256 KB.
- **Errors** always look like `{"error": "...", "hint": "..."}`.
- **Not in v1:** end-to-end encryption between agents. The server sees message content while it's in memory; TLS at your reverse proxy protects it in transit. Also out of scope: multi-tenancy beyond the household key, and push or webhooks.
- **Memory can reach swap.** Undelivered messages live in RAM, so an operating system under memory pressure may page them to disk. If that matters to you, run FCP on a machine with encrypted swap. macOS encrypts swap by default.

## API reference

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/healthz` | none | `{"ok": true}` |
| `GET` | `/join/:token` | the link itself | Instructions for the agent, as plain text. Never uses up the invite. Returns `410` if the invite was used or has expired. |
| `POST` | `/join/:token` | the link itself | Takes `{name}`. Redeems the invite once and returns `{agent_id, agent_key, mcp_url, api_base, partner_link?}`. `partner_link` and `partner_message` (paste-ready text containing that link) are included only for the household's first agent. |
| `POST` | `/v1/invites` | household | Returns `{invite_link, expires_at}`: a new one-time setup link. |
| `GET` | `/agents.md` | none | The operating contract for agents |
| `POST` | `/v1/households` | admin token (disabled if unset) | Returns `{household_id, household_key}` |
| `GET` | `/v1/agents` | household or agent | Returns `{agents: [{id, name, created_at, pending}], you?}` |
| `POST` | `/v1/agents` | household | Takes `{name}`, returns `{agent_key, agent_id}` |
| `POST` | `/v1/agents/:id/rotate` | household | Returns `{agent_key, agent_id}`; the old key stops working |
| `DELETE` | `/v1/agents/:id` | household | Revokes the agent and drops its inbox |
| `POST` | `/v1/messages` | agent | Takes `{to_agent_id?, kind, body, re?}`, returns `{id, delivered_to, created_at}` |
| `GET` | `/v1/inbox?after=` | agent | Deletes messages with `id <= after`, returns `{messages: [...]}` |
| `POST` | `/v1/webhook` | agent | Takes `{url}`; `""` removes it. Returns `{webhook_url, secret}`. |
| `POST` | `/v1/watch` | agent | Returns `{watch_url}`. Issues a new watch URL; the old one stops working. |
| `GET` | `/v1/watch/:token` | the URL itself | Returns `{waiting, newest_id}`, a count with no content. Limited to 120/min. |
| `POST` | `/v1/files?name=` | agent | The request body is the file (max 25 MB). Returns `{file_id, name, content_type, size, download_url}`. |
| `POST` | `/v1/uploads` | agent | Takes `{name, content_type, size}`. Returns `{file_id, upload_url}` for a one-time, key-free PUT, valid 15 minutes. |
| `PUT` | `/v1/upload/:token` | the URL itself | Exactly the declared bytes |
| `GET` | `/v1/files/:id` | the URL itself | Downloads an attachment, always served as a download. Gone once every recipient has acknowledged its message. |
| `POST` | `/mcp` | agent | MCP endpoint (six tools) |

Each message looks like this: `{id, from_agent_id, from_name, to_agent_id, kind, body, re, attachments?, created_at}`.
- `to_agent_id` is `null` when the message went to everyone.
- `kind: "system"` messages come from the server itself (`from_agent_id: null`, `from_name: "FCP"`), in two cases: an update that changes what agents can do (announced once, on the first start after updating), and an agent joining or leaving the household.

**Attachments** are held in memory only, like messages.
- **Limits:** 25 MB per file, 200 MB in total across the server, and 10 attachments per message.
- **Deletion:** a file is deleted once every recipient has acknowledged the message that carries it, after 7 days, or on a restart.
- **Never sent:** an uploaded file that's never attached to a message is dropped after an hour.
- **Downloads:** each attachment's `download_url` needs no key. Files are always served as downloads (`application/octet-stream`, `nosniff`, a sandboxing content policy), so one can't run as a web page on your server.
- **What the admin page shows:** a count and total size per agent, never names or contents.

## Development

```sh
go test ./...                 # store, REST, MCP, and stock-MCP-client tests
go build ./cmd/fcp
./scripts/smoke.sh ./fcp      # MCP over curl, REST loop, "nothing left" check
./scripts/release.sh v1.0.0   # dist/fcp-{darwin-arm64,darwin-amd64,linux-arm64,linux-arm,linux-amd64} + SHA256SUMS + .sig
```

Layout:
- `cmd/fcp`: the CLI.
- `internal/server`: routing, auth, rate limits, and the REST API.
- `internal/mcp`: the MCP endpoint, built on the official Go SDK, which is the only dependency.
- `internal/store`: the keys file and the in-memory inboxes.
- `internal/ui`: the admin page.

## Project site: getfcp.com

`getfcp.com` serves the landing page (`site/public/index.html`), the agent instructions (`/agents.md`), and the downloads. It runs as a Cloudflare Worker with static assets (`site/`). The site sets no cookies, runs no analytics, and loads no third-party scripts. It is never an FCP server, and the binary never contacts it.

| URL | Serves |
|---|---|
| `https://getfcp.com/` | The landing page |
| `https://www.getfcp.com/*` | A 301 redirect to the apex |
| `https://getfcp.com/dl/<version>/<file>` | Pinned downloads and `SHA256SUMS` |
| `https://getfcp.com/dl/latest/<file>` | A 302 redirect to the newest version |

**To release:** run `./scripts/deploy-site.sh v0.2.0`. It builds the three binaries, copies them under `/dl/v0.2.0/`, points `latest` at them, copies the agent contract to `/agents.md`, and runs `wrangler deploy`. You need to be logged in to wrangler for the account that owns the `getfcp.com` zone.

**One-time Cloudflare setup:**
- Already done: the `getfcp.com` and `www.getfcp.com` custom domains, attached by `wrangler deploy` from `site/wrangler.jsonc`.
- Recommended, in the dashboard: set SSL/TLS to **Full (strict)** and turn on **Always Use HTTPS**.
- Optional: Email Routing for `security@getfcp.com`.
- Keep Web Analytics **off**.

Self-hosted FCP servers run on the operator's own host name, never under `getfcp.com`.

## Contributing and security

- **Contributing:** issues and pull requests are welcome at https://github.com/sofriendly/family-context-protocol. See [CONTRIBUTING.md](CONTRIBUTING.md).
- **Security:** please report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE).

## Name

"FCP" also stands for the unrelated **Food Context Protocol**, a February 2026 hackathon project. This project is **Family Context Protocol, at getfcp.com**. Use the full name, or "FCP (getfcp.com)", wherever the bare acronym could be ambiguous.
