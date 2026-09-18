// Package escalation watches live worker sessions and escalates unattended
// states to the project's orchestrator session.
//
// AO's lifecycle reactions nudge the OWNING session (CI failures, review
// feedback, merge conflicts) and raise human notifications for input requests,
// but nothing ever tells the orchestrator that one of its workers is stuck:
// the orchestrator only learns what it reads or is told. The escalation closes
// that loop with a conservative periodic sweep. A worker that stays active
// with no activity signal, waits for input with nobody attending, or exits
// without landing work is reported to the live orchestrator once per episode,
// with the evidence attached.
//
// The escalation never touches the worker itself and never bypasses pane-write
// safety: escalation goes through the same sessionguard nudge the lifecycle
// reactions use, so a busy or decision-blocked orchestrator suppresses the
// write and the next sweep retries, exactly like any other nudge.
package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

const (
	// DefaultStuckActiveAfter is how long a worker may report active with no
	// activity signal before it counts as possibly stuck. Deliberately longer
	// than the activity observer's stale reconciliation so ordinary quiet
	// stretches demote to idle before the escalation ever fires.
	DefaultStuckActiveAfter = 10 * time.Minute
	// DefaultNeedsInputAfter is how long a worker may wait for input (or sit
	// blocked on a decision) before the wait is escalated. The human
	// notification center already covers these; escalation adds the
	// orchestrator loop so an unattended wait can be unblocked by the agent
	// that owns the plan instead of sitting until the user notices.
	DefaultNeedsInputAfter = 15 * time.Minute
	// DefaultExitedAfter is how long after an agent exit with no merged PR the
	// exit is escalated. Short enough to catch crashed workers, long enough
	// to let a normal teardown settle.
	DefaultExitedAfter = 10 * time.Minute
	// DefaultSweepInterval is the cadence for reevaluating live sessions.
	DefaultSweepInterval = time.Minute
)

// Escalation reasons reported in Result.Reason.
const (
	ReasonStuckActive = "stuck_active"
	ReasonNeedsInput  = "needs_input_unattended"
	ReasonExited      = "exited_unmerged"
)

// Store provides the durable session and PR facts used by the coordinator.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListAllSessions(context.Context) ([]domain.SessionRecord, error)
	ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error)
}

// Nudger delivers an escalation to the orchestrator session. It mirrors the
// sessionguard nudge contract: anything other than Sent means the message did
// NOT land and the escalation must stay unrecorded so the next sweep retries.
type Nudger interface {
	Nudge(context.Context, domain.SessionID, string) (sessionguard.Outcome, error)
}

// Result describes whether an evaluation escalated and why it skipped or
// escalated.
type Result struct {
	Escalated bool
	Reason    string
}

// escalationMark records one delivered escalation so a still-stuck worker is
// not reported on every sweep. The mark lifts as soon as the worker moves
// (LastActivityAt advances) or changes state, re-arming the next episode.
type escalationMark struct {
	reason         string
	lastActivityAt time.Time
}

// Coordinator evaluates worker states against the live orchestrator and owns
// the periodic sweep.
type Coordinator struct {
	store            Store
	nudger           Nudger
	clock            func() time.Time
	stuckActiveAfter time.Duration
	needsInputAfter  time.Duration
	exitedAfter      time.Duration
	sweepInterval    time.Duration
	logger           *slog.Logger

	mu        sync.Mutex
	escalated map[domain.SessionID]escalationMark
}

// Config customizes coordinator timing and logging.
type Config struct {
	Clock            func() time.Time
	StuckActiveAfter time.Duration
	NeedsInputAfter  time.Duration
	ExitedAfter      time.Duration
	SweepInterval    time.Duration
	Logger           *slog.Logger
}

