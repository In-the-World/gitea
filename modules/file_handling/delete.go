// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package file_handling

import (
	"code.gitea.io/sdk/gitea"
	"fmt"
	"strings"

	"code.gitea.io/git"
	"code.gitea.io/gitea/models"
)

// DeleteRepoFileOptions holds the repository delete file options
type DeleteRepoFileOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	TreePath     string
	Message      string
	SHA          string
	Author       *IdentityOptions
	Committer    *IdentityOptions
}

// DeleteRepoFile deletes a file in the given repository
func DeleteRepoFile(repo *models.Repository, doer *models.User, opts *DeleteRepoFileOptions) (*gitea.FileResponse, error) {
	// If no branch name is set, assume master
	if opts.OldBranch == "" {
		opts.OldBranch = "master"
	}
	if opts.NewBranch == "" {
		opts.NewBranch = opts.OldBranch
	}

	// oldBranch must exist for this operation
	if _, err := repo.GetBranch(opts.OldBranch); err != nil {
		return nil, err
	}

	// A NewBranch can be specified for the file to be created/updated in a new branch
	// Check to make sure the branch does not already exist, otherwise we can't proceed.
	// If we aren't branching to a new branch, make sure user can commit to the given branch
	if opts.NewBranch != opts.OldBranch {
		newBranch, err := repo.GetBranch(opts.NewBranch)
		if git.IsErrNotExist(err) {
			return nil, err
		}
		if newBranch != nil {
			return nil, models.ErrBranchAlreadyExists{opts.NewBranch}
		}
	} else {
		if protected, _ := repo.IsProtectedBranchForPush(opts.OldBranch, doer); protected {
			return nil, models.ErrCannotCommit{UserName: doer.LowerName}
		}
	}

	// Check that the path given in opts.treeName is valid (not a git path)
	treePath := cleanUploadFileName(opts.TreePath)
	if treePath == "" {
		return nil, models.ErrFilenameInvalid{opts.TreePath}
	}

	message := strings.TrimSpace(opts.Message)

	var committer *models.User
	var author *models.User
	if opts.Committer != nil && opts.Committer.Email == "" {
		if c, err := models.GetUserByEmail(opts.Committer.Email); err != nil {
			committer = doer
		} else {
			committer = c
		}
	}
	if opts.Author != nil && opts.Author.Email == "" {
		if a, err := models.GetUserByEmail(opts.Author.Email); err != nil {
			author = doer
		} else {
			author = a
		}
	}
	if author == nil {
		if committer != nil {
			author = committer
		} else {
			author = doer
		}
	}
	if committer == nil {
		committer = author
	}
	doer = committer // UNTIL WE FIGURE OUT HOW TO ADD AUTHOR AND COMMITTER, USING JUST COMMITTER

	t, err := NewTemporaryUploadRepository(repo)
	defer t.Close()
	if err != nil {
		return nil, err
	}
	if err := t.Clone(opts.OldBranch); err != nil {
		return nil, err
	}
	if err := t.SetDefaultIndex(); err != nil {
		return nil, err
	}

	if opts.LastCommitID == "" {
		if commitID, err := t.GetLastCommit(); err != nil {
			return nil, err
		} else {
			opts.LastCommitID = commitID
		}
	}

	gitRepo, err := git.OpenRepository(repo.RepoPath())
	if err != nil {
		return nil, err
	}

	// Get the commit of the original branch
	commit, err := gitRepo.GetBranchCommit(opts.OldBranch)
	if err != nil {
		return nil, err // Couldn't get a commit for the branch
	}

	filesInIndex, err := t.LsFiles(opts.TreePath)
	if err != nil {
		return nil, fmt.Errorf("DeleteRepoFile: %v", err)
	}

	inFilelist := false
	for _, file := range filesInIndex {
		if file == opts.TreePath {
			inFilelist = true
		}
	}
	if !inFilelist {
		return nil, git.ErrNotExist{RelPath: opts.TreePath}
	}

	// Get the entry of treePath and check if the SHA given, if updating, is the same
	entry, err := commit.GetTreeEntryByPath(treePath)
	if err != nil {
		return nil, err
	}
	if opts.SHA != "" && opts.SHA != entry.ID.String() {
		return nil, models.ErrShaDoesNotMatch{
			GivenSHA:   opts.SHA,
			CurrentSHA: entry.ID.String(),
		}
	}

	if err := t.RemoveFilesFromIndex(opts.TreePath); err != nil {
		return nil, err
	}

	// Now write the tree
	treeHash, err := t.WriteTree()
	if err != nil {
		return nil, err
	}

	// Now commit the tree
	commitHash, err := t.CommitTree(doer, treeHash, message)
	if err != nil {
		return nil, err
	}

	// Then push this tree to NewBranch
	if err := t.Push(doer, commitHash, opts.NewBranch); err != nil {
		return nil, err
	}

	// Simulate push event.
	oldCommitID := opts.LastCommitID
	if opts.NewBranch != opts.OldBranch {
		oldCommitID = git.EmptySHA
	}

	if err = repo.GetOwner(); err != nil {
		return nil, fmt.Errorf("GetOwner: %v", err)
	}
	err = models.PushUpdate(
		opts.NewBranch,
		models.PushUpdateOptions{
			PusherID:     doer.ID,
			PusherName:   doer.Name,
			RepoUserName: repo.Owner.Name,
			RepoName:     repo.Name,
			RefFullName:  git.BranchPrefix + opts.NewBranch,
			OldCommitID:  oldCommitID,
			NewCommitID:  commitHash,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("PushUpdate: %v", err)
	}

	// FIXME: Should we UpdateRepoIndexer(repo) here?

	if file, err := GetFileResponseFromCommit(repo, commit, treePath); err != nil {
		return nil, err
	} else {
		return file, nil
	}
}
