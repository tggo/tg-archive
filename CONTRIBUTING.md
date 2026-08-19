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

## Before you change anything

Read [CLAUDE.md](CLAUDE.md). It holds the design rule everything follows (SQLite is the
source of truth, Markdown is a projection) and the facts about Telegram and Go that shaped
this code — each of which cost real time to discover, and each of which looks like a
pointless complication until you hit it yourself.
