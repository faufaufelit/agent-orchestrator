package escalation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

type fakeStore struct {
	sessions []domain.SessionRecord
	prs      map[domain.SessionID][]domain.PullRequest
	err      error
}

func (f *fakeStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	if f.err != nil {
		return domain.SessionRecord{}, false, f.err
	}
	for _, s := range f.sessions {
		if s.ID == id {
			return s, true, nil
		}
	}
	return domain.SessionRecord{}, false, nil
}

func (f *fakeStore) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sessions, nil
}

func (f *fakeStore) ListPRsBySession(_ context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.prs[id], nil
}

type fakeNudger struct {
	calls   []nudgeCall
	outcome sessionguard.Outcome
	err     error
}

type nudgeCall struct {
	id  domain.SessionID
	msg string
}

func (f *fakeNudger) Nudge(_ context.Context, id domain.SessionID, msg string) (sessionguard.Outcome, error) {
	f.calls = append(f.calls, nudgeCall{id: id, msg: msg})
	if f.outcome == sessionguard.SuppressedUnknown && f.err == nil {
		return sessionguard.Sent, nil
	}
	return f.outcome, f.err
}

type fakeNotifier struct {
	notifies []ports.NotificationIntent
	resolves []ports.NotificationResolution
	err      error
}

func (f *fakeNotifier) Notify(_ context.Context, intent ports.NotificationIntent) error {
	if f.err != nil {
		return f.err
	}
	f.notifies = append(f.notifies, intent)
	return nil
}

func (f *fakeNotifier) Resolve(_ context.Context, res ports.NotificationResolution) error {
	if f.err != nil {
		return f.err
	}
	f.resolves = append(f.resolves, res)
	return nil
}

func testSetup(now time.Time, worker domain.SessionRecord) (*fakeStore, *fakeNudger, *Coordinator) {
	orch := domain.SessionRecord{
		ID:        "orch-1",
		ProjectID: "p1",
		Kind:      domain.KindOrchestrator,
		Harness:   domain.AgentHarness("opencode"),
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
	}
	worker.ID = "worker-1"
	worker.ProjectID = "p1"
	worker.Kind = domain.KindWorker
	if worker.Harness == "" {
		worker.Harness = domain.AgentHarness("opencode")
	}
	store := &fakeStore{sessions: []domain.SessionRecord{orch, worker}, prs: map[domain.SessionID][]domain.PullRequest{}}
	nudger := &fakeNudger{}
	coord := New(store, nudger, Config{Clock: func() time.Time { return now }})
	return store, nudger, coord
}

func stuckWorker(now time.Time) domain.SessionRecord {
	return domain.SessionRecord{
		DisplayName: "stuck-job",
		Activity:    domain.Activity{State: domain.ActivityActive, LastActivityAt: now.Add(-11 * time.Minute)},
	}
}

func TestEvaluateSessionEscalatesStuckActiveOnce(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	_, nudger, coord := testSetup(now, stuckWorker(now))
	ctx := context.Background()

	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated || res.Reason != ReasonStuckActive {
		t.Fatalf("escalated=%v reason=%q, want true/%q", res.Escalated, res.Reason, ReasonStuckActive)
	}
	if len(nudger.calls) != 1 {
		t.Fatalf("nudges=%d, want 1", len(nudger.calls))
	}
	call := nudger.calls[0]
	if call.id != "orch-1" {
		t.Fatalf("escalated to %q, want the orchestrator", call.id)
	}
	for _, want := range []string{"[escalation]", "worker-1", "active", "11m", "No open PR"} {
		if !strings.Contains(call.msg, want) {
			t.Fatalf("escalation message missing %q: %q", want, call.msg)
		}
	}

	// A still-stuck worker must not be reported on every sweep.
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "already_escalated" {
		t.Fatalf("second sweep escalated=%v reason=%q, want dedup", res.Escalated, res.Reason)
	}
	if len(nudger.calls) != 1 {
		t.Fatalf("nudges=%d after re-sweep, want still 1", len(nudger.calls))
	}
}

func TestEvaluateSessionRearmsWhenWorkerMoves(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store, nudger, coord := testSetup(now, stuckWorker(now))
	ctx := context.Background()

	if _, err := coord.EvaluateSession(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	// The worker produced a signal, then went quiet again: a new episode.
	for i := range store.sessions {
		if store.sessions[i].ID == "worker-1" {
			store.sessions[i].Activity.LastActivityAt = now.Add(-time.Minute)
		}
	}
	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "not_stale" {
		t.Fatalf("fresh activity escalated=%v reason=%q, want not_stale", res.Escalated, res.Reason)
	}
	for i := range store.sessions {
		if store.sessions[i].ID == "worker-1" {
			store.sessions[i].Activity.LastActivityAt = now.Add(-30 * time.Minute)
		}
	}
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatalf("re-stuck worker not re-escalated: %+v", res)
	}
	if len(nudger.calls) != 2 {
		t.Fatalf("nudges=%d, want 2 across episodes", len(nudger.calls))
	}
}

