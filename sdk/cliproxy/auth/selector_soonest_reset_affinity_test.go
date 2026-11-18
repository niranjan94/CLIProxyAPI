package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	soonestResetOpeningClaudeRequest   = `{"system":"be brief","messages":[{"role":"user","content":"hello"}]}`
	soonestResetContinuedClaudeRequest = `{"system":"be brief","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"again"}]}`
)

func soonestResetAffinityOptions(format sdktranslator.Format, payload string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{
		Headers:         http.Header{"X-Claude-Code-Session-Id": []string{"session-1"}},
		Metadata:        map[string]any{},
		SourceFormat:    format,
		OriginalRequest: []byte(payload),
	}
}

// soonestResetAffinityFixture returns an unprobed credential and one whose weekly window is
// known to reset in 12 hours, so the first pick of any session probes the unprobed one.
func soonestResetAffinityFixture(t *testing.T) (*SessionAffinitySelector, *Auth, *Auth, []*Auth) {
	t.Helper()
	now := time.Now()
	unprobed := &Auth{ID: "claude-a", Provider: "claude"}
	known := &Auth{ID: "claude-b", Provider: "claude", Quota: QuotaState{
		ObservedAt: now,
		Signals:    claudeProductionQuotaSignals(soonestResetEpoch(now.Add(12*time.Hour)), "0"),
	}}
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	t.Cleanup(selector.Stop)
	return selector, unprobed, known, []*Auth{unprobed, known}
}

func soonestResetAffinityPick(t *testing.T, selector *SessionAffinitySelector, step string, opts cliproxyexecutor.Options, auths []*Auth) string {
	t.Helper()
	picked, errPick := selector.Pick(context.Background(), "claude", "claude-opus-5-5", opts, auths)
	if errPick != nil {
		t.Fatalf("%s: Pick() error = %v", step, errPick)
	}
	if picked == nil {
		t.Fatalf("%s: Pick() returned nil", step)
	}
	return picked.ID
}

// A probe on the request that opens a conversation leaves the session unbound, so the
// session's next request is ranked by what the probe learned and binds there.
func TestSessionAffinitySoonestResetOpeningProbeDoesNotBindSession(t *testing.T) {
	selector, unprobed, known, auths := soonestResetAffinityFixture(t)

	opening := soonestResetAffinityOptions(sdktranslator.FormatClaude, soonestResetOpeningClaudeRequest)
	if got := soonestResetAffinityPick(t, selector, "opening probe", opening, auths); got != unprobed.ID {
		t.Fatalf("opening probe = %s, want %s", got, unprobed.ID)
	}
	if authID, _ := selector.LookupAffinity("claude", "claude-opus-5-5", "session-1"); authID != "" {
		t.Fatalf("LookupAffinity() after opening probe = %q, want unbound", authID)
	}
	now := time.Now()
	unprobed.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(soonestResetEpoch(now.Add(48*time.Hour)), "0.88")}

	continued := soonestResetAffinityOptions(sdktranslator.FormatClaude, soonestResetContinuedClaudeRequest)
	if got := soonestResetAffinityPick(t, selector, "ranked pick", continued, auths); got != known.ID {
		t.Fatalf("ranked pick = %s, want soonest reset %s", got, known.ID)
	}

	// The ranked pick binds: the session stays even once claude-a resets sooner.
	unprobed.Quota = QuotaState{ObservedAt: now, Signals: claudeProductionQuotaSignals(soonestResetEpoch(now.Add(time.Hour)), "0.88")}
	if got := soonestResetAffinityPick(t, selector, "bound pick", continued, auths); got != known.ID {
		t.Fatalf("bound pick = %s, want bound %s", got, known.ID)
	}
}

// A probe on a request that continues a conversation binds like any cold pick: the request
// may carry state only the probed credential accepts, and moving it loses its prompt cache.
func TestSessionAffinitySoonestResetContinuingProbeBindsSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		format  sdktranslator.Format
		payload string
	}{
		{"claude history", sdktranslator.FormatClaude, soonestResetContinuedClaudeRequest},
		{"codex previous response", sdktranslator.FormatCodex, `{"previous_response_id":"resp_1","input":[{"role":"user","content":"again"}]}`},
		{"codex encrypted reasoning", sdktranslator.FormatCodex, `{"input":[{"role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"opaque"}]}`},
		{"empty request", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, unprobed, _, auths := soonestResetAffinityFixture(t)
			opts := soonestResetAffinityOptions(tc.format, tc.payload)
			if got := soonestResetAffinityPick(t, selector, "probe", opts, auths); got != unprobed.ID {
				t.Fatalf("probe = %s, want %s", got, unprobed.ID)
			}
			if authID, _ := selector.LookupAffinity("claude", "claude-opus-5-5", "session-1"); authID != unprobed.ID {
				t.Fatalf("LookupAffinity() after probe = %q, want %q", authID, unprobed.ID)
			}
		})
	}
}
