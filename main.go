package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed index.html
var htmlPage string

//go:embed icon.png
var iconPng []byte

//go:embed manifest.json
var manifestJson string

type VMInfo struct {
	Name           string   `json:"name"`
	State          string   `json:"state"`
	CPU            int      `json:"cpu"`
	RAMMB          int      `json:"ram_mb"`
	Uptime         string   `json:"uptime"`
	IPs            []string `json:"ips"`
	ConfiguredIP   string   `json:"configured_ip"`
	StaticIPStatus string   `json:"static_ip_status"` // "active", "dropped", "none"
}

type IPStore struct {
	sync.RWMutex
	path string
	data map[string]string // vmName -> IP
}

var ipStore = &IPStore{
	data: make(map[string]string),
}

func initIPStore() {
	exe, err := os.Executable()
	if err != nil {
		ipStore.path = "ip_config.json"
	} else {
		ipStore.path = filepath.Join(filepath.Dir(exe), "ip_config.json")
	}

	raw, err := os.ReadFile(ipStore.path)
	if err == nil {
		var m map[string]string
		if json.Unmarshal(raw, &m) == nil {
			ipStore.data = m
		}
	}
}

func (s *IPStore) Get(name string) string {
	s.RLock()
	defer s.RUnlock()
	return s.data[name]
}

func (s *IPStore) Set(name, ip string) error {
	s.Lock()
	defer s.Unlock()
	s.data[name] = ip
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0644)
}

func (s *IPStore) All() map[string]string {
	s.RLock()
	defer s.RUnlock()
	res := make(map[string]string)
	for k, v := range s.data {
		res[k] = v
	}
	return res
}

const psScript = `
$neighbors = Get-NetNeighbor -AddressFamily IPv4 -ErrorAction SilentlyContinue
$adapters = Get-VMNetworkAdapter -VMName * -ErrorAction SilentlyContinue

$vms = @(Get-VM | ForEach-Object {
    $vm = $_
    $vmAdapters = $adapters | Where-Object { $_.VMName -eq $vm.Name }
    $ips = @()
    foreach ($ad in $vmAdapters) {
        $macClean = ($ad.MacAddress -replace '..(?!$)', '$0-')
        $matches = $neighbors | Where-Object { $_.LinkLayerAddress -eq $macClean }
        foreach ($m in $matches) {
            if ($m.IPAddress -notmatch '^(169\.254|127\.)') { $ips += $m.IPAddress }
        }
    }
    
    $upStr = "-"
    if ($vm.Uptime.TotalSeconds -gt 0) {
        $upStr = "{0}h {1}m" -f [int]$vm.Uptime.TotalHours, $vm.Uptime.Minutes
    }
    
    [PSCustomObject]@{
        name     = $vm.Name
        state    = $vm.State.ToString()
        cpu      = [int]$vm.CPUUsage
        ram_mb   = [math]::Round($vm.MemoryAssigned / 1MB)
        uptime   = $upStr
        ips      = @($ips | Select-Object -Unique)
    }
})
if ($vms.Count -eq 0) { "[]" } else { $vms | ConvertTo-Json -Depth 3 }
`

var (
	shell32       = syscall.NewLazyDLL("shell32.dll")
	isUserAnAdmin = shell32.NewProc("IsUserAnAdmin")
	shellExecuteW = shell32.NewProc("ShellExecuteW")
)

func isElevated() bool {
	ret, _, _ := isUserAnAdmin.Call()
	return ret != 0
}

func ensureElevated() {
	if isElevated() {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	dirPtr, _ := syscall.UTF16PtrFromString(filepath.Dir(exe))

	ret, _, _ := shellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(exePtr)), 0, uintptr(unsafe.Pointer(dirPtr)), 1)
	if ret > 32 {
		os.Exit(0)
	}
}

func isHostGatewayHealthy() bool {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-NetIPAddress -InterfaceAlias 'vEthernet (Default Switch)' -IPAddress '10.10.10.1' -ErrorAction SilentlyContinue) -ne $null")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "True"
}

func restoreHostGateway() error {
	script := `
$adapter = Get-NetAdapter -Name 'vEthernet (Default Switch)' -ErrorAction SilentlyContinue
if ($adapter) {
    $ip = Get-NetIPAddress -InterfaceAlias 'vEthernet (Default Switch)' -IPAddress '10.10.10.1' -ErrorAction SilentlyContinue
    if (-not $ip) {
        New-NetIPAddress -InterfaceAlias 'vEthernet (Default Switch)' -IPAddress '10.10.10.1' -PrefixLength 24 -SkipAsSource $false -ErrorAction SilentlyContinue
    }
}
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

func applyVMStaticIP(vmName, ip, cidr string) error {
	if cidr == "" {
		cidr = "24"
	}

	// 1. Ensure host gateway 10.10.10.1 is present
	_ = restoreHostGateway()

	// 2. Configure inside Linux VM via multipass
	script := fmt.Sprintf(`sudo bash -c '
cat << "EOF" > /etc/systemd/system/static-ip.service
[Unit]
Description=Static Dev IP %s
After=network.target

[Service]
Type=oneshot
ExecStart=/bin/ip addr replace %s/%s dev eth0
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable static-ip.service
systemctl restart static-ip.service
/bin/ip addr replace %s/%s dev eth0
'`, ip, ip, cidr, ip, cidr)

	cmd := exec.Command("multipass", "exec", vmName, "--", "bash", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s (%w)", strings.TrimSpace(string(out)), err)
	}

	// 3. Save to local IP store
	_ = ipStore.Set(vmName, ip)
	return nil
}

