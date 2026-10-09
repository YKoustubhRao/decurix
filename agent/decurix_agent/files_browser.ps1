# A worker's file browser: the folders and files of its own home, with Up and Open. A private desktop has no Explorer (it is
# the shell, and none runs there), so this is what the worker's "Files" icon starts. It never leaves the worker's home folder.
param([Parameter(Mandatory = $true)][string]$Root, [string]$StartPath)

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()

$Root = [IO.Path]::GetFullPath($Root).TrimEnd('\')
$script:Here = $Root
$script:HistoryBack = New-Object 'System.Collections.Generic.Stack[string]'
$script:HistoryForward = New-Object 'System.Collections.Generic.Stack[string]'
$script:FolderDirty = $false

$form = New-Object System.Windows.Forms.Form
$form.Text = 'Files'
$form.StartPosition = 'CenterScreen'
$form.Width = 820
$form.Height = 520

$up = New-Object System.Windows.Forms.Button
$up.Text = 'Up'
$up.SetBounds(8, 8, 60, 26)

$path = New-Object System.Windows.Forms.TextBox
$path.SetBounds(212, 9, 584, 24)
$path.Anchor = 'Top,Left,Right'

$list = New-Object System.Windows.Forms.ListView
$list.View = 'Details'
$list.FullRowSelect = $true
$list.MultiSelect = $false
$list.HideSelection = $false
$list.SetBounds(8, 42, 788, 430)
$list.Anchor = 'Top,Bottom,Left,Right'
[void]$list.Columns.Add('Name', 360)
[void]$list.Columns.Add('Type', 110)
[void]$list.Columns.Add('Size', 90)
[void]$list.Columns.Add('Modified', 160)

$back = New-Object System.Windows.Forms.Button
$back.Text = 'Back'
$back.SetBounds(8, 8, 60, 26)
$forward = New-Object System.Windows.Forms.Button
$forward.Text = 'Forward'
$forward.SetBounds(76, 8, 60, 26)
$up.SetBounds(144, 8, 60, 26)
$tips = New-Object System.Windows.Forms.ToolTip
$tips.SetToolTip($back, 'Back (Alt+Left)')
$tips.SetToolTip($forward, 'Forward (Alt+Right)')
$tips.SetToolTip($up, 'Up (Alt+Up)')
$tips.SetToolTip($path, 'Enter a Work:\ path, or press Ctrl+L to edit')

# Deliver filesystem events on the form's UI thread. Idle Files does no
# directory scans; a short timer combines bursts of writes into one refresh.
$watcher = New-Object System.IO.FileSystemWatcher
$watcher.SynchronizingObject = $form
$watcher.NotifyFilter = 'FileName,DirectoryName,LastWrite,Size'
$watcher.Add_Changed({ $script:FolderDirty = $true })
$watcher.Add_Created({ $script:FolderDirty = $true })
$watcher.Add_Deleted({ $script:FolderDirty = $true })
$watcher.Add_Renamed({ $script:FolderDirty = $true })
$watcher.Add_Error({ $script:FolderDirty = $true })

$images = New-Object System.Windows.Forms.ImageList
$images.ImageSize = New-Object System.Drawing.Size(16, 16)
try { $images.Images.Add('dir', [System.Drawing.Icon]::ExtractAssociatedIcon($env:windir)) } catch { }
try { $images.Images.Add('file', [System.Drawing.Icon]::ExtractAssociatedIcon((Join-Path $env:windir 'notepad.exe'))) } catch { }
$list.SmallImageList = $images

function Format-Size([long]$n) {
    if ($n -ge 1GB) { return ('{0:N1} GB' -f ($n / 1GB)) }
    if ($n -ge 1MB) { return ('{0:N1} MB' -f ($n / 1MB)) }
    if ($n -ge 1KB) { return ('{0:N0} KB' -f ($n / 1KB)) }
    return "$n B"
}

function Show-Folder([string]$dir, [bool]$remember = $true) {
    $full = [IO.Path]::GetFullPath($dir).TrimEnd('\')
    # Never above the worker's home: that is the person's own computer.
    if ($full -ne $Root -and -not $full.StartsWith($Root + '\', [StringComparison]::OrdinalIgnoreCase)) { $full = $Root }
    if (-not (Test-Path -LiteralPath $full -PathType Container)) { $full = $Root }
    $old = $script:Here
    if ($remember -and $full -ne $old) { $script:HistoryBack.Push($old); $script:HistoryForward.Clear() }
    $script:Here = $full

    $selected = if ($old -eq $full -and $list.SelectedItems.Count) { $list.SelectedItems[0].Text } else { $null }
    $list.BeginUpdate()
    $list.Items.Clear()
    try {
        $entries = Get-ChildItem -LiteralPath $full -Force -ErrorAction Stop | Where-Object { $_.Name -ne '.decurix' -or $full -ne $Root }
        foreach ($e in @($entries | Where-Object { $_.PSIsContainer } | Sort-Object Name) + @($entries | Where-Object { -not $_.PSIsContainer } | Sort-Object Name)) {
            $isDir = $e.PSIsContainer
            $item = New-Object System.Windows.Forms.ListViewItem($e.Name)
            $item.ImageKey = if ($isDir) { 'dir' } else { 'file' }
            $item.Tag = @{ Path = $e.FullName; Dir = $isDir }
            [void]$item.SubItems.Add($(if ($isDir) { 'Folder' } else { ($e.Extension.TrimStart('.').ToUpper() + ' file').Trim() }))
            [void]$item.SubItems.Add($(if ($isDir) { '' } else { Format-Size $e.Length }))
            [void]$item.SubItems.Add($e.LastWriteTime.ToString('g'))
            [void]$list.Items.Add($item)
            if ($e.Name -eq $selected) { $item.Selected = $true }
        }
    } catch { }
    $list.EndUpdate()

    $rel = $full.Substring($Root.Length).TrimStart('\')
    $path.Text = if ($rel -eq 'work') { 'Work:\' } elseif ($rel.StartsWith('work\')) { 'Work:\' + $rel.Substring(5) } elseif ($rel) { "Home\$rel" } else { 'Home' }
    $up.Enabled = ($full -ne $Root)
    $back.Enabled = ($script:HistoryBack.Count -gt 0)
    $forward.Enabled = ($script:HistoryForward.Count -gt 0)
    $form.Text = "Files - $($path.Text)"
    if ($watcher.Path -ne $full) { $watcher.Path = $full; $watcher.EnableRaisingEvents = $true }
    $script:FolderDirty = $false
}

function Go-Back {
    if ($script:HistoryBack.Count) { $script:HistoryForward.Push($script:Here); Show-Folder ($script:HistoryBack.Pop()) $false }
}
function Go-Forward {
    if ($script:HistoryForward.Count) { $script:HistoryBack.Push($script:Here); Show-Folder ($script:HistoryForward.Pop()) $false }
}

$back.Add_Click({ Go-Back })
$forward.Add_Click({ Go-Forward })
$path.Add_KeyDown({
    if ($_.KeyCode -ne 'Return') { return }
    $_.SuppressKeyPress = $true
    $entered = $path.Text.Trim()
    if ($entered -match '^Work:(?:[\\/]|$)') { $entered = Join-Path (Join-Path $Root 'work') $entered.Substring(5).TrimStart('\', '/') }
    elseif ($entered -match '^Home(?:[\\/]|$)') { $entered = Join-Path $Root $entered.Substring(4).TrimStart('\', '/') }
    elseif (-not [IO.Path]::IsPathRooted($entered)) { $entered = Join-Path $script:Here $entered }
    try {
        $full = [IO.Path]::GetFullPath($entered).TrimEnd('\')
        if (($full -ne $Root -and -not $full.StartsWith($Root + '\', [StringComparison]::OrdinalIgnoreCase)) -or -not (Test-Path -LiteralPath $full -PathType Container)) {
            throw 'Choose a folder inside this worker Home or Work drive.'
        }
        Show-Folder $full
        $list.Focus()
    } catch { [void][System.Windows.Forms.MessageBox]::Show($form, $_.Exception.Message, 'Folder unavailable') }
})

function Open-Selected {
    if ($list.SelectedItems.Count -eq 0) { return }
    $tag = $list.SelectedItems[0].Tag
    if ($tag.Dir) { Show-Folder $tag.Path }
    else { try { Start-Process -FilePath $tag.Path } catch { } }
}

$up.Add_Click({ Show-Folder (Split-Path -Parent $script:Here) })
$list.Add_DoubleClick({ Open-Selected })
$list.Add_KeyDown({
    if ($_.KeyCode -eq 'Return') { Open-Selected; $_.Handled = $true }
    elseif ($_.KeyCode -eq 'Back') { if ($script:Here -ne $Root) { Show-Folder (Split-Path -Parent $script:Here) }; $_.Handled = $true }
})

$form.Controls.AddRange(@($back, $forward, $up, $path, $list))
$form.KeyPreview = $true
$form.Add_KeyDown({
    if ($_.Control -and $_.KeyCode -eq 'L') { $path.Focus(); $path.SelectAll(); $_.SuppressKeyPress = $true }
    elseif ($_.Alt -and $_.KeyCode -eq 'Left') { Go-Back; $_.SuppressKeyPress = $true }
    elseif ($_.Alt -and $_.KeyCode -eq 'Right') { Go-Forward; $_.SuppressKeyPress = $true }
    elseif ($_.Alt -and $_.KeyCode -eq 'Up') { if ($script:Here -ne $Root) { Show-Folder (Split-Path -Parent $script:Here) }; $_.SuppressKeyPress = $true }
})
$refresh = New-Object System.Windows.Forms.Timer
$refresh.Interval = 250
$refresh.Add_Tick({
    if ($script:FolderDirty) { Show-Folder $script:Here $false }
})
$list.Add_KeyDown({ if ($_.KeyCode -eq 'F5') { Show-Folder $script:Here; $_.Handled = $true } })
Show-Folder $(if ($StartPath) { $StartPath } else { $Root })
$refresh.Start()
$form.Add_FormClosed({ $refresh.Stop(); $refresh.Dispose(); $watcher.Dispose(); $tips.Dispose() })
[void]$form.ShowDialog()
