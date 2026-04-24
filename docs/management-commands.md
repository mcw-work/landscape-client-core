# Management Commands

The Landscape server can send management commands to the client. Each command is an inbound message dispatched by the exchange loop to the appropriate handler. Handlers run in individual goroutines and report results back to the server via `operation-result` messages.

## Result Format

Every handler sends an `operation-result` message on completion:

| Field | Type | Description |
|---|---|---|
| `operation-id` | int64 | Echoed from the inbound command |
| `status` | int | `6` (success) or `5` (failure) |
| `result-text` | string | Human-readable output or error message |
| `result-code` | int64 | *(optional)* Exit-code style result (see per-command docs) |

## Snap Commands

All snap commands use the snapd REST API and wait up to 10 minutes for the change to complete.

### `install-snap`

Installs a snap package.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `name` | string | Snap name |
| `channel` | string | *(optional)* Channel (e.g. `stable`, `edge`). Defaults to snapd's default channel |
| `classic` | bool | *(optional)* Install in classic confinement |

**On success:** `status=6`

**On failure:** `status=5`, `result-text` contains the snapd error message.

---

### `remove-snap`

Removes an installed snap package.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `name` | string | Snap name to remove |

**On success:** `status=6`

**On failure:** `status=5`

---

### `refresh-snap`

Refreshes (updates) one or more snap packages.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `name` | string | Snap name to refresh |
| `channel` | string | *(optional)* Switch to this channel |

**On success:** `status=6`

**On failure:** `status=5`

---

## Service Commands

Service commands manage individual snap services via the snapd REST API.

### `start-snap-service`

Starts a snap service.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `snap` | string | Snap name |
| `service` | string | Service name within the snap |

---

### `stop-snap-service`

Stops a snap service.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `snap` | string | Snap name |
| `service` | string | Service name within the snap |

---

### `restart-snap-service`

Restarts a snap service.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `snap` | string | Snap name |
| `service` | string | Service name within the snap |

---

## System Commands

### `shutdown`

Shuts down or reboots the device. The result message is sent to the server **before** the shutdown command is executed to ensure delivery.

**Inbound fields:**

| Field | Type | Description |
|---|---|---|
| `reboot` | bool | If `true`, reboot the device. If `false` (or absent), power off |
| `delay` | int64 | *(optional)* Delay in minutes before shutdown |
| `operation-id` | int64 | Operation identifier |

**Notes:**
- The handler always reports success before shutting down.
- Uses `systemctl poweroff` or `systemctl reboot` depending on the `reboot` flag.
- Requires the `shutdown` snap plug.

---

### `execute-script`

Executes a script on the device. See [Remote Script Execution](remote-script-execution.md) for full details.

---

## Error Handling

If a handler returns an error or panics:

- The panic is caught by the manager runner.
- `status=5` (failed) is sent with the error message or panic description as `result-text`.
- The goroutine exits cleanly; subsequent commands for the same type will still be dispatched.

## Operation Timeout

Snap install/remove/refresh operations time out after **10 minutes**. If the operation does not complete within this time, the handler returns `status=5` with a timeout error message.
