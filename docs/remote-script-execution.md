# Remote Script Execution

The `execute-script` management command allows the Landscape server to run arbitrary scripts on the device. The handler supports interpreter selection, execution timeouts, stdout/stderr capture with a size cap, and file attachments.

## Inbound Message Fields

When the Landscape server dispatches a script, the client receives an `execute-script` message with the following fields:

| Field | Type | Required | Description |
|---|---|---|---|
| `operation-id` | int64 | yes | Correlates the result back to the server request |
| `code` | string | yes | Script body (no shebang line; the handler writes the shebang) |
| `interpreter` | string | no | Interpreter path. Default: `/bin/sh`. Can include arguments (e.g. `/usr/bin/env python3`) |
| `time-limit` | int64 | no | Execution timeout in seconds. `0` or absent = no timeout |
| `username` | string | no | **Ignored.** The client always runs as the current user (root under strict snap confinement) |
| `attachments` | map[string]int64 | no | Map of filename → attachment ID for files to download alongside the script |

## Execution Flow

1. A working directory is created under `$SNAP_COMMON/scripts/<operation-id>/`.
2. The interpreter field is split into command + arguments (e.g. `/usr/bin/env python3` → `["env", "python3"]`).
3. A script file is written to the working directory with a shebang line prepended:
   ```
   #!/usr/bin/env python3
   <script code>
   ```
4. If `time-limit` is set, the execution context is cancelled after that many seconds.
5. If `attachments` are provided, each attachment is fetched from the server and written to the working directory. The environment variable `LANDSCAPE_ATTACHMENTS` is set to the working directory path.
6. The script is executed. Stdout and stderr are captured together into a single output buffer.
7. An `operation-result` message is sent back to the server with the output and result code.

## Interpreter Selection

The `interpreter` field accepts any executable path with optional arguments:

| Value | Effective command |
|---|---|
| *(absent)* | `/bin/sh` |
| `/bin/bash` | `/bin/bash` |
| `/usr/bin/python3` | `/usr/bin/python3` |
| `/usr/bin/env python3` | `env python3` (resolved via `PATH`) |

If the interpreter binary does not exist at the specified path, the handler reports `status=5` (failure).

## Output Capture

Stdout and stderr are merged and captured together. The buffer is capped at **5 MiB (5,242,880 bytes)**. If the script produces more output:

- Output beyond the cap is discarded.
- The string `\n**OUTPUT TRUNCATED**` is appended to the captured output exactly once.

The complete output (up to cap + marker) is sent as `result-text` in the `operation-result` message.

## Result Codes

The `operation-result` message includes an optional `result-code` field:

| result-code | Meaning |
|---|---|
| *(absent)* | Script exited with code 0 (success) |
| `102` | Script timed out (exceeded `time-limit`) |
| `103` | Script exited with a non-zero exit code |
| `104` | Failed to fetch one or more attachments |

The `status` field is:
- `6` (succeeded) when the script exits with code 0
- `5` (failed) for all other outcomes including timeout, non-zero exit, and fetch failure

## Attachments

If the `attachments` field is present in the message, the handler downloads each attachment from the Landscape server before executing the script.

**URL derivation:** The attachment base URL is derived from the message-system URL by replacing the last path segment with `attachment/`:

```
https://landscape.canonical.com/message-system
→ https://landscape.canonical.com/attachment/<id>
```

**Authentication:** Each GET request carries an `X-Computer-ID` header set to the device's secure-id (loaded fresh from the persist store per request).

**File placement:** Attachments are written to the working directory alongside the script with mode `0600`. The environment variable `LANDSCAPE_ATTACHMENTS` is set to the working directory path, allowing the script to locate them:

```bash
#!/bin/bash
cat "$LANDSCAPE_ATTACHMENTS/my-config.txt"
```

**Path traversal protection:** Attachment filenames supplied by the server are validated to ensure they resolve within the working directory. Any filename containing `..` path components or that would escape the working directory is rejected with `result-code=104`.

**On fetch failure:** If any attachment cannot be fetched, the script is **not executed**. An `operation-result` with `result-code=104` and `status=5` is returned immediately.

## User Switching

The `username` field from the server is accepted but **silently ignored**. Under strict snap confinement, the client runs as root and cannot switch to other users. Scripts always run as root.

## Temporary Files

The working directory (`$SNAP_COMMON/scripts/<operation-id>/`) is created before execution and **not** cleaned up automatically after execution. This allows post-mortem inspection. Future cleanup may be added.

Script files are created with mode `0700`; attachment files are created with mode `0600`.

## Security Considerations

- Scripts run as **root** under strict confinement. The Landscape server should only be used by trusted administrators.
- Attachment filenames are sanitised against path traversal before writing.
- The `X-Computer-ID` header used for attachment authentication is the secure-id, not a static credential.
- The output cap prevents unbounded memory usage from runaway scripts.
- The `time-limit` field should always be set by the server for scripts expected to complete quickly.
