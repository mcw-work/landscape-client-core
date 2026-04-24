# Wire Protocol

This document describes the binary wire format and the higher-level exchange protocol used by `landscape-client-core` when communicating with the Landscape server.

## bpickle Encoding

All message payloads between the client and server use **bpickle** — a compact binary encoding that is a strict subset of Python's pickle protocol designed for the Landscape wire format.

### Type Encoding

| Go Type | Wire Prefix | Example |
|---|---|---|
| `nil` | `n` | `n` |
| `bool` false | `b0` | `b0` |
| `bool` true | `b1` | `b1` |
| `int64` | `i<decimal>;` | `i42;` |
| `float64` | `f<repr>;` | `f3.14;` or `f3.0;` |
| `[]byte` | `s<len>:<bytes>` | `s5:hello` |
| `string` | `u<len>:<utf8>` | `u5:hello` |
| `[]any` | `l<items>;` | `lb1i2;u3:foo;` |
| `bpickle.Tuple` | `t<items>;` | `ti1;i2;;` |
| `map[string]any` | `d<key><val>...;` | `du4:typeu8:register;` |
| `bpickle.BytesDict` | `d<s-key><val>...;` | `ds4:typeu8:register;` |

**Notes:**
- Dict keys are always sorted alphabetically for deterministic output.
- Whole-number floats include a `.0` suffix (e.g. `f3.0;`) for Python compatibility.
- String (`u`) keys and bytes (`s`) keys both unmarshal to Go `string` values.

### Example

The following Go value:

```go
map[string]any{
    "type": "register",
    "api":  "3.3",
}
```

Encodes as:

```
d u3:api u3:3.3; u4:type u8:register; ;
```

(whitespace added for readability; actual encoding has no spaces)

### Nested Messages

The full exchange payload is a list of messages:

```go
[]any{
    map[string]any{"type": "cpu-usage", ...},
    map[string]any{"type": "memory-info", ...},
}
```

Encoded as a bpickle list (`l...;`) of bpickle dicts.

---

## Exchange Protocol

The exchange protocol is a request-response loop over HTTPS. The client periodically POSTs a payload to the message-system URL and processes the server's response.

### Request Format

HTTP POST to the configured `url` (e.g. `https://landscape.canonical.com/message-system`).

**Headers:**

```
Content-Type: application/octet-stream
User-Agent: landscape-client/<version>
```

**Body:** bpickle-encoded list of messages.

The payload structure is a `map[string]any` containing:

| Field | Type | Description |
|---|---|---|
| `server-uuid` | string | Server identity (from last `set-id`) |
| `exchange-token` | string | Rotating auth token |
| `sequence` | int64 | Outbound message sequence counter |
| `accepted-types-hash` | []byte | MD5 of accepted types (omitted if changed) |
| `accepted-types` | []string | Accepted message types (only if hash changed or first exchange) |
| `messages` | []any | List of outbound messages |

### Response Format

The server returns a bpickle-encoded `map[string]any`:

| Field | Type | Description |
|---|---|---|
| `next-expected-sequence` | int64 | ACK: server confirms it received messages up to this sequence |
| `messages` | []any | List of inbound command messages |

If `next-expected-sequence` is lower than the last sent sequence, the un-ACK'd messages are re-queued for the next exchange.

### Exchange Intervals

| Condition | Interval |
|---|---|
| Normal (registered, no pending) | `exchange-interval` (default 15 min) |
| Pending messages in queue | `urgent-exchange-interval` (default 1 min) |
| Not yet registered | `urgent-exchange-interval` |
| Just registered (first exchange after `set-id`) | `urgent-exchange-interval` (to deliver device info quickly) |

The exchange loop can also be woken immediately by the ping subsystem calling `TriggerExchange()`.

---

## Protocol State Machine

### Registration

On first run (no `SecureID` in persist store), the client sends a `register` message:

```go
map[string]any{
    "type":                  "register",
    "api":                   "3.3",
    "account_name":          "my-account",
    "registration_password": "my-key",
    "computer_title":        "My Device",
    "hostname":              "ubuntu-device",
    "machine_id":            "<machine-id>",
    "tags":                  "tag1,tag2",
    "access_group":          "my-group",
}
```

The server responds with a `set-id` message (on success) or a `registration` message with an error:

```go
// set-id (success)
map[string]any{
    "type":       "set-id",
    "id":         "<secure-id>",
    "insecure-id": "<insecure-id>",
}

// registration (pending/failed)
map[string]any{
    "type":    "registration",
    "info":    "account_name",  // or "registration_password", "pending"
}
```

### Special Inbound Message Types

These messages are handled by the exchange loop itself, not by manager handlers:

| Message type | Effect |
|---|---|
| `set-id` | Saves `SecureID` and `InsecureID` to persist store; clears plugin state; triggers urgent next exchange |
| `accepted-types` | Updates the list of message types the server accepts; hashes the list |
| `set-intervals` | Updates exchange and ping intervals |
| `resynchronize` | Clears all plugin state; sends `resynchronize-response` ack |
| `unknown-id` | Clears `SecureID` and `InsecureID`; triggers re-registration |
| `registration` | Logs registration status |
| `registration-complete` | Logs registration confirmation |

### API Version

All exchanges advertise API version `3.3` in the `register` message and in all exchange payloads via the `api` field.

---

## Ping Protocol

The ping subsystem uses a separate, lighter-weight HTTP request to detect pending messages between full exchanges.

**Request:**

```
POST <ping-url>
Content-Type: application/x-www-form-urlencoded

insecure_id=<insecure-id>
```

The ping URL defaults to the message-system URL with the path replaced:

```
https://landscape.canonical.com/message-system → http://landscape.canonical.com/ping
```

**Response:** bpickle-encoded dict. If the `messages` key is `true`, the ping loop calls `TriggerExchange()` to wake the exchange loop immediately.

The ping loop does nothing if `InsecureID` is empty (device not yet registered).

---

## Attachment Protocol

Script attachments are fetched via individual GET requests:

```
GET https://<host>/attachment/<id>
X-Computer-ID: <secure-id>
User-Agent: landscape-client/<version>
```

The attachment base URL is derived by stripping the last path segment from the message-system URL and appending `attachment/`.

The server returns the raw attachment bytes on HTTP 200. Any non-2xx response is returned as an error, causing the script execution to be aborted with `result-code=104`.
