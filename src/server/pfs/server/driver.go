package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	pfsserver "github.com/pachyderm/pachyderm/src/server/pfs"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/pfsdb"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/types"
	"github.com/hashicorp/golang-lru"
	"google.golang.org/grpc"
)

const (
	splitSuffixBase  = 16
	splitSuffixWidth = 64
	splitSuffixFmt   = "%016x"

	// Makes calls to ListRepo and InspectRepo more legible
	includeAuth = true
)

// ValidateRepoName determines if a repo name is valid
func ValidateRepoName(name string) error {
	match, _ := regexp.MatchString("^[a-zA-Z0-9_-]+$", name)

	if !match {
		return fmt.Errorf("repo name (%v) invalid: only alphanumeric characters, underscores, and dashes are allowed", name)
	}

	return nil
}

// ListFileMode specifies how ListFile executes.
type ListFileMode int

const (
	// ListFileNORMAL computes sizes for files but not for directories
	ListFileNORMAL ListFileMode = iota
	// ListFileFAST does not compute sizes for files or directories
	ListFileFAST
	// ListFileRECURSE computes sizes for files and directories
	ListFileRECURSE
)

// IsPermissionError returns true if a given error is a permission error.
func IsPermissionError(err error) bool {
	return strings.Contains(err.Error(), "has already finished")
}

// CommitEvent is an event that contains a CommitInfo or an error
type CommitEvent struct {
	Err   error
	Value *pfs.CommitInfo
}

// CommitStream is a stream of CommitInfos
type CommitStream interface {
	Stream() <-chan CommitEvent
	Close()
}

type collectionFactory func(string) col.Collection

type driver struct {
	// address, and pachConn are used to connect back to Pachd's Object Store API
	// and Authorization API
	address      string
	pachConnOnce sync.Once
	onceErr      error
	pachConn     *grpc.ClientConn

	// pachClient is a cached Pachd client, that connects to Pachyderm's object
	// store API and auth API
	pachClient *client.APIClient

	// etcdClient and prefix write repo and other metadata to etcd
	etcdClient *etcd.Client
	prefix     string

	// collections
	repos         col.Collection
	repoRefCounts col.Collection
	commits       collectionFactory
	branches      collectionFactory
	openCommits   col.Collection

	// a cache for hashtrees
	treeCache *lru.Cache
}

const (
	tombstone = "delete"
)

const (
	defaultTreeCacheSize = 128
)

// newDriver is used to create a new Driver instance
func newDriver(address string, etcdAddresses []string, etcdPrefix string, treeCacheSize int64) (*driver, error) {
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints:   etcdAddresses,
		DialOptions: client.EtcdDialOptions(),
	})
	if err != nil {
		return nil, fmt.Errorf("could not connect to etcd: %v", err)
	}
	if treeCacheSize <= 0 {
		treeCacheSize = defaultTreeCacheSize
	}
	treeCache, err := lru.New(int(treeCacheSize))
	if err != nil {
		return nil, fmt.Errorf("could not initialize treeCache: %v", err)
	}

	d := &driver{
		address:       address,
		etcdClient:    etcdClient,
		prefix:        etcdPrefix,
		repos:         pfsdb.Repos(etcdClient, etcdPrefix),
		repoRefCounts: pfsdb.RepoRefCounts(etcdClient, etcdPrefix),
		commits: func(repo string) col.Collection {
			return pfsdb.Commits(etcdClient, etcdPrefix, repo)
		},
		branches: func(repo string) col.Collection {
			return pfsdb.Branches(etcdClient, etcdPrefix, repo)
		},
		openCommits: pfsdb.OpenCommits(etcdClient, etcdPrefix),
		treeCache:   treeCache,
	}
	go func() { d.initializePachConn() }() // Begin dialing connection on startup
	return d, nil
}

// newLocalDriver creates a driver using an local etcd instance.  This
// function is intended for testing purposes
func newLocalDriver(blockAddress string, etcdPrefix string) (*driver, error) {
	return newDriver(blockAddress, []string{"localhost:32379"}, etcdPrefix, defaultTreeCacheSize)
}

// initializePachConn initializes the connects that the pfs driver has with the
// Pachyderm object API and auth API, and blocks until the connection is
// established
//
// TODO(msteffen): client initialization (both etcd and pachd) might be better
// placed happen in server.go, near main(), so that we only pay the dial cost
// once, and so that pps doesn't need to have its own initialization code
func (d *driver) initializePachConn() error {
	d.pachConnOnce.Do(func() {
		d.pachConn, d.onceErr = grpc.Dial(d.address, client.PachDialOptions()...)
		d.pachClient = &client.APIClient{
			AuthAPIClient:   auth.NewAPIClient(d.pachConn),
			ObjectAPIClient: pfs.NewObjectAPIClient(d.pachConn),
		}
	})
	return d.onceErr
}

// checkIsAuthorized returns an error if the current user (in 'ctx') has
// authorization scope 's' for repo 'r'
func (d *driver) checkIsAuthorized(ctx context.Context, r *pfs.Repo, s auth.Scope) error {
	d.initializePachConn()
	resp, err := d.pachClient.AuthAPIClient.Authorize(auth.In2Out(ctx), &auth.AuthorizeRequest{
		Repo:  r.Name,
		Scope: s,
	})
	if err == nil && !resp.Authorized {
		return &auth.NotAuthorizedError{Repo: r.Name, Required: s}
	} else if err != nil && !auth.IsNotActivatedError(err) {
		return fmt.Errorf("error during authorization check for operation on \"%s\": %v",
			r.Name, grpcutil.ScrubGRPC(err))
	}
	return nil
}

func now() *types.Timestamp {
	t, err := types.TimestampProto(time.Now())
	if err != nil {
		panic(err)
	}
	return t
}

func present(key string) etcd.Cmp {
	return etcd.Compare(etcd.CreateRevision(key), ">", 0)
}

func absent(key string) etcd.Cmp {
	return etcd.Compare(etcd.CreateRevision(key), "=", 0)
}

func (d *driver) createRepo(ctx context.Context, repo *pfs.Repo, provenance []*pfs.Repo, description string, update bool) error {
	if err := ValidateRepoName(repo.Name); err != nil {
		return err
	}
	d.initializePachConn()
	if update {
		return d.updateRepo(ctx, repo, provenance, description)
	}

	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		repos := d.repos.ReadWrite(stm)
		repoRefCounts := d.repoRefCounts.ReadWriteInt(stm)

		// check if 'repo' already exists. If so, return that error. Otherwise,
		// proceed with auth check (avoids awkward "access denied" error when
		// calling "createRepo" on a repo that already exists)
		var existingRepoInfo pfs.RepoInfo
		err := repos.Get(repo.Name, &existingRepoInfo)
		if err != nil && !col.IsErrNotFound(err) {
			return fmt.Errorf("error checking whether \"%s\" exists: %v",
				repo.Name, err)
		} else if err == nil {
			return fmt.Errorf("cannot create \"%s\" as it already exists", repo.Name)
		}
		// Create ACL for new repo
		whoAmI, err := d.pachClient.AuthAPIClient.WhoAmI(auth.In2Out(ctx),
			&auth.WhoAmIRequest{})
		if err != nil && !auth.IsNotActivatedError(err) {
			return fmt.Errorf("error while creating repo \"%s\": %v",
				repo.Name, grpcutil.ScrubGRPC(err))
		} else if err == nil {
			// auth is active, and user is logged in. Make user an owner of the new
			// repo (and clear any existing ACL under this name that might have been
			// created by accident)
			_, err := d.pachClient.AuthAPIClient.SetACL(auth.In2Out(ctx), &auth.SetACLRequest{
				Repo: repo.Name,
				NewACL: &auth.ACL{
					Entries: map[string]auth.Scope{
						whoAmI.Username: auth.Scope_OWNER,
					},
				},
			})
			if err != nil {
				return fmt.Errorf("could not create ACL for new repo \"%s\": %v",
					repo.Name, grpcutil.ScrubGRPC(err))
			}
		}

		// compute the full provenance of this repo
		fullProv := make(map[string]bool)
		for _, prov := range provenance {
			fullProv[prov.Name] = true
			provRepo := new(pfs.RepoInfo)
			if err := repos.Get(prov.Name, provRepo); err != nil {
				return err
			}
			// the provenance of my provenance is my provenance
			for _, prov := range provRepo.Provenance {
				fullProv[prov.Name] = true
			}
		}

		var fullProvRepos []*pfs.Repo
		for prov := range fullProv {
			fullProvRepos = append(fullProvRepos, &pfs.Repo{prov})
			if err := repoRefCounts.Increment(prov); err != nil {
				return err
			}
		}
		if err := repoRefCounts.Create(repo.Name, 0); err != nil {
			return err
		}
		repoInfo := &pfs.RepoInfo{
			Repo:        repo,
			Created:     now(),
			Provenance:  fullProvRepos,
			Description: description,
		}
		return repos.Create(repo.Name, repoInfo)
	})
	return err
}

