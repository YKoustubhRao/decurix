# A worker's file browser: the folders and files of its own home, with Up and Open. A private desktop has no Explorer (it is
# the shell, and none runs there), so this is what the worker's "Files" icon starts. It never leaves the worker's home folder.
param([Parameter(Mandatory = $true)][string]$Root)

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()

$Root = [IO.Path]::GetFullPath($Root).TrimEnd('\')
$script:Here = $Root

$form = New-Object System.Windows.Forms.Form
$form.Text = 'Files'
$form.StartPosition = 'CenterScreen'
$form.Width = 820
$form.Height = 520

$up = New-Object System.Windows.Forms.Button
$up.Text = 'Up'
$up.SetBounds(8, 8, 60, 26)

$path = New-Object System.Windows.Forms.TextBox
$path.ReadOnly = $true
$path.SetBounds(76, 9, 720, 24)
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

function Show-Folder([string]$dir) {
    $full = [IO.Path]::GetFullPath($dir).TrimEnd('\')
    # Never above the worker's home: that is the person's own computer.
    if ($full -ne $Root -and -not $full.StartsWith($Root + '\', [StringComparison]::OrdinalIgnoreCase)) { $full = $Root }
    $script:Here = $full

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
        }
    } catch { }
    $list.EndUpdate()

    $rel = $full.Substring($Root.Length).TrimStart('\')
    $path.Text = if ($rel) { "Home\$rel" } else { 'Home' }
    $up.Enabled = ($full -ne $Root)
    $form.Text = "Files - $($path.Text)"
}

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

$form.Controls.AddRange(@($up, $path, $list))
Show-Folder $Root
[void]$form.ShowDialog()
