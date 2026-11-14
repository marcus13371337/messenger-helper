# messenger-helper

A small program that holds one person's Facebook Messenger connection and talks to the app that started it over stdin and stdout. It is built on [mautrix-meta](https://github.com/mautrix/meta)'s Messenger client code, without Matrix: no homeserver, no open port, no database server.

It was made for [Empty](https://empty.rcusahlberg.se), where a person's own builder agent adds it as a source for their Messages module, but any app can run it.

**This is an unofficial client.** Meta offers no API for personal accounts. Using it is against Meta's terms and may get an account flagged. The person running it takes that risk knowingly.

## How it works

- The app downloads a release for its platform, checks its checksum, and starts it as a child process.
- They exchange one JSON-RPC message per line, described in [PROTOCOL.md](PROTOCOL.md): the app sends requests (log in, list conversations, read history, send, fetch attachments) and the helper answers them and pushes new events (messages, reactions, read receipts, typing).
- The helper keeps the connection to Meta alive, reconnects on its own, and reads end-to-end encrypted chats.
- The helper keeps no login on disk unless the app asks: the app gives it the session and stores the refreshed one it hands back, so secrets can live in the OS keychain.

## Downloading a release

Each [release](https://github.com/marcus13371337/messenger-helper/releases) is named after its version and has one executable per platform, plus `checksums.txt` with the SHA-256 of each:

| Platform | File |
| --- | --- |
| macOS, Apple silicon | `messenger-helper-darwin-arm64` |
| macOS, Intel | `messenger-helper-darwin-x64` |
| Linux, x86-64 | `messenger-helper-linux-x64` |
| Linux, arm64 | `messenger-helper-linux-arm64` |

An app pins a version and the checksum of its platform's file, downloads it, refuses it when the SHA-256 differs, makes it executable and starts it. By hand:

```
version=v0.1.0
platform=darwin-arm64
base=https://github.com/marcus13371337/messenger-helper/releases/download/$version
curl -fLO "$base/messenger-helper-$platform"
curl -fLO "$base/checksums.txt"
shasum -a 256 -c --ignore-missing checksums.txt
chmod +x "messenger-helper-$platform"
"./messenger-helper-$platform" --version
```

`--version` prints the helper's version and the protocol version it speaks, for example `messenger-helper 0.1.0 (protocol 1)`.

The programs are not signed. macOS does not stop a program that an app or `curl` downloaded, but it quarantines one downloaded in a browser; `xattr -d com.apple.quarantine messenger-helper-darwin-arm64` lifts that.

Pushing a tag such as `v0.1.0` publishes a release; a tag with a suffix such as `v0.2.0-rc.1` publishes a prerelease. `scripts/release.sh v0.1.0` builds the same files into `dist/` locally.

## Building

Needs Go 1.26 or newer; the `go` command fetches the right toolchain on its own.

```
go build -o messenger-helper .
go test ./...
```

The tests run against a fake Meta and never log in anywhere. The code that talks to the real Meta lives in `internal/meta` and can only be tried by hand with your own account, see "Running it by hand" in [PROTOCOL.md](PROTOCOL.md).

`scripts/check.sh` runs everything CI runs: gofmt, vet, the tests with the race detector, and the release build.

## Following mautrix-meta

Meta changes its protocol often and mautrix-meta follows within days, so every Monday a workflow runs `scripts/bump-mautrix-meta.sh`. It moves `go.mau.fi/mautrix-meta` to its latest release, copies that release's `replace` lines into `go.mod` (Go does not apply a dependency's replacements), runs `scripts/check.sh` and opens or updates a PR from `bump/mautrix-meta`. When the checks fail the PR is a draft and the run is red. It can also be started by hand from the Actions tab.

The tests run against a fake Meta, so a green bump only says the code still fits. Before merging, try the PR's build against the real Meta with your own account, `go run . --state-dir /tmp/mh` as in "Running it by hand" in [PROTOCOL.md](PROTOCOL.md):

- log in
- list conversations
- read history in a plain chat and in an end-to-end encrypted one
- send a message, and receive one sent from another device

Then merge, and release from `main` with the next patch version:

```
git switch main && git pull
git tag vX.Y.Z
git push origin vX.Y.Z
```

## Licence

AGPL-3.0-or-later, like mautrix-meta, which it builds on.