func (d *driver) updateRepo(ctx context.Context, repo *pfs.Repo, provenance []*pfs.Repo, description string) error {
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		repos := d.repos.ReadWrite(stm)
		repoRefCounts := d.repoRefCounts.ReadWriteInt(stm)

		repoInfo := new(pfs.RepoInfo)
		if err := repos.Get(repo.Name, repoInfo); err != nil {
			return fmt.Errorf("error updating repo: %v", err)
		}
		// Caller only neads to be a WRITER to call UpdatePipeline(), so caller only
		// needs to be a WRITER to update the provenance.
		if err := d.checkIsAuthorized(ctx, repo, auth.Scope_WRITER); err != nil {
			return err
		}

		provToAdd := make(map[string]bool)
		provToRemove := make(map[string]bool)
		for _, newProv := range provenance {
			provToAdd[newProv.Name] = true
		}
		for _, oldProv := range repoInfo.Provenance {
			delete(provToAdd, oldProv.Name)
			provToRemove[oldProv.Name] = true
		}
		for _, newProv := range provenance {
			delete(provToRemove, newProv.Name)
		}

		// For each new provenance repo, we increase its ref count
		// by N where N is this repo's ref count.
		// For each old provenance repo we do the opposite.
		myRefCount, err := repoRefCounts.Get(repo.Name)
		if err != nil {
			return err
		}
		// +1 because we need to include ourselves.
		myRefCount++

		for newProv := range provToAdd {
			if err := repoRefCounts.IncrementBy(newProv, myRefCount); err != nil {
				return err
			}
		}

		for oldProv := range provToRemove {
			if err := repoRefCounts.DecrementBy(oldProv, myRefCount); err != nil {
				return err
			}
		}

		// We also add the new provenance repos to the provenance
		// of all downstream repos, and remove the old provenance
		// repos from their provenance.
		downstreamRepos, err := d.listRepo(ctx, []*pfs.Repo{repo}, !includeAuth)
		if err != nil {
			return err
		}

		for _, repoInfo := range downstreamRepos.RepoInfo {
		nextNewProv:
			for newProv := range provToAdd {
				for _, prov := range repoInfo.Provenance {
					if newProv == prov.Name {
						continue nextNewProv
					}
				}
				repoInfo.Provenance = append(repoInfo.Provenance, &pfs.Repo{newProv})
			}
		nextOldProv:
			for oldProv := range provToRemove {
				for i, prov := range repoInfo.Provenance {
					if oldProv == prov.Name {
						repoInfo.Provenance = append(repoInfo.Provenance[:i], repoInfo.Provenance[i+1:]...)
						continue nextOldProv
					}
				}
			}
			repos.Put(repoInfo.Repo.Name, repoInfo)
		}

		repoInfo.Description = description
		repoInfo.Provenance = provenance
		repos.Put(repo.Name, repoInfo)
		return nil
	})
	return err
}

func (d *driver) inspectRepo(ctx context.Context, repo *pfs.Repo, includeAuth bool) (*pfs.RepoInfo, error) {
	d.initializePachConn()
	result := &pfs.RepoInfo{}
	if err := d.repos.ReadOnly(ctx).Get(repo.Name, result); err != nil {
		return nil, err
	}
	if includeAuth {
		accessLevel, err := d.getAccessLevel(ctx, repo)
		if err != nil {
			if auth.IsNotActivatedError(err) {
				return result, nil
			}
			return nil, fmt.Errorf("error getting access level for \"%s\": %v",
				repo.Name, grpcutil.ScrubGRPC(err))
		}
		result.AuthInfo = &pfs.RepoAuthInfo{AccessLevel: accessLevel}
	}
	return result, nil
}

func (d *driver) getAccessLevel(ctx context.Context, repo *pfs.Repo) (auth.Scope, error) {
	who, err := d.pachClient.AuthAPIClient.WhoAmI(auth.In2Out(ctx),
		&auth.WhoAmIRequest{})
	if err != nil {
		return auth.Scope_NONE, err
	}
	if who.IsAdmin {
		return auth.Scope_OWNER, nil
	}
	resp, err := d.pachClient.AuthAPIClient.GetScope(auth.In2Out(ctx),
		&auth.GetScopeRequest{Repos: []string{repo.Name}})
	if err != nil {
		return auth.Scope_NONE, err
	}
	if len(resp.Scopes) != 1 {
		return auth.Scope_NONE, fmt.Errorf("too many results from GetScope: %#v", resp)
	}
	return resp.Scopes[0], nil
}

func (d *driver) listRepo(ctx context.Context, provenance []*pfs.Repo, includeAuth bool) (*pfs.ListRepoResponse, error) {
	repos := d.repos.ReadOnly(ctx)
	// Ensure that all provenance repos exist
	for _, prov := range provenance {
		repoInfo := &pfs.RepoInfo{}
		if err := repos.Get(prov.Name, repoInfo); err != nil {
			return nil, err
		}
	}

	iterator, err := repos.List()
	if err != nil {
		return nil, err
	}
	result := new(pfs.ListRepoResponse)
	authSeemsActive := true
nextRepo:
	for {
		repoName, repoInfo := "", new(pfs.RepoInfo)
		ok, err := iterator.Next(&repoName, repoInfo)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		// A repo needs to have *all* the given repos as provenance
		// in order to be included in the result.
		for _, reqProv := range provenance {
			var matched bool
			for _, prov := range repoInfo.Provenance {
				if reqProv.Name == prov.Name {
					matched = true
				}
			}
			if !matched {
				continue nextRepo
			}
		}
		if includeAuth && authSeemsActive {
			accessLevel, err := d.getAccessLevel(ctx, repoInfo.Repo)
			if err == nil {
				repoInfo.AuthInfo = &pfs.RepoAuthInfo{AccessLevel: accessLevel}
			} else if auth.IsNotActivatedError(err) {
				authSeemsActive = false
			} else {
				return nil, fmt.Errorf("error getting access level for \"%s\": %v",
					repoName, grpcutil.ScrubGRPC(err))
			}
		}
		result.RepoInfo = append(result.RepoInfo, repoInfo)
	}
	return result, nil
}

