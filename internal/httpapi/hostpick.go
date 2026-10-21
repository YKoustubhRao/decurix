package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// errNoHostPicker says this control plane cannot show a file chooser: it runs
// in a container, or the system's chooser program is not installed.
var errNoHostPicker = errors.New("no file chooser on this computer")

// hostPickTimeout is how long a person may browse before the chooser is closed.
const hostPickTimeout = 15 * time.Minute

// pickHostPaths shows the system's own file chooser and returns what was
// picked (nothing when it was cancelled). A variable so tests never open one.
var pickHostPaths func(ctx context.Context, folder, either bool) ([]string, error) = pickHostPathsOS

// hostPickOpen keeps a second chooser from opening over the first.
var hostPickOpen sync.Mutex

// browseHostPathHandler opens this computer's own file chooser - Explorer's on
// Windows, Finder's on a Mac, the desktop's on Linux - and answers with what
// the person picked and where each will be put in the new desktop.
// POST /api/host-paths/browse?type=folder|file (primary listener, people only)
func (s *Server) browseHostPathHandler(w http.ResponseWriter, r *http.Request) {
	if !requireMethodJSON(w, r, http.MethodPost) {
		return
	}

	kind := strings.ToLower(strings.TrimSpace(r.FormValue("type")))
	folder := kind == "folder"
	either := kind == "" || kind == "any"

	if !hostPickOpen.TryLock() {
		jsonResponse(w, map[string]any{"ok": false, "busy": true})
		return
	}
	defer hostPickOpen.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), hostPickTimeout)
	defer cancel()

	picked, err := pickHostPaths(ctx, folder, either)
	if errors.Is(err, errNoHostPicker) {
		jsonResponse(w, map[string]any{"ok": false, "unavailable": true})
		return
	}
	if err != nil {
		log.Printf("browseHostPath: %v", err)
		jsonStatusError(w, http.StatusInternalServerError, "could not open the file chooser")
		return
	}

	paths := make([]map[string]any, 0, len(picked))
	for _, p := range picked {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		paths = append(paths, map[string]any{
			"path":   p,
			"name":   fi.Name(),
			"is_dir": fi.IsDir(),
			"dest":   desktop.ComputeDefaultVMDest(p),
		})
	}

	jsonResponse(w, map[string]any{"ok": true, "paths": paths})
}

// pickHostPathsOS runs the chooser of the system the control plane is on.
func pickHostPathsOS(ctx context.Context, folder, either bool) ([]string, error) {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return nil, errNoHostPicker
	}

	title := "Choose files for the new desktop"
	if either {
		title = "Choose files, or open a folder and press Open to take it"
	}
	if folder {
		title = "Choose a folder for the new desktop"
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encodedCommand(windowsPickCommand(title, folder, either)))
	case "darwin":
		kind := "file"
		if folder {
			kind = "folder"
		}
		cmd = exec.CommandContext(ctx, "osascript", "-e", `set picked to choose `+kind+` with prompt "`+title+`" with multiple selections allowed
set out to ""
repeat with p in picked
	set out to out & (POSIX path of p) & linefeed
end repeat
return out`)
	default:
		args := []string{"--file-selection", "--multiple", "--separator=\n", "--title=" + title}
		if folder {
			args = append(args, "--directory")
		}
		cmd = exec.CommandContext(ctx, "zenity", args...)
	}

	if _, err := exec.LookPath(cmd.Path); err != nil {
		return nil, errNoHostPicker
	}
	hidePickerConsole(cmd)

	out, err := cmd.Output()
	if err != nil {
		// Cancel is not a failure: osascript reports it as error -128 and
		// zenity as exit status 1.
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil && runtime.GOOS != "windows" {
			if runtime.GOOS != "darwin" || strings.Contains(string(exit.Stderr), "-128") {
				return nil, nil
			}
		}
		return nil, err
	}

	var picked []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			picked = append(picked, filepath.Clean(line))
		}
	}
	return picked, nil
}

// windowsPickCommand is windowsPickScript with the variables it reads set: $title, $folder (a folder only) and $any
// (files or a folder in one window).
func windowsPickCommand(title string, folder, either bool) string {
	psBool := map[bool]string{true: "$true", false: "$false"}
	return "$title = '" + title + "'\n$folder = " + psBool[folder] + "\n$any = " + psBool[either && !folder] + "\n" + windowsPickScript
}

