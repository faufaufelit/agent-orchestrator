package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestBriefIncludesChangedFilesAndCommits(t *testing.T) {
	repo := newWorkspaceRepo(t)
	base := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	writeWorkspaceFile(t, repo, "src/app.go", "package main\n\n// second\n")
	runGit(t, repo, "add", "src/app.go")
	runGit(t, repo, "commit", "-m", "second")
	writeWorkspaceFile(t, repo, "README.md", "hello\nupdated\n")
	writeWorkspaceFile(t, repo, "notes.txt", "agent note\n")
	st := newFakeStore()
	st.projects["ao"] = domain.ProjectRecord{ID: "ao", Path: repo}
	now := time.Now().UTC()
	st.sessions["w1"] = domain.SessionRecord{
		ID:        "w1",
		ProjectID: "ao",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessOpenCode,
		Metadata:  domain.SessionMetadata{WorkspacePath: repo, DiffBaseSHA: base},
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
	}
	st.prFacts["w1"] = []domain.PRFacts{}

	brief, err := (&Service{store: st}).Brief(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if brief.Session.ID != "w1" {
		t.Fatalf("session id = %q, want w1", brief.Session.ID)
	}
	if brief.Session.Activity.State != domain.ActivityActive {
		t.Fatalf("activity = %q, want active", brief.Session.Activity.State)
	}
	if brief.Session.Status == "" {
		t.Fatal("derived status is empty")
	}
	if len(brief.Session.PRs) != 0 {
		t.Fatalf("prs = %d, want 0", len(brief.Session.PRs))
	}
	if !brief.WorkspaceAvailable {
		t.Fatal("workspace should be available")
	}
	byPath := map[string]BriefChangedFile{}
	for _, file := range brief.ChangedFiles {
		byPath[file.Path] = file
	}
	if byPath["README.md"].Status != WorkspaceFileModified {
		t.Fatalf("README status = %q, want modified", byPath["README.md"].Status)
	}
	if byPath["notes.txt"].Status != WorkspaceFileAdded {
		t.Fatalf("notes.txt status = %q, want added", byPath["notes.txt"].Status)
	}
	if brief.ChangedFilesTotal != len(brief.ChangedFiles) || brief.ChangedFilesCapped {
		t.Fatalf("total=%d listed=%d capped=%v, want exact uncapped list",
			brief.ChangedFilesTotal, len(brief.ChangedFiles), brief.ChangedFilesCapped)
	}
	if brief.Additions == 0 {
		t.Fatal("additions should be non-zero")
	}
	if len(brief.Commits) == 0 || brief.Commits[0].Subject != "second" {
		t.Fatalf("commits = %+v, want newest subject %q", brief.Commits, "second")
	}
	if brief.CommitsCapped {
		t.Fatal("single commit should not be capped")
	}
}

func TestBriefDegradesWithoutWorkspace(t *testing.T) {
	st := newFakeStore()
	now := time.Now().UTC()
	st.sessions["w9"] = domain.SessionRecord{
		ID:       "w9",
		Kind:     domain.KindWorker,
		Metadata: domain.SessionMetadata{WorkspacePath: t.TempDir() + "/gone"},
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
	}
	brief, err := (&Service{store: st}).Brief(context.Background(), "w9")
	if err != nil {
		t.Fatalf("missing worktree must degrade, not fail: %v", err)
	}
	if brief.Session.ID != "w9" {
		t.Fatalf("session id = %q, want w9", brief.Session.ID)
	}
	if brief.WorkspaceAvailable {
		t.Fatal("workspace must be reported unavailable")
	}
	if len(brief.ChangedFiles) != 0 || len(brief.Commits) != 0 {
		t.Fatal("file sections must be empty when the worktree is gone")
	}
}

func TestBriefUnknownSessionFails(t *testing.T) {
	st := newFakeStore()
	if _, err := (&Service{store: st}).Brief(context.Background(), "ghost"); err == nil {
		t.Fatal("unknown session must fail")
	}
}