func (d *driver) deleteRepo(ctx context.Context, repo *pfs.Repo, force bool) error {
	if err := d.checkIsAuthorized(ctx, repo, auth.Scope_OWNER); err != nil {
		return err
	}
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		repos := d.repos.ReadWrite(stm)
		repoRefCounts := d.repoRefCounts.ReadWriteInt(stm)
		commits := d.commits(repo.Name).ReadWrite(stm)
		branches := d.branches(repo.Name).ReadWrite(stm)

		// Check if this repo is the provenance of some other repos
		if !force {
			refCount, err := repoRefCounts.Get(repo.Name)
			if err != nil {
				return err
			}
			if refCount != 0 {
				return fmt.Errorf("cannot delete the provenance of other repos")
			}
		}

		repoInfo := new(pfs.RepoInfo)
		if err := repos.Get(repo.Name, repoInfo); err != nil {
			return err
		}
		for _, prov := range repoInfo.Provenance {
			if err := repoRefCounts.Decrement(prov.Name); err != nil && !col.IsErrNotFound(err) {
				// Skip NotFound error, because it's possible that the
				// provenance repo has been deleted via --force.
				return err
			}
		}

		if err := repos.Delete(repo.Name); err != nil {
			return err
		}
		if err := repoRefCounts.Delete(repo.Name); err != nil {
			return err
		}
		commits.DeleteAll()
		branches.DeleteAll()
		return nil
	})
	if err != nil {
		return err
	}

	if _, err = d.pachClient.AuthAPIClient.SetACL(auth.In2Out(ctx), &auth.SetACLRequest{
		Repo: repo.Name, // NewACL is unset, so this will clear the acl for 'repo'
	}); err != nil && !auth.IsNotActivatedError(err) {
		return grpcutil.ScrubGRPC(err)
	}
	return nil
}

func (d *driver) startCommit(ctx context.Context, parent *pfs.Commit, branch string, provenance []*pfs.Commit) (*pfs.Commit, error) {
	return d.makeCommit(ctx, parent, branch, provenance, nil)
}

func (d *driver) buildCommit(ctx context.Context, parent *pfs.Commit, branch string, provenance []*pfs.Commit, tree *pfs.Object) (*pfs.Commit, error) {
	return d.makeCommit(ctx, parent, branch, provenance, tree)
}

func (d *driver) makeCommit(ctx context.Context, parent *pfs.Commit, branch string, provenance []*pfs.Commit, treeRef *pfs.Object) (*pfs.Commit, error) {
	if err := d.checkIsAuthorized(ctx, parent.Repo, auth.Scope_WRITER); err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, fmt.Errorf("parent cannot be nil")
	}
	commit := &pfs.Commit{
		Repo: parent.Repo,
		ID:   uuid.NewWithoutDashes(),
	}
	var tree hashtree.HashTree
	if treeRef != nil {
		var buf bytes.Buffer
		if err := d.pachClient.GetObject(treeRef.Hash, &buf); err != nil {
			return nil, err
		}
		_tree, err := hashtree.Deserialize(buf.Bytes())
		if err != nil {
			return nil, err
		}
		tree = _tree
	}
	if _, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		repos := d.repos.ReadWrite(stm)
		commits := d.commits(parent.Repo.Name).ReadWrite(stm)
		branches := d.branches(parent.Repo.Name).ReadWrite(stm)

		// Check if repo exists
		repoInfo := new(pfs.RepoInfo)
		if err := repos.Get(parent.Repo.Name, repoInfo); err != nil {
			return err
		}

		commitInfo := &pfs.CommitInfo{
			Commit:  commit,
			Started: now(),
		}

		// Use a map to de-dup provenance
		provenanceMap := make(map[string]*pfs.Commit)
		// Build the full provenance; my provenance's provenance is
		// my provenance
		for _, prov := range provenance {
			provCommits := d.commits(prov.Repo.Name).ReadWrite(stm)
			provCommitInfo := new(pfs.CommitInfo)
			if err := provCommits.Get(prov.ID, provCommitInfo); err != nil {
				return err
			}
			for _, c := range provCommitInfo.Provenance {
				provenanceMap[c.ID] = c
			}
		}
		// finally include the given provenance
		for _, c := range provenance {
			provenanceMap[c.ID] = c
		}

		for _, c := range provenanceMap {
			commitInfo.Provenance = append(commitInfo.Provenance, c)
		}

		if branch != "" {
			// If we don't have an explicit parent we use the previous head of
			// branch as the parent, if it exists.
			if parent.ID == "" {
				head := new(pfs.Commit)
				if err := branches.Get(branch, head); err != nil {
					if _, ok := err.(col.ErrNotFound); !ok {
						return err
					}
				} else {
					parent.ID = head.ID
				}
			}
			// Make commit the new head of the branch
			if err := branches.Put(branch, commit); err != nil {
				return err
			}
		}
		if parent.ID != "" {
			parentCommitInfo, err := d.inspectCommit(ctx, parent)
			if err != nil {
				return err
			}
			// fail if the parent commit has not been finished
			if parentCommitInfo.Finished == nil {
				return fmt.Errorf("parent commit %s has not been finished", parent.ID)
			}
			commitInfo.ParentCommit = parent
		}
		parentTree, err := d.getTreeForCommit(ctx, parent)
		if err != nil {
			return err
		}
		if treeRef != nil {
			commitInfo.Tree = treeRef
			commitInfo.SizeBytes = uint64(tree.FSSize())
			commitInfo.Finished = now()
			repoInfo.SizeBytes += sizeChange(tree, parentTree)
			repos.Put(parent.Repo.Name, repoInfo)
		} else {
			d.openCommits.ReadWrite(stm).Put(commit.ID, commit)
		}
		return commits.Create(commit.ID, commitInfo)
	}); err != nil {
		return nil, err
	}

	return commit, nil
}

func (d *driver) finishCommit(ctx context.Context, commit *pfs.Commit) error {
	if err := d.checkIsAuthorized(ctx, commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	commitInfo, err := d.inspectCommit(ctx, commit)
	if err != nil {
		return err
	}
	if commitInfo.Finished != nil {
		return fmt.Errorf("commit %s has already been finished", commit.FullID())
	}

	prefix, err := d.scratchCommitPrefix(ctx, commit)
	if err != nil {
		return err
	}

	// Read everything under the scratch space for this commit
	resp, err := d.etcdClient.Get(ctx, prefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortAscend))
	if err != nil {
		return err
	}

	parentTree, err := d.getTreeForCommit(ctx, commitInfo.ParentCommit)
	if err != nil {
		return err
	}
	tree := parentTree.Open()

	if err := d.applyWrites(resp, tree); err != nil {
		return err
	}

	finishedTree, err := tree.Finish()
	if err != nil {
		return err
	}
	// Serialize the tree
	data, err := hashtree.Serialize(finishedTree)
	if err != nil {
		return err
	}

	if len(data) > 0 {
		// Put the tree into the blob store
		obj, _, err := d.pachClient.PutObject(bytes.NewReader(data))
		if err != nil {
			return err
		}

		commitInfo.Tree = obj
	}

	commitInfo.SizeBytes = uint64(finishedTree.FSSize())
	commitInfo.Finished = now()

	sizeChange := sizeChange(finishedTree, parentTree)
	_, err = col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		commits := d.commits(commit.Repo.Name).ReadWrite(stm)
		repos := d.repos.ReadWrite(stm)

		commits.Put(commit.ID, commitInfo)
		if err := d.openCommits.ReadWrite(stm).Delete(commit.ID); err != nil {
			return fmt.Errorf("could not confirm that commit %s is open; this is likely a bug. err: %v", commit.ID, err)
		}
		// update repo size
		repoInfo := new(pfs.RepoInfo)
		if err := repos.Get(commit.Repo.Name, repoInfo); err != nil {
			return err
		}

		// Increment the repo sizes by the sizes of the files that have
		// been added in this commit.
		repoInfo.SizeBytes += sizeChange
		repos.Put(commit.Repo.Name, repoInfo)
		return nil
	})
	if err != nil {
		return err
	}

	// Delete the scratch space for this commit
	_, err = d.etcdClient.Delete(ctx, prefix, etcd.WithPrefix())
	return err
}

