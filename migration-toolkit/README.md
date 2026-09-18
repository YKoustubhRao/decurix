# VM Desktop & Session Migration Toolkit (Zero-Duplicate Memory)

This toolkit allows you to migrate your local desktop configurations, developer toolchains, Google tokens/sessions, and project workspaces to a remote Virtual Machine (VM) while ensuring zero redundant memory and storage footprint.

---

## Architecture & How Zero Duplicate Memory is Achieved

1. **Native Dynamic Library Sharing (OS Page Cache)**:
   - Instead of copying raw `/usr` binaries across machines (which causes library mismatches and duplicates dynamic libraries in memory), we export an exact package manifest (`apt-mark showmanual`, `cargo`, `npm`, `uv`).
   - The VM installs these packages cleanly using APT. The Linux dynamic linker (`ld.so`) maps all shared `.so` libraries (such as `libc`, `libssl`, `libcrypto`, X11) into shared physical RAM pages (`MAP_SHARED`). Multiple running processes share the exact same physical memory pages.
2. **Cache & Transient State Stripping**:
   - Google Chrome profiles are transferred **without** the multi-gigabyte `Cache/`, `Code Cache/`, and `GPUCache/` directories.
   - All login sessions, cookies, localStorage, extensions, and preferences are preserved intact, while avoiding loading gigabytes of stale cache into Linux page cache.
3. **Strict Anti-Recursion & Self-Exclusion**:
   - The current application directory (`flowera`) and `migration-toolkit` are explicitly excluded from replication.
   - Prevents recursive nested copies, redundant duplicates, and circular transfers.
4. **Linux Kernel Samepage Merging (KSM)**:
   - Configures the kernel's KSM engine (`/sys/kernel/mm/ksm/run = 1`).
   - The kernel background thread periodically scans anonymous memory pages across processes and merges identical memory pages into a single copy-on-write page.
   - A persistent systemd unit (`ksm-memory-dedup.service`) ensures deduplication runs continuously.

---

## Toolkit Structure

| File | Purpose | Location Run |
| :--- | :--- | :--- |
| **`migrate.sh`** | Interactive end-to-end migration wizard | Local machine |
| **`interactive_discover.sh`** | Step-by-step discovery & one-by-one prompt (Default Empty VM) | Local machine |
| **`choice_migration.sh`** | Granular item-by-item selection table | Local machine |
| **`setup_host_bridge.sh`** | Sets up Universal Host Bridge (macOS / Windows / Linux) | Local machine |
| **`export_manifest.sh`** | Generates system packages (APT & Brew) and credential manifests | Local machine |
| **`transfer_to_vm.sh`** | Selective, modular rsync synchronization over SSH | Local machine |
| **`setup_vm_memory_dedup.sh`**| Installs native apps, activates KSM, sets up Keyring | Remote VM (sudo) |
| **`check_memory_savings.sh`** | Reports real-time RAM savings and KSM merged pages | Remote VM |

---

## Universal Host Remote Bridge (macOS / Windows / Linux)

Instead of copying bloated executables across machines, the host bridge enables the Linux VM to execute and control apps directly on your local host with **zero duplicate memory**:

1. **Activate Host OpenSSH Server (once on your local machine)**:
   - **macOS**: `bash ~/flowera/migration-toolkit/host-bridge/enable_mac_remote_login.sh`
   - **Windows**: `powershell -ExecutionPolicy Bypass -File ~/flowera/migration-toolkit/host-bridge/enable_windows_ssh_server.ps1`
   - **Linux**: `bash ~/flowera/migration-toolkit/host-bridge/enable_linux_sshd.sh`

2. **Connect from Host to Linux VM with the Bridge Tunnel**:
   ```bash
   ~/flowera/migration-toolkit/host-bridge/connect-to-vm-with-bridge.sh user@<linux-vm-ip>
   ```

3. **Use Host apps, clipboard & files from the Linux VM**:
   ```bash
   # Run host apps:
   host-run pbcopy < token.txt      # macOS
   host-run powershell.exe "dir"    # Windows
   host-open https://github.com     # Opens on host screen (Mac/Win/Linux)
   cat secret.txt | host-clip       # Copies to host clipboard

   # Mount host filesystem on Linux VM with zero storage duplication:
   host-mount
   ls /mnt/host
   ```

---

## Usage Instructions

### Quick Start (Granular Choice-by-Choice Wizard)
Run the wizard from your local terminal:
```bash
cd ~/flowera/migration-toolkit
./choice_migration.sh
```
Or run `./migrate.sh` and select Mode 1.

You can pick individual items:
- **Apps & Sessions**: Google Chrome, Firefox, GitHub CLI, Docker, Azure CLI, OpenCode / VS Code, SSH keys, Google Cloud SDK, GNOME Keyring.
- **Toolchains**: Rust/Cargo, Bun, Go, Python/UV, .NET, 103 system APT packages.
- **Shell & Dotfiles**: Fish, Bash, and Zsh dotfiles (`.bashrc`, `.profile`, `.zshrc`, `fish/`).
- **Files & Directories**: Choice-by-choice selection of any custom file or directory (with automatic anti-recursion protection).

### Manual / Non-Interactive Usage

#### 1. Preview changes first (Dry-Run):
```bash
./transfer_to_vm.sh --host user@<vm-ip> --dry-run
```

#### 2. Run Selective Transfer:
```bash
# Transfer only Google sessions and Developer tokens:
./transfer_to_vm.sh --host user@<vm-ip> --modules google,auth

# Transfer everything:
./transfer_to_vm.sh --host user@<vm-ip> --modules all
```

#### 3. Run VM Setup & Deduplication:
SSH into your VM and run:
```bash
ssh user@<vm-ip>
sudo bash ~/vm-setup/setup_vm_memory_dedup.sh
```

#### 4. Check Deduplication & Memory Savings:
Inspect real-time memory savings anytime on the VM:
```bash
~/vm-setup/check_memory_savings.sh
```
