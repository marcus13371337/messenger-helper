# messenger-helper protocol, version 1

The helper is a child process. The app that starts it writes requests to the helper's stdin and reads answers and events from its stdout. Logs go to stderr and never contain message text, cookies or keys.

## Lines

- One JSON-RPC 2.0 message per line, UTF-8, ending in `\n`. No line is longer than 4 MB in either direction.
- The app sends requests: `{"jsonrpc":"2.0","id":1,"method":"ping"}`. The `id` is a number or string the app picks.
- The helper answers each request once, echoing the `id`: `{"jsonrpc":"2.0","id":1,"result":{}}`.
- The helper sends events as notifications, which have no `id`: `{"jsonrpc":"2.0","method":"state","params":{"state":"connected"}}`.
- Keys are `snake_case`. Dates are ISO 8601 in UTC with milliseconds, like `2026-11-14T08:00:00.000Z`. Ids are strings and stable; treat them as opaque.
- Answers to different requests can arrive out of order. `ping` is answered even while a slow request is running.

## Errors

```json
{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"Nobody is logged in yet. Log in first.","data":{"code":"not_logged_in"}}}
```

`error.data.code` is the code to branch on. `error.message` is a sentence that can be shown to a person as it is. `error.code` is `-32600`, `-32601` or `-32603` for the matching JSON-RPC cases and `-32000` otherwise. A line that cannot be read gets an error with `"id":null`.

| `data.code` | Meaning |
| --- | --- |
| `bad_request` | The line or its `params` were not in the expected shape. |
| `unknown_method` | The helper has no such request. |
| `hello_first` | A request came before `hello`. |
| `unsupported_protocol` | `hello` asked for another protocol version. |
| `bad_folder` | The folder in `hello` is missing or not a folder. |
| `not_logged_in` | The request needs `login` first. |
| `already_logged_in` | `login` while a login is active or running. |
| `bad_session` | The cookies are missing, expired or rejected, or the encryption state is unreadable. The person has to sign in again. |
| `needs_person` | Meta wants the person to confirm something in a browser. |
| `not_connected` | The connection to Meta is down right now. Try again later. |
| `not_found` | The conversation or attachment is unknown. |
| `not_available` | Meta cannot serve this, for example the history of an encrypted chat or an attachment whose link ran out. |
| `outside_folder` | A file path points outside the helper's folder. |
| `file_problem` | A file is missing or unreadable, or the folder is not writable. |
| `too_large` | A line would pass 4 MB. |
| `meta_refused` | Meta did not accept the action. |
| `internal` | A bug in the helper. Details are in its log. |

## Lifecycle

1. The app starts the helper with the helper's folder as its working directory. No secrets go in arguments or environment variables.
2. The app sends `hello`, then `login` with the session it stored.
3. The helper sends `state` on every connection change and `session` whenever the session changed. The app stores the newest `session` and discards older ones.
4. To stop, the app closes the helper's stdin. The helper disconnects, sends a final `session`, and exits with code 0. SIGTERM does the same.
5. After a crash or a stop the app starts the helper again and repeats step 2. Given the session, the helper needs nothing else from the previous run.

The helper writes only inside its folder. It never reads the OS keychain.

## Requests

### `hello`

Must be the first request.