func sizeChange(tree hashtree.HashTree, parentTree hashtree.HashTree) uint64 {
	if parentTree == nil {
		return uint64(tree.FSSize())
	}
	var result uint64
	tree.Diff(parentTree, "", "", -1, func(path string, node *hashtree.NodeProto, new bool) error {
		if node.FileNode != nil && new {
			result += uint64(node.SubtreeSize)
		}
		return nil
	})
	return result
}

// inspectCommit takes a Commit and returns the corresponding CommitInfo.
//
// As a side effect, this function also replaces the ID in the given commit
// with a real commit ID.
func (d *driver) inspectCommit(ctx context.Context, commit *pfs.Commit) (*pfs.CommitInfo, error) {
	if commit == nil {
		return nil, fmt.Errorf("cannot inspect nil commit")
	}
	if err := d.checkIsAuthorized(ctx, commit.Repo, auth.Scope_READER); err != nil {
		return nil, err
	}

	commitID, ancestryLength := parseCommitID(commit.ID)

	// Check if the commitID is a branch name
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		branches := d.branches(commit.Repo.Name).ReadWrite(stm)

		head := new(pfs.Commit)
		// See if we are given a branch
		if err := branches.Get(commitID, head); err != nil {
			if _, ok := err.(col.ErrNotFound); !ok {
				return err
			}
			// If it's not a branch, use it as it is
			return nil
		}
		commitID = head.ID
		return nil
	})
	if err != nil {
		return nil, err
	}

	var commitInfo *pfs.CommitInfo
	nextCommit := &pfs.Commit{
		Repo: commit.Repo,
		ID:   commitID,
	}
	for i := 0; i <= ancestryLength; i++ {
		if nextCommit == nil {
			return nil, pfsserver.ErrCommitNotFound{commit}
		}
		commits := d.commits(commit.Repo.Name).ReadOnly(ctx)
		commitInfo = new(pfs.CommitInfo)
		if err := commits.Get(nextCommit.ID, commitInfo); err != nil {
			return nil, pfsserver.ErrCommitNotFound{nextCommit}
		}
		nextCommit = commitInfo.ParentCommit
	}

	commit.ID = commitInfo.Commit.ID
	return commitInfo, nil
}

// parseCommitID accepts a commit ID that might contain the Git ancestry
// syntax, such as "master^2", "master~~", "master^^", "master~5", etc.
// It then returns the ID component such as "master" and the depth of the
// ancestor.  For instance, for "master^2" it'd return "master" and 2.
func parseCommitID(commitID string) (string, int) {
	sepIndex := strings.IndexAny(commitID, "^~")
	if sepIndex == -1 {
		return commitID, 0
	}

	// Find the separator, which is either "^" or "~"
	sep := commitID[sepIndex]
	strAfterSep := commitID[sepIndex+1:]

	// Try convert the string after the separator to an int.
	intAfterSep, err := strconv.Atoi(strAfterSep)
	// If it works, return
	if err == nil {
		return commitID[:sepIndex], intAfterSep
	}

	// Otherwise, we check if there's a sequence of separators, as in
	// "master^^^^" or "master~~~~"
	for i := sepIndex + 1; i < len(commitID); i++ {
		if commitID[i] != sep {
			// If we find a character that's not the separator, as in
			// "master~whatever", then we return.
			return commitID, 0
		}
	}

	// Here we've confirmed that the commit ID ends with a sequence of
	// (the same) separators and therefore uses the correct ancestry
	// syntax.
	return commitID[:sepIndex], len(commitID) - sepIndex
}

