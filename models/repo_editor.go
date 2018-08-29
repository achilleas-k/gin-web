// Copyright 2016 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unknwon/com"
	gouuid "github.com/satori/go.uuid"

	git "github.com/G-Node/git-module"

	"github.com/G-Node/gogs/pkg/process"
	"github.com/G-Node/gogs/pkg/setting"
	"github.com/G-Node/go-annex"
	"encoding/json"
	"bytes"
	"net/http"
)

const (
	ENV_AUTH_USER_ID           = "GOGS_AUTH_USER_ID"
	ENV_AUTH_USER_NAME         = "GOGS_AUTH_USER_NAME"
	ENV_AUTH_USER_EMAIL        = "GOGS_AUTH_USER_EMAIL"
	ENV_REPO_OWNER_NAME        = "GOGS_REPO_OWNER_NAME"
	ENV_REPO_OWNER_SALT_MD5    = "GOGS_REPO_OWNER_SALT_MD5"
	ENV_REPO_ID                = "GOGS_REPO_ID"
	ENV_REPO_NAME              = "GOGS_REPO_NAME"
	ENV_REPO_CUSTOM_HOOKS_PATH = "GOGS_REPO_CUSTOM_HOOKS_PATH"
)

type ComposeHookEnvsOptions struct {
	AuthUser  *User
	OwnerName string
	OwnerSalt string
	RepoID    int64
	RepoName  string
	RepoPath  string
}

func ComposeHookEnvs(opts ComposeHookEnvsOptions) []string {
	envs := []string{
		"SSH_ORIGINAL_COMMAND=1",
		ENV_AUTH_USER_ID + "=" + com.ToStr(opts.AuthUser.ID),
		ENV_AUTH_USER_NAME + "=" + opts.AuthUser.Name,
		ENV_AUTH_USER_EMAIL + "=" + opts.AuthUser.Email,
		ENV_REPO_OWNER_NAME + "=" + opts.OwnerName,
		ENV_REPO_OWNER_SALT_MD5 + "=" + tool.MD5(opts.OwnerSalt),
		ENV_REPO_ID + "=" + com.ToStr(opts.RepoID),
		ENV_REPO_NAME + "=" + opts.RepoName,
		ENV_REPO_CUSTOM_HOOKS_PATH + "=" + path.Join(opts.RepoPath, "custom_hooks"),
	}
	return envs
}

// ___________    .___.__  __    ___________.__.__
// \_   _____/  __| _/|__|/  |_  \_   _____/|__|  |   ____
//  |    __)_  / __ | |  \   __\  |    __)  |  |  | _/ __ \
//  |        \/ /_/ | |  ||  |    |     \   |  |  |_\  ___/
// /_______  /\____ | |__||__|    \___  /   |__|____/\___  >
//         \/      \/                 \/                 \/

// discardLocalRepoBranchChanges discards local commits/changes of
// given branch to make sure it is even to remote branch.
func discardLocalRepoBranchChanges(localPath, branch string) error {
	if !com.IsExist(localPath) {
		return nil
	}
	// No need to check if nothing in the repository.
	if !git.IsBranchExist(localPath, branch) {
		return nil
	}

	refName := "origin/" + branch
	if err := git.ResetHEAD(localPath, true, refName); err != nil {
		return fmt.Errorf("git reset --hard %s: %v", refName, err)
	}
	return nil
}

func (repo *Repository) DiscardLocalRepoBranchChanges(branch string) error {
	return discardLocalRepoBranchChanges(repo.LocalCopyPath(), branch)
}

// checkoutNewBranch checks out to a new branch from the a branch name.
func checkoutNewBranch(repoPath, localPath, oldBranch, newBranch string) error {
	if err := git.Checkout(localPath, git.CheckoutOptions{
		Timeout:   time.Duration(setting.Git.Timeout.Pull) * time.Second,
		Branch:    newBranch,
		OldBranch: oldBranch,
	}); err != nil {
		return fmt.Errorf("git checkout -b %s %s: %v", newBranch, oldBranch, err)
	}
	return nil
}

