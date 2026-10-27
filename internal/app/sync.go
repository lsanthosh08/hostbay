package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	gosync "sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/container"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	hbsync "hostbay/internal/sync"
)

// syncTick is how often the background loop checks for work; a local
// change is uploaded on the next tick, once edits have settled.
const syncTick = time.Minute

// syncTimeout bounds one sync, configure or passphrase change: a hung
// target must not hold SyncService.mu forever.
const syncTimeout = 5 * time.Minute

// SyncService configures sync, runs it on demand and on a timer, and lists
// conflicts and settings waiting for confirmation (R35–R37).
type SyncService struct {
	st    *store.Store
	vault *secrets.Vault
	ver   secrets.Verifier
	trust *HostKeyTrust
	deps  hbsync.Deps
	// connect opens a target; tests replace it.
	connect func(ctx context.Context, t hbsync.Target, secret string, deps hbsync.Deps) (hbsync.Backend, error)
	confirm container.Confirm // a dialog Go opens, which the page cannot answer

	mu    gosync.Mutex // one sync, configure or edit at a time
	state *hbsync.State
	stop  context.CancelFunc
}

func NewSyncService(st *store.Store, vault *secrets.Vault, ver secrets.Verifier, trust *HostKeyTrust, deps hbsync.Deps) *SyncService {
	return &SyncService{st: st, vault: vault, ver: ver, trust: trust, deps: deps, connect: hbsync.Connect, confirm: container.NativeConfirm}
}

// SyncTargetInput is the settings form. Secret is the token, password or
// S3 secret key; empty keeps the saved one when the target is unchanged.
type SyncTargetInput struct {
	hbsync.Target
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// SyncStatus is what the settings panel shows. It carries no secret.
type SyncStatus struct {
	Enabled   bool          `json:"enabled"`
	Target    hbsync.Target `json:"target"`
	LastSync  string        `json:"lastSync"`
	LastError string        `json:"lastError"`
	// Paused is a code (decrypt, updateNeeded, rollback, passphraseChanged,
	// hostKeyChanged, auth, clock): sync waits for the user.
	Paused      string        `json:"paused"`
	NeedsChoice bool          `json:"needsChoice"` // first sync: use the target's data or merge
	Conflicts   int           `json:"conflicts"`
	Holds       []hbsync.Hold `json:"holds"`
}

// SyncConflict is one entry of the conflicts list.
type SyncConflict struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	ItemID     string `json:"itemId"`
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	DeviceID   string `json:"deviceId"`
	ModifiedAt string `json:"modifiedAt"`
}

func (s *SyncService) load() (*hbsync.State, error) {
	if s.state == nil {
		st, err := hbsync.LoadState(s.st)
		if err != nil {
			return nil, err
		}
		s.state = st
	}
	return s.state, nil
}

// targetAAD binds the sealed credential and sync key to the target, so a
// script cannot point sync elsewhere and have them sent there (KTD6).
func targetAAD(st *hbsync.State, what string) []byte {
	return []byte("hostbay sync " + what + "\x00" + st.TargetID + "\x00" + st.Target.Where())
}

// Status reports the sync settings and last result.
func (s *SyncService) Status() (SyncStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return SyncStatus{}, err
	}
	out := SyncStatus{Enabled: st.Enabled, Target: st.Target, LastError: st.LastError, Paused: st.Paused,
		NeedsChoice: !st.Synced && st.LastError == hbsync.ErrChoiceNeeded.Error(),
		Conflicts:   len(st.Conflicts), Holds: st.Holds}
	if out.Holds == nil {
		out.Holds = []hbsync.Hold{}
	}
	if !st.LastSync.IsZero() {
		out.LastSync = st.LastSync.Format(time.RFC3339)
	}
	return out, nil
}

