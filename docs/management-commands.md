# Management commands

The server sends commands as inbound messages. The exchange passes each one to
`manager.Runner`, which runs the matching handler in its own goroutine (see
[architecture.md](architecture.md#inbound-commands-from-the-server)). Every
command carries an `operation-id`, and the handler reports back with an
`operation-result`.

## Result message

| Field | Value |
|---|---|
| `operation-id` | Copied from the command. |
| `status` | `6` succeeded, `5` failed. |
| `result-text` | Output or error text. |
| `result-code` | Only set by `execute-script`. |
| `timestamp` | Unix time when the result was queued. |

Queuing a result wakes the exchange loop, so results reach the server without
waiting for the next scheduled exchange.

## Commands

| Type | Fields | What it does |
|---|---|---|
| `install-snaps` | `snaps`: list of `{name, args: {channel, classic}}` | Installs each snap in order. |
| `remove-snaps` | `snaps`: list of `{name}` | Removes each snap in order. |
| `refresh-snaps` | `snaps`: list of `{name}` | Refreshes each snap on its tracked channel. |
| `start-snap-service` | `snap`, `service` | Starts `<snap>.<service>`. |
| `stop-snap-service` | `snap`, `service` | Stops `<snap>.<service>`. |
| `restart-snap-service` | `snap`, `service` | Restarts `<snap>.<service>`. |
| `shutdown` | `reboot` (bool) | Powers off, or reboots if `reboot` is true. |
| `execute-script` | see [remote-script-execution.md](remote-script-execution.md) | Runs a script. |
| `cancel-operation` | `operation-id` of the target | Cancels a running operation. Sends no result itself. |

### Snap commands

Each snap is sent to snapd (`POST /v2/snaps/<name>`), and the handler waits up
to 10 minutes for the snapd change to finish before moving to the next one. The
first failure stops the list and is reported as `status=5` with snapd's error.
After the whole list succeeds the client sends a fresh `snaps` message straight
away rather than waiting for the next 30-minute report.

### Service commands

These post one action to snapd (`POST /v2/apps`). Success means snapd accepted
the request; the handler does not wait for the service to change state.

### Shutdown

The handler queues `status=6` first, because the reboot kills the process,
then calls logind over D-Bus (`org.freedesktop.login1.Manager.PowerOff` or
`Reboot`) with a 30 s timeout. If that call fails it sends a second result with
`status=5`. Needs the `shutdown` plug.

### Cancellation

Snap, service and script handlers register a cancel function under their
`operation-id`. `cancel-operation` calls it, which aborts the snapd wait or
kills the script. The cancelled handler reports its own failure result.

## Errors

- A panic in a handler is recovered and reported as `status=5` with the panic
  text. Other commands are unaffected.
- A command missing `operation-id`, or a service command missing `snap` or
  `service`, is logged and dropped with no result.
- If the concurrency limit cannot be acquired before shutdown, the command is
  reported as `status=5`.
