package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
)

// IIS on Windows hosts over the SSH session.

type IisSite struct {
	Name         string `json:"name"`
	ID           int    `json:"id"`
	State        string `json:"state"`
	PhysicalPath string `json:"physicalPath"`
	Bindings     string `json:"bindings"`
	AppPool      string `json:"appPool"`
}

type IisPool struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Runtime string `json:"runtime"`
}

// psPrelude keeps progress records (Import-Module shows some) out of the
// output.
const psPrelude = "$ProgressPreference='SilentlyContinue'; "

// powerShell runs script with -EncodedCommand (UTF-16LE, base64), so only
// base64 text reaches the remote shell, whether that is cmd.exe, PowerShell
// or sh. iis.rs wrapped the script in double quotes for cmd.exe, which does
// not honour the \" it escaped quotes with.
func powerShell(ctx context.Context, c *ssh.Client, script string) (Result, error) {
	u := utf16.Encode([]rune(psPrelude + script))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return Exec(ctx, c, "powershell -NoProfile -NonInteractive -EncodedCommand "+base64.StdEncoding.EncodeToString(b))
}

// psQuote makes s a PowerShell single-quoted string. Names are validated
// first, so this never has a quote to double; it is the second wall.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// jsonMany decodes ConvertTo-Json output, which is a bare object when there
// is one item and an array otherwise; anything else is no items.
func jsonMany[T any](out string) []T {
	s := []byte(strings.TrimSpace(out))
	var list []T
	if json.Unmarshal(s, &list) == nil {
		return list
	}
	var one T
	if json.Unmarshal(s, &one) == nil {
		return []T{one}
	}
	return nil
}

func IisSites(ctx context.Context, c *ssh.Client) ([]IisSite, error) {
	r, err := powerShell(ctx, c, `Import-Module WebAdministration -ErrorAction SilentlyContinue; Get-Website | ForEach-Object { [pscustomobject]@{ name=$_.name; id=$_.id; state=$_.state; physicalPath=$_.physicalPath; bindings=(($_.bindings.Collection | ForEach-Object { $_.bindingInformation }) -join ', '); appPool=$_.applicationPool } } | ConvertTo-Json -Compress -Depth 4`)
	if err != nil {
		return nil, err
	}
	type raw struct {
		IisSite
		ID any `json:"id"` // iis.rs read it as a JSON value and took 0 unless it was an integer
	}
	var list []IisSite
	for _, s := range jsonMany[raw](r.Stdout) {
		if f, ok := s.ID.(float64); ok && f == float64(int(f)) {
			s.IisSite.ID = int(f)
		}
		list = append(list, s.IisSite)
	}
	return list, nil
}

func IisPools(ctx context.Context, c *ssh.Client) ([]IisPool, error) {
	r, err := powerShell(ctx, c, `Import-Module WebAdministration -ErrorAction SilentlyContinue; Get-ChildItem IIS:\AppPools | ForEach-Object { [pscustomobject]@{ name=$_.name; state=$_.state; runtime=$_.managedRuntimeVersion } } | ConvertTo-Json -Compress -Depth 3`)
	if err != nil {
		return nil, err
	}
	return jsonMany[IisPool](r.Stdout), nil
}

func IisSiteAction(ctx context.Context, c *ssh.Client, name, action string) (Result, error) {
	if validIisName(name) != nil {
		return Result{}, errors.New("invalid site name")
	}
	n := psQuote(name)
	cmd, ok := map[string]string{
		"start":   "Start-Website -Name " + n,
		"stop":    "Stop-Website -Name " + n,
		"restart": "Stop-Website -Name " + n + "; Start-Website -Name " + n,
	}[action]
	if !ok {
		return Result{}, errors.New("invalid action")
	}
	return powerShell(ctx, c, "Import-Module WebAdministration; "+cmd)
}

func IisPoolAction(ctx context.Context, c *ssh.Client, name, action string) (Result, error) {
	if validIisName(name) != nil {
		return Result{}, errors.New("invalid pool name")
	}
	n := psQuote(name)
	cmd, ok := map[string]string{
		"start":   "Start-WebAppPool -Name " + n,
		"stop":    "Stop-WebAppPool -Name " + n,
		"recycle": "Restart-WebAppPool -Name " + n,
	}[action]
	if !ok {
		return Result{}, errors.New("invalid action")
	}
	return powerShell(ctx, c, "Import-Module WebAdministration; "+cmd)
}