// Configure turns sync on, or changes its target or passphrase. It counts
// as a whole-store export, so it needs OS verification first (KTD7), and it
// changes nothing until the target answers, refuses racing uploads and,
// when it already holds data, opens with the passphrase.
func (s *SyncService) Configure(ctx context.Context, in SyncTargetInput) error {
	if err := secrets.Gate(ctx, s.ver, "turn on sync or change its settings"); err != nil {
		return err
	}
	if err := hbsync.CheckPassphrase(in.Passphrase); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	st, err := s.load()
	if err != nil {
		return err
	}
	same := st.TargetID != "" && st.Target.Where() == in.Target.Where()
	secret := in.Secret
	if secret == "" && same && st.Credential != "" {
		plain, err := s.vault.Open(st.Credential, targetAAD(st, "credential"))
		if err != nil {
			return err
		}
		secret = string(plain)
	}
	b, err := s.dial(ctx, in.Target, secret, true)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := hbsync.ProbeCAS(ctx, b); err != nil {
		return err
	}
	files, _, err := b.Fetch(ctx)
	if err != nil {
		return err
	}

	next := *st
	if !same {
		next = hbsync.State{TargetID: rand.Text(), Conflicts: st.Conflicts}
	}
	next.Target, next.Enabled, next.Paused, next.LastError = in.Target, true, "", ""
	var key []byte
	if len(files) > 0 {
		// The target holds data: join it with its newest snapshot's key
		// setting, and prove the passphrase opens it before saving anything.
		f, m, err := newestSnapshot(files, s.st.DeviceID())
		if err != nil {
			return err
		}
		if key, err = m.KDF.Derive(in.Passphrase); err != nil {
			return err
		}
		if _, _, err := hbsync.Open(f.Data, key); err != nil {
			return err
		}
		next.KDF, next.KeyID = m.KDF, m.KeyID
	} else {
		if next.KDF, err = hbsync.NewKDF(); err != nil {
			return err
		}
		next.KeyID = rand.Text()
		if key, err = next.KDF.Derive(in.Passphrase); err != nil {
			return err
		}
	}
	if next.SyncKey, err = s.vault.Seal(key, targetAAD(&next, "key")); err != nil {
		return err
	}
	next.Credential = ""
	if secret != "" {
		if next.Credential, err = s.vault.Seal([]byte(secret), targetAAD(&next, "credential")); err != nil {
			return err
		}
	}
	if err := hbsync.SaveState(s.st, &next); err != nil {
		return err
	}
	*st = next
	return nil
}

// newestSnapshot picks the snapshot Configure joins: the newest one. After
// a passphrase change only the changing computer's file is under the new
// key, so an older file must not decide which passphrase is right, and the
// old one must no longer get in. This computer's own file counts only when
// no other computer wrote one.
func newestSnapshot(files []hbsync.File, me string) (hbsync.File, hbsync.Manifest, error) {
	var best hbsync.File
	var bm hbsync.Manifest
	for i, f := range files {
		m, err := hbsync.ReadManifest(f.Data)
		if err != nil {
			return hbsync.File{}, hbsync.Manifest{}, err
		}
		mine, bestMine := m.DeviceID == me, bm.DeviceID == me
		if i == 0 || (bestMine && !mine) || (mine == bestMine && m.CreatedAt.After(bm.CreatedAt)) {
			best, bm = f, m
		}
	}
	return best, bm, nil
}

