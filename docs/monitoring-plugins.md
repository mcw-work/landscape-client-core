# Monitoring Plugins

The monitor subsystem collects system information and reports it to the Landscape server. Each plugin runs in its own goroutine with exponential backoff on failure (1 s initial, 5 min cap). If a plugin runs healthily for at least 30 seconds, its backoff counter resets.

Plugins enqueue messages to the exchange loop which delivers them to the server on the next exchange. Plugin state (e.g. "last known value") is persisted across restarts via the JSON state file so plugins can send only changes.

## Plugin List

### CPUUsage

**Message type:** `cpu-usage`

Reports CPU utilisation as a percentage. Reads from `/proc/stat` and calculates the proportion of non-idle CPU time since the last collection.

---

### MemoryInfo

**Message type:** `memory-info`

Reports total and free physical memory and swap space in kilobytes. Reads from `/proc/meminfo`.

---

### LoadAverage

**Message type:** `load-average`

Reports 1-minute, 5-minute, and 15-minute load averages. Reads from `/proc/loadavg`.

---

### NetworkActivity

**Message type:** `network-activity`

Reports per-interface bytes sent and received since the last collection. Reads from `/proc/net/dev`. Only interfaces with activity changes are reported.

---

### NetworkDevice

**Message type:** `network-device`

Reports network interface configuration: name, MAC address, IP addresses, speed, and flags. Uses `net.Interfaces()` and `netlink` to enumerate interfaces.

---

### ActiveProcessInfo

**Message type:** `active-process-info`

Reports running processes including PID, parent PID, name, state, CPU usage, memory usage, start time, and owner UID. Reads from `/proc/<pid>/stat` and `/proc/<pid>/status`.

---

### Temperature

**Message type:** `temperature`

Reports CPU and other thermal zone temperatures. Reads from `/sys/class/thermal/thermal_zone*/temp`.

---

### RebootRequired

**Message type:** `reboot-required`

Indicates whether the device requires a reboot (e.g. after a kernel snap update). Uses the snapd REST API to query pending changes that require a reboot.

---

### ComputerInfo

**Message type:** `computer-info`

Reports static device identity information:

- Hostname
- Total memory
- OS name and version (from snapd)
- Kernel version
- Architecture

---

### ProcessorInfo

**Message type:** `processor-info`

Reports details about the CPU(s):

- Model name
- Vendor ID
- Speed (MHz)
- Number of physical processors and cores
- Cache size

Reads from `/proc/cpuinfo`.

---

### MountInfo

**Message type:** `mount-info`

Reports mounted filesystems including device, mount point, filesystem type, and total/free/used space in bytes. Reads from `/proc/self/mountinfo` and uses `syscall.Statfs` for usage data.

---

### Users

**Message type:** `users`

Reports local user accounts from `/etc/passwd` including username, UID, GID, home directory, and shell. Only reports changes since the last collection.

---

### HardwareInfo

**Message type:** `hardware-info`

Reports detailed hardware inventory via the `lshw` tool. The output is structured as a JSON tree describing all hardware components (CPU, memory, disks, network adapters, etc.). `lshw` is bundled as part of the snap.

---

### SnapPackages

**Message type:** `snap-packages`

Reports all installed snaps via the snapd REST API. For each snap, reports:

- Name and version
- Revision
- Channel
- Developer/publisher
- Confinement type
- Status (active, inactive)

Only sends changes (installed, removed, or updated snaps) since the last exchange.

---

### SnapServices

**Message type:** `snap-services`

Reports the status of snap services via the snapd REST API. For each service, reports:

- Snap name
- Service name
- Whether it is enabled
- Whether it is currently active (running)

## Backoff Behaviour

If a plugin returns an error:

1. It is restarted after the current backoff interval.
2. Backoff doubles on each consecutive failure: `1s → 2s → 4s → … → 5m`.
3. Backoff resets to 1 s if the plugin runs successfully for 30 seconds.

Panics inside plugins are caught, logged with a stack trace, and treated as failures (triggering the same backoff logic).

## Plugin State

Plugins that only report changes (NetworkActivity, ActiveProcessInfo, SnapPackages, SnapServices, Users) store their previous known state in the persist store under the plugin's name key. This state is cleared on server-initiated resynchronisation (`resynchronize` message).
