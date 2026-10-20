package main

import (
	_ "embed"
	"encoding/base64"
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

type BackupItem struct {
	ID        string  `json:"id"`
	VMName    string  `json:"vm_name"`
	Type      string  `json:"type"` // "project" or "vm"
	Filename  string  `json:"filename"`
	SizeMB    float64 `json:"size_mb"`
	CreatedAt string  `json:"created_at"`
	Path      string  `json:"path"`
}

type JobStatus struct {
	ID        string `json:"id"`
	VMName    string `json:"vm_name"`
	Type      string `json:"type"`
	Status    string `json:"status"` // "running", "completed", "error"
	Message   string `json:"message"`
	StartedAt string `json:"started_at"`
}

type IPStore struct {
	sync.RWMutex
	path string
	data map[string]string // vmName -> IP
}

var (
	ipStore = &IPStore{data: make(map[string]string)}

	activeJobsMu sync.RWMutex
	activeJobs   = make(map[string]*JobStatus)
)

func setJob(id, vm, jobType, status, msg string) {
	activeJobsMu.Lock()
	defer activeJobsMu.Unlock()
	activeJobs[id] = &JobStatus{
		ID:        id,
		VMName:    vm,
		Type:      jobType,
		Status:    status,
		Message:   msg,
		StartedAt: time.Now().Format("15:04:05"),
	}
}

func getActiveJobs() []*JobStatus {
	activeJobsMu.RLock()
	defer activeJobsMu.RUnlock()
	var list []*JobStatus
	for _, j := range activeJobs {
		list = append(list, j)
	}
	return list
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

func getBackupsDir() string {
	exe, err := os.Executable()
	base := "C:\\tools\\hv-monitor"
	if err == nil {
		base = filepath.Dir(exe)
	}
	dir := filepath.Join(base, "backups")
	os.MkdirAll(filepath.Join(dir, "projects"), 0755)
	os.MkdirAll(filepath.Join(dir, "vms"), 0755)
	return dir
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
	_ = restoreHostGateway()

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

	for i := range list {
		configured := ipStore.Get(list[i].Name)
		list[i].ConfiguredIP = configured

		if configured == "" {
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

// ----------------- BACKUP ENGINE -----------------

func backupProjectAndDB(vmName string) (string, error) {
	timestamp := time.Now().Format("20060102_150405")
	script := fmt.Sprintf(`
VM_NAME="%s"
TIMESTAMP="%s"
STAGE_DIR="/tmp/hv_backup_${TIMESTAMP}"
mkdir -p "$STAGE_DIR/database" "$STAGE_DIR/project"

# 1. Detect and dump database from containers or host
DB_STATUS="none"
MYSQL_CONTAINER=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -iE 'mysql|mariadb|db' | head -n 1 || true)
POSTGRES_CONTAINER=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -iE 'postgres|psql' | head -n 1 || true)

if [ -n "$MYSQL_CONTAINER" ]; then
    PASS=$(docker inspect "$MYSQL_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null | grep -iE '^MYSQL_ROOT_PASSWORD=' | head -n 1 | cut -d= -f2 || true)
    if [ -z "$PASS" ]; then
        PASS=$(docker inspect "$MYSQL_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null | grep -iE '^MYSQL_PASSWORD=' | head -n 1 | cut -d= -f2 || true)
    fi
    USER="root"
    if [ -n "$PASS" ]; then
        docker exec "$MYSQL_CONTAINER" mysqldump -u "$USER" -p"$PASS" --all-databases > "$STAGE_DIR/database/mysql_all_databases.sql" 2>/dev/null || true
    else
        docker exec "$MYSQL_CONTAINER" mysqldump --all-databases > "$STAGE_DIR/database/mysql_all_databases.sql" 2>/dev/null || true
    fi
    if [ -s "$STAGE_DIR/database/mysql_all_databases.sql" ]; then
        DB_STATUS="mysql:$MYSQL_CONTAINER"
    fi
elif [ -n "$POSTGRES_CONTAINER" ]; then
    docker exec "$POSTGRES_CONTAINER" pg_dumpall -U postgres > "$STAGE_DIR/database/postgres_all_databases.sql" 2>/dev/null || true
    if [ -s "$STAGE_DIR/database/postgres_all_databases.sql" ]; then
        DB_STATUS="postgres:$POSTGRES_CONTAINER"
    fi
fi

# Fallback: check for docker volumes data
if [ "$DB_STATUS" = "none" ]; then
    for vdir in /var/lib/docker/volumes/*data*/_data; do
        if [ -d "$vdir" ]; then
            mkdir -p "$STAGE_DIR/database/volume_data"
            cp -r "$vdir"/* "$STAGE_DIR/database/volume_data/" 2>/dev/null || true
            DB_STATUS="volume:$vdir"
            break
        fi
    done
fi

# 2. Detect project in /home/ubuntu or /var/www
PROJECT_DIR=""
for dir in /home/ubuntu/* /var/www/*; do
    if [ -d "$dir" ] && [ "$(basename "$dir")" != "snap" ] && [ "$(basename "$dir")" != "html" ]; then
        PROJECT_DIR="$dir"
        break
    fi
done

if [ -n "$PROJECT_DIR" ]; then
    PARENT=$(dirname "$PROJECT_DIR")
    NAME=$(basename "$PROJECT_DIR")
    tar -czf "$STAGE_DIR/project/source_code.tar.gz" -C "$PARENT" --exclude="node_modules" --exclude=".git" --exclude="vendor" --exclude="cache" "$NAME" 2>/dev/null || true
fi

echo "vm_name: $VM_NAME" > "$STAGE_DIR/backup_info.txt"
echo "timestamp: $TIMESTAMP" >> "$STAGE_DIR/backup_info.txt"
echo "database_source: $DB_STATUS" >> "$STAGE_DIR/backup_info.txt"
echo "project_source: $PROJECT_DIR" >> "$STAGE_DIR/backup_info.txt"

FINAL_TAR="/tmp/${VM_NAME}_project_${TIMESTAMP}.tar.gz"
tar -czf "$FINAL_TAR" -C "$STAGE_DIR" .
rm -rf "$STAGE_DIR"
echo "BACKUP_READY:$FINAL_TAR"
`, vmName, timestamp)

	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := exec.Command("multipass", "exec", vmName, "--", "bash", "-c", fmt.Sprintf("echo %s | base64 -d | bash", b64))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("backup execution failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	outStr := string(out)
	idx := strings.Index(outStr, "BACKUP_READY:")
	if idx == -1 {
		return "", fmt.Errorf("backup failed to produce archive: %s", outStr)
	}
	remoteTar := strings.TrimSpace(outStr[idx+len("BACKUP_READY:"):])

	backupsDir := getBackupsDir()
	localTarName := fmt.Sprintf("%s_project_%s.tar.gz", vmName, timestamp)
	localTarPath := filepath.Join(backupsDir, "projects", localTarName)

	transferCmd := exec.Command("multipass", "transfer", fmt.Sprintf("%s:%s", vmName, remoteTar), localTarPath)
	transferCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := transferCmd.Run(); err != nil {
		return "", fmt.Errorf("failed to transfer backup to host: %w", err)
	}

	cleanCmd := exec.Command("multipass", "exec", vmName, "--", "rm", "-f", remoteTar)
	cleanCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cleanCmd.Run()

	return localTarPath, nil
}

func backupFullVM(vmName string) (string, error) {
	timestamp := time.Now().Format("20060102_150405")
	backupsDir := getBackupsDir()
	exportPath := filepath.Join(backupsDir, "vms", fmt.Sprintf("%s_vm_%s", vmName, timestamp))
	os.MkdirAll(exportPath, 0755)

	psCmd := fmt.Sprintf("Export-VM -Name '%s' -Path '%s'", vmName, exportPath)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Hyper-V export failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	return exportPath, nil
}

func listBackups() ([]BackupItem, error) {
	backupsDir := getBackupsDir()
	var items []BackupItem

	// 1. Projects
	projDir := filepath.Join(backupsDir, "projects")
	if entries, err := os.ReadDir(projDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tar.gz") {
				info, _ := e.Info()
				sizeMB := float64(0)
				modTime := ""
				if info != nil {
					sizeMB = float64(info.Size()) / (1024 * 1024)
					modTime = info.ModTime().Format("2006-01-02 15:04:05")
				}
				parts := strings.Split(e.Name(), "_")
				vmName := parts[0]

				items = append(items, BackupItem{
					ID:        e.Name(),
					VMName:    vmName,
					Type:      "project",
					Filename:  e.Name(),
					SizeMB:    sizeMB,
					CreatedAt: modTime,
					Path:      filepath.Join(projDir, e.Name()),
				})
			}
		}
	}

	// 2. Full VMs
	vmDir := filepath.Join(backupsDir, "vms")
	if entries, err := os.ReadDir(vmDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				info, _ := e.Info()
				modTime := ""
				if info != nil {
					modTime = info.ModTime().Format("2006-01-02 15:04:05")
				}
				parts := strings.Split(e.Name(), "_")
				vmName := parts[0]
				fullPath := filepath.Join(vmDir, e.Name())

				var totalSize int64
				filepath.Walk(fullPath, func(_ string, f os.FileInfo, _ error) error {
					if f != nil && !f.IsDir() {
						totalSize += f.Size()
					}
					return nil
				})

				items = append(items, BackupItem{
					ID:        e.Name(),
					VMName:    vmName,
					Type:      "vm",
					Filename:  e.Name(),
					SizeMB:    float64(totalSize) / (1024 * 1024),
					CreatedAt: modTime,
					Path:      fullPath,
				})
			}
		}
	}

	return items, nil
}

// ----------------- HANDLERS -----------------

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
		sizeArg := "--window-size=1180,780"
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
	getBackupsDir() // ensure directories exist

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

	// Restore All IPs
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

	// Backups API
	http.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items, err := listBackups()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(items)
	})

	http.HandleFunc("/api/backup/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getActiveJobs())
	})

	http.HandleFunc("/api/backup/project", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing VM name", 400)
			return
		}

		jobID := fmt.Sprintf("proj_%s_%d", name, time.Now().Unix())
		setJob(jobID, name, "project", "running", "Creating Project & Database dump...")

		go func() {
			path, err := backupProjectAndDB(name)
			if err != nil {
				setJob(jobID, name, "project", "error", err.Error())
			} else {
				setJob(jobID, name, "project", "completed", fmt.Sprintf("Saved to %s", filepath.Base(path)))
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	http.HandleFunc("/api/backup/vm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing VM name", 400)
			return
		}

		jobID := fmt.Sprintf("vm_%s_%d", name, time.Now().Unix())
		setJob(jobID, name, "vm", "running", "Exporting Full Hyper-V VM image...")

		go func() {
			path, err := backupFullVM(name)
			if err != nil {
				setJob(jobID, name, "vm", "error", err.Error())
			} else {
				setJob(jobID, name, "vm", "completed", fmt.Sprintf("Exported to %s", filepath.Base(path)))
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	http.HandleFunc("/api/backup/open-folder", func(w http.ResponseWriter, r *http.Request) {
		dir := getBackupsDir()
		exec.Command("explorer.exe", dir).Start()
		w.Write([]byte(`{"status":"ok"}`))
	})

	http.HandleFunc("/api/backup/delete", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		backupsDir := getBackupsDir()
		cleanPath := filepath.Clean(path)
		cleanBase := filepath.Clean(backupsDir)

		if !strings.HasPrefix(cleanPath, cleanBase) || cleanPath == cleanBase {
			http.Error(w, "invalid path", 400)
			return
		}

		if err := os.RemoveAll(cleanPath); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
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