func TestEvaluateSessionGateTable(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mutate     func(*fakeStore)
		id         domain.SessionID
		wantReason string
	}{
		{name: "orchestrator never targeted", id: "orch-1", wantReason: "not_worker"},
		{name: "terminated", mutate: func(f *fakeStore) {
			for i := range f.sessions {
				if f.sessions[i].ID == "worker-1" {
					f.sessions[i].IsTerminated = true
				}
			}
		}, id: "worker-1", wantReason: "terminated"},
		{name: "idle ignored", mutate: func(f *fakeStore) {
			for i := range f.sessions {
				if f.sessions[i].ID == "worker-1" {
					f.sessions[i].Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Hour)}
				}
			}
		}, id: "worker-1", wantReason: "state_ignored"},
		{name: "active fresh", mutate: func(f *fakeStore) {
			for i := range f.sessions {
				if f.sessions[i].ID == "worker-1" {
					f.sessions[i].Activity.LastActivityAt = now.Add(-time.Minute)
				}
			}
		}, id: "worker-1", wantReason: "not_stale"},
		{name: "no signal yet", mutate: func(f *fakeStore) {
			for i := range f.sessions {
				if f.sessions[i].ID == "worker-1" {
					f.sessions[i].Activity.LastActivityAt = time.Time{}
				}
			}
		}, id: "worker-1", wantReason: "no_activity_yet"},
		{name: "missing session", id: "ghost", wantReason: "session_not_found"},
		{name: "exited merged PR skipped", mutate: func(f *fakeStore) {
			for i := range f.sessions {
				if f.sessions[i].ID == "worker-1" {
					f.sessions[i].Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now.Add(-30 * time.Minute)}
				}
			}
			f.prs["worker-1"] = []domain.PullRequest{{URL: "pr1", Number: 7, Merged: true}}
		}, id: "worker-1", wantReason: "merged_pr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, nudger, coord := testSetup(now, stuckWorker(now))
			if tt.mutate != nil {
				tt.mutate(store)
			}
			res, err := coord.EvaluateSession(context.Background(), tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if res.Escalated || res.Reason != tt.wantReason {
				t.Fatalf("escalated=%v reason=%q, want false/%q", res.Escalated, res.Reason, tt.wantReason)
			}
			if len(nudger.calls) != 0 {
				t.Fatalf("nudges=%d, want 0", len(nudger.calls))
			}
		})
	}
}

func TestEvaluateSessionNeedsInputAndExited(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		state  domain.ActivityState
		ago    time.Duration
		reason string
		prs    []domain.PullRequest
	}{
		{name: "waiting input", state: domain.ActivityWaitingInput, ago: 16 * time.Minute, reason: ReasonNeedsInput},
		{name: "blocked", state: domain.ActivityBlocked, ago: 20 * time.Minute, reason: ReasonNeedsInput},
		{name: "exited no PR", state: domain.ActivityExited, ago: 11 * time.Minute, reason: ReasonExited},
		{name: "exited open PR", state: domain.ActivityExited, ago: 30 * time.Minute, reason: ReasonExited,
			prs: []domain.PullRequest{{URL: "pr1", Number: 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := stuckWorker(now)
			worker.Activity = domain.Activity{State: tt.state, LastActivityAt: now.Add(-tt.ago)}
			store, nudger, coord := testSetup(now, worker)
			store.prs["worker-1"] = tt.prs
			res, err := coord.EvaluateSession(context.Background(), "worker-1")
			if err != nil {
				t.Fatal(err)
			}
			if !res.Escalated || res.Reason != tt.reason {
				t.Fatalf("escalated=%v reason=%q, want true/%q", res.Escalated, res.Reason, tt.reason)
			}
			if len(nudger.calls) != 1 || nudger.calls[0].id != "orch-1" {
				t.Fatalf("nudges=%+v, want exactly one to orch-1", nudger.calls)
			}
			if tt.prs != nil && !strings.Contains(nudger.calls[0].msg, "#3") {
				t.Fatalf("escalation missing PR reference: %q", nudger.calls[0].msg)
			}
		})
	}
}

func TestEvaluateSessionNoOrchestratorStaysSilent(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store, nudger, coord := testSetup(now, stuckWorker(now))
	// Drop the orchestrator: escalation has nowhere to go.
	store.sessions = store.sessions[1:]
	res, err := coord.EvaluateSession(context.Background(), "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "no_orchestrator" {
		t.Fatalf("escalated=%v reason=%q, want false/no_orchestrator", res.Escalated, res.Reason)
	}
	if len(nudger.calls) != 0 {
		t.Fatalf("nudges=%d, want 0", len(nudger.calls))
	}
}