// ChangePassphrase re-encrypts the target's data under a new passphrase
// and bumps the key ID, so other computers ask for it (KTD13). It needs
// OS verification first (KTD7).
func (s *SyncService) ChangePassphrase(ctx context.Context, passphrase string) (hbsync.Report, error) {
	if err := secrets.Gate(ctx, s.ver, "change the sync passphrase"); err != nil {
		return hbsync.Report{}, err
	}
	if err := hbsync.CheckPassphrase(passphrase); err != nil {
		return hbsync.Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	eng, b, err := s.engine(ctx)
	if err != nil {
		return hbsync.Report{}, err
	}
	defer b.Close()
	kdf, err := hbsync.NewKDF()
	if err != nil {
		return hbsync.Report{}, err
	}
	key, err := kdf.Derive(passphrase)
	if err != nil {
		return hbsync.Report{}, err
	}
	eng.PrevKey, eng.PrevKeyID = eng.Key, eng.KeyID
	eng.Key, eng.KDF, eng.KeyID = key, kdf, rand.Text()
	rep, err := eng.Sync(ctx, "")
	if rep.Notes == nil {
		rep.Notes = []string{}
	}
	if err != nil {
		return rep, err // the old key stays in force
	}
	st := s.state
	st.KDF, st.KeyID = kdf, eng.KeyID
	if st.SyncKey, err = s.vault.Seal(key, targetAAD(st, "key")); err != nil {
		return rep, err
	}
	return rep, hbsync.SaveState(s.st, st)
}

// Disable turns sync off and forgets its credential and key. Local data
// and the target stay as they are.
func (s *SyncService) Disable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	st.Enabled, st.Credential, st.SyncKey = false, "", ""
	return hbsync.SaveState(s.st, st)
}

// SyncNow syncs once. choice answers the first sync on a computer that
// already has data: "remote" or "merge".
func (s *SyncService) SyncNow(ctx context.Context, choice string) (hbsync.Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	eng, b, err := s.engine(ctx)
	if err != nil {
		return hbsync.Report{}, err
	}
	defer b.Close()
	rep, err := eng.Sync(ctx, choice)
	if rep.Notes == nil {
		rep.Notes = []string{}
	}
	return rep, err
}

// engine unseals the sync key and credential and opens the target. This
// is the re-wrap path KTD13 allows without a fresh verification, so it
// only runs once sync was enabled under KTD7.
func (s *SyncService) engine(ctx context.Context) (*hbsync.Engine, hbsync.Backend, error) {
	st, err := s.load()
	if err != nil {
		return nil, nil, err
	}
	if !st.Enabled || st.SyncKey == "" {
		return nil, nil, errors.New("sync is off")
	}
	key, err := s.vault.Open(st.SyncKey, targetAAD(st, "key"))
	if err != nil {
		return nil, nil, err
	}
	var secret []byte
	if st.Credential != "" {
		if secret, err = s.vault.Open(st.Credential, targetAAD(st, "credential")); err != nil {
			return nil, nil, err
		}
	}
	b, err := s.dial(ctx, st.Target, string(secret), false)
	if err != nil {
		st.LastError = err.Error()
		if code := hbsync.PauseCode(err); code != "" {
			st.Paused = code // a dropped network keeps any pause already set
		}
		if serr := hbsync.SaveState(s.st, st); serr != nil {
			err = errors.Join(err, serr)
		}
		return nil, nil, err
	}
	return &hbsync.Engine{Store: s.st, Vault: s.vault, Backend: b, Key: key, KDF: st.KDF, KeyID: st.KeyID, State: st}, b, nil
}

// dial opens a target. An unknown SFTP host key becomes a trust prompt
// only during setup; at any other time it is an error (KTD13).
func (s *SyncService) dial(ctx context.Context, t hbsync.Target, secret string, setup bool) (hbsync.Backend, error) {
	b, err := s.connect(ctx, t, secret, s.deps)
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) && !hk.Changed() && setup && s.trust != nil {
		return nil, s.trust.Request(hk.Host, hk.Port, hk.KeyType, hk.Key, hk.Fingerprint).With("hop", strconv.Itoa(hk.Hop))
	}
	return b, err
}

// Conflicts lists the losing sides of concurrent changes (R37).
func (s *SyncService) Conflicts() ([]SyncConflict, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []SyncConflict{}
	for _, c := range st.Conflicts {
		out = append(out, SyncConflict{ID: c.ID, Collection: c.Collection, ItemID: c.ItemID, Label: c.Label, Kind: c.Kind,
			DeviceID: c.DeviceID, ModifiedAt: c.ModifiedAt.Format(time.RFC3339)})
	}
	return out, nil
}

