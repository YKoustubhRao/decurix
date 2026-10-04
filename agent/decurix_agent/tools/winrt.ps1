<#
.SYNOPSIS
    What Windows itself can do with documents, for a native Windows worker.
.DESCRIPTION
    A Linux desktop reads scans with tesseract, pictures PDF pages with poppler
    and makes PDFs with LibreOffice - programs its image installs. A native
    worker runs on a person's own computer, where none of them is installed
    and nothing may be: so the same jobs are done with what every Windows 10
    and 11 already has - its OCR engine (Windows.Media.Ocr), its PDF renderer
    (Windows.Data.Pdf) and GDI+ - and, for turning an Office document into a
    PDF, with Microsoft Office when the person has it.

    Run by decurix_agent/tools/winoffice.py, in Windows PowerShell 5.1 (the one
    every Windows has; it can call these WinRT APIs with no SDK). One operation
    per run; the answer is one line of JSON on stdout, {"ok": true, ...} or
    {"ok": false, "error": "..."}.

        winrt.ps1 ocr      <image>
        winrt.ps1 pdfpages <pdf> <first> <last> <out dir> <width px>
        winrt.ps1 tojpeg   <image> <out.jpg>
        winrt.ps1 topdf    <office document> <out.pdf>
#>
param(
    [Parameter(Mandatory = $true, Position = 0)][string]$Op,
    [Parameter(Position = 1, ValueFromRemainingArguments = $true)][string[]]$Rest
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)

function Reply($data) {
    [Console]::Out.WriteLine(($data | ConvertTo-Json -Compress -Depth 6))
}

# WinRT's asynchronous calls, awaited: PowerShell 5.1 has no await, so each is
# turned into a .NET task through the extension method the framework has for it.
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$asTaskMethods = [System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object { $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 }
$asTaskOfT = $asTaskMethods | Where-Object { $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1' } | Select-Object -First 1
$asTaskPlain = $asTaskMethods | Where-Object { $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncAction' } | Select-Object -First 1

function Await($operation, [Type]$resultType) {
    $task = $asTaskOfT.MakeGenericMethod($resultType).Invoke($null, @($operation))
    $task.Wait() | Out-Null
    $task.Result
}

function AwaitAction($action) {
    $task = $asTaskPlain.Invoke($null, @($action))
    $task.Wait() | Out-Null
}

function Get-File([string]$path) {
    [void][Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime]
    Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync((Resolve-Path -LiteralPath $path).ProviderPath)) ([Windows.Storage.StorageFile])
}

# ocr: the lines of text Windows reads off a picture, each with its box in the
# picture's own pixels (the replay ladder places clicks by them).
function Invoke-Ocr([string]$image) {
    [void][Windows.Media.Ocr.OcrEngine, Windows.Foundation, ContentType = WindowsRuntime]
    [void][Windows.Graphics.Imaging.BitmapDecoder, Windows.Foundation, ContentType = WindowsRuntime]
    [void][Windows.Storage.Streams.IRandomAccessStream, Windows.Storage.Streams, ContentType = WindowsRuntime]

    $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
    if ($null -eq $engine) { throw 'Windows has no text-recognition language installed for this user' }

    $file = Get-File $image
    $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
    try {
        $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])

        # The engine refuses a picture larger than its limit on either side: such a one is read scaled down, and
        # the boxes scaled back up.
        $limit = [Windows.Media.Ocr.OcrEngine]::MaxImageDimension
        $scale = 1.0
        $largest = [Math]::Max($decoder.PixelWidth, $decoder.PixelHeight)
        if ($largest -gt $limit) { $scale = $limit / $largest }

        if ($scale -lt 1.0) {
            $transform = New-Object Windows.Graphics.Imaging.BitmapTransform
            $transform.ScaledWidth = [uint32][Math]::Floor($decoder.PixelWidth * $scale)
            $transform.ScaledHeight = [uint32][Math]::Floor($decoder.PixelHeight * $scale)
            $bitmap = Await ($decoder.GetSoftwareBitmapAsync(
                    [Windows.Graphics.Imaging.BitmapPixelFormat]::Bgra8, [Windows.Graphics.Imaging.BitmapAlphaMode]::Premultiplied, $transform,
                    [Windows.Graphics.Imaging.ExifOrientationMode]::RespectExifOrientation, [Windows.Graphics.Imaging.ColorManagementMode]::DoNotColorManage
                )) ([Windows.Graphics.Imaging.SoftwareBitmap])
        } else {
            $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
        }

        $result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
    } finally {
        $stream.Dispose()
    }

    $lines = @()
    foreach ($line in $result.Lines) {
        $l = [double]::MaxValue; $t = [double]::MaxValue; $r = 0.0; $b = 0.0
        foreach ($word in $line.Words) {
            $box = $word.BoundingRect
            $l = [Math]::Min($l, $box.X); $t = [Math]::Min($t, $box.Y)
            $r = [Math]::Max($r, $box.X + $box.Width); $b = [Math]::Max($b, $box.Y + $box.Height)
        }
        if ($line.Words.Count -eq 0) { continue }
        $lines += @{ text = $line.Text; box = @([int]($l / $scale), [int]($t / $scale), [int](($r - $l) / $scale), [int](($b - $t) / $scale)) }
    }

    Reply @{ ok = $true; language = $engine.RecognizerLanguage.LanguageTag; lines = @($lines) }
}

