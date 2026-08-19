# tg-archive

Archive **your own** Telegram into plain Markdown, near real-time. Reply from the CLI.

**New here? Start with the step-by-step guide** —
[English](https://tggo.github.io/tg-archive/) · [Українською](https://tggo.github.io/tg-archive/uk/).
It walks through Homebrew, getting your Telegram API keys, and the first run, assuming no
terminal experience.

Telegram's MTProto API is open and your messages are your data — this is a normal user
client (like the desktop app), not a scrape. Signed with a Developer ID and notarized by
Apple, so macOS runs it without a Gatekeeper fight.

```
$ tg-archive live
live: Ruslan · chats watched: 353
13:41:07 files written: 2
```

```markdown
## 2026-08-19

**14:32** · **Anna** ↳ replying to «are you around tomorrow?»: yes, after 6
**14:33** · **me** [voice 12s]
**14:41** · **Anna** (edited): make it 7, actually
```

## Why it is built this way

**SQLite is the source of truth; Markdown is a projection.** Every message lands in
`state.db` first, and the `.md` files are re-rendered from it. That is what makes edits
and deletions honest — an edited message updates in place and gets `(edited)`, a deleted
one is kept and marked, and `tg-archive rerender` rebuilds the entire archive without
touching Telegram again. Append-only writers cannot do any of that.

One file per chat per month, atomically replaced, so Obsidian never sees a half-written
file:

```
~/TelegramArchive/
├── index.md                       # table of chats, message counts, last activity
└── chats/
    └── anna-smith-428424641/
        ├── 2017-05.md
        └── 2026-08.md
```

## Install

```bash
brew install tggo/tap/tg-archive
```

The binary is universal (arm64 + x86_64) and signed with a Developer ID, but **not yet
notarized by Apple**. Homebrew does not quarantine what it installs, so it runs as-is; a
browser download would be gated until a notarized build ships.

Or build from source (Go 1.24+):

```bash
go install github.com/tggo/tg-archive/cmd/tg-archive@latest
```

## Setup

Telegram issues API credentials per account, so you use your own — there are no shared
keys baked into this binary:

```bash
tg-archive setup     # walks you through my.telegram.org → api_id / api_hash
tg-archive login     # phone → code (arrives in Telegram) → 2FA if enabled
tg-archive backfill  # full history; interrupt and re-run, it resumes
tg-archive live      # daemon: new/edited/deleted → .md within ~3s
```

Migrating from a Telethon-based setup? Keep your session instead of logging in again.
Print the StringSession from your existing session file:

```python
from telethon.sessions import StringSession, SQLiteSession
print(StringSession.save(SQLiteSession("/path/to/your.session")))
```

```bash
tg-archive import-telethon "1BVtsOHYBu..."
```

That string is full access to the account — treat it like a password, and never paste it
anywhere public.

## Commands

| Command | What it does |
|---|---|
| `setup` | Interactive config: credentials, output dir, which chats |
| `login [--phone +380…]` | Authorize this device |
| `chats` | List dialogs with their ids |
| `backfill [--limit N] [--force]` | Pull history, resumable |
| `live` | Watch for new/edited/deleted messages |
| `send --chat X --text "…" [--reply-to N]` | Send a message (and archive it) |
| `rerender` | Rebuild every `.md` from the database |
| `search "words"` | Full-text search: `--chat`, `--sender`, `--from`, `--to` |
| `media [--chat X]` | Download attachments for messages that have none yet |
| `doctor [--fix]` | Find holes in the archived history, and fill them |
| `status` | What is in the archive right now |
| `mcp [--allow-send]` | Serve the archive to an MCP client over stdio |
| `import-telethon <StringSession>` | Reuse a Telethon session instead of logging in |

`--chat` takes an id, an `@username`, or part of a chat title — ambiguous matches are
listed rather than guessed.

Two accounts? Put `--profile <name>` before the command and everything — config, session,
database, archive — lives separately:

```bash
tg-archive --profile work setup
tg-archive --profile work backfill
```

### Search

Search runs on a SQLite FTS5 index, so it folds case for Cyrillic and Greek too, which
`LIKE` does not. Several words must all appear; `"quoted words"` is an exact phrase, a
trailing `*` matches a prefix.

```bash
tg-archive search "delivery address" --from 2026-01-01
tg-archive search 'фундамент' --chat VABRAM
```

### Holes in the history

```bash
tg-archive doctor         # what is missing
tg-archive doctor --fix   # fetch it
```

It reports two different things, and refuses to conflate them: chats never walked back to
their first message, and missing id ranges — the latter **only** for supergroups and
channels. Telegram numbers messages per *account* in private chats, so an id jump there
means you wrote elsewhere in between, not that anything is missing.

### Media

Set `"media": "small"` (with `media_max_mb`) or `"all"` in the config, then:

```bash
tg-archive media            # download what is still missing
tg-archive media --chat X
```

Files land in `attachments/<chat>/` and the Markdown switches from `[photo]` to
`![[attachments/anna-smith-428424641/352698.jpg]]`, which Obsidian renders inline. History
comes first and files second on purpose: an interrupted download never costs you messages.

## Use it from Claude (MCP)

The same binary is an MCP server, so Claude can read your Telegram history, search it, and
(optionally) send messages:

```bash
claude mcp add tg-archive -- tg-archive mcp
```

Reads are served from the local SQLite archive, which makes them instant and keeps working
when Telegram is unreachable. Only `sync_chat` and `send_message` touch the network, and
the connection is opened lazily — a read-only session never dials Telegram at all.

| Tool | What it does |
|---|---|
| `list_chats` | Find a chat: id, kind, message count, last activity |
| `read_chat` | Messages of one chat, oldest-first, pageable with `before_id` |
| `search_messages` | Substring search across the archive or one chat |
| `archive_status` | Paths and counts |
| `sync_chat` | Pull anything newer than the archive has, from Telegram |
| `download_media` | Fetch attachments for messages that have none |
| `check_archive` | Report unfinished chats and gaps |
| `send_message` | Send as your account — **only exposed with `--allow-send`** |

Sending is off by default, because an MCP server that can message your contacts is a
different risk from one that can read a local database. Turn it on deliberately:

```bash
claude mcp add tg-archive -- tg-archive mcp --allow-send
```

`send_message` is annotated as destructive and open-world, so a well-behaved client asks
before calling it.

Chats are also exposed as MCP **resources** (`tg-archive://chat/<id>`), so a client can
attach one by name instead of calling a tool.

## Configuration

`~/.config/tg-archive/config.json`:

```json
{
  "api_id": 1234567,
  "api_hash": "…",
  "out_dir": "~/TelegramArchive",
  "timezone": "Europe/Kyiv",
  "private": true,
  "groups": true,
  "saved": true,
  "channels": false,
  "bots": false,
  "skip_ids": [],
  "only_ids": []
}
```

Media is not downloaded by default — messages keep a marker (`[voice 12s]`,
`[file spec.pdf 2.1MB]`) so an archive of years of chats stays in the tens of megabytes.
Set `media` to `small` or `all` to fetch the files themselves.

Run it as a background service on macOS:

```bash
cp packaging/com.tggo.tg-archive.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.tggo.tg-archive.plist
```

## Reliability

- gotd's gap manager recovers updates missed while the process was down.
- Every 10 minutes the daemon additionally pulls anything newer than the last known
  message id per chat — a backstop for updates that never arrive, which is a normal
  hazard of any MTProto client, not a bug you can code away.
- `floodwait` sits out `FLOOD_WAIT` server-side limits, `ratelimit` keeps request pace
  below them in the first place.

## Limits

- **Secret chats are not accessible.** They are end-to-end encrypted and exist only on
  the device that created them. No client can export them, including this one.
- This is a user account API. Do not use this session for bulk messaging — Telegram bans
  accounts for that, and the ban takes your archive's access with it.
- Bots and broadcast channels are off by default; enable them in the config if you want
  the noise.

## License

MIT