// offline is an Engine for edits that need the store and state only.
func (s *SyncService) offline() (*hbsync.Engine, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	return &hbsync.Engine{Store: s.st, Vault: s.vault, State: st}, nil
}

// RestoreConflict brings the listed version back as a new local change.
func (s *SyncService) RestoreConflict(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng, err := s.offline()
	if err != nil {
		return err
	}
	return eng.RestoreConflict(id)
}

// DismissConflict drops a conflict and keeps what won.
func (s *SyncService) DismissConflict(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng, err := s.offline()
	if err != nil {
		return err
	}
	return eng.DismissConflict(id)
}

// ConfirmHold applies, or refuses, a synced security setting (KTD13).
// Accepting one asks as making the change here would: a weaker AI mode
// needs Go's dialog and OS verification, as SetMode does, and a post-login
// rule that types a saved secret needs OS verification, as saving it does.
// A failed check leaves the hold; refusing needs none.
func (s *SyncService) ConfirmHold(ctx context.Context, id string, accept bool) error {
	var h hbsync.Hold
	if accept {
		var err error
		if h, err = s.hold(id); err != nil {
			return err
		}
		if err := s.verifyHold(ctx, h); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	eng, err := s.offline()
	if err != nil {
		return err
	}
	if accept {
		// A sync during the dialog may have replaced what was confirmed.
		i := slices.IndexFunc(eng.State.Holds, func(c hbsync.Hold) bool { return c.ID == id })
		if i < 0 || !reflect.DeepEqual(eng.State.Holds[i], h) {
			return errors.New("this setting changed while you were confirming it; review it again")
		}
	}
	return eng.ResolveHold(id, accept)
}

func (s *SyncService) hold(id string) (hbsync.Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return hbsync.Hold{}, err
	}
	for _, h := range st.Holds {
		if h.ID == id {
			return h, nil
		}
	}
	return hbsync.Hold{}, errors.New("nothing is waiting for confirmation with that ID")
}

func (s *SyncService) verifyHold(ctx context.Context, h hbsync.Hold) error {
	gate := func(ctx context.Context, reason string) error { return secrets.Gate(ctx, s.ver, reason) }
	switch h.Kind {
	case "aimode":
		text, ok := modeText[h.Mode]
		if !ok {
			return fmt.Errorf("unknown AI execution mode %q", h.Mode)
		}
		return widen(ctx, s.confirm, gate, modeTitle, text)
	case "postlogin":
		d, err := s.st.Data()
		if err != nil {
			return err
		}
		if r := d.PostLoginRule(h.ID); r != nil && slices.ContainsFunc(r.Steps, store.PostLoginStep.Secret) {
			return gate(ctx, "let a synced post-login rule send a saved password or secret")
		}
	}
	return nil
}

// ServiceStartup starts the background loop: every tick, sync when local
// data or the target changed, unless sync is off, paused or still waiting
// for its first run.
func (s *SyncService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	ctx, s.stop = context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(syncTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.tick(ctx)
			}
		}
	}()
	return nil
}

func (s *SyncService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
	}
	return nil
}

func (s *SyncService) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	st, err := s.load()
	if err != nil || !st.Enabled || st.Paused != "" || !st.Synced {
		return
	}
	eng, b, err := s.engine(ctx)
	if err != nil {
		return
	}
	defer b.Close()
	due, err := eng.Due(ctx)
	// Git, WebDAV and S3 sign in on the first request, which is Due's
	// version check, so a refusal has to pause here too.
	if code := hbsync.PauseCode(err); code != "" {
		eng.State.LastError, eng.State.Paused = err.Error(), code
		_ = hbsync.SaveState(s.st, eng.State)
		return
	}
	if err == nil && due {
		_, _ = eng.Sync(ctx, "") // the result lands in the state Status shows
	}
}
