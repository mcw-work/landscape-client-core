package monitor

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/persist"
)

type userRecord struct {
	Username   string `json:"username"`
	Name       string `json:"name"`
	UID        int    `json:"uid"`
	PrimaryGID int    `json:"primary-gid"`
	HomePhone  string `json:"home-phone"`
	WorkPhone  string `json:"work-phone"`
	Location   string `json:"location"`
	Enabled    bool   `json:"enabled"`
}

type groupRecord struct {
	Name    string   `json:"name"`
	GID     int      `json:"gid"`
	Members []string `json:"members"`
}

type usersState struct {
	Users  map[string]userRecord  `json:"users"`
	Groups map[string]groupRecord `json:"groups"`
}

type UserMonitor struct {
	passwdPath string
	groupPath  string
	interval   time.Duration
}

func NewUsers() *UserMonitor {
	return &UserMonitor{
		passwdPath: "/var/lib/extrausers/passwd",
		groupPath:  "/var/lib/extrausers/group",
		interval:   time.Hour,
	}
}

func (p *UserMonitor) Name() string { return "users" }

func (p *UserMonitor) Run(ctx context.Context, sink exchange.MessageSink, state *persist.PluginStateAccessor) error {
	var saved usersState
	if state != nil {
		_ = state.GetPluginState(&saved)
	}
	if saved.Users == nil {
		saved.Users = make(map[string]userRecord)
	}
	if saved.Groups == nil {
		saved.Groups = make(map[string]groupRecord)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			newUsers, err := p.parsePasswd()
			if err != nil {
				log.Printf("users: parsing passwd: %v", err)
				newUsers = make(map[string]userRecord)
			}
			newGroups, err := p.parseGroup(newUsers)
			if err != nil {
				log.Printf("users: parsing group: %v", err)
				newGroups = make(map[string]groupRecord)
			}
			msg := buildUsersDiff(saved.Users, newUsers, saved.Groups, newGroups)
			if msg != nil {
				msg["type"] = "users"
				if err := sink.Send(ctx, msg); err != nil {
					log.Printf("users: send: %v", err)
				}
			}
			saved.Users = newUsers
			saved.Groups = newGroups
			if state != nil {
				if err := state.SetPluginState(saved); err != nil {
					log.Printf("users: saving state: %v", err)
				}
			}
		}
	}
}

func (p *UserMonitor) parsePasswd() (map[string]userRecord, error) {
	users := make(map[string]userRecord)
	f, err := os.Open(p.passwdPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.passwdPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			continue
		}
		username := parts[0]
		uid, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		gid, err := strconv.Atoi(parts[3])
		if err != nil {
			continue
		}
		gecos := parts[4]
		gecosFields := strings.SplitN(gecos, ",", 5)
		var name, location, workPhone, homePhone string
		if len(gecosFields) > 0 {
			name = gecosFields[0]
		}
		if len(gecosFields) > 1 {
			location = gecosFields[1]
		}
		if len(gecosFields) > 2 {
			workPhone = gecosFields[2]
		}
		if len(gecosFields) > 3 {
			homePhone = gecosFields[3]
		}
		users[username] = userRecord{
			Username:   username,
			Name:       name,
			UID:        uid,
			PrimaryGID: gid,
			HomePhone:  homePhone,
			WorkPhone:  workPhone,
			Location:   location,
			Enabled:    true,
		}
	}
	return users, scanner.Err()
}

func (p *UserMonitor) parseGroup(users map[string]userRecord) (map[string]groupRecord, error) {
	groups := make(map[string]groupRecord)
	f, err := os.Open(p.groupPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", p.groupPath, err)
	}
	defer f.Close()

	userSet := make(map[string]bool)
	for username := range users {
		userSet[username] = true
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 4 {
			continue
		}
		groupName := parts[0]
		gid, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		var members []string
		if parts[3] != "" {
			for _, m := range strings.Split(parts[3], ",") {
				m = strings.TrimSpace(m)
				if userSet[m] {
					members = append(members, m)
				}
			}
		}
		if members == nil {
			members = []string{}
		}
		sort.Strings(members)
		groups[groupName] = groupRecord{
			Name:    groupName,
			GID:     gid,
			Members: members,
		}
	}
	return groups, scanner.Err()
}

