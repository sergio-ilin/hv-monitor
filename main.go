package main

import (
	"archive/tar"
	"compress/gzip"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

//go:embed ShortcutHelper.exe
var shortcutHelperExe []byte

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

type BackupSettings struct {
	Enabled        bool   `json:"enabled"`
	IntervalHours  int    `json:"interval_hours"`
	RetentionCount int    `json:"retention_count"`
	TargetType     string `json:"target_type"`
}

type SyncTarget struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	LocalPath     string `json:"local_path"`
	VMName        string `json:"vm_name"`
	RemotePath    string `json:"remote_path"`
	Excludes      string `json:"excludes"`
	PostSyncCmd   string `json:"post_sync_cmd"`
	LastSyncedAt  string `json:"last_synced_at"`
	LastSyncFiles int    `json:"last_sync_files"`

	// Dynamically inspected
	IsModified    bool   `json:"is_modified"`
	ModifiedCount int    `json:"modified_count"`
	LocalExists   bool   `json:"local_exists"`
	LatestModTime string `json:"latest_mod_time"`
}

type ChangedFile struct {
	Path    string `json:"path"`
	ModTime string `json:"mod_time"`
	Size    int64  `json:"size"`
}

type IPStore struct {
	sync.RWMutex
	path string
	data map[string]string
}

type SyncStore struct {
	sync.RWMutex
	path    string
	targets []SyncTarget
}

var (
	ipStore   = &IPStore{data: make(map[string]string)}
	syncStore = &SyncStore{}

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

// ----------------- SYNC STORE -----------------

func initSyncStore() {
	exe, err := os.Executable()
	if err != nil {
		syncStore.path = "sync_targets.json"
	} else {
		syncStore.path = filepath.Join(filepath.Dir(exe), "sync_targets.json")
	}

	raw, err := os.ReadFile(syncStore.path)
	if err == nil {
		var list []SyncTarget
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			syncStore.targets = list
			return
		}
	}

	// Default targets auto-detected from C:\devs
	defaultExcludes := "node_modules, .git, vendor, .env, .next, dist, build, .venv, __pycache__, cache, tmp, .idea, .vscode, storage/framework, storage/logs"
	candidates := []struct {
		ID         string
		Name       string
		LocalPath  string
		VMName     string
		RemotePath string
	}{
		{"chunkly", "Chunkly", `C:\devs\chunklen\Chunkly`, "chunklen", "/home/ubuntu/Chunkly"},
		{"aicatalog", "AI Catalog", `C:\devs\aicatalog\aicatalog.lc`, "aicatalog", "/home/ubuntu/aicatalog.lc"},
		{"u0life", "u0life", `C:\devs\u0life\u0life`, "u0life-beta", "/home/ubuntu/u0life"},
		{"rx2go", "rx2go", `C:\devs\rx2go`, "rx2go", "/home/ubuntu/rx2go"},
	}

	for _, c := range candidates {
		if _, err := os.Stat(c.LocalPath); err == nil {
			syncStore.targets = append(syncStore.targets, SyncTarget{
				ID:         c.ID,
				Name:       c.Name,
				LocalPath:  c.LocalPath,
				VMName:     c.VMName,
				RemotePath: c.RemotePath,
				Excludes:   defaultExcludes,
			})
		}
	}
	_ = syncStore.saveToFile()
}

func (s *SyncStore) saveToFile() error {
	raw, err := json.MarshalIndent(s.targets, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0644)
}

func (s *SyncStore) All() []SyncTarget {
	s.RLock()
	defer s.RUnlock()
	copied := make([]SyncTarget, len(s.targets))
	copy(copied, s.targets)
	for i := range copied {
		inspectSyncTarget(&copied[i])
	}
	return copied
}

func (s *SyncStore) Get(id string) (SyncTarget, bool) {
	s.RLock()
	defer s.RUnlock()
	for _, t := range s.targets {
		if t.ID == id {
			inspected := t
			inspectSyncTarget(&inspected)
			return inspected, true
		}
	}
	return SyncTarget{}, false
}