func (repo *Repository) CheckoutNewBranch(oldBranch, newBranch string) error {
	return checkoutNewBranch(repo.RepoPath(), repo.LocalCopyPath(), oldBranch, newBranch)
}

type UpdateRepoFileOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	OldTreeName  string
	NewTreeName  string
	Message      string
	Content      string
	IsNewFile    bool
}

// UpdateRepoFile adds or updates a file in repository.
func (repo *Repository) UpdateRepoFile(doer *User, opts UpdateRepoFileOptions) (err error) {
	repoWorkingPool.CheckIn(com.ToStr(repo.ID))
	defer repoWorkingPool.CheckOut(com.ToStr(repo.ID))

	if err = repo.DiscardLocalRepoBranchChanges(opts.OldBranch); err != nil {
		return fmt.Errorf("discard local repo branch[%s] changes: %v", opts.OldBranch, err)
	} else if err = repo.UpdateLocalCopyBranch(opts.OldBranch); err != nil {
		return fmt.Errorf("update local copy branch[%s]: %v", opts.OldBranch, err)
	}

	repoPath := repo.RepoPath()
	localPath := repo.LocalCopyPath()

	if opts.OldBranch != opts.NewBranch {
		// Directly return error if new branch already exists in the server
		if git.IsBranchExist(repoPath, opts.NewBranch) {
			return errors.BranchAlreadyExists{opts.NewBranch}
		}

		// Otherwise, delete branch from local copy in case out of sync
		if git.IsBranchExist(localPath, opts.NewBranch) {
			if err = git.DeleteBranch(localPath, opts.NewBranch, git.DeleteBranchOptions{
				Force: true,
			}); err != nil {
				return fmt.Errorf("delete branch[%s]: %v", opts.NewBranch, err)
			}
		}

		if err := repo.CheckoutNewBranch(opts.OldBranch, opts.NewBranch); err != nil {
			return fmt.Errorf("checkout new branch[%s] from old branch[%s]: %v", opts.NewBranch, opts.OldBranch, err)
		}
	}

	oldFilePath := path.Join(localPath, opts.OldTreeName)
	filePath := path.Join(localPath, opts.NewTreeName)
	os.MkdirAll(path.Dir(filePath), os.ModePerm)

	// If it's meant to be a new file, make sure it doesn't exist.
	if opts.IsNewFile {
		if com.IsExist(filePath) {
			return ErrRepoFileAlreadyExist{filePath}
		}
	}

	// Ignore move step if it's a new file under a directory.
	// Otherwise, move the file when name changed.
	if com.IsFile(oldFilePath) && opts.OldTreeName != opts.NewTreeName {
		if err = git.MoveFile(localPath, opts.OldTreeName, opts.NewTreeName); err != nil {
			return fmt.Errorf("git mv %q %q: %v", opts.OldTreeName, opts.NewTreeName, err)
		}
	}

	if err = ioutil.WriteFile(filePath, []byte(opts.Content), 0666); err != nil {
		return fmt.Errorf("write file: %v", err)
	}

	if err = git.AddChanges(localPath, true); err != nil {
		return fmt.Errorf("git add --all: %v", err)
	} else if err = git.CommitChanges(localPath, git.CommitChangesOptions{
		Committer: doer.NewGitSig(),
		Message:   opts.Message,
	}); err != nil {
		return fmt.Errorf("commit changes on %q: %v", localPath, err)
	} else if err = git.PushWithEnvs(localPath, "origin", opts.NewBranch,
		ComposeHookEnvs(ComposeHookEnvsOptions{
			AuthUser:  doer,
			OwnerName: repo.MustOwner().Name,
			OwnerSalt: repo.MustOwner().Salt,
			RepoID:    repo.ID,
			RepoName:  repo.Name,
			RepoPath:  repo.RepoPath(),
		})); err != nil {
		return fmt.Errorf("git push origin %s: %v", opts.NewBranch, err)
	}

	gitRepo, err := git.OpenRepository(repo.RepoPath())
	if err != nil {
		log.Error(2, "OpenRepository: %v", err)
		return nil
	}
	commit, err := gitRepo.GetBranchCommit(opts.NewBranch)
	if err != nil {
		log.Error(2, "GetBranchCommit [branch: %s]: %v", opts.NewBranch, err)
		return nil
	}

	// Simulate push event.
	pushCommits := &PushCommits{
		Len:     1,
		Commits: []*PushCommit{CommitToPushCommit(commit)},
	}
	oldCommitID := opts.LastCommitID
	if opts.NewBranch != opts.OldBranch {
		oldCommitID = git.EMPTY_SHA
	}
	if err := CommitRepoAction(CommitRepoActionOptions{
		PusherName:  doer.Name,
		RepoOwnerID: repo.MustOwner().ID,
		RepoName:    repo.Name,
		RefFullName: git.BRANCH_PREFIX + opts.NewBranch,
		OldCommitID: oldCommitID,
		NewCommitID: commit.ID.String(),
		Commits:     pushCommits,
	}); err != nil {
		log.Error(2, "CommitRepoAction: %v", err)
		return nil
	}

	go AddTestPullRequestTask(doer, repo.ID, opts.NewBranch, true)
	if setting.Search.Do {
		StartIndexing(doer, repo.MustOwner(), repo)
	}
	return nil
}

