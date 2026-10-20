Add-Type -AssemblyName System.Drawing

function Add-RoundRect($path, [float]$x, [float]$y, [float]$w, [float]$h, [float]$r) {
    $d = $r * 2
    $path.AddArc($x, $y, $d, $d, 180, 90)
    $path.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $path.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $path.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $path.CloseFigure()
}

$size = 256
$bmp = New-Object System.Drawing.Bitmap($size, $size)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
$g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
$g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic

# Background squircle
$path = New-Object System.Drawing.Drawing2D.GraphicsPath
Add-RoundRect $path 10 10 236 236 50

# Fill dark gradient
$bgBrush = New-Object System.Drawing.Drawing2D.LinearGradientBrush(
    [System.Drawing.Point]::new(0, 0),
    [System.Drawing.Point]::new($size, $size),
    [System.Drawing.Color]::FromArgb(255, 15, 23, 42),
    [System.Drawing.Color]::FromArgb(255, 30, 41, 59)
)
$g.FillPath($bgBrush, $path)

# Subtle Blue Border
$pen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(180, 59, 130, 246), 6)
$g.DrawPath($pen, $path)

# 3 Rack server units
$accentBrush = New-Object System.Drawing.Drawing2D.LinearGradientBrush(
    [System.Drawing.Point]::new(50, 50),
    [System.Drawing.Point]::new(206, 206),
    [System.Drawing.Color]::FromArgb(255, 14, 165, 233),
    [System.Drawing.Color]::FromArgb(255, 99, 102, 241)
)

foreach ($y in @(56, 104, 152)) {
    $sp = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundRect $sp 50 $y 156 36 10
    $g.FillPath($accentBrush, $sp)
    $g.FillEllipse([System.Drawing.Brushes]::White, 68, ($y + 13), 10, 10)
    $g.FillEllipse([System.Drawing.Brushes]::Aqua, 86, ($y + 13), 10, 10)
    $linePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(140, 255, 255, 255), 2)
    $g.DrawLine($linePen, 115, ($y + 18), 185, ($y + 18))
}

# Glowing Live Status Dot in bottom right
$glow = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(90, 16, 185, 129))
$pulse = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 16, 185, 129))
$g.FillEllipse($glow, 172, 172, 44, 44)
$g.FillEllipse($pulse, 180, 180, 28, 28)
$g.FillEllipse([System.Drawing.Brushes]::White, 186, 186, 8, 8)

$bmp.Save("C:\tools\hv-monitor\icon.png", [System.Drawing.Imaging.ImageFormat]::Png)

$hIcon = $bmp.GetHicon()
$icon = [System.Drawing.Icon]::FromHandle($hIcon)
$icoStream = [System.IO.File]::OpenWrite("C:\tools\hv-monitor\icon.ico")
$icon.Save($icoStream)
$icoStream.Close()

$g.Dispose()
$bmp.Dispose()
Write-Output "Done creating icon"