func (d *driver) listCommit(ctx context.Context, repo *pfs.Repo, to *pfs.Commit, from *pfs.Commit, number uint64) ([]*pfs.CommitInfo, error) {
	if err := d.checkIsAuthorized(ctx, repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	if from != nil && from.Repo.Name != repo.Name || to != nil && to.Repo.Name != repo.Name {
		return nil, fmt.Errorf("`from` and `to` commits need to be from repo %s", repo.Name)
	}

	// Make sure that the repo exists
	_, err := d.inspectRepo(ctx, repo, !includeAuth)
	if err != nil {
		return nil, err
	}

	// Make sure that both from and to are valid commits
	if from != nil {
		_, err = d.inspectCommit(ctx, from)
		if err != nil {
			return nil, err
		}
	}
	if to != nil {
		_, err = d.inspectCommit(ctx, to)
		if err != nil {
			return nil, err
		}
	}

	// if number is 0, we return all commits that match the criteria
	if number == 0 {
		number = math.MaxUint64
	}
	var commitInfos []*pfs.CommitInfo
	commits := d.commits(repo.Name).ReadOnly(ctx)

	if from != nil && to == nil {
		return nil, fmt.Errorf("cannot use `from` commit without `to` commit")
	} else if from == nil && to == nil {
		// if neither from and to is given, we list all commits in
		// the repo, sorted by revision timestamp
		iterator, err := commits.List()
		if err != nil {
			return nil, err
		}
		var commitID string
		for number != 0 {
			var commitInfo pfs.CommitInfo
			ok, err := iterator.Next(&commitID, &commitInfo)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			commitInfos = append(commitInfos, &commitInfo)
			number--
		}
	} else {
		cursor := to
		for number != 0 && cursor != nil && (from == nil || cursor.ID != from.ID) {
			var commitInfo pfs.CommitInfo
			if err := commits.Get(cursor.ID, &commitInfo); err != nil {
				return nil, err
			}
			commitInfos = append(commitInfos, &commitInfo)
			cursor = commitInfo.ParentCommit
			number--
		}
	}
	return commitInfos, nil
}

type commitStream struct {
	stream chan CommitEvent
	done   chan struct{}
}

func (c *commitStream) Stream() <-chan CommitEvent {
	return c.stream
}

func (c *commitStream) Close() {
	close(c.done)
}

func (d *driver) subscribeCommit(ctx context.Context, repo *pfs.Repo, branch string, from *pfs.Commit) (CommitStream, error) {
	d.initializePachConn()
	if from != nil && from.Repo.Name != repo.Name {
		return nil, fmt.Errorf("the `from` commit needs to be from repo %s", repo.Name)
	}

	// We need to watch for new commits before we start listing commits,
	// because otherwise we might miss some commits in between when we
	// finish listing and when we start watching.
	branches := d.branches(repo.Name).ReadOnly(ctx)
	newCommitWatcher, err := branches.WatchOne(branch)
	if err != nil {
		return nil, err
	}

	stream := make(chan CommitEvent)
	done := make(chan struct{})

	go func() (retErr error) {
		defer newCommitWatcher.Close()
		defer func() {
			if retErr != nil {
				select {
				case stream <- CommitEvent{
					Err: retErr,
				}:
				case <-done:
				}
			}
			close(stream)
		}()
		// keep track of the commits that have been sent
		seen := make(map[string]bool)
		// include all commits that are currently on the given branch,
		// but only the ones that have been finished
		commitInfos, err := d.listCommit(ctx, repo, &pfs.Commit{
			Repo: repo,
			ID:   branch,
		}, from, 0)
		if err != nil {
			// We skip NotFound error because it's ok if the branch
			// doesn't exist yet, in which case ListCommit returns
			// a NotFound error.
			if !isNotFoundErr(err) {
				return err
			}
		}
		// ListCommit returns commits in newest-first order,
		// but SubscribeCommit should return commit in oldest-first
		// order, so we reverse the order.
		for i := range commitInfos {
			commitInfo := commitInfos[len(commitInfos)-i-1]
			if commitInfo.Finished != nil {
				select {
				case stream <- CommitEvent{
					Value: commitInfo,
				}:
					seen[commitInfo.Commit.ID] = true
				case <-done:
					return nil
				}
			}
		}

		for {
			var branchName string
			commit := new(pfs.Commit)
			for {
				var event *watch.Event
				var ok bool
				select {
				case event, ok = <-newCommitWatcher.Watch():
				case <-done:
					return nil
				}
				if !ok {
					return nil
				}
				switch event.Type {
				case watch.EventError:
					return event.Err
				case watch.EventPut:
					event.Unmarshal(&branchName, commit)
				case watch.EventDelete:
					continue
				}

				// We don't want to include the `from` commit itself
				if !(seen[commit.ID] || (from != nil && from.ID == commit.ID)) {
					break
				}
			}
			// Now we watch the CommitInfo until the commit has been finished
			commits := d.commits(commit.Repo.Name).ReadOnly(ctx)
			// closure for defer
			if err := func() error {
				commitInfoWatcher, err := commits.WatchOne(commit.ID)
				if err != nil {
					return err
				}
				defer commitInfoWatcher.Close()
				for {
					var commitID string
					commitInfo := new(pfs.CommitInfo)
					event := <-commitInfoWatcher.Watch()
					switch event.Type {
					case watch.EventError:
						return event.Err
					case watch.EventPut:
						event.Unmarshal(&commitID, commitInfo)
					case watch.EventDelete:
						// if this commit that we are waiting for is
						// deleted, then we go back to watch the branch
						// to get a new commit
						return nil
					}
					if commitInfo.Finished != nil {
						select {
						case stream <- CommitEvent{
							Value: commitInfo,
						}:
							seen[commitInfo.Commit.ID] = true
						case <-done:
							return nil
						}
						return nil
					}
				}
			}(); err != nil {
				return err
			}
		}
	}()

	return &commitStream{
		stream: stream,
		done:   done,
	}, nil
}

func (d *driver) flushCommit(ctx context.Context, fromCommits []*pfs.Commit, toRepos []*pfs.Repo) (CommitStream, error) {
	if len(fromCommits) == 0 {
		return nil, fmt.Errorf("fromCommits cannot be empty")
	}
	d.initializePachConn()

	for _, commit := range fromCommits {
		if _, err := d.inspectCommit(ctx, commit); err != nil {
			return nil, err
		}
	}

	var repos []*pfs.Repo
	if toRepos != nil {
		repos = toRepos
	} else {
		var downstreamRepos []*pfs.Repo
		// keep track of how many times a repo appears downstream of
		// a repo in fromCommits.
		repoCounts := make(map[string]int)
		// Find the repos that have *all* the given repos as provenance
		for _, commit := range fromCommits {
			// get repos that have the commit's repo as provenance
			repoInfos, err := d.flushRepo(ctx, commit.Repo)
			if err != nil {
				return nil, err
			}

		NextRepoInfo:
			for _, repoInfo := range repoInfos {
				repoCounts[repoInfo.Repo.Name]++
				for _, repo := range downstreamRepos {
					if repoInfo.Repo.Name == repo.Name {
						// Already in the list; skip it
						continue NextRepoInfo
					}
				}
				downstreamRepos = append(downstreamRepos, repoInfo.Repo)
			}
		}
		for _, repo := range downstreamRepos {
			// Only the repos that showed up as a downstream repo for
			// len(fromCommits) repos will contain commits that are
			// downstream of all fromCommits.
			if repoCounts[repo.Name] == len(fromCommits) {
				repos = append(repos, repo)
			}
		}
	}

	// A commit needs to show up len(fromCommits) times in order to
	// prove that it indeed has all the fromCommits as provenance.
	commitCounts := make(map[string]int)
	var commitCountsLock sync.Mutex
	stream := make(chan CommitEvent, len(repos))
	done := make(chan struct{})

	if len(repos) == 0 {
		close(stream)
		return &commitStream{
			stream: stream,
			done:   done,
		}, nil
	}

	for _, commit := range fromCommits {
		for _, repo := range repos {
			commitWatcher, err := d.commits(repo.Name).ReadOnly(ctx).WatchByIndex(pfsdb.ProvenanceIndex, commit)
			if err != nil {
				return nil, err
			}
			go func(commit *pfs.Commit) (retErr error) {
				defer commitWatcher.Close()
				defer func() {
					if retErr != nil {
						select {
						case stream <- CommitEvent{
							Err: retErr,
						}:
						case <-done:
						}
					}
				}()
				for {
					var ev *watch.Event
					var ok bool
					select {
					case ev, ok = <-commitWatcher.Watch():
					case <-done:
						return
					}
					if !ok {
						return
					}
					var commitID string
					var commitInfo pfs.CommitInfo
					switch ev.Type {
					case watch.EventError:
						return ev.Err
					case watch.EventDelete:
						continue
					case watch.EventPut:
						if err := ev.Unmarshal(&commitID, &commitInfo); err != nil {
							return err
						}
					}
					// Using a func just so we can unlock the commits in
					// a refer function
					if func() bool {
						commitCountsLock.Lock()
						defer commitCountsLock.Unlock()
						commitCounts[commitID]++
						return commitCounts[commitID] == len(fromCommits)
					}() {
						select {
						case stream <- CommitEvent{
							Value: &commitInfo,
						}:
						case <-done:
							return
						}
					}
				}
			}(commit)
		}
	}

	respStream := make(chan CommitEvent)
	respDone := make(chan struct{})

	go func() {
		// When we've sent len(repos) commits, we are done
		var numCommitsSent int
		for {
			select {
			case ev := <-stream:
				respStream <- ev
				numCommitsSent++
				if numCommitsSent == len(repos) {
					close(respStream)
					close(done)
					return
				}
			case <-respDone:
				close(done)
				return
			}
		}
	}()

	return &commitStream{
		stream: respStream,
		done:   respDone,
	}, nil
}

func (d *driver) flushRepo(ctx context.Context, repo *pfs.Repo) ([]*pfs.RepoInfo, error) {
	iter, err := d.repos.ReadOnly(ctx).GetByIndex(pfsdb.ProvenanceIndex, repo)
	if err != nil {
		return nil, err
	}
	var repoInfos []*pfs.RepoInfo
	for {
		var repoName string
		repoInfo := new(pfs.RepoInfo)
		ok, err := iter.Next(&repoName, repoInfo)
		if !ok {
			return repoInfos, nil
		}
		if err != nil {
			return nil, err
		}
		repoInfos = append(repoInfos, repoInfo)
	}
}

func (d *driver) deleteCommit(ctx context.Context, commit *pfs.Commit) error {
	if err := d.checkIsAuthorized(ctx, commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	commitInfo, err := d.inspectCommit(ctx, commit)
	if err != nil {
		return err
	}

	if commitInfo.Finished != nil {
		return fmt.Errorf("cannot delete finished commit")
	}

	// Delete the scratch space for this commit
	prefix, err := d.scratchCommitPrefix(ctx, commit)
	if err != nil {
		return err
	}
	_, err = d.etcdClient.Delete(ctx, prefix, etcd.WithPrefix())
	if err != nil {
		return err
	}

	// If this commit is the head of a branch, make the commit's parent
	// the head instead.
	branches, err := d.listBranch(ctx, commit.Repo)
	if err != nil {
		return err
	}

	for _, branch := range branches {
		if branch.Head.ID == commitInfo.Commit.ID {
			if commitInfo.ParentCommit != nil {
				if err := d.setBranch(ctx, commitInfo.ParentCommit, branch.Name); err != nil {
					return err
				}
			} else {
				// If this commit doesn't have a parent, delete the branch
				if err := d.deleteBranch(ctx, commit.Repo, branch.Name); err != nil {
					return err
				}
			}
		}
	}

	// Delete the commit itself and subtract the size of the commit
	// from repo size.
	_, err = col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		repos := d.repos.ReadWrite(stm)
		repoInfo := new(pfs.RepoInfo)
		if err := repos.Get(commit.Repo.Name, repoInfo); err != nil {
			return err
		}
		repoInfo.SizeBytes -= commitInfo.SizeBytes
		repos.Put(commit.Repo.Name, repoInfo)

		commits := d.commits(commit.Repo.Name).ReadWrite(stm)
		return commits.Delete(commit.ID)
	})

	return err
}