// windowsPickScript shows Explorer's own open dialog (IFileOpenDialog - the
// .NET Framework's folder dialog is the old tree, not Explorer) over a topmost
// owner, so it opens in front of the dashboard, and prints one path a line as
// UTF-8 bytes: there may be no console to set an encoding on.
const windowsPickScript = `$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;

public static class DecurixPick {
    [ComImport, Guid("DC1C5A9C-E88A-4DDE-A5A1-60F82A20AEF7")]
    class FileOpenDialog {}

    [ComImport, Guid("43826D1E-E718-42EE-BC55-A1E261C37BFE"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    interface IShellItem {
        void BindToHandler(IntPtr pbc, ref Guid bhid, ref Guid riid, out IntPtr ppv);
        void GetParent(out IShellItem ppsi);
        [PreserveSig] int GetDisplayName(uint sigdnName, [MarshalAs(UnmanagedType.LPWStr)] out string ppszName);
        void GetAttributes(uint sfgaoMask, out uint psfgaoAttribs);
        void Compare(IShellItem psi, uint hint, out int piOrder);
    }

    [ComImport, Guid("B63EA76D-1F85-456F-A19C-48159EFA858B"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    interface IShellItemArray {
        void BindToHandler(IntPtr pbc, ref Guid bhid, ref Guid riid, out IntPtr ppvOut);
        void GetPropertyStore(int flags, ref Guid riid, out IntPtr ppv);
        void GetPropertyDescriptionList(IntPtr keyType, ref Guid riid, out IntPtr ppv);
        void GetAttributes(int attribFlags, uint sfgaoMask, out uint psfgaoAttribs);
        void GetCount(out uint pdwNumItems);
        void GetItemAt(uint dwIndex, out IShellItem ppsi);
        void EnumItems(out IntPtr ppenumShellItems);
    }

    [ComImport, Guid("D57C7288-D4AD-4768-BE02-9D969532D960"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    interface IFileOpenDialog {
        [PreserveSig] int Show(IntPtr parent);
        void SetFileTypes(uint cFileTypes, IntPtr rgFilterSpec);
        void SetFileTypeIndex(uint iFileType);
        void GetFileTypeIndex(out uint piFileType);
        void Advise(IntPtr pfde, out uint pdwCookie);
        void Unadvise(uint dwCookie);
        void SetOptions(uint fos);
        void GetOptions(out uint pfos);
        void SetDefaultFolder(IShellItem psi);
        void SetFolder(IShellItem psi);
        void GetFolder(out IShellItem ppsi);
        void GetCurrentSelection(out IShellItem ppsi);
        void SetFileName([MarshalAs(UnmanagedType.LPWStr)] string pszName);
        void GetFileName([MarshalAs(UnmanagedType.LPWStr)] out string pszName);
        void SetTitle([MarshalAs(UnmanagedType.LPWStr)] string pszTitle);
        void SetOkButtonLabel([MarshalAs(UnmanagedType.LPWStr)] string pszText);
        void SetFileNameLabel([MarshalAs(UnmanagedType.LPWStr)] string pszLabel);
        void GetResult(out IShellItem ppsi);
        void AddPlace(IShellItem psi, int fdap);
        void SetDefaultExtension([MarshalAs(UnmanagedType.LPWStr)] string pszDefaultExtension);
        void Close(int hr);
        void SetClientGuid(ref Guid guid);
        void ClearClientData();
        void SetFilter(IntPtr pFilter);
        void GetResults(out IShellItemArray ppenum);
        void GetSelectedItems(out IShellItemArray ppsai);
    }

    const uint PickFolders = 0x20, ForceFileSystem = 0x40, MultiSelect = 0x200, PathMustExist = 0x800, FileMustExist = 0x1000;
    const uint FileSystemPath = 0x80058000;

    public static string[] Show(IntPtr owner, bool folder, string title) {
        IFileOpenDialog dialog = (IFileOpenDialog)new FileOpenDialog();
        uint options;
        dialog.GetOptions(out options);
        options |= ForceFileSystem | MultiSelect | PathMustExist | (folder ? PickFolders : FileMustExist);
        dialog.SetOptions(options);
        dialog.SetTitle(title);

        List<string> picked = new List<string>();
        if (dialog.Show(owner) != 0) return picked.ToArray();

        IShellItemArray items;
        dialog.GetResults(out items);
        uint count;
        items.GetCount(out count);
        for (uint i = 0; i < count; i++) {
            IShellItem item;
            items.GetItemAt(i, out item);
            string path;
            if (item.GetDisplayName(FileSystemPath, out path) == 0 && !String.IsNullOrEmpty(path)) picked.Add(path);
        }
        return picked.ToArray();
    }
}
'@
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
$owner.ShowInTaskbar = $false
$owner.FormBorderStyle = 'None'
$owner.StartPosition = 'CenterScreen'
$owner.Width = 1
$owner.Height = 1
$owner.Opacity = 0
$owner.Show()
$owner.Activate()
if ($any) {
    # Files and folders in one window: the classic dialog, which accepts a folder opened with the placeholder name left in the box.
    $placeholder = 'Select files or a folder'
    $dlg = New-Object System.Windows.Forms.OpenFileDialog
    $dlg.Title = $title
    $dlg.Multiselect = $true
    $dlg.CheckFileExists = $false
    $dlg.CheckPathExists = $true
    $dlg.ValidateNames = $false
    $dlg.DereferenceLinks = $false
    $dlg.FileName = $placeholder
    $picked = @()
    if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) {
        foreach ($f in $dlg.FileNames) {
            if ((Split-Path -Leaf $f) -eq $placeholder -and -not (Test-Path -LiteralPath $f)) { $picked += (Split-Path -Parent $f) }
            elseif (Test-Path -LiteralPath $f) { $picked += $f }
        }
    }
} else {
    $picked = [DecurixPick]::Show($owner.Handle, $folder, $title)
}
$owner.Close()
$bytes = [System.Text.Encoding]::UTF8.GetBytes(($picked -join [char]10))
$stdout = [Console]::OpenStandardOutput()
$stdout.Write($bytes, 0, $bytes.Length)
$stdout.Flush()
`
