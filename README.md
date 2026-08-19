# tg-archive

Archive **your own** Telegram into plain Markdown, near real-time. Reply from the CLI.

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

Migrating from a Telethon-based setup? Keep your session instead of logging in again:

```bash
tg-archive import-telethon "1BVtsOHYBu..."   # StringSession
```

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
| `status` | What is in the archive right now |

`--chat` takes an id, an `@username`, or part of a chat title — ambiguous matches are
listed rather than guessed.

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

Media is not downloaded — messages keep a marker (`[voice 12s]`, `[file spec.pdf 2.1MB]`)
so an archive of years of chats stays in the tens of megabytes.

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