// New constructs a escalation coordinator.
func New(store Store, nudger Nudger, cfg Config) *Coordinator {
	c := &Coordinator{
		store:            store,
		nudger:           nudger,
		clock:            cfg.Clock,
		stuckActiveAfter: cfg.StuckActiveAfter,
		needsInputAfter:  cfg.NeedsInputAfter,
		exitedAfter:      cfg.ExitedAfter,
		sweepInterval:    cfg.SweepInterval,
		logger:           cfg.Logger,
		escalated:        make(map[domain.SessionID]escalationMark),
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.stuckActiveAfter <= 0 {
		c.stuckActiveAfter = DefaultStuckActiveAfter
	}
	if c.needsInputAfter <= 0 {
		c.needsInputAfter = DefaultNeedsInputAfter
	}
	if c.exitedAfter <= 0 {
		c.exitedAfter = DefaultExitedAfter
	}
	if c.sweepInterval <= 0 {
		c.sweepInterval = DefaultSweepInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// EvaluateSession evaluates one session and escalates an unattended worker
// state to the project's live orchestrator.
func (c *Coordinator) EvaluateSession(ctx context.Context, id domain.SessionID) (Result, error) {
	worker, ok, err := c.store.GetSession(ctx, id)
	if err != nil || !ok {
		if err == nil {
			return Result{Reason: "session_not_found"}, nil
		}
		return Result{}, err
	}
	if worker.Kind != domain.KindWorker {
		return Result{Reason: "not_worker"}, nil
	}
	if worker.IsTerminated {
		return Result{Reason: "terminated"}, nil
	}
	sessions, err := c.store.ListAllSessions(ctx)
	if err != nil {
		return Result{}, err
	}
	orchestrator, ok := liveOrchestrator(sessions, worker.ProjectID)
	if !ok {
		return Result{Reason: "no_orchestrator"}, nil
	}
	last := worker.Activity.LastActivityAt
	if last.IsZero() {
		// No signal yet: a freshly spawned worker must not escalate before
		// its harness ever reports.
		return Result{Reason: "no_activity_yet"}, nil
	}
	now := c.clock()
	var reason string
	var threshold time.Duration
	switch worker.Activity.State {
	case domain.ActivityActive:
		reason, threshold = ReasonStuckActive, c.stuckActiveAfter
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		reason, threshold = ReasonNeedsInput, c.needsInputAfter
	case domain.ActivityExited:
		reason, threshold = ReasonExited, c.exitedAfter
	default:
		// Idle and unknown states are deliberately out of scope: without
		// worktree evidence an idle worker is indistinguishable from a
		// finished one, and nagging parked sessions would train users to
		// ignore the escalation.
		return Result{Reason: "state_ignored"}, nil
	}
	if age := now.Sub(last); age < threshold {
		return Result{Reason: "not_stale"}, nil
	}
	c.mu.Lock()
	if mark, dup := c.escalated[id]; dup {
		if mark.lastActivityAt.Equal(last) && mark.reason == reason {
			c.mu.Unlock()
			return Result{Reason: "already_escalated"}, nil
		}
		// The worker moved or changed state since the last escalation: the
		// old episode is over, evaluate the new one fresh.
		delete(c.escalated, id)
	}
	c.mu.Unlock()

	prs, err := c.store.ListPRsBySession(ctx, id)
	if err != nil {
		if reason == ReasonExited {
			// The merged-PR skip below is load-bearing for exited workers;
			// without PR facts fail closed and retry on the next sweep.
			return Result{}, err
		}
		// For liveness escalations PRs are context, not policy: proceed
		// without them rather than letting a PR-store outage mute the
		// escalation.
		c.logger.Warn("escalation: PR facts unavailable, escalating without them",
			"session_id", id, "err", err)
		prs = nil
	}
	if reason == ReasonExited && hasMergedPR(prs) {
		// The work landed; the lingering exited session is housekeeping, not
		// an incident.
		return Result{Reason: "merged_pr"}, nil
	}
	msg := escalationMessage(worker, reason, now.Sub(last), prs)
	outcome, err := c.nudger.Nudge(ctx, orchestrator.ID, msg)
	if outcome != sessionguard.Sent {
		return Result{Reason: outcome.String()}, err
	}
	c.mu.Lock()
	c.escalated[id] = escalationMark{reason: reason, lastActivityAt: last}
	c.mu.Unlock()
	c.logger.Info("escalation: escalated worker state to orchestrator",
		"session_id", id, "orchestrator_id", orchestrator.ID, "reason", reason)
	return Result{Escalated: true, Reason: reason}, err
}

// Sweep evaluates every live worker session, isolating per-session failures.
func (c *Coordinator) Sweep(ctx context.Context) error {
	sessions, err := c.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	evaluated := 0
	for _, session := range sessions {
		if session.Kind != domain.KindWorker || session.IsTerminated {
			continue
		}
		if _, err := c.EvaluateSession(ctx, session.ID); err != nil {
			c.logger.Error("escalation: evaluate session failed", "session_id", session.ID, "err", err)
		} else {
			evaluated++
		}
	}
	c.logger.Debug("escalation: sweep complete", "workers_evaluated", evaluated)
	return nil
}

// Start evaluates persisted session facts on the configured cadence until ctx
// is cancelled.
func (c *Coordinator) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Sweep(ctx); err != nil {
					c.logger.Error("escalation: sweep failed", "err", err)
				}
			}
		}
	}()
	return done
}

