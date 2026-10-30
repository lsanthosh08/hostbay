package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"hostbay/internal/app"
	"hostbay/internal/container"
	"hostbay/internal/importer/sshmanager"
	"hostbay/internal/mcp"
	"hostbay/internal/secrets"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	hbsync "hostbay/internal/sync"
	"hostbay/internal/tlstrust"
)

// The Vite build of the React UI in src/ lands in dist/ and is embedded here.
//
//go:embed all:dist
var assets embed.FS

// The tray shows the app icon: there is no monochrome one for a macOS
// template icon.
//
//go:embed build/appicon.png
var trayIcon []byte

func main() {
	// `hostbay mcp` is a stdio relay to the running app. It branches before
	// the single-instance check and never opens the store or takes the
	// lock (R30, KTD35).
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcpRelay())
	}
	var mainWindow *application.WebviewWindow
	var tunnels *app.TunnelService
	var vpns *app.VPNService
	var quitting atomic.Bool // ShouldQuit said yes; closing the window goes through
	var st *store.Store
	a := application.New(application.Options{
		Name:        app.Name,
		Description: "SSH, RDP and server manager",
		// A second launch focuses the running window instead of opening another
		// app that would fight the first over the data store lock.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "dev.hostbay.app",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if mainWindow != nil {
					mainWindow.Show() // it may be hidden in the menu bar
					mainWindow.Restore()
					mainWindow.Focus()
				}
			},
		},
		Services: []application.Service{
			application.NewService(app.New()),
		},
		MarshalError: app.MarshalError,
		// Quitting stops running tunnels and disconnects the VPN profiles
		// Hostbay connected, either of which may be carrying someone's work,
		// so ask first (R19).
		ShouldQuit: func() bool {
			// Quit runs ShouldQuit again, so the dialog's answer has to stick.
			if quitting.Load() {
				return true
			}
			n, v := 0, 0
			if tunnels != nil {
				n = tunnels.RunningTunnels()
			}
			if vpns != nil {
				v = vpns.ConnectedVPNs()
			}
			msg := app.QuitQuestion(n, v)
			if msg == "" {
				quitting.Store(true)
				return true
			}
			d := application.Get().Dialog.Question().
				SetTitle("Quit " + app.Name + "?").
				SetMessage(msg)
			yes, no := container.Buttons(runtime.GOOS, "Quit", "Cancel")
			d.AddButton(yes).OnClick(func() {
				quitting.Store(true)
				application.Get().Quit()
			})
			cancel := d.AddButton(no)
			d.SetDefaultButton(cancel).SetCancelButton(cancel)
			d.Show()
			return false
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		// Runs after the single-instance lock is released, and on macOS in
		// place of main's return, so a restart here is not handed back to
		// this process.
		PostShutdown: func() {
			if st != nil {
				st.Close()
			}
			if app.RelaunchRequested() {
				relaunch()
			}
		},
	})

	// Opened only after New, whose single-instance check hands a second launch
	// to the running app before it can trip over the store lock. The lock
	// stays in the default folder; the data may have moved (KTD42), and a
	// requested move is copied here, before any service has the store.
	home, err := store.DefaultDir()
	if err != nil {
		log.Fatal(err)
	}
	st, err = store.Open(home)
	var missing *store.FolderMissingError
	if errors.As(err, &missing) {
		log.Print(err)
		a.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			go askMissingFolder(home, missing)
		})
		if err := a.Run(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	if notice := st.Notice(); notice != "" {
		log.Print(notice)
		a.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
			a.Dialog.Warning().SetTitle(app.Name + " did not move its data").SetMessage(notice).Show()
		})
	}
	dir := st.Dir() // every service keeps its files in the data folder
	ver := &windowVerifier{st: st}
	vault := secrets.New(st, ver)
	// Linux verifies with the Hostbay master password, asked for in the UI.
	passwordPrompt := app.NewMasterPasswordPrompt()
	ver.ask = vault.UnlockingPrompt(passwordPrompt.Ask)
	// First launch brings SSH Manager's data across once. If the keychain
	// refuses, hosts still come across without passwords (AE7); any other
	// failure writes nothing and retries on the next launch.
	if src, err := sshmanager.DefaultSourceDir(); err == nil {
		if rep, err := sshmanager.ImportOnce(st, vault, src); err != nil {
			log.Printf("SSH Manager import: %v", err)
		} else if rep.Ran {
			log.Printf("SSH Manager import: %+v", rep)
		}
	}
	a.RegisterService(application.NewService(app.NewHostService(st, vault, ver)))
	a.RegisterService(application.NewService(app.NewSettingsService(st)))
	a.RegisterService(application.NewService(app.NewAppearanceService(st, dir)))
	a.RegisterService(application.NewService(app.NewMasterPasswordService(vault, passwordPrompt)))

	knownHosts, err := sshx.DefaultKnownHostsPath()
	if err != nil {
		log.Fatal(err)
	}
	trust := app.NewHostKeyTrust(sshx.NewKnownHostAppender(knownHosts))
	// Sync dials SFTP targets through the same known_hosts check as SSH
	// sessions; the HTTP timeout keeps a hung WebDAV or S3 server from
	// stalling the background loop forever.
	a.RegisterService(application.NewService(app.NewSyncService(st, vault, ver, trust, hbsync.Deps{
		Dialer:   &sshx.Dialer{KnownHostsPath: knownHosts, Timeout: sshx.DefaultTimeout},
		CacheDir: filepath.Join(dir, "sync-git"),
		HTTP:     &http.Client{Timeout: time.Minute},
	})))
	conns := sshx.NewManager(&sshx.Dialer{KnownHostsPath: knownHosts, Timeout: sshx.DefaultTimeout})
	creds := app.NewHostCredentials(st, vault)
	connSvc := app.NewConnService(trust, creds, conns)
	a.RegisterService(application.NewService(connSvc))
	files := app.NewFilesService(conns)
	a.RegisterService(application.NewService(files))
	// One pin store, so RDP, VNC, SPICE and the file hosts answer prompts the same way.
	certs := tlstrust.New(dir)
	a.RegisterService(application.NewService(app.NewTransferService(files, creds, certs)))
	a.RegisterService(application.NewService(app.NewAdminService(conns, connSvc)))
	a.RegisterService(application.NewService(app.NewUpdateService(dir)))
	rdpSvc := app.NewRdpService(creds, connSvc, certs)
	a.RegisterService(application.NewService(rdpSvc))
	a.HandleStream(app.RdpStreamName, app.ServeRdpStream(rdpSvc))
	vnc := app.NewVncService(creds, connSvc, certs)
	a.RegisterService(application.NewService(vnc))
	a.HandleStream(app.VncStreamName, app.ServeVncStream(vnc))
	spice := app.NewSpiceService(creds, connSvc, certs)
	a.RegisterService(application.NewService(spice))
	a.HandleStream(app.SpiceStreamName, app.ServeSpiceStream(spice))
	dbSvc := app.NewDatabaseService(creds, connSvc, certs, dir)
	a.RegisterService(application.NewService(dbSvc))
	a.RegisterService(application.NewService(app.NewKubernetesService(creds, connSvc, certs)))
	// X11 desktops render in the VNC tab over the vnc stream.
	a.RegisterService(application.NewService(app.NewX11Service(connSvc, vnc)))
	zmodem := app.NewZmodemService(connSvc)
	a.RegisterService(application.NewService(zmodem))
	a.HandleStream(app.ZmodemStreamName, app.ServeZmodemStream(zmodem))
	tunnels = app.NewTunnelService(st, connSvc)
	a.RegisterService(application.NewService(tunnels))
	vpns = app.NewVPNService(st, vault)
	app.LinkVPNs(creds, vpns) // before any dial, which runs only once the app does
	a.RegisterService(application.NewService(vpns))
	// System routes for system-wide and external VPNs go through the
	// privileged helper (KTD8, KTD9); the helper's connection ends at quit,
	// which removes them.
	netdSvc := app.NewNetdService()
	a.RegisterService(application.NewService(netdSvc))
	a.RegisterService(application.NewService(app.NewRoutingService(st, vpns, netdSvc)))
	a.RegisterService(application.NewService(app.NewMonitorService(connSvc)))
	a.RegisterService(application.NewService(app.NewTerminalToolsService(connSvc)))
	a.RegisterService(application.NewService(app.NewWorkspaceService(st)))
	// The AI agent: its key stays in Go, and it reaches terminals only
	// through the gate in internal/agentexec (KTD31, KTD32).
	aiSvc := app.NewAIService(st, vault, creds, connSvc, certs)
	app.LinkAIDatabase(aiSvc, dbSvc) // questions to SQL read schema names only
	a.RegisterService(application.NewService(aiSvc))
	// MCP clients share the agent's gate and approval tray (KTD32, KTD35).
	mcpSvc := app.NewMCPService(st, connSvc, creds, aiSvc)
	a.RegisterService(application.NewService(mcpSvc))

	mainWindow = a.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            app.Name,
		Width:            1280,
		Height:           820,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: application.NewRGB(23, 23, 25), // dark --ground, src/styles.css
		URL:              "/",
		// Files dropped on an element marked data-file-drop-target (the VPN
		// list) reach the UI by path, which a web drop event does not give.
		EnableFileDrop: true,
	})
	mainWindow.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		a.Event.Emit("files-dropped", e.Context().DroppedFiles())
	})
	ver.win = mainWindow
	mcpSvc.Raise = func() {
		mainWindow.Show()
		mainWindow.Restore()
		mainWindow.Focus()
	}
	// Closing the window hides it while Hostbay keeps running in the menu
	// bar (KTD10); otherwise it quits through ShouldQuit, which asks first
	// while tunnels or VPNs are up (R19).
	mainWindow.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		l, err := st.Local()
		if err != nil {
			log.Printf("local settings: %v", err)
		}
		switch app.WindowClose(l.KeepRunning, quitting.Load()) {
		case app.CloseHide:
			e.Cancel()
			mainWindow.Hide()
		case app.CloseQuit:
			e.Cancel()
			go a.Quit()
		}
	})
	tray := app.NewTray(vpns, func() {
		mainWindow.Show()
		mainWindow.UnMinimise()
		mainWindow.Focus()
	}, a.Quit)
	tray.Attach(a.SystemTray.New().SetIcon(trayIcon))
	a.Event.On(app.VPNStateEvent, func(e *application.CustomEvent) {
		if s, ok := e.Data.(app.VPNState); ok {
			tray.StateChanged(s)
		}
	})

	if err := a.Run(); err != nil {
		log.Fatal(err)
	}
}

