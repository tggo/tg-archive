# tg-archive — working notes

A Telegram MTProto client (gotd) that keeps your own chats as Markdown, and serves them to
MCP clients. macOS-first, pure Go, no CGO.

```
cmd/tg-archive       CLI entry point and command wiring
internal/config      settings, profiles, paths
internal/store       SQLite: the source of truth
internal/render      Markdown projection of the database
internal/tgclient    everything that talks to Telegram
internal/mcpserver   MCP tools and resources over stdio
build/release.sh     universal build, codesign, notarize, tarballs
docs/                GitHub Pages guide (en + uk)
```

## The one rule everything else follows

**SQLite is the source of truth; Markdown is a projection.** Nothing may write a `.md`
file without going through the database first. This is what makes edits, deletions,
late-arriving attachments and reactions all work, and what makes `rerender` able to rebuild
the whole archive offline. Any feature that would need an append-only writer is a design
error, not a shortcut.

Corollaries:

- Markdown writes are atomic (`.tmp` + `os.Rename`), so an editor watching the folder never
  reads half a file.
- Rendering is driven by the `dirty` table, so a change touches one month of one chat.
- Chat ids are Telethon-marked (`user > 0`, `chat = -id`, `channel = -100…`). Keep it that
  way: it makes the database interchangeable with Telethon-based tools, and the project's
  own history was migrated from one.

## Truths about Telegram that shaped the code

These were each learned the expensive way. Do not "simplify" them away.

- **Message ids are per account, not per chat** — except in supergroups and channels. So
  gap detection by id is meaningless for private chats: the first version of `doctor`
  reported 8195 holes and 6.3M "missing" messages, all false. `Store.Gaps` deliberately
  filters to `chat_id < -1000000000000`.
- **`LIKE` does not fold case for Cyrillic** in Go's SQLite. That is why search is FTS5
  with `unicode61`, not a substring match. On a 538k-message archive it also went from
  104 ms to under 2 ms.
- **FTS5 with `content='messages'` answers `COUNT(*)` from the source table**, not the
  index. An empty index therefore reports as full. Use the `meta.fts_version` flag plus
  `messages_fts_data` to decide whether a rebuild is needed.
- **`GetHistory` has no MinID.** "Newer than X" is done by iterating newest-first and
  breaking early.
- **Downloading media needs a fresh file reference**, which stored rows do not have — hence
  `fetchMessage` before every download.
- **Link previews arrive as `MessageMediaWebPage`** and must be ignored, or the text fills
  with `[MessageMediaWebPage]`.
- **Secret chats can never be exported.** If someone asks, the answer is no, by design of
  end-to-end encryption — not a missing feature.

## Go gotchas that bit us here

- `flag` stops parsing at the first positional argument, so `search "word" --limit 5` sees
  the flags as part of the query. `reorderFlags` in `cmd/tg-archive/main.go` fixes this;
  use it for any command that takes free text.
- `modernc.org/sqlite` with WAL wants a single writer: `db.SetMaxOpenConns(1)`.

## Testing

```bash
go test ./...      # unit tests, no network
go vet ./...
```

Unit tests cover the store and the renderer. There is no mocked MTProto layer, so anything
touching `internal/tgclient` gets verified against a real account instead — and that has
caught more than the tests did. When you change fetching, sync or media, run it against a
real archive and read the output before believing it works.

## Releasing

```bash
./build/release.sh v0.2.1
```

Builds a universal macOS binary, signs it with the Developer ID from the keychain,
notarizes it if a `notarytool` profile exists (otherwise warns loudly and continues), and
writes tarballs plus sha256 for macOS and both Linux architectures. The signing certificate
is not in CI on purpose, so CI only runs tests and a smoke build. After a release, update
`Formula/tg-archive.rb` in `tggo/homebrew-tap` with the new version and darwin sha256.

## Style

- User-facing strings and comments are English. The docs exist in English and Ukrainian.
- Comments explain *why*, especially where the code looks odd because Telegram is odd.
- Errors tell the user what to do next (`run \`tg-archive setup\``), not just what failed.