func buildUsersDiff(
	oldUsers, newUsers map[string]userRecord,
	oldGroups, newGroups map[string]groupRecord,
) exchange.Message {
	msg := exchange.Message{}

	var createUsers, updateUsers []map[string]any
	var deleteUsers []string
	for username, newUser := range newUsers {
		if oldUser, exists := oldUsers[username]; !exists {
			createUsers = append(createUsers, userToMessage(newUser))
		} else if oldUser != newUser {
			updateUsers = append(updateUsers, userToMessage(newUser))
		}
	}
	for username := range oldUsers {
		if _, exists := newUsers[username]; !exists {
			deleteUsers = append(deleteUsers, username)
		}
	}
	if len(createUsers) > 0 {
		msg["create-users"] = createUsers
	}
	if len(updateUsers) > 0 {
		msg["update-users"] = updateUsers
	}
	if len(deleteUsers) > 0 {
		sort.Strings(deleteUsers)
		msg["delete-users"] = deleteUsers
	}

	var createGroups, updateGroups []map[string]any
	var deleteGroups []string
	createGroupMembers := make(map[string][]string)
	deleteGroupMembers := make(map[string][]string)

	for groupName, newGroup := range newGroups {
		if oldGroup, exists := oldGroups[groupName]; !exists {
			createGroups = append(createGroups, map[string]any{"name": newGroup.Name, "gid": newGroup.GID})
			if len(newGroup.Members) > 0 {
				createGroupMembers[groupName] = newGroup.Members
			}
		} else {
			if oldGroup.GID != newGroup.GID {
				updateGroups = append(updateGroups, map[string]any{"name": newGroup.Name, "gid": newGroup.GID})
			}
			oldSet := make(map[string]bool)
			for _, m := range oldGroup.Members {
				oldSet[m] = true
			}
			newSet := make(map[string]bool)
			for _, m := range newGroup.Members {
				newSet[m] = true
			}
			var added, removed []string
			for m := range newSet {
				if !oldSet[m] {
					added = append(added, m)
				}
			}
			for m := range oldSet {
				if !newSet[m] {
					removed = append(removed, m)
				}
			}
			if len(added) > 0 {
				sort.Strings(added)
				createGroupMembers[groupName] = added
			}
			if len(removed) > 0 {
				sort.Strings(removed)
				deleteGroupMembers[groupName] = removed
			}
		}
	}
	for groupName := range oldGroups {
		if _, exists := newGroups[groupName]; !exists {
			deleteGroups = append(deleteGroups, groupName)
		}
	}

	if len(createGroups) > 0 {
		msg["create-groups"] = createGroups
	}
	if len(updateGroups) > 0 {
		msg["update-groups"] = updateGroups
	}
	if len(deleteGroups) > 0 {
		sort.Strings(deleteGroups)
		msg["delete-groups"] = deleteGroups
	}
	if len(createGroupMembers) > 0 {
		msg["create-group-members"] = createGroupMembers
	}
	if len(deleteGroupMembers) > 0 {
		msg["delete-group-members"] = deleteGroupMembers
	}

	if len(msg) == 0 {
		return nil
	}
	return msg
}

func userToMessage(u userRecord) map[string]any {
	return map[string]any{
		"username":    u.Username,
		"name":        u.Name,
		"uid":         u.UID,
		"enabled":     u.Enabled,
		"location":    u.Location,
		"work-phone":  u.WorkPhone,
		"home-phone":  u.HomePhone,
		"primary-gid": u.PrimaryGID,
	}
}