func (s *SyncStore) Save(target SyncTarget) error {
	s.Lock()
	defer s.Unlock()

	if target.ID == "" {
		target.ID = fmt.Sprintf("tgt_%d", time.Now().UnixNano())
	}
	if target.Excludes == "" {
		target.Excludes = "node_modules, .git, vendor, .env, .next, dist, build, .venv, __pycache__, cache, tmp, .idea, .vscode, storage/framework, storage/logs"
	}

	found := false
	for i, t := range s.targets {
		if t.ID == target.ID {
			target.LastSyncedAt = t.LastSyncedAt
			target.LastSyncFiles = t.LastSyncFiles
			s.targets[i] = target
			found = true
			break
		}
	}
	if !found {
		s.targets = append(s.targets, target)
	}

	return s.saveToFile()
}

func (s *SyncStore) Delete(id string) error {
	s.Lock()
	defer s.Unlock()

	var updated []SyncTarget
	for _, t := range s.targets {
		if t.ID != id {
			updated = append(updated, t)
		}
	}
	s.targets = updated
	return s.saveToFile()
}

func (s *SyncStore) UpdateLastSync(id string, filesCount int) error {
	s.Lock()
	defer s.Unlock()

	for i, t := range s.targets {
		if t.ID == id {
			s.targets[i].LastSyncedAt = time.Now().Format("2006-01-02 15:04:05")
			s.targets[i].LastSyncFiles = filesCount
			return s.saveToFile()
		}
	}
	return nil
}

func parseSyncTime(str string) time.Time {
	if str == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, str); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", str); err == nil {
		return t
	}
	return time.Time{}
}

func shouldExclude(name, relPath string, isDir bool, cleanExcludes []string) bool {
	nameLower := strings.ToLower(name)
	relLower := strings.ToLower(filepath.ToSlash(relPath))

	for _, ex := range cleanExcludes {
		ex = strings.TrimSpace(strings.ToLower(ex))
		if ex == "" {
			continue
		}
		if nameLower == ex {
			return true
		}
		if isDir && (nameLower == ex || strings.HasPrefix(nameLower, ex)) {
			return true
		}
		if strings.Contains(ex, "/") {
			if strings.HasPrefix(relLower, ex) || strings.Contains(relLower, "/"+ex) {
				return true
			}
		}
		parts := strings.Split(relLower, "/")
		for _, p := range parts {
			if p == ex {
				return true
			}
		}
	}
	return false
}

func inspectSyncTarget(target *SyncTarget) {
	info, err := os.Stat(target.LocalPath)
	if err != nil || !info.IsDir() {
		target.LocalExists = false
		target.IsModified = false
		return
	}
	target.LocalExists = true

	lastSync := parseSyncTime(target.LastSyncedAt)
	excludeTokens := strings.Split(target.Excludes, ",")
	var cleanExcludes []string
	for _, ex := range excludeTokens {
		trimmed := strings.TrimSpace(strings.ToLower(ex))
		if trimmed != "" {
			cleanExcludes = append(cleanExcludes, trimmed)
		}
	}

	modifiedCount := 0
	var maxMod time.Time

	_ = filepath.WalkDir(target.LocalPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(target.LocalPath, path)
		if err != nil || rel == "." {
			return nil
		}
		isDir := d.IsDir()
		name := d.Name()
		if shouldExclude(name, rel, isDir, cleanExcludes) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if isDir {
			return nil
		}
		finfo, err := d.Info()
		if err != nil || !finfo.Mode().IsRegular() {
			return nil
		}

		mod := finfo.ModTime()
		if mod.After(maxMod) {
			maxMod = mod
		}

		if lastSync.IsZero() || mod.After(lastSync) {
			modifiedCount++
		}
		return nil
	})

	target.ModifiedCount = modifiedCount
	target.IsModified = (modifiedCount > 0)
	if !maxMod.IsZero() {
		target.LatestModTime = maxMod.Format("2006-01-02 15:04:05")
	}
}

