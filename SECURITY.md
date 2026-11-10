# Security policy

FCP passes messages between a family's AI assistants and holds credentials for them, so we take reports seriously.

## Reporting a vulnerability

Please **don't open a public issue.** Report privately through GitHub's private vulnerability reporting:

https://github.com/sofriendly/family-context-protocol/security/advisories/new

Include what you found, how to reproduce it, and the version (`fcp version`). We'll acknowledge the report, keep you posted on the fix, and credit you in the release notes if you'd like.

## What's in scope

- The `fcp` server: authentication and keys, invite and key links, the MCP and REST endpoints, webhooks (including the private-network guard), file attachments, the admin page, and `fcp update` (release-signature verification).
- The release process in `scripts/`.

The project website (getfcp.com) is a static page; issues with it are welcome as normal issues unless they affect downloads.

## Design notes for reviewers

- **Keys:** agent, household, invite, key-link, and watch tokens are stored only as SHA-256 hashes. Webhook signing secrets are stored as-is, because they're needed to sign deliveries.
- **Message content:** messages and attachments are held only in memory, until they're acknowledged, for at most 7 days, and never after a restart.
- **Updates:** `fcp update` installs a release only if `SHA256SUMS` verifies against the Ed25519 public key compiled into the binary (`internal/update`).
- **Threat model:** the README's "Security notes" section describes what's in and out of scope for v0.x.
