package bridge

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/mkende/screenshotter_slack_bridge/internal/config"
)

// authzCacheTTL bounds how long user-status and group-membership lookups are
// cached, trading staleness for far fewer Slack API calls under load.
const authzCacheTTL = 5 * time.Minute

// authorizer decides whether a link_shared event from a given user should be
// acted on, based on the bridge's user-restriction configuration. It is only
// constructed when at least one restriction is enabled.
//
// All lookups fail closed: if Slack cannot be queried, the user is denied.
type authorizer struct {
	api           slackAPI
	blockExternal bool
	groups        []string
	timeout       time.Duration
	log           *log.Logger

	mu            sync.Mutex
	externalCache map[string]boolEntry  // userID -> is-external
	groupCache    map[string]groupEntry // groupID -> member set
}

type boolEntry struct {
	val    bool
	expiry time.Time
}

type groupEntry struct {
	members map[string]bool
	expiry  time.Time
}

func newAuthorizer(api slackAPI, cfg *config.Config, logger *log.Logger) *authorizer {
	return &authorizer{
		api:           api,
		blockExternal: cfg.BlockExternalUsers,
		groups:        cfg.AllowedUserGroups,
		timeout:       cfg.RequestTimeout.Duration,
		log:           logger,
		externalCache: make(map[string]boolEntry),
		groupCache:    make(map[string]groupEntry),
	}
}

// Allow reports whether the bridge should render links shared by userID in the
// workspace teamID.
func (a *authorizer) Allow(ctx context.Context, userID, teamID string) bool {
	if userID == "" {
		// No user to attribute the share to; deny when restrictions are on.
		return false
	}
	if a.blockExternal {
		external, err := a.isExternal(ctx, userID, teamID)
		if err != nil {
			a.log.Printf("authz: cannot determine if user %q is external (%v); denying", userID, err)
			return false
		}
		if external {
			return false
		}
	}
	if len(a.groups) > 0 {
		member, err := a.inAllowedGroup(ctx, userID)
		if err != nil {
			a.log.Printf("authz: cannot check group membership for user %q (%v); denying", userID, err)
			return false
		}
		if !member {
			return false
		}
	}
	return true
}

// isExternal reports whether the user is a Slack Connect stranger, belongs to a
// different team than the event's workspace, or is a guest account. It fails
// closed: if the workspace team is unknown, or the user's team does not match
// it, the user is treated as external.
func (a *authorizer) isExternal(ctx context.Context, userID, teamID string) (bool, error) {
	// Without the workspace's team ID we cannot confirm the user is internal,
	// so treat them as external. This is not cached: the result depends on
	// teamID, which is normally stable but may be absent on some events.
	if teamID == "" {
		return true, nil
	}

	a.mu.Lock()
	if e, ok := a.externalCache[userID]; ok && time.Now().Before(e.expiry) {
		a.mu.Unlock()
		return e.val, nil
	}
	a.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	u, err := a.api.GetUserInfoContext(cctx, userID)
	if err != nil {
		return false, err
	}
	// Internal means a full member whose team matches this workspace and who
	// carries no guest/stranger flag; anything else (including an unknown team)
	// is external.
	external := u.IsStranger || u.IsRestricted || u.IsUltraRestricted || u.TeamID != teamID

	a.mu.Lock()
	a.externalCache[userID] = boolEntry{val: external, expiry: time.Now().Add(authzCacheTTL)}
	a.mu.Unlock()
	return external, nil
}

// inAllowedGroup reports whether userID is a member of any configured group.
func (a *authorizer) inAllowedGroup(ctx context.Context, userID string) (bool, error) {
	for _, group := range a.groups {
		members, err := a.groupMembers(ctx, group)
		if err != nil {
			return false, err
		}
		if members[userID] {
			return true, nil
		}
	}
	return false, nil
}

// groupMembers returns the (cached) member set of a Slack user group.
func (a *authorizer) groupMembers(ctx context.Context, group string) (map[string]bool, error) {
	a.mu.Lock()
	if e, ok := a.groupCache[group]; ok && time.Now().Before(e.expiry) {
		a.mu.Unlock()
		return e.members, nil
	}
	a.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	ids, err := a.api.GetUserGroupMembersContext(cctx, group)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}

	a.mu.Lock()
	a.groupCache[group] = groupEntry{members: set, expiry: time.Now().Add(authzCacheTTL)}
	a.mu.Unlock()
	return set, nil
}
