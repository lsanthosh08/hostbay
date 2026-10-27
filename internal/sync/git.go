package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// NewGit syncs through a private Git repo over https:// (or a local path).
// Each upload is a commit on the previous snapshot's commit, pushed
// without force: if another computer pushed first the push is not a
// fast-forward and is refused, so nothing is overwritten (KTD13). The repo
// is kept as a bare clone in cacheDir ("" keeps it in memory) so each sync
// fetches only what is new.
//
// shortcut: ssh:// remotes are refused, because go-git would dial them
// without internal/sshx's known_hosts checks (KTD4); use https or the SFTP
// target until sshx exports a host key callback go-git can use.
func NewGit(t Target, token, cacheDir string) (Backend, error) {
	u := strings.TrimSpace(t.URL)
	switch {
	case u == "":
		return nil, errors.New("enter the Git repository URL")
	case strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git://") || (!strings.Contains(u, "://") && strings.Contains(u, "@")):
		return nil, errors.New("Git sync over SSH is not supported yet; use an https:// URL with a token, or the SFTP target")
	}
	var repo *git.Repository
	var err error
	if cacheDir == "" {
		repo, err = git.Init(memory.NewStorage(), nil)
	} else if repo, err = git.PlainOpen(cacheDir); errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInit(cacheDir, true)
	}
	if err != nil {
		return nil, fmt.Errorf("git cache: %w", err)
	}
	if r, err := repo.Remote("origin"); err == nil && !slices.Equal(r.Config().URLs, []string{u}) {
		if err := repo.DeleteRemote("origin"); err != nil {
			return nil, err
		}
	}
	if _, err := repo.Remote("origin"); err != nil {
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{u}}); err != nil {
			return nil, err
		}
	}
	g := &gitTarget{repo: repo, branch: t.Branch}
	if g.branch == "" {
		g.branch = "main"
	}
	if token != "" && (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) {
		user := t.User
		if user == "" {
			user = "hostbay" // hosts that take a token as the password ignore the name
		}
		g.auth = &githttp.BasicAuth{Username: user, Password: token}
	}
	return g, nil
}

type gitTarget struct {
	repo   *git.Repository
	branch string
	auth   transport.AuthMethod
}

func (g *gitTarget) tracking() plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", g.branch)
}

func emptyRemote(err error) bool {
	var noMatch git.NoMatchingRefSpecError
	return errors.Is(err, transport.ErrEmptyRemoteRepository) || errors.As(err, &noMatch)
}

func (g *gitTarget) Fetch(ctx context.Context) ([]File, string, error) {
	spec := config.RefSpec("+refs/heads/" + g.branch + ":" + g.tracking().String())
	err := g.repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{spec}, Auth: g.auth, Force: true})
	if emptyRemote(err) {
		return nil, "", nil
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, "", fmt.Errorf("git fetch: %w", err)
	}
	ref, err := g.repo.Reference(g.tracking(), true)
	if err != nil {
		return nil, "", fmt.Errorf("git: %w", err)
	}
	c, err := g.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, "", err
	}
	f, err := c.File(SharedName)
	if errors.Is(err, object.ErrFileNotFound) {
		return nil, ref.Hash().String(), nil
	}
	if err != nil {
		return nil, "", err
	}
	r, err := f.Reader()
	if err != nil {
		return nil, "", err
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	return []File{{Name: SharedName, Data: data}}, ref.Hash().String(), err
}

func (g *gitTarget) Store(ctx context.Context, name string, data []byte, version string) error {
	st := g.repo.Storer
	blob := st.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	w.Close()
	blobHash, err := st.SetEncodedObject(blob)
	if err != nil {
		return err
	}

	// Keep whatever else the branch holds, such as a README.
	entries := []object.TreeEntry{{Name: name, Mode: filemode.Regular, Hash: blobHash}}
	var parents []plumbing.Hash
	if version != "" {
		parent, err := g.repo.CommitObject(plumbing.NewHash(version))
		if err != nil {
			return err
		}
		tree, err := parent.Tree()
		if err != nil {
			return err
		}
		for _, e := range tree.Entries {
			if e.Name != name {
				entries = append(entries, e)
			}
		}
		parents = []plumbing.Hash{parent.Hash}
	}
	slices.SortFunc(entries, func(a, b object.TreeEntry) int { return strings.Compare(sortName(a), sortName(b)) })
	treeObj := st.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(treeObj); err != nil {
		return err
	}
	treeHash, err := st.SetEncodedObject(treeObj)
	if err != nil {
		return err
	}
	sig := object.Signature{Name: "Hostbay", Email: "sync@hostbay.invalid", When: time.Now()}
	commitObj := st.NewEncodedObject()
	commit := &object.Commit{Author: sig, Committer: sig, Message: "Hostbay sync\n", TreeHash: treeHash, ParentHashes: parents}
	if err := commit.Encode(commitObj); err != nil {
		return err
	}
	hash, err := st.SetEncodedObject(commitObj)
	if err != nil {
		return err
	}
	local := plumbing.NewBranchReferenceName("hostbay-sync")
	if err := st.SetReference(plumbing.NewHashReference(local, hash)); err != nil {
		return err
	}
	spec := config.RefSpec(local.String() + ":refs/heads/" + g.branch)
	err = g.repo.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{spec}, Auth: g.auth})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if msg := err.Error(); strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first") || strings.Contains(msg, "stale info") {
		return ErrConflict
	}
	return fmt.Errorf("git push: %w", err)
}

// sortName is how Git orders tree entries: a directory sorts as if its
// name ended in "/".
func sortName(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}

func (g *gitTarget) Version(ctx context.Context) (string, error) {
	r, err := g.repo.Remote("origin")
	if err != nil {
		return "", err
	}
	refs, err := r.ListContext(ctx, &git.ListOptions{Auth: g.auth})
	if emptyRemote(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.NewBranchReferenceName(g.branch) {
			return ref.Hash().String(), nil
		}
	}
	return "", nil
}

func (g *gitTarget) Prune(context.Context) error { return nil }
func (g *gitTarget) PerDevice() bool             { return false }
func (g *gitTarget) Close() error                { return nil }