// GetDiffPreview produces and returns diff result of a file which is not yet committed.
func (repo *Repository) GetDiffPreview(branch, treePath, content string) (diff *Diff, err error) {
	repoWorkingPool.CheckIn(com.ToStr(repo.ID))
	defer repoWorkingPool.CheckOut(com.ToStr(repo.ID))

	if err = repo.DiscardLocalRepoBranchChanges(branch); err != nil {
		return nil, fmt.Errorf("discard local repo branch[%s] changes: %v", branch, err)
	} else if err = repo.UpdateLocalCopyBranch(branch); err != nil {
		return nil, fmt.Errorf("update local copy branch[%s]: %v", branch, err)
	}

	localPath := repo.LocalCopyPath()
	filePath := path.Join(localPath, treePath)
	os.MkdirAll(filepath.Dir(filePath), os.ModePerm)
	if err = ioutil.WriteFile(filePath, []byte(content), 0666); err != nil {
		return nil, fmt.Errorf("write file: %v", err)
	}

	cmd := exec.Command("git", "diff", treePath)
	cmd.Dir = localPath
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("get stdout pipe: %v", err)
	}

	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %v", err)
	}

	pid := process.Add(fmt.Sprintf("GetDiffPreview [repo_path: %s]", repo.RepoPath()), cmd)
	defer process.Remove(pid)

	diff, err = ParsePatch(setting.Git.MaxGitDiffLines, setting.Git.MaxGitDiffLineCharacters, setting.Git.MaxGitDiffFiles, stdout)
	if err != nil {
		return nil, fmt.Errorf("parse path: %v", err)
	}

	if err = cmd.Wait(); err != nil {
		return nil, fmt.Errorf("wait: %v", err)
	}

	return diff, nil
}

// ________         .__          __           ___________.__.__
// \______ \   ____ |  |   _____/  |_  ____   \_   _____/|__|  |   ____
//  |    |  \_/ __ \|  | _/ __ \   __\/ __ \   |    __)  |  |  | _/ __ \
//  |    `   \  ___/|  |_\  ___/|  | \  ___/   |     \   |  |  |_\  ___/
// /_______  /\___  >____/\___  >__|  \___  >  \___  /   |__|____/\___  >
//         \/     \/          \/          \/       \/                 \/
//

type DeleteRepoFileOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	TreePath     string
	Message      string
}

