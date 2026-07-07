package users

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/plex-language-sync/internal/plex"
)

// RefreshConfig bundles retry tunables for the initial token-refresh
// loop. The zero value is not useful; production code uses
// DefaultRefreshConfig. Tests construct a Config with shrunk durations
// to exercise the retry path without sleeping in real time.
type RefreshConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// DefaultRefreshConfig returns the production retry parameters: 5
// attempts, 5s base, 2× backoff, 60s cap. Worst-case additional startup
// delay ≈ 75s before proceeding with an empty shared-users map;
// zero-cost in the good case because the first attempt exits early.
func DefaultRefreshConfig() RefreshConfig {
	return RefreshConfig{
		MaxAttempts: 5,
		BaseDelay:   5 * time.Second,
		MaxDelay:    60 * time.Second,
	}
}

// periodicRefreshInterval is the cadence for the background token
// refresh loop. Preserved from the pre-extraction package-level const
// userTokenRefreshInterval so the operational behavior is unchanged.
const periodicRefreshInterval = 12 * time.Hour

// PeriodicRefreshInterval returns the background refresh cadence. The
// composition root uses this to log the interval at startup.
func PeriodicRefreshInterval() time.Duration { return periodicRefreshInterval }

// RefreshTokens fetches shared user tokens from plex.tv and updates the
// cache. The plex.tv response is the source of truth: shared users
// absent from the response are pruned from the manager's shared map,
// the per-user client cache, and the cache's user-tokens map so revoked
// tokens stop being used for subsequent operations. A transient plex.tv
// failure short-circuits above the state rebuild; existing state is
// left untouched. LanguageProfiles are kept untouched — a re-shared
// user recovers their learned audio→subtitle mappings on return.
func (m *Manager) RefreshTokens(ctx context.Context, adminClient *plex.Client, machineID string) error {
	servers, err := adminClient.SharedUserTokens(ctx, machineID)
	if err != nil {
		slog.Warn("failed to refresh shared user tokens", "error", err)
		return err
	}

	m.mu.Lock()
	newShared := make(map[ID]Info, len(servers))
	for _, s := range servers {
		if s.UserID == "" || s.AccessToken == "" {
			continue
		}
		uid := ID(s.UserID)
		// Skip the admin's own ID if plex.tv returns it in the shared
		// list, matching LoadFromCache's guard. Without this the admin
		// lands in m.shared and All() emits it twice (from m.admin and
		// m.shared), double-processing the admin episode; it would also
		// persist the admin token into cache.json via tokensCopy.
		// m.admin.ID is read under m.mu, held here.
		if uid == m.admin.ID {
			continue
		}
		newShared[uid] = Info{
			ID:    uid,
			Name:  s.Username,
			Token: s.AccessToken,
		}
	}
	// Evict cached clients for users who are no longer shared or whose
	// token rotated.
	for uid, cc := range m.clients {
		if info, ok := newShared[uid]; !ok || info.Token != cc.Token() {
			delete(m.clients, uid)
		}
	}
	m.shared = newShared
	appliedCount := len(newShared)

	tokensCopy := make(map[string]string, len(newShared))
	for uid, info := range newShared {
		tokensCopy[uid.String()] = info.Token
	}
	m.mu.Unlock()

	// Persist to cache (separate lock scope — no nesting). Full replace
	// so the cache mirrors plex.tv; revoked tokens do not linger across
	// restarts.
	m.cache.SetUserTokens(tokensCopy)

	slog.Info("shared user tokens refreshed", "users", appliedCount)
	return nil
}

// InitialRefreshWithRetry runs the initial plex.tv shared-user-token
// refresh with bounded exponential backoff. It exits early on any of:
//
//   - plex.tv answered successfully, even with zero shared users: a
//     server with no shared users legitimately returns an empty list,
//     and retrying cannot conjure users that do not exist,
//   - at least one shared user is already known (e.g. cached tokens
//     from a prior run, loaded via LoadFromCache),
//   - the context is cancelled (e.g., shutdown during startup),
//   - the attempt budget is exhausted.
//
// Failures are rare but happen in practice: plex.tv auth has had
// multi-minute outages, and local Plex can be up while plex.tv is
// unreachable. Without retry, a fresh install during such an outage
// would leave the shared-users map empty for up to
// PeriodicRefreshInterval() (12h). The retry bounds the degraded
// window to tens of seconds in the common case.
//
// Cached tokens from a previous run short-circuit this entirely: if
// LoadFromCache already populated the shared map, the first attempt
// sees SharedCount > 0 and returns immediately even if plex.tv itself
// failed.
func (m *Manager) InitialRefreshWithRetry(ctx context.Context, adminClient *plex.Client, machineID string, cfg RefreshConfig) {
	delay := cfg.BaseDelay
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}

		err := m.RefreshTokens(ctx, adminClient, machineID)

		// Exit as soon as plex.tv answers successfully: a server with
		// zero shared users legitimately returns an empty list, and
		// retrying cannot conjure users that do not exist. Only a real
		// plex.tv failure (err != nil) is worth retrying. Cached tokens
		// from a prior run also satisfy the exit via SharedCount > 0.
		if err == nil || m.SharedCount() > 0 {
			return
		}

		if attempt == cfg.MaxAttempts {
			slog.Warn("initial user token refresh yielded no users after retries; "+
				"proceeding with empty shared-user map",
				"attempts", attempt,
				"next_periodic_refresh_in", periodicRefreshInterval)
			return
		}

		// Sleep until the next attempt; cancellable by context for
		// fast shutdown.
		slog.Info("initial user token refresh yielded no users, retrying",
			"attempt", attempt,
			"next_wait", delay.Round(time.Millisecond))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}

		delay *= 2
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
	}
}

// RefreshLoop periodically refreshes shared user tokens from plex.tv on
// the PeriodicRefreshInterval cadence. Exits on context cancellation.
// The initial synchronous refresh is not the responsibility of this
// loop — run InitialRefreshWithRetry before starting the loop.
func (m *Manager) RefreshLoop(ctx context.Context, adminClient *plex.Client, machineID string) {
	defer slog.Info("user token refresh loop stopped")
	slog.Info("user token refresh loop started",
		"interval", periodicRefreshInterval)

	ticker := time.NewTicker(periodicRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_ = m.RefreshTokens(ctx, adminClient, machineID)
		case <-ctx.Done():
			return
		}
	}
}