func getVMs() ([]VMInfo, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s (%w)", strings.TrimSpace(string(output)), err)
	}

	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || trimmed == "null" {
		return []VMInfo{}, nil
	}

	var list []VMInfo
	if strings.HasPrefix(trimmed, "{") {
		var single VMInfo
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, err
		}
		list = []VMInfo{single}
	} else {
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, err
		}
	}

	// Populate static IP status from persistent store
	for i := range list {
		configured := ipStore.Get(list[i].Name)
		list[i].ConfiguredIP = configured

		if configured == "" {
			// Check if any detected IP looks like 10.10.10.x
			for _, ip := range list[i].IPs {
				if strings.HasPrefix(ip, "10.10.10.") {
					list[i].ConfiguredIP = ip
					list[i].StaticIPStatus = "active"
					_ = ipStore.Set(list[i].Name, ip)
					break
				}
			}
			if list[i].StaticIPStatus == "" {
				list[i].StaticIPStatus = "none"
			}
		} else {
			// Check if configured IP is currently detected in IPs list
			found := false
			for _, ip := range list[i].IPs {
				if ip == configured {
					found = true
					break
				}
			}
			if found {
				list[i].StaticIPStatus = "active"
			} else {
				if list[i].State == "Running" {
					list[i].StaticIPStatus = "dropped"
				} else {
					list[i].StaticIPStatus = "off"
				}
			}
		}
	}

	return list, nil
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "missing VM name", http.StatusBadRequest)
		return
	}

	var psCmd string
	switch action {
	case "start":
		psCmd = fmt.Sprintf("Start-VM -Name '%s'", name)
	case "stop":
		psCmd = fmt.Sprintf("Stop-VM -Name '%s' -Force", name)
	case "restart":
		psCmd = fmt.Sprintf("Restart-VM -Name '%s' -Force", name)
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func findAppBrowser() string {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func launchAppWindow(url string) {
	time.Sleep(150 * time.Millisecond)

	browser := findAppBrowser()
	if browser != "" {
		userData := filepath.Join(os.TempDir(), "hv-monitor-profile")
		appArg := fmt.Sprintf("--app=%s", url)
		sizeArg := "--window-size=1140,760"
		profileArg := fmt.Sprintf("--user-data-dir=%s", userData)
		appIdArg := "--app-id=HVMonitor"

		cmd := exec.Command(browser, appArg, sizeArg, profileArg, appIdArg, "--no-first-run", "--no-default-browser-check")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Start(); err == nil {
			return
		}
	}

	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func main() {
	ensureElevated()
	initIPStore()

	port := "18288"
	url := "http://127.0.0.1:" + port

	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatalf("Port %s busy or error: %v", port, err)
	}

	// Assets
	http.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(iconPng)
	})

	http.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write([]byte(manifestJson))
	})

	// VM status API
	http.HandleFunc("/api/vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		vms, err := getVMs()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(vms)
	})

	// Host Gateway status API
	http.HandleFunc("/api/network/gateway", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		healthy := isHostGatewayHealthy()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ip":      "10.10.10.1",
			"healthy": healthy,
		})
	})

	// Restore Host Gateway API
	http.HandleFunc("/api/network/restore-gateway", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := restoreHostGateway(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Set/Apply VM Static IP API
	http.HandleFunc("/api/network/set-vm-ip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Name string `json:"name"`
			IP   string `json:"ip"`
			CIDR string `json:"cidr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		if err := applyVMStaticIP(req.Name, req.IP, req.CIDR); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Restore single VM Static IP
	http.HandleFunc("/api/network/restore-vm-ip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		ip := ipStore.Get(name)
		if ip == "" {
			http.Error(w, "no configured static IP for this VM", 400)
			return
		}
		if err := applyVMStaticIP(name, ip, "24"); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Restore All IPs (Host Gateway + All Running VMs)
	http.HandleFunc("/api/network/restore-all", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = restoreHostGateway()

		allConfigured := ipStore.All()
		vms, _ := getVMs()
		var restored []string

		for _, vm := range vms {
			if vm.State == "Running" {
				if ip, ok := allConfigured[vm.Name]; ok && ip != "" {
					_ = applyVMStaticIP(vm.Name, ip, "24")
					restored = append(restored, vm.Name)
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"restored": restored,
		})
	})

	http.HandleFunc("/api/action", handleAction)

	http.HandleFunc("/api/exit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(300 * time.Millisecond)
			os.Exit(0)
		}()
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlPage))
	})

	go launchAppWindow(url)

	if err := http.Serve(listener, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