// askMissingFolder stops start-up while the pointer names a data folder
// that is gone or unreadable, such as an unplugged disk (KTD42). It never
// opens an empty store in its place: Retry and a chosen folder restart
// Hostbay, and a folder is accepted only when it holds a Hostbay store.
func askMissingFolder(home string, missing error) {
	const retry, other, useDefault, quit = "Retry", "Choose another folder", "Use the default folder", "Quit"
	msg := missing.Error()
	for {
		labels := []string{retry, other, quit}
		if store.HoldsStore(home) {
			labels = []string{retry, other, useDefault, quit}
		}
		dir := home
		switch choose(app.Name+" cannot find its data", msg, labels...) {
		case retry:
			app.Relaunch()
			return
		case other:
			var err error
			if dir, err = app.PickDataFolder("Choose the folder that holds Hostbay's data"); err != nil || dir == "" {
				continue
			}
		case useDefault:
		default:
			application.Get().Quit()
			return
		}
		if err := store.PointTo(home, dir); err != nil {
			msg = missing.Error() + "\n\n" + err.Error()
			continue
		}
		app.Relaunch()
		return
	}
}

// mcpRelay runs `hostbay mcp`: it reads the token from HOSTBAY_MCP_TOKEN,
// as the MCP spec asks of stdio servers, and exits with a message when
// Hostbay is not running rather than starting one.
func mcpRelay() int {
	home, err := store.DefaultDir()
	if err == nil {
		err = mcp.Relay(context.Background(), home, os.Getenv("HOSTBAY_MCP_TOKEN"), os.Stdin, os.Stdout, os.Stderr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostbay mcp:", err)
		return 1
	}
	return 0
}

// relaunch starts Hostbay again after this process has let go of its locks.
func relaunch() {
	exe, err := os.Executable()
	if p := os.Getenv("APPIMAGE"); p != "" {
		exe, err = p, nil // os.Executable is inside the AppImage's mount, gone once this process exits
	}
	if err == nil {
		err = exec.Command(exe, os.Args[1:]...).Start()
	}
	if err != nil {
		log.Printf("restart %s: %v", app.Name, err)
	}
}

// windowVerifier builds the platform verifier when it is used, because
// Windows Hello needs the main window's handle and the window is created
// after the services.
type windowVerifier struct {
	st  *store.Store
	win *application.WebviewWindow
	ask secrets.PasswordPrompt // Linux's master password prompt
}

func (w *windowVerifier) Verify(ctx context.Context, reason string) secrets.Result {
	var hwnd uintptr
	if w.win != nil {
		hwnd = uintptr(w.win.NativeWindow())
	}
	return secrets.NewVerifier(w.st, hwnd, w.ask).Verify(ctx, reason)
}