func TestEvaluateSessionSuppressedRetriesLater(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	_, nudger, coord := testSetup(now, stuckWorker(now))
	nudger.outcome = sessionguard.SuppressedAwaitingUser
	ctx := context.Background()

	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated {
		t.Fatalf("suppressed write reported as escalated: %+v", res)
	}
	// Suppression must not consume the episode: the next sweep retries.
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated {
		t.Fatalf("suppressed write reported as escalated on retry: %+v", res)
	}
	if len(nudger.calls) != 2 {
		t.Fatalf("nudges=%d, want a retry on the next sweep", len(nudger.calls))
	}

	// Once the orchestrator accepts, the episode is recorded.
	nudger.outcome = sessionguard.SuppressedUnknown
	nudger.err = nil
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatalf("accepted write not recorded: %+v", res)
	}
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "already_escalated" {
		t.Fatalf("post-accept sweep escalated=%v reason=%q, want dedup", res.Escalated, res.Reason)
	}
}

func TestSweepIsolatesSessions(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	_, nudger, coord := testSetup(now, stuckWorker(now))
	if err := coord.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(nudger.calls) != 1 {
		t.Fatalf("nudges=%d, want exactly the stuck worker escalated", len(nudger.calls))
	}
}

func testSetupNoOrchestrator(now time.Time, worker domain.SessionRecord, notifier *fakeNotifier) (*fakeStore, *fakeNudger, *Coordinator) {
	worker.ID = "worker-1"
	worker.ProjectID = "p1"
	worker.Kind = domain.KindWorker
	if worker.Harness == "" {
		worker.Harness = domain.AgentHarness("opencode")
	}
	store := &fakeStore{sessions: []domain.SessionRecord{worker}, prs: map[domain.SessionID][]domain.PullRequest{}}
	nudger := &fakeNudger{}
	coord := New(store, nudger, Config{Clock: func() time.Time { return now }, Notifier: notifier})
	return store, nudger, coord
}

func TestEvaluateSessionNoOrchestratorNotifiesHumanOnce(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	store, nudger, coord := testSetupNoOrchestrator(now, stuckWorker(now), notifier)
	ctx := context.Background()

	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated || res.Reason != ReasonStuckActive {
		t.Fatalf("escalated=%v reason=%q, want human fallback", res.Escalated, res.Reason)
	}
	if len(nudger.calls) != 0 {
		t.Fatalf("nudges=%d, want 0 without orchestrator", len(nudger.calls))
	}
	if len(notifier.notifies) != 1 {
		t.Fatalf("notifies=%d, want 1", len(notifier.notifies))
	}
	got := notifier.notifies[0]
	if got.Type != domain.NotificationNeedsInput || got.SessionID != "worker-1" || got.ProjectID != "p1" {
		t.Fatalf("intent=%+v, want needs_input for worker-1/p1", got)
	}

	// Same episode: no second notification.
	res, err = coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "already_escalated" {
		t.Fatalf("re-sweep escalated=%v reason=%q, want dedup", res.Escalated, res.Reason)
	}
	if len(notifier.notifies) != 1 {
		t.Fatalf("notifies=%d after re-sweep, want still 1", len(notifier.notifies))
	}

	// The worker moves and gets stuck again: resolve the old fallback,
	// then raise a fresh one.
	for i := range store.sessions {
		store.sessions[i].Activity.LastActivityAt = now.Add(-time.Minute)
	}
	if _, err := coord.EvaluateSession(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if len(notifier.resolves) != 1 || notifier.resolves[0].SessionID != "worker-1" {
		t.Fatalf("resolves=%+v, want the fallback closed on recovery", notifier.resolves)
	}
	for i := range store.sessions {
		store.sessions[i].Activity.LastActivityAt = now.Add(-30 * time.Minute)
	}
	if _, err := coord.EvaluateSession(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if len(notifier.notifies) != 2 {
		t.Fatalf("notifies=%d, want a fresh fallback for the new episode", len(notifier.notifies))
	}
}

func TestEvaluateSessionTerminatedResolvesFallback(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{}
	store, _, coord := testSetupNoOrchestrator(now, stuckWorker(now), notifier)
	ctx := context.Background()

	if _, err := coord.EvaluateSession(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}
	for i := range store.sessions {
		store.sessions[i].IsTerminated = true
	}
	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated || res.Reason != "terminated" {
		t.Fatalf("escalated=%v reason=%q, want terminated", res.Escalated, res.Reason)
	}
	if len(notifier.resolves) != 1 {
		t.Fatalf("resolves=%d, want the fallback closed on termination", len(notifier.resolves))
	}
}

func TestEvaluateSessionNotifyErrorRetries(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	notifier := &fakeNotifier{err: context.DeadlineExceeded}
	_, _, coord := testSetupNoOrchestrator(now, stuckWorker(now), notifier)
	ctx := context.Background()

	if _, err := coord.EvaluateSession(ctx, "worker-1"); err == nil {
		t.Fatal("want the notify error surfaced for retry")
	}
	notifier.err = nil
	res, err := coord.EvaluateSession(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated || len(notifier.notifies) != 1 {
		t.Fatalf("escalated=%v notifies=%d, want fallback after retry", res.Escalated, len(notifier.notifies))
	}
}
