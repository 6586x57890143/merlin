package roles

import (
	"fmt"
	"sync"

	"github.com/6586x57890143/merlin/internal/core"
)

func (p *Plugin) lockVisibility(guildID string) func() {
	value, _ := p.visibilityLocks.LoadOrStore(guildID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (p *Plugin) markVisibilityDirty(guildID string) {
	p.visibilityMu.Lock()
	defer p.visibilityMu.Unlock()
	if p.visibilityPending == nil {
		p.visibilityPending = make(map[string]uint64)
	}
	p.visibilityPending[guildID]++
}

// existingJailRole resolves a cold cache without creating a role merely
// because someone edited a list or the startup reconciliation ran.
func (p *Plugin) existingJailRole(guildID string) (string, error) {
	roles, err := p.ops(guildID).GetRoles(core.ID(guildID))
	if err != nil {
		return "", err
	}
	if id := p.knownJailRole(guildID); id != "" {
		if findRole(roles, id) != nil {
			return id, nil
		}
		return "", fmt.Errorf("jail marker %s no longer exists", id)
	}
	for _, role := range roles {
		if role.Name == jailRoleName {
			return role.ID.String(), nil
		}
	}
	return "", nil
}

// Retry on each sweep until both markers agree with settings. Each process
// also reconciles once after startup, applying the read-only migration to
// Discord. A settings edit during a sync keeps its newer generation pending.
func (p *Plugin) reconcileVisibility(guildID string, force bool) error {
	p.visibilityMu.Lock()
	generation := p.visibilityPending[guildID]
	needed := force || !p.visibilityReady[guildID] || generation != 0
	p.visibilityMu.Unlock()
	if !needed {
		return nil
	}
	if p.dryRun(guildID) {
		return fmt.Errorf("channel sync pending while dry-run is enabled")
	}
	id, err := p.existingJailRole(guildID)
	if err != nil {
		return err
	}
	if id != "" {
		if err := p.syncAllJailChannelOverwrites(guildID, id); err != nil {
			return err
		}
	}
	if err := p.syncAllVacationOverwrites(guildID); err != nil {
		return err
	}
	p.visibilityMu.Lock()
	defer p.visibilityMu.Unlock()
	if p.visibilityReady == nil {
		p.visibilityReady = make(map[string]bool)
	}
	p.visibilityReady[guildID] = true
	if p.visibilityPending[guildID] == generation {
		delete(p.visibilityPending, guildID)
	}
	return nil
}
