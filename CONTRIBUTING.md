# Contributing

```bash
go test ./...
go vet ./...
```

Releases are built and signed locally, because the Developer ID certificate does not
live in CI:

```bash
export NOTARY_PROFILE=tg-archive-notary   # see build/notary-setup.md
./build/release.sh v0.1.0
```

That produces a universal, signed, notarized binary plus the tarball and sha256 that the
Homebrew formula points at.

## Design rules worth keeping

- **SQLite is the source of truth.** Anything that renders Markdown must be able to run
  again from scratch and produce the same files. Never write to `.md` without going
  through the database first.
- **Markdown writes are atomic** (`.tmp` + rename) so a watching editor never reads a
  partial file.
- **Chat ids are Telethon-marked** (`user > 0`, `chat = -id`, `channel = -100…`) so the
  database stays interchangeable with Telethon-based tools.