// liveOrchestrator resolves the project's current orchestrator at delivery
// time: the first non-terminated orchestrator session. There is no stored
// pointer to go stale when orchestrator sessions are replaced.
func liveOrchestrator(sessions []domain.SessionRecord, project domain.ProjectID) (domain.SessionRecord, bool) {
	for _, rec := range sessions {
		if rec.ProjectID == project && rec.Kind == domain.KindOrchestrator && !rec.IsTerminated {
			return rec, true
		}
	}
	return domain.SessionRecord{}, false
}

func hasMergedPR(prs []domain.PullRequest) bool {
	for _, pr := range prs {
		if pr.Merged {
			return true
		}
	}
	return false
}

// openPRSummary renders the PR facts attached to an escalation. Open PRs are
// evidence, not policy: a stuck worker with an open PR is still stuck.
func openPRSummary(prs []domain.PullRequest) string {
	var open []domain.PullRequest
	for _, pr := range prs {
		if !pr.Merged && !pr.Closed {
			open = append(open, pr)
		}
	}
	if len(open) == 0 {
		return "No open PR."
	}
	shown := open
	suffix := ""
	if len(open) > 3 {
		shown = open[:3]
		suffix = fmt.Sprintf(" (+%d more)", len(open)-3)
	}
	parts := make([]string, 0, len(shown))
	for _, pr := range shown {
		ref := fmt.Sprintf("#%d", pr.Number)
		if pr.URL != "" {
			ref += " " + pr.URL
		}
		parts = append(parts, ref)
	}
	return "Open PRs: " + strings.Join(parts, ", ") + suffix + "."
}

// escalationMessage builds the fact-first report the orchestrator receives.
// It names the evidence (state, silence duration, PRs) and asks for a
// decision, never for blind action.
func escalationMessage(worker domain.SessionRecord, reason string, age time.Duration, prs []domain.PullRequest) string {
	name := string(worker.ID)
	if worker.DisplayName != "" && worker.DisplayName != string(worker.ID) {
		name += fmt.Sprintf(" %q", worker.DisplayName)
	}
	last := worker.Activity.LastActivityAt.UTC().Format(time.RFC3339)
	mins := int(age.Minutes())
	var what string
	switch reason {
	case ReasonStuckActive:
		what = fmt.Sprintf("may be stuck: state=active with no activity for %dm (last %s)", mins, last)
	case ReasonNeedsInput:
		what = fmt.Sprintf("has needed input for %dm without attention: state=%s, last activity %s", mins, worker.Activity.State, last)
	case ReasonExited:
		what = fmt.Sprintf("exited %dm ago (%s) with no merged PR", mins, last)
	default:
		what = fmt.Sprintf("needs attention: state=%s, last activity %s", worker.Activity.State, last)
	}
	return fmt.Sprintf("[escalation] worker %s (%s) %s. %s Please check the session and resume, re-brief, or close it.",
		name, worker.Harness, what, openPRSummary(prs))
}
