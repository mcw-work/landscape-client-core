# Monitoring plugins

`monitor.Runner` runs 15 plugins, each in its own goroutine. A plugin samples
on its own interval and queues messages with `exchange.Send`; they go out on
the next exchange. Sending does not trigger an exchange.

## Plugins

| Plugin | Message type(s) | Sample interval | When it sends | Source |
|---|---|---|---|---|
| CPUUsage | `cpu-usage` | 30 s | Batched every 5 min | `/proc/stat` |
| MemoryInfo | `memory-info` | 15 s | Batched every 5 min | `/proc/meminfo` |
| LoadAverage | `load-average` | 5 min | Batched every 5 min | `/proc/loadavg` |
| NetworkActivity | `network-activity` | 30 s | Batched every 5 min, per interface | `/proc/net/dev` |
| NetworkDevice | `network-device` | 30 s | On change | interfaces, `/sys/class/net` |
| ActiveProcessInfo | `active-process-info` | 30 s | Full list first, then creates, updates and deletes | `/proc/<pid>/stat`, `/proc/<pid>/status` |
| Temperature | `temperature` | 5 min | Every sample, one message per zone | `/sys/class/thermal` |
| RebootRequired | `reboot-required-info` | 5 min | On change | snapd `/v2/system-info` (`refresh.pending == "restart"`) |
| ComputerInfo | `computer-info`, `distribution-info`, `snap-info` | 5 min | Changed fields only | hostname, `/proc/meminfo`, `/etc/machine-id`, `/etc/os-release`, snapd model and serial assertions |
| ProcessorInfo | `processor-info` | 1 h | On change | `/proc/cpuinfo` |
| MountInfo | `mount-info`, `free-space` | 5 min | Each type when it changes | `/proc/mounts`, `statfs` (real filesystems only) |
| Users | `users` | 1 h | Changes to users and groups | `/var/lib/extrausers/passwd`, `/var/lib/extrausers/group` |
| HardwareInfo | `hardware-info` | 24 h | At startup, then every sample | `lshw -xml -quiet` (bundled; 2 min timeout) |
| SnapPackages | `snaps` | 30 min | Full list every sample, and after snap commands | snapd `/v2/snaps` |
| SnapServices | `snap-services` | 1 min | On change | snapd `/v2/apps?select=service` |

Notes:

- **Batched** plugins collect data points and send them in one message per
  5-minute window instead of one message per sample.
- `snap-info` is only sent when the device has a serial assertion, which in
  practice means Ubuntu Core.
- `hardware-info` output that is empty or not valid XML is discarded. lshw can
  be partly blocked by AppArmor under strict confinement.
- Snapd calls time out after 30 s so a stalled snapd cannot wedge a plugin.
- Recurring samples start after a random delay of up to 10% of the interval,
  so plugins with the same interval do not all fire together.

## Plugin state

Change-only plugins keep their last reported value (or a hash of it) in the
`plugin_state` section of `state.json`, keyed by plugin name, so they do not
resend unchanged data after a restart. State is cleared when the device
registers (`set-id`) or the server sends `resynchronize`; every plugin then
reports in full.

## Failures

If a plugin returns an error or panics, the runner logs it (panics with a
stack trace) and restarts the plugin after a backoff: 1 s, doubling to a 5 min
cap, reset after any run that lasted 30 s or more.

A plugin that stops ticking without returning is caught by the watchdog
described in [architecture.md](architecture.md#goroutines-and-lifecycle).
