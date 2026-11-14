# messenger-helper

A small program that holds one person's Facebook Messenger connection and talks to the app that started it over stdin and stdout. It is built on [mautrix-meta](https://github.com/mautrix/meta)'s Messenger client code, without Matrix: no homeserver, no open port, no database server.

It was made for [Empty](https://empty.rcusahlberg.se), where a person's own builder agent adds it as a source for their Messages module, but any app can run it.

**This is an unofficial client.** Meta offers no API for personal accounts. Using it is against Meta's terms and may get an account flagged. The person running it takes that risk knowingly.

## How it works

- The app downloads a release for its platform, checks its checksum, and starts it as a child process.
- They exchange one JSON-RPC message per line, described in [PROTOCOL.md](PROTOCOL.md): the app sends requests (log in, list conversations, read history, send, fetch attachments) and the helper answers them and pushes new events (messages, reactions, read receipts, typing).
- The helper keeps the connection to Meta alive, reconnects on its own, and reads end-to-end encrypted chats.
- The helper keeps no login on disk unless the app asks: the app gives it the session and stores the refreshed one it hands back, so secrets can live in the OS keychain.

## Building

Needs Go 1.26 or newer; the `go` command fetches the right toolchain on its own.

```
go build -o messenger-helper .
go test ./...
```

The tests run against a fake Meta and never log in anywhere. The code that talks to the real Meta lives in `internal/meta` and can only be tried by hand with your own account, see "Running it by hand" in [PROTOCOL.md](PROTOCOL.md).

`go.mod` repeats the `replace` lines of mautrix-meta's own `go.mod`, because Go does not apply a dependency's replacements. Copy them again when bumping mautrix-meta.

## Licence

AGPL-3.0-or-later, like mautrix-meta, which it builds on.
