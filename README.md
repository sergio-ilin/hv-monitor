# Hyper-V Monitor & Static IP Manager

<p align="center">
  <img src="icon.png" width="100" height="100" alt="Hyper-V Monitor Icon" style="border-radius: 20px;">
</p>

<p align="center">
  <strong>A modern, lightweight Windows desktop manager and static IP recovery tool for Hyper-V and Multipass virtual machines written in pure Go.</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Language-Go%201.21+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/Platform-Windows%2010%20%2F%2011-0078D6?style=flat-square&logo=windows&logoColor=white" alt="Windows">
  <img src="https://img.shields.io/badge/Hypervisor-Hyper--V-0078D7?style=flat-square" alt="Hyper-V">
  <img src="https://img.shields.io/badge/License-MIT-green?style=flat-square" alt="License">
</p>

---

## ⚡ The Problem It Solves

When running Linux development virtual machines on Windows via **Hyper-V** or **Multipass**:
1. **Unstable IPs:** The default Hyper-V virtual switch resets its internal DHCP subnet after reboots or sleep, breaking access to your local development sites and services.
2. **Missing GUI:** Hyper-V's default MMC interface is dated and awkward to use, while Multipass daemons (`multipassd`) are prone to crashes and socket disconnects.
3. **Gateway Drops:** Custom static IP configurations on `vEthernet (Default Switch)` (e.g. `10.10.10.1`) regularly disappear on host restarts.

**Hyper-V Monitor** solves this by providing a standalone, high-performance desktop interface with automatic static IP persistence and instant one-click recovery.

---

## ✨ Features

- **📑 5-Tab Minimalist Workspace Interface:**
  - **📊 Analytics:** Pure telemetry dashboard with cluster-wide CPU/RAM metrics and per-machine Canvas sparkline charts (no action buttons, no IP clutter).
  - **🖥️ Machines:** Minimalist power management view with status badges, filters (`All`, `Running`, `Stopped`), and one-click power controls (`Restart`, `Stop`, `Start`) without distracting charts or IP clutter.
  - **🌐 IP Management:** Dedicated network table for managing, assigning, and persisting static IPv4 addresses (`10.10.10.X`) with host gateway auto-repair.
  - **📦 Backups & Recovery:** Full-page backup repository with automated scheduling, retention policies, and one-click deployment.
  - **🚀 Code Sync & Hot-Deploy:** Real-time Windows host folder watcher and 1-click code synchronizer to push modified files directly into Linux VMs without touching container dependencies, `.env`, or volumes.
- **🎨 Minimalist Typography-First Design & Theme Switcher:**
  - Modern, readable developer typography stack featuring **Geist Sans**, **Geist Mono**, and **JetBrains Mono**.
  - High-contrast, clean visual surfaces with zero unnecessary gradients or bloated margins.
  - Dedicated **User Profile & Environment** section (`👤`) independent of tabs.
  - Seamless theme toggle: **Light**, **Dark**, and **System** (auto-synchronizing with Windows dark/light OS mode).
- **📌 Native Windows Taskbar Integration:**
  - Explicit COM **AppUserModelID** (`Sergio.HVMonitor`) association for both process and shortcut.
  - Pinning the app to the Windows Taskbar preserves the clean custom icon and shortcut target without defaulting to browser icons.
- **🚀 Code Sync & Hot-Deploy Engine:**
  - **⚡ Automatic Change Detection:** Recursively inspects project directories on the Windows host and detects modified files compared to the last sync timestamp in milliseconds.
  - **🛡️ Intelligent Exclusions:** Preserves Linux guest environment by automatically filtering out `node_modules`, `vendor`, `.git`, `.env`, `storage/framework`, `storage/logs`, cache, and temporary build files.
  - **🔍 Modified Files Inspector:** View exact lists of changed files, their sizes, and edit timestamps before triggering a sync.
  - **⚡ Post-Sync Commands:** Automatically trigger arbitrary commands inside the VM after sync (e.g. `docker compose restart web`, `npm run build`, etc.).
  - **📦 1-Click Push & Batch Push:** Sync an individual project or click **`Push All Modified`** to deploy all pending changes across all VMs.
- **📦 Dual Backup & Deploy Engine:**
  - **⚡ Project & DB Smart Backup:** Automatically discovers project code, dumps MySQL/PostgreSQL databases to SQL, strips `node_modules` and git cache, and transfers a lean archive (`~20MB`) to Windows in ~2 seconds.
  - **🚀 One-Click Deploy / Restore:** Select any historical project backup and click **`Deploy / Restore`** to unpack project files and import the database SQL dump back into the VM automatically.
  - **🖥️ Full Hyper-V VM Export:** Full disaster recovery using native `Export-VM` (complete VHDX virtual disk image + hardware configuration).
  - **⏱️ Automated Scheduler & Retention:** Integrated with native **Windows Task Scheduler** to execute silent background backups every 1h, 4h, 8h, or 24h even when the GUI window is closed (consumes 0% RAM between runs).
  - **📜 Background Run Logging:** All automated headless backup events, databases dumped, and retention purges are logged to `backups/cron.log`.
- **⚡ One-Click Network Recovery:** 
  - Restores the host gateway (`10.10.10.1`) on `vEthernet (Default Switch)` if dropped.
  - Dedicated **`⚡ Restore All IPs`** button to re-inject static routes into all running Linux instances simultaneously without rebooting.
- **🖥️ Standalone Desktop Experience:** Runs as a dedicated native desktop window (no browser tabs, navigation bars, or browser chrome).
- **🔒 Zero External CGO Dependencies:** Built entirely with standard Go libraries and native Windows APIs (`shell32.dll`, Windows manifests, embedded HTML/CSS/JS assets).
- **⚡ UAC & Hyper-V Privileges:** Automatically verifies and requests necessary administrative permissions on startup.

---

## 🚀 Getting Started

### Prerequisites
- Windows 10 / 11 (Pro, Enterprise, or Education with Hyper-V enabled)
- Go 1.21+ (only if building from source)
- Multipass (optional, supported for Ubuntu cloud instances)

### Building from Source

1. Clone the repository:
   ```powershell
   git clone https://github.com/itunlimit/hv-monitor.git
   cd hv-monitor
   ```

2. (Optional) Rebuild the Windows resource file containing the icon and elevation manifest:
   ```powershell
   go install github.com/akavel/rsrc@latest
   rsrc -manifest app.manifest -ico icon.ico -o rsrc.syso
   ```

3. Compile the standalone executable:
   ```powershell
   go build -ldflags "-s -w -H windowsgui" -o hv-monitor.exe
   ```

4. Run `hv-monitor.exe` directly or pin it to your Taskbar / Desktop.

---

## 🛠️ Architecture

- **Backend:** Go HTTP daemon (`net/http`) listening on local loopback `127.0.0.1:18288`.
- **Hyper-V Bridge:** Native PowerShell and WMI integration querying `Get-VM`, `Get-VMNetworkAdapter`, and `Get-NetNeighbor`.
- **Frontend:** Glassmorphism dark-themed dashboard embedded into the binary using Go's `//go:embed` directive.
- **App Launcher:** Launches Microsoft Edge in standalone Application Mode (`--app`), automatically terminating background processes when the window is closed.
- **Persistence:** Local JSON registry (`ip_config.json`) tracking assigned static IPs across machine lifecycles.

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).