func getChangedFiles(target SyncTarget) []ChangedFile {
	info, err := os.Stat(target.LocalPath)
	if err != nil || !info.IsDir() {
		return []ChangedFile{}
	}

	lastSync := parseSyncTime(target.LastSyncedAt)
	excludeTokens := strings.Split(target.Excludes, ",")
	var cleanExcludes []string
	for _, ex := range excludeTokens {
		trimmed := strings.TrimSpace(strings.ToLower(ex))
		if trimmed != "" {
			cleanExcludes = append(cleanExcludes, trimmed)
		}
	}

	var changed []ChangedFile
	_ = filepath.WalkDir(target.LocalPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(target.LocalPath, path)
		if err != nil || rel == "." {
			return nil
		}
		isDir := d.IsDir()
		name := d.Name()
		if shouldExclude(name, rel, isDir, cleanExcludes) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if isDir {
			return nil
		}

		finfo, err := d.Info()
		if err != nil || !finfo.Mode().IsRegular() {
			return nil
		}

		mod := finfo.ModTime()
		if lastSync.IsZero() || mod.After(lastSync) {
			changed = append(changed, ChangedFile{
				Path:    filepath.ToSlash(rel),
				ModTime: mod.Format("2006-01-02 15:04:05"),
				Size:    finfo.Size(),
			})
		}
		return nil
	})

	sort.Slice(changed, func(i, j int) bool {
		return changed[i].ModTime > changed[j].ModTime
	})

	return changed
}

func pushCodeToVM(target SyncTarget) (int, error) {
	info, err := os.Stat(target.LocalPath)
	if err != nil || !info.IsDir() {
		return 0, fmt.Errorf("local folder does not exist: %s", target.LocalPath)
	}

	excludeTokens := strings.Split(target.Excludes, ",")
	var cleanExcludes []string
	for _, ex := range excludeTokens {
		trimmed := strings.TrimSpace(strings.ToLower(ex))
		if trimmed != "" {
			cleanExcludes = append(cleanExcludes, trimmed)
		}
	}

	tmpTar, err := os.CreateTemp("", fmt.Sprintf("hv_sync_%s_*.tar.gz", target.ID))
	if err != nil {
		return 0, fmt.Errorf("failed creating temp archive: %w", err)
	}
	tmpTarPath := tmpTar.Name()
	defer os.Remove(tmpTarPath)

	gzWriter := gzip.NewWriter(tmpTar)
	tarWriter := tar.NewWriter(gzWriter)

	fileCount := 0

	err = filepath.WalkDir(target.LocalPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(target.LocalPath, path)
		if err != nil || rel == "." {
			return nil
		}
		isDir := d.IsDir()
		name := d.Name()
		if shouldExclude(name, rel, isDir, cleanExcludes) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		finfo, err := d.Info()
		if err != nil {
			return nil
		}
		if !isDir && !finfo.Mode().IsRegular() {
			return nil
		}

		header, err := tar.FileInfoHeader(finfo, finfo.Name())
		if err != nil {
			return nil
		}
		header.Name = filepath.ToSlash(rel)
		if isDir {
			header.Name += "/"
			header.Mode = 0755
		} else {
			header.Mode = 0644
		}

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !isDir {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			_, _ = io.Copy(tarWriter, f)
			f.Close()
			fileCount++
		}
		return nil
	})

	_ = tarWriter.Close()
	_ = gzWriter.Close()
	_ = tmpTar.Close()

	if err != nil {
		return 0, fmt.Errorf("archive creation error: %w", err)
	}

	if fileCount == 0 {
		return 0, fmt.Errorf("no files to sync after applying excludes")
	}

	remoteTar := fmt.Sprintf("/tmp/hv_sync_%s_%d.tar.gz", target.ID, time.Now().Unix())
	transferCmd := exec.Command("multipass", "transfer", tmpTarPath, fmt.Sprintf("%s:%s", target.VMName, remoteTar))
	transferCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := transferCmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("multipass transfer failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	remoteDest := target.RemotePath
	if remoteDest == "" {
		remoteDest = fmt.Sprintf("/home/ubuntu/%s", target.Name)
	}

	remoteScript := fmt.Sprintf(`
set -e
TARGET_DIR="%s"
ARCHIVE="%s"
POST_CMD="%s"

if [ -w "$TARGET_DIR" ] || [ ! -e "$TARGET_DIR" ]; then
    mkdir -p "$TARGET_DIR"
    tar -xzf "$ARCHIVE" -C "$TARGET_DIR"
else
    sudo mkdir -p "$TARGET_DIR"
    sudo tar -xzf "$ARCHIVE" -C "$TARGET_DIR"
fi
rm -f "$ARCHIVE"

if [ -n "$POST_CMD" ]; then
    cd "$TARGET_DIR"
    eval "$POST_CMD"
fi
`, remoteDest, remoteTar, target.PostSyncCmd)

	b64 := base64.StdEncoding.EncodeToString([]byte(remoteScript))
	execCmd := exec.Command("multipass", "exec", target.VMName, "--", "bash", "-c", fmt.Sprintf("echo %s | base64 -d | bash", b64))
	execCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err = execCmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("multipass remote unpack failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	_ = syncStore.UpdateLastSync(target.ID, fileCount)
	return fileCount, nil
}

// ----------------- BACKUP ENGINE -----------------

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

func loadBackupSettings() BackupSettings {
	dir := getBackupsDir()
	path := filepath.Join(filepath.Dir(dir), "backup_settings.json")
	var s BackupSettings
	raw, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal(raw, &s)
	}
	if s.IntervalHours <= 0 {
		s.IntervalHours = 4
	}
	if s.RetentionCount <= 0 {
		s.RetentionCount = 10
	}
	if s.TargetType == "" {
		s.TargetType = "project"
	}
	return s
}