func (repo *Repository) DeleteRepoFile(doer *User, opts DeleteRepoFileOptions) (err error) {
	repoWorkingPool.CheckIn(com.ToStr(repo.ID))
	defer repoWorkingPool.CheckOut(com.ToStr(repo.ID))

	if err = repo.DiscardLocalRepoBranchChanges(opts.OldBranch); err != nil {
		return fmt.Errorf("discard local repo branch[%s] changes: %v", opts.OldBranch, err)
	} else if err = repo.UpdateLocalCopyBranch(opts.OldBranch); err != nil {
		return fmt.Errorf("update local copy branch[%s]: %v", opts.OldBranch, err)
	}

	if opts.OldBranch != opts.NewBranch {
		if err := repo.CheckoutNewBranch(opts.OldBranch, opts.NewBranch); err != nil {
			return fmt.Errorf("checkout new branch[%s] from old branch[%s]: %v", opts.NewBranch, opts.OldBranch, err)
		}
	}

	localPath := repo.LocalCopyPath()
	if err = os.Remove(path.Join(localPath, opts.TreePath)); err != nil {
		return fmt.Errorf("remove file %q: %v", opts.TreePath, err)
	}

	if err = git.AddChanges(localPath, true); err != nil {
		return fmt.Errorf("git add --all: %v", err)
	} else if err = git.CommitChanges(localPath, git.CommitChangesOptions{
		Committer: doer.NewGitSig(),
		Message:   opts.Message,
	}); err != nil {
		return fmt.Errorf("commit changes to %q: %v", localPath, err)
	} else if err = git.PushWithEnvs(localPath, "origin", opts.NewBranch,
		ComposeHookEnvs(ComposeHookEnvsOptions{
			AuthUser:  doer,
			OwnerName: repo.MustOwner().Name,
			OwnerSalt: repo.MustOwner().Salt,
			RepoID:    repo.ID,
			RepoName:  repo.Name,
			RepoPath:  repo.RepoPath(),
		})); err != nil {
		return fmt.Errorf("git push origin %s: %v", opts.NewBranch, err)
	}
	return nil
}

//  ____ ___        .__                    .___ ___________.___.__
// |    |   \______ |  |   _________     __| _/ \_   _____/|   |  |   ____   ______
// |    |   /\____ \|  |  /  _ \__  \   / __ |   |    __)  |   |  | _/ __ \ /  ___/
// |    |  / |  |_> >  |_(  <_> ) __ \_/ /_/ |   |     \   |   |  |_\  ___/ \___ \
// |______/  |   __/|____/\____(____  /\____ |   \___  /   |___|____/\___  >____  >
//           |__|                   \/      \/       \/                  \/     \/
//

// Upload represent a uploaded file to a repo to be deleted when moved
type Upload struct {
	ID   int64
	UUID string `xorm:"uuid UNIQUE"`
	Name string
}

// UploadLocalPath returns where uploads is stored in local file system based on given UUID.
func UploadLocalPath(uuid string) string {
	return path.Join(setting.Repository.Upload.TempPath, uuid[0:1], uuid[1:2], uuid)
}

// LocalPath returns where uploads are temporarily stored in local file system.
func (upload *Upload) LocalPath() string {
	return UploadLocalPath(upload.UUID)
}

// NewUpload creates a new upload object.
func NewUpload(name string, buf []byte, file multipart.File) (_ *Upload, err error) {
	if tool.IsMaliciousPath(name) {
		return nil, fmt.Errorf("malicious path detected: %s", name)
	}

	upload := &Upload{
		UUID: gouuid.NewV4().String(),
		Name: name,
	}

	localPath := upload.LocalPath()
	if err = os.MkdirAll(path.Dir(localPath), os.ModePerm); err != nil {
		return nil, fmt.Errorf("mkdir all: %v", err)
	}

	fw, err := os.Create(localPath)
	if err != nil {
		return nil, fmt.Errorf("create: %v", err)
	}
	defer fw.Close()

	if _, err = fw.Write(buf); err != nil {
		return nil, fmt.Errorf("write: %v", err)
	} else if _, err = io.Copy(fw, file); err != nil {
		return nil, fmt.Errorf("copy: %v", err)
	}

	if _, err := x.Insert(upload); err != nil {
		return nil, err
	}

	return upload, nil
}

func GetUploadByUUID(uuid string) (*Upload, error) {
	upload := &Upload{UUID: uuid}
	has, err := x.Get(upload)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrUploadNotExist{0, uuid}
	}
	return upload, nil
}

func GetUploadsByUUIDs(uuids []string) ([]*Upload, error) {
	if len(uuids) == 0 {
		return []*Upload{}, nil
	}

	// Silently drop invalid uuids.
	uploads := make([]*Upload, 0, len(uuids))
	return uploads, x.In("uuid", uuids).Find(&uploads)
}

