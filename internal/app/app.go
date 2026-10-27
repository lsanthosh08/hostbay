// Package app holds the Wails services the UI binds to.
package app

// Name is the product name shown in the window and in About.
const Name = "Hostbay"

// Version is overridden at build time with -ldflags "-X hostbay/internal/app.Version=...".
var Version = "0.0.1"

// Info identifies the running build.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// App is the root service bound to the UI.
type App struct{}

func New() *App { return &App{} }

// Info reports the product name and version.
func (a *App) Info() Info { return Info{Name: Name, Version: Version} }