# pdfpages: pages of a PDF as PNG files, page-001.png ..., each this wide.
function Export-PdfPages([string]$pdf, [int]$first, [int]$last, [string]$outDir, [int]$width) {
    [void][Windows.Data.Pdf.PdfDocument, Windows.Data.Pdf, ContentType = WindowsRuntime]
    [void][Windows.Storage.Streams.InMemoryRandomAccessStream, Windows.Storage.Streams, ContentType = WindowsRuntime]

    $document = Await ([Windows.Data.Pdf.PdfDocument]::LoadFromFileAsync((Get-File $pdf))) ([Windows.Data.Pdf.PdfDocument])
    $count = [int]$document.PageCount
    $first = [Math]::Max(1, $first); $last = [Math]::Min($count, $last)
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null

    $files = @()
    for ($n = $first; $n -le $last; $n++) {
        $page = $document.GetPage([uint32]($n - 1))
        try {
            $options = New-Object Windows.Data.Pdf.PdfPageRenderOptions
            $options.DestinationWidth = [uint32]$width
            $stream = New-Object Windows.Storage.Streams.InMemoryRandomAccessStream
            AwaitAction ($page.RenderToStreamAsync($stream, $options))

            $target = Join-Path $outDir ('page-{0:d3}.png' -f $n)
            $reader = [System.IO.WindowsRuntimeStreamExtensions]::AsStreamForRead($stream.GetInputStreamAt(0))
            $out = [System.IO.File]::Create($target)
            try { $reader.CopyTo($out) } finally { $out.Dispose(); $reader.Dispose(); $stream.Dispose() }
            $files += $target
        } finally {
            $page.Dispose()
        }
    }

    Reply @{ ok = $true; pages = $count; files = @($files) }
}

# tojpeg: any picture Windows can open, as a JPEG on a white ground (a PDF takes a JPEG as it is).
function Convert-ToJpeg([string]$image, [string]$target) {
    Add-Type -AssemblyName System.Drawing
    $source = [System.Drawing.Image]::FromFile((Resolve-Path -LiteralPath $image).ProviderPath)
    try {
        $flat = New-Object System.Drawing.Bitmap($source.Width, $source.Height, [System.Drawing.Imaging.PixelFormat]::Format24bppRgb)
        try {
            $g = [System.Drawing.Graphics]::FromImage($flat)
            try { $g.Clear([System.Drawing.Color]::White); $g.DrawImage($source, 0, 0, $source.Width, $source.Height) } finally { $g.Dispose() }
            $codec = [System.Drawing.Imaging.ImageCodecInfo]::GetImageEncoders() | Where-Object { $_.MimeType -eq 'image/jpeg' }
            $quality = New-Object System.Drawing.Imaging.EncoderParameters(1)
            $quality.Param[0] = New-Object System.Drawing.Imaging.EncoderParameter([System.Drawing.Imaging.Encoder]::Quality, [long]88)
            $flat.Save($target, $codec, $quality)
            Reply @{ ok = $true; width = $flat.Width; height = $flat.Height }
        } finally { $flat.Dispose() }
    } finally { $source.Dispose() }
}

