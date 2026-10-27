package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	gosync "sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/update"
)

// updateEvery is how often the background loop may ask GitHub for a newer
// release; Check asks whenever the user does.
const updateEvery = 24 * time.Hour

// updateCheckedFile in the data directory records, by its modification
// time, the last check, so restarts do not check more than daily.
const updateCheckedFile = "update-checked"

// UpdateStatus is what the About screen shows.
type UpdateStatus struct {
	Current string `json:"current"`
	// Signed is false while this build has no release key: updates are not
	// signed yet and Hostbay never downloads one.
	Signed    bool   `json:"signed"`
	LastCheck string `json:"lastCheck"` // RFC 3339, "" before the first check
	Available string `json:"available"` // a verified newer version, "" when none
	PageURL   string `json:"pageUrl"`   // its release page
	// CanInstall is false for deb and rpm installs, which update from
	// PageURL by hand.
	CanInstall bool   `json:"canInstall"`
	Installed  bool   `json:"installed"` // restart Hostbay to finish
	Error      string `json:"error"`
}

// UpdateService checks for verified updates daily and on demand, and
// installs one when the user asks (R55, KTD21).
type UpdateService struct {
	dir string
	// check, download, install and quit reach GitHub, the installer and the
	// app; tests replace them.
	check    func(ctx context.Context, prerelease bool) (*update.Release, error)
	download func(ctx context.Context, r *update.Release, dir string) (string, error)
	install  func(path string, a update.Asset) error
	quit     func()

	run     gosync.Mutex // one check or install at a time
	mu      gosync.Mutex // guards status and release
	status  UpdateStatus
	release *update.Release
	stop    context.CancelFunc
}

// NewUpdateService keeps its last-check time in the data directory dir.
func NewUpdateService(dir string) *UpdateService {
	u := update.New(Version)
	return &UpdateService{
		dir: dir, check: u.Check, download: u.Download, install: update.Install,
		quit:   func() { application.Get().Quit() },
		status: UpdateStatus{Current: Version, Signed: update.Signed()},
	}
}

func (s *UpdateService) checkedFile() string { return filepath.Join(s.dir, updateCheckedFile) }

// Status reports the last check without asking GitHub.
func (s *UpdateService) Status() UpdateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if fi, err := os.Stat(s.checkedFile()); err == nil {
		st.LastCheck = fi.ModTime().UTC().Format(time.RFC3339)
	}
	return st
}

// Check asks GitHub for a newer verified release now. prerelease includes
// pre-releases.
func (s *UpdateService) Check(ctx context.Context, prerelease bool) (UpdateStatus, error) {
	s.run.Lock()
	defer s.run.Unlock()
	// Recorded before asking, so a failing check still counts toward the day.
	_ = os.WriteFile(s.checkedFile(), nil, 0o600)
	r, err := s.check(ctx, prerelease)
	s.mu.Lock()
	s.release = r
	s.status.Available, s.status.PageURL, s.status.CanInstall, s.status.Error = "", "", false, ""
	if err != nil {
		s.status.Error = err.Error()
	} else if r != nil {
		s.status.Available, s.status.PageURL, s.status.CanInstall = r.Version, r.PageURL, r.Asset != nil
	}
	s.mu.Unlock()
	return s.Status(), err
}

// Install downloads and installs the release the last check found. On
// Windows the installer starts and Hostbay quits; elsewhere the new version
// runs from the next launch.
func (s *UpdateService) Install(ctx context.Context) (UpdateStatus, error) {
	s.run.Lock()
	defer s.run.Unlock()
	s.mu.Lock()
	r := s.release
	s.mu.Unlock()
	if r == nil || r.Asset == nil {
		return s.Status(), errors.New("no update to install; check for updates first")
	}
	dir, err := os.MkdirTemp("", "hostbay-update-")
	if err != nil {
		return s.Status(), err
	}
	path, err := s.download(ctx, r, dir)
	if err == nil {
		err = s.install(path, *r.Asset)
	}
	// The Windows installer runs from dir after Hostbay quits.
	if err != nil || runtime.GOOS != "windows" {
		os.RemoveAll(dir)
	}
	if err != nil {
		return s.Status(), err
	}
	s.mu.Lock()
	s.status.Installed = true
	s.mu.Unlock()
	if runtime.GOOS == "windows" {
		s.quit()
	}
	return s.Status(), nil
}

// autoCheck asks GitHub unless a check ran within updateEvery.
func (s *UpdateService) autoCheck(ctx context.Context) {
	// An unsigned build can never install, so it does not check by itself.
	if !s.status.Signed {
		return
	}
	if fi, err := os.Stat(s.checkedFile()); err == nil && time.Since(fi.ModTime()) < updateEvery {
		return
	}
	s.Check(ctx, false)
}

// ServiceStartup checks at launch and then hourly, each time only if the
// last check is a day old.
func (s *UpdateService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	ctx, s.stop = context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			s.autoCheck(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (s *UpdateService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
	}
	return nil
}