func syncWindowsScheduledTask(st BackupSettings) {
	exe, err := os.Executable()
	if err != nil {
		return
	}

	taskName := "HVMonitor_AutoBackup"

	if !st.Enabled {
		cmd := exec.Command("schtasks", "/delete", "/tn", taskName, "/f")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
		return
	}

	var sc, mo string
	if st.IntervalHours >= 24 {
		sc = "daily"
		mo = "1"
	} else {
		sc = "hourly"
		mo = fmt.Sprintf("%d", st.IntervalHours)
		if st.IntervalHours <= 0 {
			mo = "4"
		}
	}

	taskRun := fmt.Sprintf("\"%s\" -backup-now", exe)
	cmd := exec.Command("schtasks", "/create", "/tn", taskName,
		"/tr", taskRun,
		"/sc", sc,
		"/mo", mo,
		"/rl", "HIGHEST",
		"/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

func runHeadlessBackup() {
	st := loadBackupSettings()
	if !st.Enabled {
		return
	}

	backupsDir := getBackupsDir()
	logFile := filepath.Join(backupsDir, "cron.log")

	appendLog := func(msg string) {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			timestamp := time.Now().Format("2006-01-02 15:04:05")
			fmt.Fprintf(f, "[%s] %s\n", timestamp, msg)
			f.Close()
		}
	}

	appendLog("=== Starting scheduled headless backup ===")
	vms, err := getVMs()
	if err != nil {
		appendLog(fmt.Sprintf("ERROR: failed to get VMs: %v", err))
		return
	}

	successCount := 0
	for _, vm := range vms {
		if vm.State == "Running" {
			appendLog(fmt.Sprintf("Backing up VM: %s", vm.Name))
			path, err := backupProjectAndDB(vm.Name)
			if err != nil {
				appendLog(fmt.Sprintf("ERROR backing up %s: %v", vm.Name, err))
			} else {
				appendLog(fmt.Sprintf("SUCCESS backing up %s -> %s", vm.Name, filepath.Base(path)))
				successCount++
			}
		}
	}

	enforceRetention(st.RetentionCount)
	appendLog(fmt.Sprintf("Completed: %d VMs backed up. Retention enforced (keep %d).", successCount, st.RetentionCount))
}

func saveBackupSettings(s BackupSettings) error {
	dir := getBackupsDir()
	path := filepath.Join(filepath.Dir(dir), "backup_settings.json")
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	err = os.WriteFile(path, raw, 0644)
	if err == nil {
		syncWindowsScheduledTask(s)
	}
	return err
}

func enforceRetention(retentionCount int) {
	if retentionCount <= 0 {
		return
	}
	backups, err := listBackups()
	if err != nil {
		return
	}

	type key struct {
		vm    string
		btype string
	}
	groups := make(map[key][]BackupItem)
	for _, b := range backups {
		k := key{vm: b.VMName, btype: b.Type}
		groups[k] = append(groups[k], b)
	}

	for _, list := range groups {
		sort.Slice(list, func(i, j int) bool {
			return list[i].CreatedAt > list[j].CreatedAt
		})
		if len(list) > retentionCount {
			for _, item := range list[retentionCount:] {
				_ = os.RemoveAll(item.Path)
			}
		}
	}
}

func startBackupScheduler() {
	go func() {
		settings := loadBackupSettings()
		enforceRetention(settings.RetentionCount)

		ticker := time.NewTicker(15 * time.Minute)
		var lastRun time.Time

		for range ticker.C {
			st := loadBackupSettings()
			if !st.Enabled {
				continue
			}

			interval := time.Duration(st.IntervalHours) * time.Hour
			if time.Since(lastRun) >= interval {
				lastRun = time.Now()
				vms, err := getVMs()
				if err == nil {
					for _, vm := range vms {
						if vm.State == "Running" {
							jobID := fmt.Sprintf("auto_%s_%d", vm.Name, time.Now().Unix())
							setJob(jobID, vm.Name, "project", "running", "Auto scheduled project backup...")
							_, err := backupProjectAndDB(vm.Name)
							if err != nil {
								setJob(jobID, vm.Name, "project", "error", err.Error())
							} else {
								setJob(jobID, vm.Name, "project", "completed", "Auto backup completed")
							}
						}
					}
					enforceRetention(st.RetentionCount)
				}
			}
		}
	}()
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
	shell32                                     = syscall.NewLazyDLL("shell32.dll")
	isUserAnAdmin                               = shell32.NewProc("IsUserAnAdmin")
	shellExecuteW                               = shell32.NewProc("ShellExecuteW")
	procSetCurrentProcessExplicitAppUserModelID = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
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

	var argsPtr *uint16
	if len(os.Args) > 1 {
		argsPtr, _ = syscall.UTF16PtrFromString(strings.Join(os.Args[1:], " "))
	}

	ret, _, _ := shellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(exePtr)), uintptr(unsafe.Pointer(argsPtr)), uintptr(unsafe.Pointer(dirPtr)), 1)
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