func DeleteUploads(uploads ...*Upload) (err error) {
	if len(uploads) == 0 {
		return nil
	}

	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	ids := make([]int64, len(uploads))
	for i := 0; i < len(uploads); i++ {
		ids[i] = uploads[i].ID
	}
	if _, err = sess.In("id", ids).Delete(new(Upload)); err != nil {
		return fmt.Errorf("delete uploads: %v", err)
	}

	for _, upload := range uploads {
		localPath := upload.LocalPath()
		if !com.IsFile(localPath) {
			continue
		}

		if err := os.Remove(localPath); err != nil {
			return fmt.Errorf("remove upload: %v", err)
		}
	}

	return sess.Commit()
}

func DeleteUpload(u *Upload) error {
	return DeleteUploads(u)
}

func DeleteUploadByUUID(uuid string) error {
	upload, err := GetUploadByUUID(uuid)
	if err != nil {
		if IsErrUploadNotExist(err) {
			return nil
		}
		return fmt.Errorf("get upload by UUID[%s]: %v", uuid, err)
	}

	if err := DeleteUpload(upload); err != nil {
		return fmt.Errorf("delete upload: %v", err)
	}

	return nil
}

type UploadRepoFileOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	TreePath     string
	Message      string
	Files        []string // In UUID format
}

// isRepositoryGitPath returns true if given path is or resides inside ".git" path of the repository.
func isRepositoryGitPath(path string) bool {
	return strings.HasSuffix(path, ".git") || strings.Contains(path, ".git"+string(os.PathSeparator))
}

