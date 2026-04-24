package monitor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

var stableFilesystems = map[string]bool{
	"ext": true, "ext2": true, "ext3": true, "ext4": true,
	"reiserfs": true, "ntfs": true, "msdos": true, "dos": true,
	"vfat": true, "xfs": true, "hpfs": true, "jfs": true,
	"ufs": true, "hfs": true, "hfsplus": true, "simfs": true,
	"drvfs": true, "lxfs": true, "zfs": true, "btrfs": true,
}

type mountInfoState struct {
	Hash string `json:"hash"`
}

type MountInfo struct {
	mountsPath string
	statvfs    func(path string) (syscall.Statfs_t, error)
	interval   time.Duration
}

func NewMountInfo() *MountInfo {
	return &MountInfo{
		mountsPath: "/proc/mounts",
		statvfs:    defaultStatfs,
		interval:   5 * time.Minute,
	}
}

func defaultStatfs(path string) (syscall.Statfs_t, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	return stat, err
}

func (p *MountInfo) Name() string { return "mount-info" }

func (p *MountInfo) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var saved mountInfoState
	if state != nil {
		_ = state.GetPluginState(&saved)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			now := time.Now().Unix()
			mounts, err := p.readMounts()
			if err != nil {
				log.Printf("mount-info: %v", err)
				continue
			}

			var hashEntries []any
			var mountInfoEntries []any
			var freeSpaceEntries []any

			for _, m := range mounts {
				mountPoint, ok := m["mount-point"].(string)
				if !ok {
					log.Printf("mount-info: unexpected type for mount-point: %T", m["mount-point"])
					continue
				}
				totalSpace, ok := m["total-space"].(int64)
				if !ok {
					log.Printf("mount-info: unexpected type for total-space: %T", m["total-space"])
					continue
				}
				freeSpace, ok := m["free-space"].(int64)
				if !ok {
					log.Printf("mount-info: unexpected type for free-space: %T", m["free-space"])
					continue
				}

				mountInfoMap := map[string]any{
					"device":      m["device"],
					"mount-point": mountPoint,
					"filesystem":  m["filesystem"],
					"total-space": totalSpace,
				}
				hashEntries = append(hashEntries, mountInfoMap)
				mountInfoEntries = append(mountInfoEntries, bpickle.Tuple{now, mountInfoMap})
				freeSpaceEntries = append(freeSpaceEntries, bpickle.Tuple{now, mountPoint, freeSpace})
			}

			layoutData, _ := json.Marshal(hashEntries)
			hash := fmt.Sprintf("%x", sha256.Sum256(layoutData))
			if hash != saved.Hash {
				saved.Hash = hash
				if state != nil {
					_ = state.SetPluginState(saved)
				}
				if mountInfoEntries != nil {
					msg := exchange.Message{
						"type":       "mount-info",
						"mount-info": mountInfoEntries,
					}
					if err := sink.Send(ctx, msg); err != nil {
						log.Printf("mount-info: send mount-info: %v", err)
					}
				}
			}

			if freeSpaceEntries != nil {
				msg := exchange.Message{
					"type":       "free-space",
					"free-space": freeSpaceEntries,
				}
				if err := sink.Send(ctx, msg); err != nil {
					log.Printf("mount-info: send free-space: %v", err)
				}
			}
		}
	}
}

func (p *MountInfo) readMounts() ([]map[string]any, error) {
	f, err := os.Open(p.mountsPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.mountsPath, err)
	}
	defer f.Close()

	var results []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device := fields[0]
		mountPoint := fields[1]
		filesystem := fields[2]

		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		if strings.HasPrefix(mountPoint, "/dev/") {
			continue
		}
		if !stableFilesystems[filesystem] {
			continue
		}

		stat, err := p.statvfs(mountPoint)
		if err != nil {
			log.Printf("mount-info: stat %s: %v", mountPoint, err)
			continue
		}

		const mb = int64(1024 * 1024)
		bsize := int64(stat.Bsize)
		totalSpace := int64(stat.Blocks) * bsize / mb
		freeSpace := int64(stat.Bfree) * bsize / mb

		results = append(results, map[string]any{
			"device":      device,
			"mount-point": mountPoint,
			"filesystem":  filesystem,
			"total-space": totalSpace,
			"free-space":  freeSpace,
		})
	}
	return results, nil
}