func (d *driver) listBranch(ctx context.Context, repo *pfs.Repo) ([]*pfs.BranchInfo, error) {
	if err := d.checkIsAuthorized(ctx, repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	branches := d.branches(repo.Name).ReadOnly(ctx)
	iterator, err := branches.List()
	if err != nil {
		return nil, err
	}

	var res []*pfs.BranchInfo
	for {
		var branchName string
		head := new(pfs.Commit)
		ok, err := iterator.Next(&branchName, head)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		res = append(res, &pfs.BranchInfo{
			Name: path.Base(branchName),
			Head: head,
		})
	}
	return res, nil
}

func (d *driver) setBranch(ctx context.Context, commit *pfs.Commit, name string) error {
	if err := d.checkIsAuthorized(ctx, commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	if _, err := d.inspectCommit(ctx, commit); err != nil {
		return err
	}
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		commits := d.commits(commit.Repo.Name).ReadWrite(stm)
		branches := d.branches(commit.Repo.Name).ReadWrite(stm)

		// Make sure that the commit exists
		var commitInfo pfs.CommitInfo
		if err := commits.Get(commit.ID, &commitInfo); err != nil {
			return err
		}

		return branches.Put(name, commit)
	})
	return err
}

func (d *driver) deleteBranch(ctx context.Context, repo *pfs.Repo, name string) error {
	if err := d.checkIsAuthorized(ctx, repo, auth.Scope_WRITER); err != nil {
		return err
	}
	_, err := col.NewSTM(ctx, d.etcdClient, func(stm col.STM) error {
		branches := d.branches(repo.Name).ReadWrite(stm)
		return branches.Delete(name)
	})
	return err
}

func (d *driver) scratchPrefix() string {
	return path.Join(d.prefix, "scratch")
}

// scratchCommitPrefix returns an etcd prefix that's used to temporarily
// store the state of a file in an open commit.  Once the commit is finished,
// the scratch space is removed.
func (d *driver) scratchCommitPrefix(ctx context.Context, commit *pfs.Commit) (string, error) {
	if _, err := d.inspectCommit(ctx, commit); err != nil {
		return "", err
	}
	return path.Join(d.scratchPrefix(), commit.Repo.Name, commit.ID), nil
}

// scratchFilePrefix returns an etcd prefix that's used to temporarily
// store the state of a file in an open commit.  Once the commit is finished,
// the scratch space is removed.
func (d *driver) scratchFilePrefix(ctx context.Context, file *pfs.File) (string, error) {
	return path.Join(d.scratchPrefix(), file.Commit.Repo.Name, file.Commit.ID, file.Path), nil
}

func (d *driver) filePathFromEtcdPath(etcdPath string) string {
	trimmed := strings.TrimPrefix(etcdPath, d.scratchPrefix())
	// trimmed looks like /repo/commit/path/to/file
	split := strings.Split(trimmed, "/")
	// we only want /path/to/file so we use index 3 (note that there's an "" at
	// the beginning of the slice because of the lead /)
	return path.Join(split[3:]...)
}

// checkPath checks if a file path is legal
func checkPath(path string) error {
	if strings.Contains(path, "\x00") {
		return fmt.Errorf("filename cannot contain null character: %s", path)
	}
	return nil
}

