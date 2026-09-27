// Package mergegate decides whether a pull request is safe to merge without
// a human looking at it.
package mergegate

import "time"

// PullRequest is the subset of a pull request that the gate looks at.
type PullRequest struct {
	Repo   string // owner/name
	Number int
	Title  string
	Body   string
	Author string
	IsBot  bool
	Draft  bool
	// BaseRef is the branch the PR merges into; the gate config is read from it.
	BaseRef string
	// BaseSHA is the base commit the diff is against; history is read from it.
	BaseSHA string
	// HeadSHA pins the commit that was judged.
	HeadSHA string
	Files   []File

	Labels []string
	// Comments are conversation comments, review bodies and inline review
	// comments, oldest first.
	Comments []Comment
	// Reviews are submitted reviews, oldest first.
	Reviews []Review
	// History is recent commits on the base branch (see rules.History),
	// newest first. Empty unless it was fetched.
	History []Commit
}

// Comment is one piece of discussion on a pull request.
type Comment struct {
	Author string
	// Association is GitHub's author_association: OWNER, MEMBER,
	// COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE, ...
	Association string
	Body        string
	Kind        string // "comment", "review" or "review_comment"
	CreatedAt   time.Time
}

// Review is a submitted pull request review.
type Review struct {
	Author      string
	Association string
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	SubmittedAt time.Time
}

// Commit is one commit from the base branch's history.
type Commit struct {
	SHA     string
	Message string
	Date    time.Time
}

// File is one changed file in a pull request.
type File struct {
	Path         string
	PreviousPath string // set when Status is "renamed"
	Status       string // added, removed, modified, renamed, ...
	Additions    int
	Deletions    int
	// Patch is the unified diff for the file. GitHub leaves it empty for
	// binary files and for diffs it considers too large to render.
	Patch string
}

// ChangedLines is the total number of added and removed lines.
func (pr PullRequest) ChangedLines() int {
	n := 0
	for _, f := range pr.Files {
		n += f.Additions + f.Deletions
	}
	return n
}
