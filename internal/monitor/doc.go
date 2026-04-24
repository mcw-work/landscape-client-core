// Package monitor runs one goroutine per monitoring plugin (CPU, memory,
// network, disk, processes, snap packages, etc.) and feeds data to the
// exchange loop via a MessageSink.
package monitor