func restoreProjectBackup(vmName, localTarPath string) error {
	remoteTar := fmt.Sprintf("/tmp/restore_%d.tar.gz", time.Now().Unix())
	transferCmd := exec.Command("multipass", "transfer", localTarPath, fmt.Sprintf("%s:%s", vmName, remoteTar))
	transferCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := transferCmd.Run(); err != nil {
		return fmt.Errorf("failed to transfer backup archive to VM: %w", err)
	}

	script := fmt.Sprintf(`
set -e
STAGE_DIR="/tmp/hv_restore_%d"
mkdir -p "$STAGE_DIR"
tar -xzf "%s" -C "$STAGE_DIR"

# 1. Restore Database
if [ -f "$STAGE_DIR/database/mysql_all_databases.sql" ]; then
    MYSQL_CONTAINER=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -iE 'mysql|mariadb|db' | head -n 1 || true)
    if [ -n "$MYSQL_CONTAINER" ]; then
        PASS=$(docker inspect "$MYSQL_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null | grep -iE '^MYSQL_ROOT_PASSWORD=' | head -n 1 | cut -d= -f2 || true)
        if [ -z "$PASS" ]; then
            PASS=$(docker inspect "$MYSQL_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null | grep -iE '^MYSQL_PASSWORD=' | head -n 1 | cut -d= -f2 || true)
        fi
        USER="root"
        if [ -n "$PASS" ]; then
            docker exec -i "$MYSQL_CONTAINER" mysql -u "$USER" -p"$PASS" < "$STAGE_DIR/database/mysql_all_databases.sql" 2>/dev/null || true
        else
            docker exec -i "$MYSQL_CONTAINER" mysql < "$STAGE_DIR/database/mysql_all_databases.sql" 2>/dev/null || true
        fi
    fi
elif [ -f "$STAGE_DIR/database/postgres_all_databases.sql" ]; then
    POSTGRES_CONTAINER=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -iE 'postgres|psql' | head -n 1 || true)
    if [ -n "$POSTGRES_CONTAINER" ]; then
        docker exec -i "$POSTGRES_CONTAINER" psql -U postgres < "$STAGE_DIR/database/postgres_all_databases.sql" 2>/dev/null || true
    fi
fi

# 2. Restore Project Source Code
if [ -f "$STAGE_DIR/project/source_code.tar.gz" ]; then
    PROJECT_TARGET=""
    if [ -f "$STAGE_DIR/backup_info.txt" ]; then
        PROJECT_TARGET=$(grep -iE '^project_source:' "$STAGE_DIR/backup_info.txt" | cut -d: -f2- | xargs || true)
    fi
    if [ -z "$PROJECT_TARGET" ]; then
        PROJECT_TARGET="/home/ubuntu"
    fi
    PARENT_DIR=$(dirname "$PROJECT_TARGET")
    tar -xzf "$STAGE_DIR/project/source_code.tar.gz" -C "$PARENT_DIR"
fi

rm -rf "$STAGE_DIR" "%s"
`, time.Now().Unix(), remoteTar, remoteTar)

	b64 := base64.StdEncoding.EncodeToString([]byte(script))
	cmd := exec.Command("multipass", "exec", vmName, "--", "bash", "-c", fmt.Sprintf("echo %s | base64 -d | bash", b64))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restore script failed inside VM: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	return nil
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

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})

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

