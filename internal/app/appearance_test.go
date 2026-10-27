package app

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hostbay/internal/store"
)

func newAppearanceService(t *testing.T) (*AppearanceService, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewAppearanceService(st, dir), dir
}

// pickFile makes the service's dialog return a file holding data.
func pickFile(t *testing.T, s *AppearanceService, name string, data []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s.pick = func() (string, error) { return p, nil }
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBackgroundCopiesAValidImage(t *testing.T) {
	s, dir := newAppearanceService(t)
	img := pngBytes(t)
	pickFile(t, s, "photo.png", img)
	url, err := s.BackgroundPick()
	if err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
	if url != want {
		t.Fatalf("BackgroundPick = %.60q…", url)
	}
	if got, err := s.BackgroundGet(); err != nil || got != want {
		t.Fatalf("BackgroundGet = %.60q, %v", got, err)
	}
	copies, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*"))
	if len(copies) != 1 {
		t.Fatalf("backgrounds/ holds %v", copies)
	}
	if fi, _ := os.Stat(copies[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("copy mode = %v", fi.Mode().Perm())
	}

	// Picking again replaces the copy; clearing removes it.
	pickFile(t, s, "other.png", img)
	if _, err := s.BackgroundPick(); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*")); len(again) != 1 || again[0] == copies[0] {
		t.Fatalf("after a second pick backgrounds/ holds %v", again)
	}
	if err := s.BackgroundClear(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*")); len(left) != 0 {
		t.Fatalf("after clear backgrounds/ holds %v", left)
	}
	if got, err := s.BackgroundGet(); err != nil || got != "" {
		t.Fatalf("BackgroundGet after clear = %.60q, %v", got, err)
	}
}

func TestBackgroundRefusesLargeAndNonImageFiles(t *testing.T) {
	s, dir := newAppearanceService(t)
	big := append(pngBytes(t), make([]byte, 20<<20)...)
	for name, data := range map[string][]byte{
		"huge.png": big,
		"page.png": []byte("<!doctype html><html><body><script>alert(1)</script></body></html>"),
	} {
		pickFile(t, s, name, data)
		if url, err := s.BackgroundPick(); err == nil {
			t.Errorf("%s accepted: %.40q", name, url)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*")); len(left) != 0 {
		t.Fatalf("a refused file was copied: %v", left)
	}
}

func TestBackgroundGoneShowsNoImage(t *testing.T) {
	s, dir := newAppearanceService(t)
	pickFile(t, s, "photo.png", pngBytes(t))
	if _, err := s.BackgroundPick(); err != nil {
		t.Fatal(err)
	}
	copies, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*"))
	for _, c := range copies {
		os.Remove(c)
	}
	if got, err := s.BackgroundGet(); err != nil || got != "" {
		t.Fatalf("BackgroundGet with the copy deleted = %.60q, %v", got, err)
	}
}

func TestBackgroundCancelledDialogChangesNothing(t *testing.T) {
	s, _ := newAppearanceService(t)
	pickFile(t, s, "photo.png", pngBytes(t))
	first, err := s.BackgroundPick()
	if err != nil {
		t.Fatal(err)
	}
	s.pick = func() (string, error) { return "", nil }
	if url, err := s.BackgroundPick(); err != nil || url != "" {
		t.Fatalf("cancelled pick = %.40q, %v", url, err)
	}
	if got, _ := s.BackgroundGet(); got != first {
		t.Fatal("a cancelled pick changed the background")
	}
}

// The page never names a path (KTD41): no binding takes a string.
func TestNoAppearanceBindingTakesAPath(t *testing.T) {
	typ := reflect.TypeOf(&AppearanceService{})
	for i := range typ.NumMethod() {
		m := typ.Method(i)
		for j := 1; j < m.Type.NumIn(); j++ {
			if m.Type.In(j).Kind() == reflect.String {
				t.Errorf("%s takes a string argument", m.Name)
			}
		}
	}
	// The page's local setter cannot name the image either.
	ss, _ := newSettingsService(t)
	if err := ss.SetLocal(map[string]any{"background": "../../etc/passwd"}); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Fatalf("SetLocal(background) = %v", err)
	}
}
