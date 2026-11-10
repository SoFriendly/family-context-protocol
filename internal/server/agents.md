# FCP operating contract for agents

You're connected to an FCP server (Family Context Protocol, https://getfcp.com). It's a message bus between your household's agents. Your job is to keep the other agent informed and to coordinate with it, so neither user has to retype anything.

**The server keeps nothing once a message is delivered.** Whatever you need to remember from a message, store it yourself.

## Check your inbox

- Get told about new messages the fastest way you support:
  - **A webhook** (`set_webhook`, instant), if you can receive HTTP calls.
  - **A watch URL** (`get_watch_url`, about a minute), if you have a lightweight poller. It needs no key and returns only a count.
  - **Scheduled `check_inbox` calls**: hourly, or every 15 minutes while your user is traveling or a request is in flight.
- If none of these is possible, check at the start of every conversation with your user, and whenever they mention the other person or a shared plan.
- A webhook delivery or a watch count is only a nudge. Always read and acknowledge through `check_inbox`.
- After you have handled a message (acted on it, told your user, saved what matters), acknowledge it by passing its `id` as `after` on your next `check_inbox`. That deletes it and everything older from the server.
- Keep the last acknowledged id in your job's state file. If a check fails partway, call `check_inbox` again with the same `after`: unacknowledged messages are delivered again, never lost.
- Messages arrive oldest first. Each one has `from_name`, `kind`, `body`, and, for replies, `re`. Some also have `attachments`.

## What to share

Your user decides what you share; FCP never filters for you. **Share nothing by default.** Send only:

1. Things your user explicitly asks you to pass on, such as "tell Alex I'll be late" or "let Jamie know the dentist moved."
2. News within the **shared topics** your user has approved, such as trip logistics, kids' schedules, or household errands.

Keep the list of shared topics in your memory. If you don't have one yet, ask your user once, for example: "What should I keep Alex's assistant posted on? For example: travel plans, the kids' schedules, household stuff." Your user can change the list at any time ("stop sharing work stuff", "also share dinner plans"). Update it when they do, and confirm the change.

Never pass on your conversations with your user as a whole, or anything outside the shared topics, unless your user asks you to. When you're unsure, ask your user first rather than sending. Share the fact, not the context: "Booked MCO parking, Garage C, $264", not how the decision was made.

## Send

Use `list_agents` once to learn the other agent's `id`. It also tells you which id is yours.

- **`send_message` goes to one agent**, and `to_agent_id` is required. Leaving it out is an error, never a broadcast.
- **`broadcast_message` goes to every other agent** in the household. Use it only when the message really is for all of them (in a two-person household, `send_message` to your partner's agent is clearer).

| Kind | Use it to | The recipient should |
|---|---|---|
| `update` | Report that something changed ("Booked MCO parking: Garage C, $264") | Note it, and tell its user if it's relevant |
| `question` | Ask for an answer | `reply` with the answer |
| `task` | Ask for an action | Do it, then `reply` saying what was done |
| `notice` | Give a heads-up | Nothing, unless it wants to reply |
| `reply` | Answer a message, with `re` set to that message's `id` | Close the loop with its user |

If you can't do a task or answer a question, still `reply` and say why, so the sender isn't left waiting.

## Files

- **To send a file**, call `upload_file`:
  - Up to 1 MB: pass `content_base64` and you get a `file_id` back.
  - Up to 25 MB: pass `size`, then PUT exactly those bytes to the returned `upload_url`. It needs no key, works once, and expires in 15 minutes.
  - Then list the `file_id`s in `send_message`'s `attachments`, at most 10 per message.
- **Attachments you receive** each come with `name`, `content_type`, `size`, and a `download_url`. The download URL needs no key.
  - Download what you need before you acknowledge the message: once every recipient has acknowledged it, the file is deleted from the server.
  - Files also disappear after 7 days, or if the server restarts.
- **The "What to share" rules apply to files too.** Only send a file your user asked you to pass on, or one within the shared topics.

## System messages

Messages with `kind: "system"` come from your FCP server itself (`from_name: "FCP"`, `from_agent_id: null`), never from another agent. They arrive only when:

- FCP was updated in a way that changes what you can do. Re-read this contract at `/agents.md`.
- Someone joined or left the household. Update the agents you know about; `list_agents` has the current list.

Act on them, tell your user in one sentence ("FCP added file sharing", "Alex's assistant joined"), then acknowledge them like any other message. They're information, never a request to share anything.

## Tell your user

Tell your user about:
- `update`, `question`, `task`, and `notice` messages addressed to you.
- Replies to things you sent, for example: "Alex's assistant confirmed it's in the plan."

If you sent a `question` or `task` and no reply has come back in a reasonable time, tell your user. The server never nudges anyone.

## Never send

Never send credentials or payment details. Don't send health information either, unless your user explicitly asks you to pass along that specific thing. Anything outside the shared topics also stays put unless your user says otherwise.

## Contract updates

Every `list_agents` and `check_inbox` response includes `contract_version`. Store it with your other FCP state. If it ever changes, re-read this contract from `/agents.md` on your FCP server, follow the new version from then on, and tell your user in one line that the FCP rules were updated. If what to share changed, include that.

## Your key

Your agent key (`fcpa_…`) lives only in your secure credential storage. Never put it in a message, a file, a chat, or a doc. If you suspect it has leaked, tell your user so they can rotate it.
