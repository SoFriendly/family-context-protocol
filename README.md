<img src="site/public/logo.svg" width="48" alt="">

# FCP — Family Context Protocol

**https://getfcp.com** · MIT licensed

FCP connects your AI assistant with your partner's, so they can keep each other up to date and coordinate. You don't have to relay anything yourself. Your assistant tells theirs what changed or asks it to do something; it acts and replies. You can each use a different assistant (Muse, Grok, OpenAI Dot, Claude Duet, or any other agent that can open a link and make web requests).

FCP is one small program that you run at home on a Mac, a Raspberry Pi, a Linux box, or a NAS. There's no account and no database, and messages are deleted as soon as they've been read.

## Quick start

**1. Download** the file for your computer, from [getfcp.com](https://getfcp.com) or with curl:

| Computer | File |
|---|---|
| Mac with Apple silicon (M1 and later) | `fcp-darwin-arm64` |
| Mac with Intel | `fcp-darwin-amd64` |
| Raspberry Pi on 64-bit OS, ARM Linux | `fcp-linux-arm64` |
| Raspberry Pi on 32-bit OS (any model) | `fcp-linux-arm` |
| Intel/AMD Linux, most NAS boxes | `fcp-linux-amd64` |

```sh
curl -fLo fcp https://getfcp.com/dl/latest/fcp-darwin-arm64     # use your file name
curl -fLO https://getfcp.com/dl/latest/SHA256SUMS && shasum -a 256 -c SHA256SUMS --ignore-missing
chmod +x fcp
xattr -d com.apple.quarantine fcp 2>/dev/null                   # macOS only
```

Not sure which Raspberry Pi file you need? Run `uname -m` on the Pi: `aarch64` means 64-bit; `armv7l` or `armv6l` means 32-bit. If your shell says `permission denied`, run the `chmod` line.

**2. Make it reachable.** Most assistants run in the cloud, so FCP needs an HTTPS address on the internet. The easiest way is [Tailscale Funnel](https://tailscale.com/kb/1223/funnel):

```sh
tailscale funnel --bg 8080                      # prints https://<machine>.<tailnet>.ts.net
./fcp serve --public-url https://<machine>.<tailnet>.ts.net
```

A reverse proxy on your own domain works too, for example a Caddyfile entry: `fcp.yourfamily.net { reverse_proxy 127.0.0.1:8080 }`.

**3. Paste the setup message.** The first start prints a setup message:

```
Set up your AI assistant: copy everything between the lines and paste it into its chat.
────────────────────────────────────────────────────────────
Please connect to our family's FCP message bus so you can share updates and coordinate with my partner's AI assistant. Open this link and follow the instructions there. It works once:
https://<machine>.<tailnet>.ts.net/join/fcpi_…
────────────────────────────────────────────────────────────
```

1. Paste it into your assistant's chat. Your assistant connects, saves its own key, and starts checking for messages. Then it asks what to keep your partner posted on.
2. It gives you a similar message to forward to your partner, who pastes it into their assistant.

The first start also writes a **household key** to `household-key.txt`. It signs you in to the admin page at `/admin`. Save it in your password manager, then delete the file.

**Lost the setup message?** Restart FCP and it prints a new one, as long as no assistant has joined yet. After that, use `/admin`:
- **Set up assistant** is for an assistant you have in front of you. It gives you the chat message plus the key for the assistant's secure prompt.
- **Create invite message** is for someone else's assistant.

**Other ways to install:**
- From source, with Go 1.26 or newer: `go install github.com/sofriendly/family-context-protocol/cmd/fcp@latest`
- With Docker: `FCP_PUBLIC_URL=https://<your-address> docker compose up -d`. Then `docker compose logs fcp` shows the setup message, and `docker compose cp fcp:/data/household-key.txt .` copies out the household key.

## What gets stored

| What | Where | How long |
|---|---|---|
| Agent names and key **hashes** | `fcp-keys.json` on your computer (mode 0600) | Until you remove the agent |
| Messages and attached files waiting to be read | **Memory only** | Until read, 7 days, or a restart |

That's everything.
- FCP keeps no message history and no logs of what was said.
- Your household key can add and remove assistants, but it can't read anyone's messages.
- FCP sends no telemetry and never checks for updates on its own. It contacts the internet only to deliver webhooks your assistants asked for, and when you check for updates yourself.

Because nothing is kept, each assistant remembers what it was told.

## How it works

1. Your assistant books airport parking and sends your partner's assistant a `task`: "Add MCO parking (Garage C, $264) to the trip plan."
2. Your partner's assistant picks it up, adds it to the plan, and sends back a `reply`: "Added to trip plan."
3. Your assistant tells you it's confirmed. Once both messages have been read, the server holds nothing.

Assistants can send an `update`, `question`, `task`, `notice`, or `reply`, attach files up to 25 MB, and decide what to share based on the topics you approve. The rules every assistant follows are in [`agents.md`](internal/server/agents.md).

**How fast messages arrive** depends on what the receiving assistant supports:

| Option | Delay | For |
|---|---|---|
| Webhook | Instant | Assistants or tools that can receive HTTP calls: n8n, Zapier, a custom bot |
| Watch URL | About a minute | Assistants with a lightweight poller that can't authenticate |
| Scheduled checks | 15–60 minutes | Everything else |

Nothing is lost either way. Messages wait until they're read.

## Connecting other agents

Pasting a setup message works for any assistant. To connect an agent by hand:
1. Use **Set up assistant** in `/admin` to get a key.
2. Point any MCP client at `https://<your-address>/mcp` with `Authorization: Bearer <key>`. For example, in Claude Code:

```sh
claude mcp add --transport http fcp https://<your-address>/mcp --header "Authorization: Bearer $AGENT_KEY"
```

Agents get six tools: `list_agents`, `send_message`, `check_inbox`, `upload_file`, `get_watch_url`, and `set_webhook`. Everything is also available over plain HTTPS. The full API, including the message format, webhooks, and attachments, is in [docs/api.md](docs/api.md).

## Updating

FCP never updates itself without being asked. Either:
- in `/admin`, press **Check for updates**, then **Install and restart**, or
- run `./fcp update` and restart `fcp serve`.

**Verification:** every release is signed, and FCP refuses any download whose signature doesn't match the key built into it.

**What carries over:** your assistants and keys, so nothing needs setting up again. Connected assistants pick up any rule changes on their own.

**What doesn't:** a restart drops messages that haven't been read yet, so check that `/admin` shows **0 waiting** first.

**Docker:** rebuild instead: `docker compose build && docker compose up -d`.

## Configuration

```
fcp serve  [--listen 127.0.0.1:8080] [--keys ./fcp-keys.json] [--public-url https://fcp.example.net]
fcp update [--check]
fcp health      # exits 0 if the server is up (for Docker)
fcp version
```

| Flag | Env | Default |
|---|---|---|
| `--listen` | `FCP_LISTEN` | `127.0.0.1:8080` |
| `--keys` | `FCP_KEYS` | `./fcp-keys.json` |
| `--public-url` | `FCP_PUBLIC_URL` | None. The address used in setup links. Set it whenever FCP runs behind Funnel or a proxy. |
| — | `FCP_ADMIN_TOKEN` | Unset. Setting it enables `POST /v1/households`, for creating more households on one server. |
| — | `FCP_WEBHOOK_ALLOW_PRIVATE` | Unset. Set it to `1` to allow webhooks to `http://` and private-network addresses. |

## Security

- **Agent keys are bearer credentials,** and each is shown once. If one leaks, rotate it in `/admin`; the old key stops working immediately.
- **Every stored key and token is a hash.** The household key is never printed to logs.
- **The setup message is printed to logs, including `docker logs`.** Its link works only once, but whoever redeems it first gets in.
- **Downloads and webhooks are locked down.** Attachments are always served as downloads, so they can't run as pages on your server. Webhooks to private-network addresses are refused unless you allow them.
- **The API is rate-limited,** and each inbox holds at most 1,000 waiting messages.
- **There's no end-to-end encryption between assistants.** The server sees messages while they're waiting in memory, and your HTTPS address protects them in transit. Waiting messages live in RAM, which an operating system can page to swap, so use encrypted swap if that matters to you. macOS encrypts swap by default.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
