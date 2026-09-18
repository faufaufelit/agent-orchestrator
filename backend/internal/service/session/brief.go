package session

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// briefMaxChangedFiles caps the changed-file rows in a session brief.
	// Totals stay exact: the cap bounds payload size for @mention
	// consumers, not the accounting.
	briefMaxChangedFiles = 50
	// briefMaxCommits caps the recent-commit rows in a session brief.
	briefMaxCommits = 10
)

// BriefChangedFile is one changed file row in a session brief.
type BriefChangedFile struct {
	Path         string
	PreviousPath string
	Status       WorkspaceFileStatus
	Additions    int
	Deletions    int
}

// BriefCommit is one recent commit row in a session brief, newest first.
type BriefCommit struct {
	SHA       string
	Subject   string
	Author    string
	Timestamp string
}

// SessionBrief is the @mention context for one session: who it is, what
// state it is in, what it changed, and where its PRs stand. It is what the
// orchestrator reads when the user names a worker, so it must answer
// "où t'en es" in a single round trip instead of one probe per facet.
type SessionBrief struct {
	Session domain.Session
	// WorkspaceAvailable is false when the worktree cannot be read
	// (terminated or reaped session): state and PR facts still answer, the
	// file/commit sections stay empty.
	WorkspaceAvailable bool
	ChangedFiles       []BriefChangedFile
	ChangedFilesTotal  int
	ChangedFilesCapped bool
	Commits            []BriefCommit
	CommitsCapped      bool
	Additions          int
	Deletions          int
	Ahead              *int
	Behind             *int
}

// Brief returns the @mention context for one session. Unknown sessions 404
// through Get; unreadable workspaces degrade to session facts rather than
// failing the brief.
func (s *Service) Brief(ctx context.Context, id domain.SessionID) (SessionBrief, error) {
	sess, err := s.Get(ctx, id)
	if err != nil {
		return SessionBrief{}, err
	}
	brief := SessionBrief{Session: sess}
	files, err := s.ListWorkspaceFiles(ctx, id)
	if err != nil {
		return brief, nil
	}
	brief.WorkspaceAvailable = true
	brief.Additions = files.Summary.Additions
	brief.Deletions = files.Summary.Deletions
	brief.Ahead = files.Ahead
	brief.Behind = files.Behind
	for _, file := range files.Files {
		if file.Status == WorkspaceFileUnmodified {
			continue
		}
		brief.ChangedFilesTotal++
		if len(brief.ChangedFiles) < briefMaxChangedFiles {
			brief.ChangedFiles = append(brief.ChangedFiles, BriefChangedFile{
				Path:         file.Path,
				PreviousPath: file.PreviousPath,
				Status:       file.Status,
				Additions:    file.Additions,
				Deletions:    file.Deletions,
			})
		}
	}
	brief.ChangedFilesCapped = brief.ChangedFilesTotal > len(brief.ChangedFiles)
	for i, commit := range files.Commits {
		if i >= briefMaxCommits {
			break
		}
		brief.Commits = append(brief.Commits, BriefCommit{
			SHA:       commit.SHA,
			Subject:   commit.Subject,
			Author:    commit.Author,
			Timestamp: commit.Timestamp.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	brief.CommitsCapped = len(files.Commits) > len(brief.Commits)
	return brief, nil
}
