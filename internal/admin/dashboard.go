package admin

import (
	"context"
	"strings"

	"golang.org/x/crypto/ssh"
)

type DashboardInfo struct {
	OS                  string `json:"os"`
	Uptime              string `json:"uptime"`
	Load                string `json:"load"`
	Disk                string `json:"disk"`
	NginxInstalled      bool   `json:"nginxInstalled"`
	DockerInstalled     bool   `json:"dockerInstalled"`
	DockerPermission    bool   `json:"dockerPermission"`
	PodmanInstalled     bool   `json:"podmanInstalled"`
	NerdctlInstalled    bool   `json:"nerdctlInstalled"`
	AzureAgentInstalled bool   `json:"azureAgentInstalled"`
}

type WinDashboard struct {
	OS           string `json:"os"`
	Uptime       string `json:"uptime"`
	CPU          string `json:"cpu"`
	Memory       string `json:"memory"`
	Disk         string `json:"disk"`
	IisInstalled bool   `json:"iisInstalled"`
}

// One round trip printing KEY::value lines.
const linuxDashboard = `
echo "OS::$( (. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME") || uname -sr )"
echo "UPTIME::$(uptime -p 2>/dev/null || uptime)"
echo "LOAD::$(cat /proc/loadavg 2>/dev/null | cut -d' ' -f1-3)"
echo "DISK::$(df -h / 2>/dev/null | awk 'NR==2{print $3" / "$2" ("$5")"}')"
echo "NGINX::$(command -v nginx >/dev/null 2>&1 && echo yes || echo no)"
echo "DOCKER::$(command -v docker >/dev/null 2>&1 && echo yes || echo no)"
echo "DOCKERPERM::$(docker ps >/dev/null 2>&1 && echo yes || echo no)"
echo "PODMAN::$(command -v podman >/dev/null 2>&1 && echo yes || echo no)"
echo "NERDCTL::$(command -v nerdctl >/dev/null 2>&1 && echo yes || echo no)"
echo "AZURE::$(ls /etc/systemd/system/vsts.agent.* >/dev/null 2>&1 && echo yes || echo no)"
`

// The iis.rs script with its quoting written for PowerShell itself, since
// it now travels encoded instead of through cmd.exe: the disk filter is
// DeviceID='C:'.
const windowsDashboard = `$os=Get-CimInstance Win32_OperatingSystem; $cs=Get-CimInstance Win32_ComputerSystem; $up=(Get-Date)-$os.LastBootUpTime; $c=Get-CimInstance Win32_Processor | Select-Object -First 1; $d=Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"; Write-Output ('OS::'+$os.Caption+' '+$os.Version); Write-Output ('UPTIME::'+[int]$up.TotalHours+'h '+$up.Minutes+'m'); Write-Output ('CPU::'+$c.Name.Trim()); Write-Output ('MEM::'+[math]::Round(($cs.TotalPhysicalMemory-($os.FreePhysicalMemory*1024))/1GB,1)+' / '+[math]::Round($cs.TotalPhysicalMemory/1GB,1)+' GB'); Write-Output ('DISK::'+[math]::Round(($d.Size-$d.FreeSpace)/1GB,0)+' / '+[math]::Round($d.Size/1GB,0)+' GB'); Write-Output ('IIS::'+((Get-Service W3SVC -ErrorAction SilentlyContinue) -ne $null))`

// labeled maps each KEY::value line to its trimmed value.
func labeled(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "::"); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}

func Dashboard(ctx context.Context, c *ssh.Client) (DashboardInfo, error) {
	r, err := Exec(ctx, c, linuxDashboard)
	if err != nil {
		return DashboardInfo{}, err
	}
	m := labeled(r.Stdout)
	return DashboardInfo{
		OS: m["OS"], Uptime: m["UPTIME"], Load: m["LOAD"], Disk: m["DISK"],
		NginxInstalled: m["NGINX"] == "yes", DockerInstalled: m["DOCKER"] == "yes",
		DockerPermission: m["DOCKERPERM"] == "yes", PodmanInstalled: m["PODMAN"] == "yes", NerdctlInstalled: m["NERDCTL"] == "yes",
		AzureAgentInstalled: m["AZURE"] == "yes",
	}, nil
}

func WindowsDashboard(ctx context.Context, c *ssh.Client) (WinDashboard, error) {
	r, err := powerShell(ctx, c, windowsDashboard)
	if err != nil {
		return WinDashboard{}, err
	}
	m := labeled(r.Stdout)
	return WinDashboard{
		OS: m["OS"], Uptime: m["UPTIME"], CPU: m["CPU"], Memory: m["MEM"], Disk: m["DISK"],
		IisInstalled: strings.EqualFold(m["IIS"], "true"),
	}, nil
}
