package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// A probe ranks a credential first only to learn its weekly reset, so the session that
// carried it must stay unbound until a pick is made on what the probes learned.
func TestSessionAffinitySoonestResetProbeDoesNotBindSession(t *testing.T) {
	now := time.Now()
	later := soonestResetEpoch(now.Add(48 * time.Hour))
	sooner := soonestResetEpoch(now.Add(12 * time.Hour))

	teamAuth := &Auth{ID: "claude-a", Provider: "claude"}
	proAuth := &Auth{ID: "claude-b", Provider: "claude"}
	auths := []*Auth{teamAuth, proAuth}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	pick := func(step string) string {
		t.Helper()
		opts := cliproxyexecutor.Options{
			Headers: http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}},
		}
		picked, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
		if errPick != nil {
			t.Fatalf("%s: Pick() error = %v", step, errPick)
		}
		if picked == nil {
			t.Fatalf("%s: Pick() returned nil", step)
		}
		return picked.ID
	}

	if got := pick("first probe"); got != teamAuth.ID {
		t.Fatalf("first probe = %s, want %s", got, teamAuth.ID)
	}
	teamAuth.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(later, "0.88")}

	// Bound to the first probe, the session would stay on claude-a here.
	if got := pick("second probe"); got != proAuth.ID {
		t.Fatalf("second probe = %s, want %s: the first probe bound the session", got, proAuth.ID)
	}
	proAuth.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(sooner, "0")}

	if got := pick("ranked pick"); got != proAuth.ID {
		t.Fatalf("ranked pick = %s, want soonest reset %s", got, proAuth.ID)
	}

	// The ranked pick binds: the session stays even once claude-a resets sooner.
	teamAuth.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(soonestResetEpoch(now.Add(time.Hour)), "0.88")}
	if got := pick("bound pick"); got != proAuth.ID {
		t.Fatalf("bound pick = %s, want bound %s", got, proAuth.ID)
	}
}

// A ranked pick after a probe binds the session, even when the request metadata is reused.
func TestSessionAffinitySoonestResetRankedPickAfterProbeBinds(t *testing.T) {
	now := time.Now()
	soloAuth := &Auth{ID: "claude-a", Provider: "claude"}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	opts := cliproxyexecutor.Options{
		Headers:  http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}},
		Metadata: map[string]any{},
	}
	if _, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{soloAuth}); errPick != nil {
		t.Fatalf("probe Pick() error = %v", errPick)
	}
	if authID, _ := selector.LookupAffinity("claude", "claude-opus-5-5", "session-1"); authID != "" {
		t.Fatalf("LookupAffinity() after probe = %q, want unbound", authID)
	}

	soloAuth.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(soonestResetEpoch(now.Add(12*time.Hour)), "0")}
	if _, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, []*Auth{soloAuth}); errPick != nil {
		t.Fatalf("ranked Pick() error = %v", errPick)
	}
	if authID, _ := selector.LookupAffinity("claude", "claude-opus-5-5", "session-1"); authID != soloAuth.ID {
		t.Fatalf("LookupAffinity() = %q, want %q", authID, soloAuth.ID)
	}
}