# topdf: a Word, Excel or PowerPoint document as a PDF, laid out as that program prints it. Through Microsoft Office,
# started unseen and closed again; without it there is nothing on a Windows computer that can lay one out.
function Convert-ToPdf([string]$document, [string]$target) {
    $source = (Resolve-Path -LiteralPath $document).ProviderPath
    $ext = [System.IO.Path]::GetExtension($source).ToLowerInvariant()

    # The program is quit afterwards only when it had nothing open before: asking for PowerPoint hands back the one
    # the person is already working in (it runs as a single program), and quitting that would close their work.
    # Word and Excel start a program of their own for each asker, but are held to the same rule.
    switch -Regex ($ext) {
        '^\.(docx?|rtf|odt)$' {
            $word = New-Object -ComObject Word.Application
            $ours = $word.Documents.Count -eq 0
            try {
                if ($ours) { $word.Visible = $false; $word.DisplayAlerts = 0 }
                $doc = $word.Documents.Open($source, $false, $true)   # no conversion prompt, read-only
                try { $doc.ExportAsFixedFormat($target, 17) } finally { $doc.Close($false) }
            } finally { if ($ours) { $word.Quit() }; [void][Runtime.InteropServices.Marshal]::ReleaseComObject($word) }
        }
        '^\.(xlsx?|xlsm|ods)$' {
            $excel = New-Object -ComObject Excel.Application
            $ours = $excel.Workbooks.Count -eq 0
            try {
                if ($ours) { $excel.Visible = $false; $excel.DisplayAlerts = $false }
                $book = $excel.Workbooks.Open($source, 0, $true)       # do not update links, read-only
                try { $book.ExportAsFixedFormat(0, $target) } finally { $book.Close($false) }
            } finally { if ($ours) { $excel.Quit() }; [void][Runtime.InteropServices.Marshal]::ReleaseComObject($excel) }
        }
        '^\.(pptx?|odp)$' {
            $powerpoint = New-Object -ComObject PowerPoint.Application
            $ours = $powerpoint.Presentations.Count -eq 0
            try {
                $deck = $powerpoint.Presentations.Open($source, -1, 0, 0)  # read-only, untitled no, no window
                try { $deck.SaveAs($target, 32) } finally { $deck.Close() }
            } finally { if ($ours) { $powerpoint.Quit() }; [void][Runtime.InteropServices.Marshal]::ReleaseComObject($powerpoint) }
        }
        default { throw "cannot make a PDF of a $ext file" }
    }

    if (-not (Test-Path -LiteralPath $target)) { throw 'the program did not write the PDF' }
    Reply @{ ok = $true }
}

try {
    switch ($Op) {
        'ocr' { Invoke-Ocr $Rest[0] }
        'pdfpages' { Export-PdfPages $Rest[0] ([int]$Rest[1]) ([int]$Rest[2]) $Rest[3] ([int]$Rest[4]) }
        'tojpeg' { Convert-ToJpeg $Rest[0] $Rest[1] }
        'topdf' { Convert-ToPdf $Rest[0] $Rest[1] }
        default { throw "unknown operation $Op" }
    }
} catch {
    $message = $_.Exception.Message
    if ($_.Exception.InnerException) { $message = $_.Exception.InnerException.Message }
    Reply @{ ok = $false; error = $message }
    exit 1
}
