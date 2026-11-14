# messenger-helper

A small program that holds one person's Facebook Messenger connection and talks to the app that started it over stdin and stdout. It is built on [mautrix-meta](https://github.com/mautrix/meta)'s Messenger client code, without Matrix: no homeserver, no open port, no database server.

It was made for [Empty](https://empty.rcusahlberg.se), where a person's own builder agent adds it as a source for their Messages module, but any app can run it.

**This is an unofficial client.** Meta offers no API for personal accounts. Using it is against Meta's terms and may get an account flagged. The person running it takes that risk knowingly.

## How it works

- The app downloads a release for its platform, checks its checksum, and starts it as a child process.
- They exchange one JSON message per line: the app sends requests (log in, list conversations, read history, send, fetch attachments) and the helper answers them and pushes new events (messages, reactions, read receipts, typing).
- The helper keeps the connection to Meta alive, reconnects on its own, and reads end-to-end encrypted chats.
- The helper keeps no login on disk unless the app asks: the app gives it the session and stores the refreshed one it hands back, so secrets can live in the OS keychain.

## Licence

AGPL-3.0-or-later, like mautrix-meta, which it builds on.