func (repo *Repository) UploadRepoFiles(doer *User, opts UploadRepoFileOptions) (err error) {
	if len(opts.Files) == 0 {
		return nil
	}

	uploads, err := GetUploadsByUUIDs(opts.Files)
	if err != nil {
		return fmt.Errorf("get uploads by UUIDs[%v]: %v", opts.Files, err)
	}

	repoWorkingPool.CheckIn(com.ToStr(repo.ID))
	defer repoWorkingPool.CheckOut(com.ToStr(repo.ID))

	if err = repo.DiscardLocalRepoBranchChanges(opts.OldBranch); err != nil {
		return fmt.Errorf("discard local repo branch[%s] changes: %v", opts.OldBranch, err)
	} else if err = repo.UpdateLocalCopyBranch(opts.OldBranch); err != nil {
		return fmt.Errorf("update local copy branch[%s]: %v", opts.OldBranch, err)
	}

	if opts.OldBranch != opts.NewBranch {
		if err = repo.CheckoutNewBranch(opts.OldBranch, opts.NewBranch); err != nil {
			return fmt.Errorf("checkout new branch[%s] from old branch[%s]: %v", opts.NewBranch, opts.OldBranch, err)
		}
	}

	localPath := repo.LocalCopyPath()
	dirPath := path.Join(localPath, opts.TreePath)
	os.MkdirAll(dirPath, os.ModePerm)
	log.Trace("localpath:%s", localPath)
	// prepare annex

	// Copy uploaded files into repository.
	for _, upload := range uploads {
		tmpPath := upload.LocalPath()
		targetPath := path.Join(dirPath, upload.Name)
		os.MkdirAll(filepath.Dir(targetPath), os.ModePerm)
		repoFileName := path.Join(opts.TreePath, upload.Name)
		if !com.IsFile(tmpPath) {
			continue
		}
		// needed for annex, due to symlinks
		os.Remove(targetPath)
		if err = com.Copy(tmpPath, targetPath); err != nil {
			return fmt.Errorf("copy: %v", err)
		}
		log.Trace("Check for annexing: %s", upload.Name)
		if finfo, err := os.Stat(targetPath); err == nil {
			log.Trace("Filesize is:%d", finfo.Size())
			// Should we annex
			if finfo.Size() > setting.Repository.Upload.AnexFileMinSize*gannex.MEGABYTE {
				log.Trace("This file should be annexed: %s", upload.Name)
				// annex init in case it isnt yet
				if msg, err := gannex.AInit(localPath, "annex.backend"); err != nil {
					log.Error(1, "Annex init failed with error: %v,%s,%s", err, msg, repoFileName)
				}
				// worm for compatibility
				gannex.Md5(localPath)
				if msg, err := gannex.Add(repoFileName, localPath); err != nil {
					log.Error(1, "Annex add failed with error: %v,%s,%s", err, msg, repoFileName)
				}
			}
		} else {
			log.Error(1, "could not stat: %s", targetPath)
		}
	}
	if err = git.AddChanges(localPath, true); err != nil {
		return fmt.Errorf("git add --all: %v", err)
	} else if err = git.CommitChanges(localPath, git.CommitChangesOptions{
		Committer: doer.NewGitSig(),
		Message:   opts.Message,
	}); err != nil {
		return fmt.Errorf("commit changes on %q: %v", localPath, err)
	} else if err = git.PushWithEnvs(localPath, "origin", opts.NewBranch,
		ComposeHookEnvs(ComposeHookEnvsOptions{
			AuthUser:  doer,
			OwnerName: repo.MustOwner().Name,
			OwnerSalt: repo.MustOwner().Salt,
			RepoID:    repo.ID,
			RepoName:  repo.Name,
			RepoPath:  repo.RepoPath(),
		})); err != nil {
		return fmt.Errorf("git push origin %s: %v", opts.NewBranch, err)
	}

	// Sometimes you need this twice
	if msg, err := gannex.ASync(localPath, "--content", "--no-pull"); err != nil {
		log.Error(1, "Annex sync failed with error: %v,%s", err, msg)
	} else {
		log.Trace("Annex sync:%s", msg)
	}
	if msg, err := gannex.ASync(localPath, "--content", "--no-pull"); err != nil {
		log.Error(1, "Annex sync failed with error: %v,%s", err, msg)
	} else {
		log.Trace("Annex sync:%s", msg)
	}

	gitRepo, err := git.OpenRepository(repo.RepoPath())
	if err != nil {
		log.Error(2, "OpenRepository: %v", err)
		return nil
	}
	commit, err := gitRepo.GetBranchCommit(opts.NewBranch)
	if err != nil {
		log.Error(2, "GetBranchCommit [branch: %s]: %v", opts.NewBranch, err)
		return nil
	}

	// Simulate push event.
	pushCommits := &PushCommits{
		Len:     1,
		Commits: []*PushCommit{CommitToPushCommit(commit)},
	}
	if err := CommitRepoAction(CommitRepoActionOptions{
		PusherName:  doer.Name,
		RepoOwnerID: repo.MustOwner().ID,
		RepoName:    repo.Name,
		RefFullName: git.BRANCH_PREFIX + opts.NewBranch,
		OldCommitID: opts.LastCommitID,
		NewCommitID: commit.ID.String(),
		Commits:     pushCommits,
	}); err != nil {
		log.Error(2, "CommitRepoAction: %v", err)
		return nil
	}
	go AddTestPullRequestTask(doer, repo.ID, opts.NewBranch, true)

	// We better start out cleaning now. No use keeping files around with annex
	if msg, err := gannex.AUInit(localPath); err != nil {
		log.Error(1, "Annex uninit failed with error: %v,%s, at: %s/ This repository might fail at "+
			"subsequent uploads!", err, msg, localPath)
	}
	// Indexing support
	if setting.Search.Do {
		StartIndexing(doer, repo.MustOwner(), repo)
	}

	RemoveAllWithNotice("Cleaning out after upload", localPath)
	return DeleteUploads(uploads...)
}

func StartIndexing(user, owner *User, repo *Repository) {
	var ireq struct{ RepoID, RepoPath string }
	ireq.RepoID = fmt.Sprintf("%d", repo.ID)
	ireq.RepoPath = repo.FullName()
	data, err := json.Marshal(ireq)
	if err != nil {
		log.Trace("could not marshal index request :%+v", err)
		return
	}
	req, _ := http.NewRequest(http.MethodPost, setting.Search.IndexUrl, bytes.NewReader(data))
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Trace("Error doing index request:%+v", err)
		return
	}
}