func initAppUserModelID() {
	appId, _ := syscall.UTF16PtrFromString("Sergio.HVMonitor")
	procSetCurrentProcessExplicitAppUserModelID.Call(uintptr(unsafe.Pointer(appId)))
}

func ensureShortcutAppId() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	lnkPath := filepath.Join(home, "Desktop", "Hyper-V Monitor.lnk")
	if _, err := os.Stat(lnkPath); os.IsNotExist(err) {
		return
	}

	helperPath := filepath.Join(os.TempDir(), "HVShortcutHelper.exe")
	if len(shortcutHelperExe) > 0 {
		_ = os.WriteFile(helperPath, shortcutHelperExe, 0755)
		cmd := exec.Command(helperPath, lnkPath, "Sergio.HVMonitor")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
	}
}

func checkTaskSchedulerInstalled() bool {
	cmd := exec.Command("schtasks.exe", "/query", "/tn", "HVMonitor_AutoBackup")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run() == nil
}

func launchAppWindow(url string) {
	time.Sleep(150 * time.Millisecond)

	ensureShortcutAppId()

	browser := findAppBrowser()
	if browser != "" {
		userData := filepath.Join(os.Getenv("LOCALAPPDATA"), "HVMonitorProfile")
		_ = os.MkdirAll(userData, 0755)

		appArg := fmt.Sprintf("--app=%s", url)
		sizeArg := "--window-size=1280,880"
		profileArg := fmt.Sprintf("--user-data-dir=%s", userData)
		appIdArg := "--app-id=Sergio.HVMonitor"
		aumidArg := "--app-user-model-id=Sergio.HVMonitor"

		cmd := exec.Command(browser,
			appArg,
			sizeArg,
			profileArg,
			appIdArg,
			aumidArg,
			"--no-first-run",
			"--no-default-browser-check",
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Start(); err == nil {
			return
		}
	}

	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func main() {
	initAppUserModelID()

	for _, arg := range os.Args[1:] {
		if arg == "-backup-now" || arg == "--backup-now" {
			ensureElevated()
			initIPStore()
			runHeadlessBackup()
			return
		}
	}

	ensureElevated()
	initIPStore()
	initSyncStore()
	getBackupsDir()
	startBackupScheduler()

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

	// System / User Information API
	http.HandleFunc("/api/system/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		hostname, _ := os.Hostname()
		user := os.Getenv("USERNAME")
		if user == "" {
			user = "Administrator"
		}

		info := map[string]interface{}{
			"username":         user,
			"hostname":         hostname,
			"os":               runtime.GOOS + "/" + runtime.GOARCH,
			"is_elevated":      isElevated(),
			"version":          "1.4.0",
			"task_scheduler":   checkTaskSchedulerInstalled(),
			"backups_path":     getBackupsDir(),
			"sync_targets_num": len(syncStore.All()),
		}
		json.NewEncoder(w).Encode(info)
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

	// Backups list & jobs
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

	// Trigger Project & DB backup
	http.HandleFunc("/api/backup/project", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing VM name", 400)
			return
		}

		jobID := fmt.Sprintf("proj_%s_%d", name, time.Now().Unix())
		setJob(jobID, name, "project", "running", "Dumping database & packaging project code...")

		go func() {
			path, err := backupProjectAndDB(name)
			if err != nil {
				setJob(jobID, name, "project", "error", err.Error())
			} else {
				setJob(jobID, name, "project", "completed", fmt.Sprintf("Saved to %s", filepath.Base(path)))
				st := loadBackupSettings()
				enforceRetention(st.RetentionCount)
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	// Trigger Full VM Export
	http.HandleFunc("/api/backup/vm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing VM name", 400)
			return
		}

		jobID := fmt.Sprintf("vm_%s_%d", name, time.Now().Unix())
		setJob(jobID, name, "vm", "running", "Exporting complete Hyper-V VM image...")

		go func() {
			path, err := backupFullVM(name)
			if err != nil {
				setJob(jobID, name, "vm", "error", err.Error())
			} else {
				setJob(jobID, name, "vm", "completed", fmt.Sprintf("Exported to %s", filepath.Base(path)))
				st := loadBackupSettings()
				enforceRetention(st.RetentionCount)
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	// Deploy / Restore Project & DB into VM
	http.HandleFunc("/api/backup/restore", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		vm := r.URL.Query().Get("vm")
		path := r.URL.Query().Get("path")

		if vm == "" || path == "" {
			http.Error(w, "missing vm or path", 400)
			return
		}

		jobID := fmt.Sprintf("restore_%s_%d", vm, time.Now().Unix())
		setJob(jobID, vm, "restore", "running", "Deploying and restoring database & files...")

		go func() {
			err := restoreProjectBackup(vm, path)
			if err != nil {
				setJob(jobID, vm, "restore", "error", err.Error())
			} else {
				setJob(jobID, vm, "restore", "completed", "Database and project code restored successfully!")
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	// Backup schedule settings
	http.HandleFunc("/api/backup/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var st BackupSettings
			if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := saveBackupSettings(st); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			enforceRetention(st.RetentionCount)
			json.NewEncoder(w).Encode(st)
			return
		}
		json.NewEncoder(w).Encode(loadBackupSettings())
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

	// ----------------- CODE SYNC APIS -----------------

	http.HandleFunc("/api/sync/targets", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		targets := syncStore.All()
		json.NewEncoder(w).Encode(targets)
	})

	http.HandleFunc("/api/sync/save", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var target SyncTarget
		if err := json.NewDecoder(r.Body).Decode(&target); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := syncStore.Save(target); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/sync/delete", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		if err := syncStore.Delete(id); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/sync/changed-files", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Query().Get("id")
		target, ok := syncStore.Get(id)
		if !ok {
			http.Error(w, "target not found", 404)
			return
		}
		files := getChangedFiles(target)
		json.NewEncoder(w).Encode(files)
	})

	http.HandleFunc("/api/sync/push", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Query().Get("id")
		target, ok := syncStore.Get(id)
		if !ok {
			http.Error(w, "target not found", 404)
			return
		}

		jobID := fmt.Sprintf("sync_%s_%d", target.ID, time.Now().Unix())
		setJob(jobID, target.VMName, "sync", "running", fmt.Sprintf("Syncing %s code to %s:%s...", target.Name, target.VMName, target.RemotePath))

		go func() {
			count, err := pushCodeToVM(target)
			if err != nil {
				setJob(jobID, target.VMName, "sync", "error", err.Error())
			} else {
				setJob(jobID, target.VMName, "sync", "completed", fmt.Sprintf("Synced %d files to %s:%s", count, target.VMName, target.RemotePath))
			}
		}()

		json.NewEncoder(w).Encode(map[string]string{
			"job_id": jobID,
			"status": "started",
		})
	})

	http.HandleFunc("/api/sync/push-all", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		targets := syncStore.All()
		var modifiedTargets []SyncTarget
		for _, t := range targets {
			if t.IsModified && t.LocalExists {
				modifiedTargets = append(modifiedTargets, t)
			}
		}

		if len(modifiedTargets) == 0 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "ok",
				"message": "No modified projects found",
				"count":   0,
			})
			return
		}

		jobID := fmt.Sprintf("sync_all_%d", time.Now().Unix())
		setJob(jobID, "All VMs", "sync", "running", fmt.Sprintf("Syncing %d modified projects...", len(modifiedTargets)))

		go func() {
			successCount := 0
			for _, t := range modifiedTargets {
				_, err := pushCodeToVM(t)
				if err == nil {
					successCount++
				}
			}
			setJob(jobID, "All VMs", "sync", "completed", fmt.Sprintf("Batch sync completed: %d/%d projects updated", successCount, len(modifiedTargets)))
		}()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"job_id": jobID,
			"status": "started",
			"count":  len(modifiedTargets),
		})
	})

	http.HandleFunc("/api/sync/open-folder", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		target, ok := syncStore.Get(id)
		if !ok || target.LocalPath == "" {
			http.Error(w, "target not found", 404)
			return
		}
		exec.Command("explorer.exe", target.LocalPath).Start()
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