func (d *driver) putFile(ctx context.Context, file *pfs.File, delimiter pfs.Delimiter,
	targetFileDatums int64, targetFileBytes int64, overwriteIndex *pfs.OverwriteIndex, reader io.Reader) error {
	if err := d.checkIsAuthorized(ctx, file.Commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	// Check if the commit ID is a branch name.  If so, we have to
	// get the real commit ID in order to check if the commit does exist
	// and is open.
	// Since we use UUIDv4 for commit IDs, the 13th character would be 4 if
	// this is a commit ID.
	if len(file.Commit.ID) != uuid.UUIDWithoutDashesLength || file.Commit.ID[12] != '4' {
		commitInfo, err := d.inspectCommit(ctx, file.Commit)
		if err != nil {
			return err
		}
		file.Commit = commitInfo.Commit
	}

	if overwriteIndex != nil && overwriteIndex.Index == 0 {
		if err := d.deleteFile(ctx, file); err != nil {
			return err
		}
	}

	records := &pfs.PutFileRecords{}
	if err := checkPath(file.Path); err != nil {
		return err
	}
	prefix, err := d.scratchFilePrefix(ctx, file)
	if err != nil {
		return err
	}

	// Put the tree into the blob store
	// Only write the records to etcd if the commit does exist and is open.
	// To check that a key exists in etcd, we assert that its CreateRevision
	// is greater than zero.
	putRecords := func() error {
		marshalledRecords, err := records.Marshal()
		if err != nil {
			return err
		}
		kvc := etcd.NewKV(d.etcdClient)

		txnResp, err := kvc.Txn(ctx).
			If(etcd.Compare(etcd.CreateRevision(d.openCommits.Path(file.Commit.ID)), ">", 0)).Then(etcd.OpPut(path.Join(prefix, uuid.NewWithoutDashes()), string(marshalledRecords))).Commit()
		if err != nil {
			return err
		}
		if !txnResp.Succeeded {
			return fmt.Errorf("commit %v is not open", file.Commit.ID)
		}
		return nil
	}

	if delimiter == pfs.Delimiter_NONE {
		objects, size, err := d.pachClient.PutObjectSplit(reader)
		if err != nil {
			return err
		}

		// Here we use the invariant that every one but the last object
		// should have a size of ChunkSize.
		for i, object := range objects {
			record := &pfs.PutFileRecord{
				ObjectHash: object.Hash,
			}

			if size > pfs.ChunkSize {
				record.SizeBytes = pfs.ChunkSize
			} else {
				record.SizeBytes = size
			}
			size -= pfs.ChunkSize

			// The first record takes care of the overwriting
			if i == 0 && overwriteIndex != nil && overwriteIndex.Index != 0 {
				record.OverwriteIndex = overwriteIndex
			}

			records.Records = append(records.Records, record)
		}

		return putRecords()
	}
	buffer := &bytes.Buffer{}
	var datumsWritten int64
	var bytesWritten int64
	var filesPut int
	EOF := false
	var eg errgroup.Group
	decoder := json.NewDecoder(reader)
	bufioR := bufio.NewReader(reader)

	indexToRecord := make(map[int]*pfs.PutFileRecord)
	var mu sync.Mutex
	for !EOF {
		var err error
		var value []byte
		switch delimiter {
		case pfs.Delimiter_JSON:
			var jsonValue json.RawMessage
			err = decoder.Decode(&jsonValue)
			value = jsonValue
		case pfs.Delimiter_LINE:
			value, err = bufioR.ReadBytes('\n')
		default:
			return fmt.Errorf("unrecognized delimiter %s", delimiter.String())
		}
		if err != nil {
			if err == io.EOF {
				EOF = true
			} else {
				return err
			}
		}
		buffer.Write(value)
		bytesWritten += int64(len(value))
		datumsWritten++
		if buffer.Len() != 0 &&
			((targetFileBytes != 0 && bytesWritten >= targetFileBytes) ||
				(targetFileDatums != 0 && datumsWritten >= targetFileDatums) ||
				(targetFileBytes == 0 && targetFileDatums == 0) ||
				EOF) {
			_buffer := buffer
			index := filesPut
			eg.Go(func() error {
				object, size, err := d.pachClient.PutObject(_buffer)
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				indexToRecord[index] = &pfs.PutFileRecord{
					SizeBytes:  size,
					ObjectHash: object.Hash,
				}
				return nil
			})
			datumsWritten = 0
			bytesWritten = 0
			buffer = &bytes.Buffer{}
			filesPut++
		}
	}
	if err := eg.Wait(); err != nil {
		return err
	}

	records.Split = true
	for i := 0; i < len(indexToRecord); i++ {
		records.Records = append(records.Records, indexToRecord[i])
	}

	return putRecords()
}

func (d *driver) copyFile(ctx context.Context, src *pfs.File, dst *pfs.File, overwrite bool) error {
	if err := d.checkIsAuthorized(ctx, src.Commit.Repo, auth.Scope_READER); err != nil {
		return err
	}
	if err := d.checkIsAuthorized(ctx, dst.Commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	if err := checkPath(dst.Path); err != nil {
		return err
	}
	// Check if the commit ID is a branch name.  If so, we have to
	// get the real commit ID in order to check if the commit does exist
	// and is open.
	// Since we use UUIDv4 for commit IDs, the 13th character would be 4 if
	// this is a commit ID.
	if len(dst.Commit.ID) != uuid.UUIDWithoutDashesLength || dst.Commit.ID[12] != '4' {
		commitInfo, err := d.inspectCommit(ctx, dst.Commit)
		if err != nil {
			return err
		}
		dst.Commit = commitInfo.Commit
	}
	if overwrite {
		if err := d.deleteFile(ctx, dst); err != nil {
			return err
		}
	}
	srcTree, err := d.getTreeForFile(ctx, src)
	if err != nil {
		return err
	}
	// This is necessary so we can call filepath.Rel below
	if !strings.HasPrefix(src.Path, "/") {
		src.Path = "/" + src.Path
	}
	var eg errgroup.Group
	if err := srcTree.Walk(src.Path, func(walkPath string, node *hashtree.NodeProto) error {
		if node.FileNode == nil {
			return nil
		}
		eg.Go(func() error {
			relPath, err := filepath.Rel(src.Path, walkPath)
			if err != nil {
				// This shouldn't be possible
				return fmt.Errorf("error from filepath.Rel: %+v (this is likely a bug)", err)
			}
			records := &pfs.PutFileRecords{}
			file := client.NewFile(dst.Commit.Repo.Name, dst.Commit.ID, path.Clean(path.Join(dst.Path, relPath)))
			prefix, err := d.scratchFilePrefix(ctx, file)
			if err != nil {
				return err
			}
			for i, object := range node.FileNode.Objects {
				var size int64
				if i == 0 {
					size = node.SubtreeSize
				}
				records.Records = append(records.Records, &pfs.PutFileRecord{
					SizeBytes:  size,
					ObjectHash: object.Hash,
				})
			}
			marshalledRecords, err := records.Marshal()
			if err != nil {
				return err
			}
			kvc := etcd.NewKV(d.etcdClient)
			txnResp, err := kvc.Txn(ctx).
				If(etcd.Compare(etcd.CreateRevision(d.openCommits.Path(file.Commit.ID)), ">", 0)).Then(etcd.OpPut(path.Join(prefix, uuid.NewWithoutDashes()), string(marshalledRecords))).Commit()
			if err != nil {
				return err
			}
			if !txnResp.Succeeded {
				return fmt.Errorf("commit %v is not open", file.Commit.ID)
			}
			return nil
		})
		return nil
	}); err != nil {
		return err
	}
	return eg.Wait()
}

func (d *driver) getTreeForCommit(ctx context.Context, commit *pfs.Commit) (hashtree.HashTree, error) {
	if commit == nil || commit.ID == "" {
		t, err := hashtree.NewHashTree().Finish()
		if err != nil {
			return nil, err
		}
		return t, nil
	}

	tree, ok := d.treeCache.Get(commit.ID)
	if ok {
		h, ok := tree.(hashtree.HashTree)
		if ok {
			return h, nil
		}
		return nil, fmt.Errorf("corrupted cache: expected hashtree.Hashtree, found %v", tree)
	}

	if _, err := d.inspectCommit(ctx, commit); err != nil {
		return nil, err
	}

	commits := d.commits(commit.Repo.Name).ReadOnly(ctx)
	commitInfo := &pfs.CommitInfo{}
	if err := commits.Get(commit.ID, commitInfo); err != nil {
		return nil, err
	}
	if commitInfo.Finished == nil {
		return nil, fmt.Errorf("cannot read from an open commit")
	}
	treeRef := commitInfo.Tree

	if treeRef == nil {
		t, err := hashtree.NewHashTree().Finish()
		if err != nil {
			return nil, err
		}
		return t, nil
	}

	// read the tree from the block store
	var buf bytes.Buffer
	if err := d.pachClient.GetObject(treeRef.Hash, &buf); err != nil {
		return nil, err
	}

	h, err := hashtree.Deserialize(buf.Bytes())
	if err != nil {
		return nil, err
	}

	d.treeCache.Add(commit.ID, h)

	return h, nil
}

// getTreeForFile is like getTreeForCommit except that it can handle open commits.
// It takes a file instead of a commit so that it can apply the changes for
// that path to the tree before it returns it.
func (d *driver) getTreeForFile(ctx context.Context, file *pfs.File) (hashtree.HashTree, error) {
	if file.Commit == nil {
		t, err := hashtree.NewHashTree().Finish()
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	commitInfo, err := d.inspectCommit(ctx, file.Commit)
	if err != nil {
		return nil, err
	}
	if commitInfo.Finished != nil {
		tree, err := d.getTreeForCommit(ctx, file.Commit)
		if err != nil {
			return nil, err
		}
		return tree, nil
	}
	prefix, err := d.scratchFilePrefix(ctx, file)
	if err != nil {
		return nil, err
	}
	// Read everything under the scratch space for this commit
	resp, err := d.etcdClient.Get(ctx, prefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortAscend))
	if err != nil {
		return nil, err
	}

	parentTree, err := d.getTreeForCommit(ctx, commitInfo.ParentCommit)
	if err != nil {
		return nil, err
	}
	openTree := parentTree.Open()
	if err := d.applyWrites(resp, openTree); err != nil {
		return nil, err
	}
	tree, err := openTree.Finish()
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func (d *driver) getFile(ctx context.Context, file *pfs.File, offset int64, size int64) (io.Reader, error) {
	if err := d.checkIsAuthorized(ctx, file.Commit.Repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	tree, err := d.getTreeForFile(ctx, file)
	if err != nil {
		return nil, err
	}

	node, err := tree.Get(file.Path)
	if err != nil {
		return nil, pfsserver.ErrFileNotFound{file}
	}

	if node.FileNode == nil {
		return nil, fmt.Errorf("%s is a directory", file.Path)
	}

	getObjectsClient, err := d.pachClient.ObjectAPIClient.GetObjects(
		ctx,
		&pfs.GetObjectsRequest{
			Objects:     node.FileNode.Objects,
			OffsetBytes: uint64(offset),
			SizeBytes:   uint64(size),
		})
	if err != nil {
		return nil, err
	}
	return grpcutil.NewStreamingBytesReader(getObjectsClient), nil
}

// If full is false, exclude potentially large fields such as `Objects`
// and `Children`
func nodeToFileInfo(commit *pfs.Commit, path string, node *hashtree.NodeProto, full bool) *pfs.FileInfo {
	fileInfo := &pfs.FileInfo{
		File: &pfs.File{
			Commit: commit,
			Path:   path,
		},
		SizeBytes: uint64(node.SubtreeSize),
		Hash:      node.Hash,
	}
	if node.FileNode != nil {
		fileInfo.FileType = pfs.FileType_FILE
		if full {
			fileInfo.Objects = node.FileNode.Objects
		}
	} else if node.DirNode != nil {
		fileInfo.FileType = pfs.FileType_DIR
		if full {
			fileInfo.Children = node.DirNode.Children
		}
	}
	return fileInfo
}

func (d *driver) inspectFile(ctx context.Context, file *pfs.File) (*pfs.FileInfo, error) {
	if err := d.checkIsAuthorized(ctx, file.Commit.Repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	tree, err := d.getTreeForFile(ctx, file)
	if err != nil {
		return nil, err
	}

	node, err := tree.Get(file.Path)
	if err != nil {
		return nil, pfsserver.ErrFileNotFound{file}
	}

	return nodeToFileInfo(file.Commit, file.Path, node, true), nil
}

func (d *driver) listFile(ctx context.Context, file *pfs.File, full bool) ([]*pfs.FileInfo, error) {
	if err := d.checkIsAuthorized(ctx, file.Commit.Repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	tree, err := d.getTreeForFile(ctx, file)
	if err != nil {
		return nil, err
	}

	nodes, err := tree.List(file.Path)
	if err != nil {
		return nil, err
	}

	var fileInfos []*pfs.FileInfo
	for _, node := range nodes {
		fileInfos = append(fileInfos, nodeToFileInfo(file.Commit, path.Join(file.Path, node.Name), node, full))
	}
	return fileInfos, nil
}

func (d *driver) globFile(ctx context.Context, commit *pfs.Commit, pattern string) ([]*pfs.FileInfo, error) {
	if err := d.checkIsAuthorized(ctx, commit.Repo, auth.Scope_READER); err != nil {
		return nil, err
	}
	tree, err := d.getTreeForFile(ctx, client.NewFile(commit.Repo.Name, commit.ID, ""))
	if err != nil {
		return nil, err
	}

	nodes, err := tree.Glob(pattern)
	if err != nil {
		return nil, err
	}

	var fileInfos []*pfs.FileInfo
	for _, node := range nodes {
		fileInfos = append(fileInfos, nodeToFileInfo(commit, node.Name, node, false))
	}
	return fileInfos, nil
}

func (d *driver) diffFile(ctx context.Context, newFile *pfs.File, oldFile *pfs.File, shallow bool) ([]*pfs.FileInfo, []*pfs.FileInfo, error) {
	// Do READER authorization check for both newFile and oldFile
	if oldFile != nil && oldFile.Commit != nil {
		//	if oldFile != nil {
		if err := d.checkIsAuthorized(ctx, oldFile.Commit.Repo, auth.Scope_READER); err != nil {
			return nil, nil, err
		}
	}
	if newFile != nil && newFile.Commit != nil {
		//	if newFile != nil {
		if err := d.checkIsAuthorized(ctx, newFile.Commit.Repo, auth.Scope_READER); err != nil {
			return nil, nil, err
		}
	}
	newTree, err := d.getTreeForFile(ctx, newFile)
	if err != nil {
		return nil, nil, err
	}
	// if oldFile is new we use the parent of newFile
	if oldFile == nil {
		oldFile = &pfs.File{}
		newCommitInfo, err := d.inspectCommit(ctx, newFile.Commit)
		if err != nil {
			return nil, nil, err
		}
		// ParentCommit may be nil, that's fine because getTreeForCommit
		// handles nil
		oldFile.Commit = newCommitInfo.ParentCommit
		oldFile.Path = newFile.Path
	}
	oldTree, err := d.getTreeForFile(ctx, oldFile)
	if err != nil {
		return nil, nil, err
	}
	var newFileInfos []*pfs.FileInfo
	var oldFileInfos []*pfs.FileInfo
	recursiveDepth := -1
	if shallow {
		recursiveDepth = 1
	}
	if err := newTree.Diff(oldTree, newFile.Path, oldFile.Path, int64(recursiveDepth), func(path string, node *hashtree.NodeProto, new bool) error {
		if new {
			newFileInfos = append(newFileInfos, nodeToFileInfo(newFile.Commit, path, node, false))
		} else {
			oldFileInfos = append(oldFileInfos, nodeToFileInfo(oldFile.Commit, path, node, false))
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return newFileInfos, oldFileInfos, nil
}

func (d *driver) deleteFile(ctx context.Context, file *pfs.File) error {
	if err := d.checkIsAuthorized(ctx, file.Commit.Repo, auth.Scope_WRITER); err != nil {
		return err
	}
	commitInfo, err := d.inspectCommit(ctx, file.Commit)
	if err != nil {
		return err
	}

	if commitInfo.Finished != nil {
		return pfsserver.ErrCommitFinished{file.Commit}
	}

	prefix, err := d.scratchFilePrefix(ctx, file)
	if err != nil {
		return err
	}

	_, err = d.etcdClient.Put(ctx, path.Join(prefix, uuid.NewWithoutDashes()), tombstone)
	return err
}

func (d *driver) deleteAll(ctx context.Context) error {
	repoInfos, err := d.listRepo(ctx, nil, !includeAuth)
	if err != nil {
		return err
	}
	for _, repoInfo := range repoInfos.RepoInfo {
		if err := d.deleteRepo(ctx, repoInfo.Repo, true); err != nil && !auth.IsNotAuthorizedError(err) {
			return err
		}
	}
	return nil
}

func (d *driver) applyWrites(resp *etcd.GetResponse, tree hashtree.OpenHashTree) error {
	// a map that keeps track of the sizes of objects
	sizeMap := make(map[string]int64)
	for _, kv := range resp.Kvs {
		// fileStr is going to look like "some/path/UUID"
		fileStr := d.filePathFromEtcdPath(string(kv.Key))
		// the last element of `parts` is going to be UUID
		parts := strings.Split(fileStr, "/")
		// filePath should look like "some/path"
		filePath := strings.Join(parts[:len(parts)-1], "/")

		if string(kv.Value) == tombstone {
			if err := tree.DeleteFile(filePath); err != nil {
				// Deleting a non-existent file in an open commit should
				// be a no-op
				if hashtree.Code(err) != hashtree.PathNotFound {
					return err
				}
			}
		} else {
			records := &pfs.PutFileRecords{}
			if err := records.Unmarshal(kv.Value); err != nil {
				return err
			}
			if !records.Split {
				if len(records.Records) == 0 {
					return fmt.Errorf("unexpect %d length pfs.PutFileRecord (this is likely a bug)", len(records.Records))
				}
				for _, record := range records.Records {
					sizeMap[record.ObjectHash] = record.SizeBytes
					if record.OverwriteIndex != nil {
						// Computing size delta
						delta := record.SizeBytes
						fileNode, err := tree.Get(filePath)
						if err == nil {
							// If we can't find the file, that's fine.
							for i := record.OverwriteIndex.Index; int(i) < len(fileNode.FileNode.Objects); i++ {
								delta -= sizeMap[fileNode.FileNode.Objects[i].Hash]
							}
						}

						if err := tree.PutFileOverwrite(filePath, []*pfs.Object{{Hash: record.ObjectHash}}, record.OverwriteIndex, delta); err != nil {
							return err
						}
					} else {
						if err := tree.PutFile(filePath, []*pfs.Object{{Hash: record.ObjectHash}}, record.SizeBytes); err != nil {
							return err
						}
					}
				}
			} else {
				nodes, err := tree.List(filePath)
				if err != nil && hashtree.Code(err) != hashtree.PathNotFound {
					return err
				}
				var indexOffset int64
				if len(nodes) > 0 {
					indexOffset, err = strconv.ParseInt(path.Base(nodes[len(nodes)-1].Name), splitSuffixBase, splitSuffixWidth)
					if err != nil {
						return fmt.Errorf("error parsing filename %s as int, this likely means you're "+
							"using split on a directory which contains other data that wasn't put with split",
							path.Base(nodes[len(nodes)-1].Name))
					}
					indexOffset++ // start writing to the file after the last file
				}
				for i, record := range records.Records {
					if err := tree.PutFile(path.Join(filePath, fmt.Sprintf(splitSuffixFmt, i+int(indexOffset))), []*pfs.Object{{Hash: record.ObjectHash}}, record.SizeBytes); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}
