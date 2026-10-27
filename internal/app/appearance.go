package app

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/store"
)

// AppearanceService keeps the terminal background image (KTD41). Go picks
// the file in its own dialog, checks it is an image, copies it into
// backgrounds/ and hands the page a data: URL, so the page never names a
// path and the CSP stays as it is.
type AppearanceService struct {
	st   *store.Store
	dir  string
	pick func() (string, error)
}

func NewAppearanceService(st *store.Store, dir string) *AppearanceService {
	return &AppearanceService{st: st, dir: dir, pick: pickBackgroundFile}
}

func pickBackgroundFile() (string, error) {
	return application.Get().Dialog.OpenFile().CanChooseFiles(true).SetTitle("Choose a background image").
		AddFilter("Images", "*.png;*.jpg;*.jpeg;*.webp").PromptForSingleSelection()
}

const maxBackground = 8 << 20

// The file types a background may be, by sniffed content, not by name.
var backgroundTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}

// A name BackgroundPick wrote; local.json never names anything else.
var backgroundName = regexp.MustCompile(`^background-[A-Za-z0-9]+\.(png|jpg|webp)$`)

func (s *AppearanceService) backgrounds() string { return filepath.Join(s.dir, "backgrounds") }

// BackgroundPick asks for an image in Go's dialog, copies it into
// backgrounds/ in place of the previous one and returns it as a data: URL.
// A cancelled dialog returns "" and changes nothing.
func (s *AppearanceService) BackgroundPick() (string, error) {
	path, err := s.pick()
	if err != nil || path == "" {
		return "", err
	}
	data, mime, err := readBackground(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.backgrounds(), 0o700); err != nil {
		return "", err
	}
	name := "background-" + rand.Text() + backgroundTypes[mime]
	f, err := os.OpenFile(filepath.Join(s.backgrounds(), name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	var old string
	if err == nil {
		err = s.st.EditLocal(func(l *store.Local) error {
			old, l.Background = l.Background, name
			return nil
		})
	}
	if err != nil {
		os.Remove(filepath.Join(s.backgrounds(), name))
		return "", err
	}
	s.remove(old)
	return dataURL(mime, data), nil
}

// BackgroundGet returns the background image as a data: URL, or "" when
// none is set or its copy is gone.
func (s *AppearanceService) BackgroundGet() (string, error) {
	l, err := s.st.Local()
	if err != nil || !backgroundName.MatchString(l.Background) {
		return "", err
	}
	data, mime, err := readBackground(filepath.Join(s.backgrounds(), l.Background))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return dataURL(mime, data), nil
}

// BackgroundClear removes the background image.
func (s *AppearanceService) BackgroundClear() error {
	var old string
	err := s.st.EditLocal(func(l *store.Local) error {
		old, l.Background = l.Background, ""
		return nil
	})
	if err == nil {
		s.remove(old)
	}
	return err
}

func (s *AppearanceService) remove(name string) {
	if backgroundName.MatchString(name) {
		os.Remove(filepath.Join(s.backgrounds(), name))
	}
}

// readBackground reads at most maxBackground bytes of a regular file and
// returns them with their sniffed type, which must be PNG, JPEG or WebP.
func readBackground(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, "", errors.New("the background must be an image file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBackground+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxBackground {
		return nil, "", fmt.Errorf("the image is larger than %d MiB", maxBackground>>20)
	}
	mime := http.DetectContentType(data)
	if _, ok := backgroundTypes[mime]; !ok {
		return nil, "", errors.New("the background must be a PNG, JPEG or WebP image")
	}
	return data, mime, nil
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