- Params: `protocol` (number, must be `1`), `folder` (absolute path of the helper's folder; defaults to the working directory).
- Result: `protocol` (number), `version` (the helper's version, a string).

### `ping`

- Result: `{}`.

### `login`

- Params: `session`, as described under [Session](#session). The first time, it holds only cookies.
- Result: `user` (`id`, `name`, `avatar_url`): the account that logged in.

`login` answers once Meta accepted the cookies. The connection then comes up in the background: wait for `state` `connected` before expecting live events. When no encryption state is given, the helper registers itself as a new device for encrypted chats, which the person can see in Messenger's list of logged-in devices.

### `logout`

- Result: `{}`.

Disconnects, forgets the encryption device and sends `state` `logged_out`. The app should delete its stored session. The cookies stay valid at Meta until the person logs out in the browser they came from.

### `conversations`

- Params: `limit` (default 20, at most 100), `before` (the `next` of an earlier answer).
- Result: `conversations` (newest activity first), `next` (present when there are more).

A conversation:

| Field | |
| --- | --- |
| `id` | Stable id. |
| `name` | The chat's name, or the other people's names. |
| `kind` | `direct` or `group`. |
| `encrypted` | Whether the chat is end-to-end encrypted. This can turn `true` later. |
| `participants` | The other people: `id`, `name`, `avatar_url`. |
| `last_activity_at` | Date. |
| `unread` | Whether there is activity after the person's last read. |

### `messages`

History of one conversation, newest page first.

- Params: `conversation_id`, `limit` (default 50, at most 200), `before` (the `next` of an earlier answer).
- Result: `messages` (oldest first within the page), `next` (present when there may be more).

Encrypted conversations answer `not_available`: Meta delivers each encrypted message once and keeps no readable copy. The app is the store for those chats and keeps what `message` events deliver.

Messages that arrived in an unencrypted chat while the connection was down do not come as `message` events. After `state` returns to `connected`, ask `messages` for the conversations that changed.

The newest page comes from what Meta sent when the helper connected, kept current with later messages and reactions; older pages are fetched from Meta on each request.

A message:

| Field | |
| --- | --- |
| `id` | Stable id. |
| `conversation_id` | |
| `sender_id` | A user id. |
| `from_me` | Whether the logged-in account sent it, from any device. |
| `sent_at` | Date. |
| `text` | May be empty. Shared links arrive as text. |
| `reply_to_id` | The message this one answers, when it is a reply. |
| `attachments` | List, possibly empty. Each has `id`, `kind` (`image`, `video`, `audio`, `sticker`, `file`), `type` (media type), `name`, `size` (bytes, when known) and `ref`. |
| `reactions` | On history only: `sender_id`, `emoji`. Live changes come as `reaction` events. |
| `encrypted` | Whether it travelled end-to-end encrypted. |
| `unsupported` | `true` when the message has content this protocol cannot carry. Show a pointer to Messenger. |

Keep an attachment's `ref` with the message: it is what `attachment` needs. A `ref` of an encrypted attachment contains its decryption key, so store it like the message itself.

### `send`

- Params: `conversation_id`, `text`, `reply_to_id`, `files` (list of `{"path": "...", "name": "..."}`; `name` is optional). `text` or `files` is required.
- Result: `messages`: what was sent, in the same shape as above.

A file's `path` is relative to the helper's folder, or absolute inside it. Anything outside the folder, including through a symlink, is refused with `outside_folder`.

Sends to one conversation reach Meta in the order the app wrote them; the app does not have to wait for one answer before writing the next. Sends to different conversations run side by side.

In an encrypted chat each file is its own message, and the text is the caption of the first picture or video or else a message of its own, so `messages` can hold several. If Meta accepts some of them and then fails, the answer is still a result: `messages` holds what was sent and `problem` (`code`, `message`) says what went wrong. Send only the rest again.

The same messages also arrive as `message` events with the same ids. In an unencrypted chat the attachments in the answer have no `ref`; the `message` event carries it.

### `react`

- Params: `conversation_id`, `message_id`, `emoji`. An empty `emoji` removes the reaction.
- Result: `{}`.

### `mark_read`

- Params: `conversation_id`, `read_at` (date, defaults to now).
- Result: `{}`.

In an encrypted chat this confirms the messages the helper received since it started. Messages from earlier runs stay unconfirmed towards the sender.

### `attachment`

- Params: `ref` (from a message's attachment), `name` (optional file name to use).
- Result: `path` (absolute, inside the helper's folder), `name`, `size` (bytes), `type` (media type).

The helper downloads the attachment, decrypting it when needed, into a new file in its folder. The app moves or deletes the file when done. Links of unencrypted attachments run out after a while; the answer is then `not_available`. Older pages of `messages` always carry a fresh `ref`. The newest page gets fresh ones when the helper connects again.

## Events

| Method | Params |
| --- | --- |
| `message` | A message, as above. Also sent for the account's own messages from other devices. |
| `reaction` | `conversation_id`, `message_id`, `sender_id`, `emoji` (empty when removed), `at`. |
| `read` | `conversation_id`, `reader_id`, `read_at`: that person has read up to then. |
| `typing` | `conversation_id`, `sender_id`, `typing` (boolean). Treat `true` as running out after a few seconds. |
| `conversation` | A conversation, as above: new, or something about it changed. |
| `session` | `session`: store it, replacing the previous one. |
| `state` | `state`, and sometimes `code` and `message` with the reason as a plain sentence. |

States:

| `state` | |
| --- | --- |
| `connecting` | First connection after `login`. |
| `connected` | Live. With `code` `not_available`, encrypted chats are down while the rest works. With `code` `too_large`, the session outgrew one line and could not be handed over. |
| `reconnecting` | The connection dropped; the helper is retrying on its own. |
| `logged_out` | Meta ended the login, or `logout` finished. A new `login` with fresh cookies is needed. |
| `needs_person` | Meta asks the person for something, such as a security check. They finish it in a browser, then the app logs in again. |

Not in version 1: edits, deleted messages, polls, calls, starting a new conversation, and typing or presence from the helper's side.

## Session

```json
{"platform":"messenger","cookies":{"c_user":"…","xs":"…","datr":"…"},"encryption":"…"}
```

- `platform`: `messenger` (cookies from messenger.com, the default) or `facebook` (cookies from facebook.com).
- `cookies`: every cookie of the signed-in browser session, name to value. `c_user`, `xs` and `datr` are required.
- `encryption`: opaque text holding the keys of the helper's encrypted-chat device. Absent at the first login. It changes with nearly every encrypted message and can grow to a few megabytes.

The whole session is a secret equal to the person's Messenger login. Hand it to the helper only in `login`. The helper sends a new `session` at most every five seconds while things change, and once more as it exits. Losing the newest one means some encrypted messages may fail to decrypt until the keys settle again, so store each one as it arrives.

## Getting the cookies

The helper never sees a password. The app opens a browser window it controls at `https://www.messenger.com/`, lets the person sign in there, including any two-factor step, and waits until the `c_user` and `xs` cookies exist. It then reads all cookies for that site from the window's cookie store, closes the window, and passes them in `login`. If Meta later reports `bad_session` or `logged_out`, the app repeats this.

## What the PIN is for

Messenger asks people to set a PIN for "secure storage". The PIN protects a copy of their encrypted chat history that Meta stores, so a new phone or browser can restore old encrypted messages. It is not needed to read or send encrypted messages: a new device, such as this helper, gets its own keys at `login` and can read everything sent from then on. Version 1 never asks for the PIN and does not restore that stored history, so encrypted chats start empty at the first login.

## Running it by hand

`messenger-helper --state-dir DIR` also keeps the newest session in `DIR/session.json` and uses it when `login` carries no session. This puts the login on disk in the clear and is meant for trying the helper from a terminal, not for apps. `messenger-helper --version` prints the helper and protocol versions.
