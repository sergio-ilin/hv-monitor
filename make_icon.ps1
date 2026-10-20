Add-Type -AssemblyName System.Drawing

function Add-RoundRect($path, [float]$x, [float]$y, [float]$w, [float]$h, [float]$r) {
    $d = $r * 2
    $path.AddArc($x, $y, $d, $d, 180, 90)
    $path.AddArc($x + $w - $d, $y, $d, $d, 270, 90)
    $path.AddArc($x + $w - $d, $y + $h - $d, $d, $d, 0, 90)
    $path.AddArc($x, $y + $h - $d, $d, $d, 90, 90)
    $path.CloseFigure()
}

function Create-AppBitmap([int]$size) {
    $scale = $size / 256.0
    $bmp = New-Object System.Drawing.Bitmap($size, $size)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    $g.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
    $g.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
    $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic

    # Pure minimalist dark background squircle
    $path = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundRect $path (12 * $scale) (12 * $scale) (232 * $scale) (232 * $scale) (54 * $scale)
    
    $bgBrush = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 13, 17, 23))
    $g.FillPath($bgBrush, $path)

    $borderPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 39, 46, 57), [Math]::Max(1.0, 3.5 * $scale))
    $g.DrawPath($borderPen, $path)

    # Minimalist Hypervisor: 2 clean stacked virtualization nodes + hypervisor base
    # Node 1 (Top Left):
    $nodePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 240, 246, 252), [Math]::Max(1.5, 7.0 * $scale))
    $nodePen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $nodePen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $nodePen.LineJoin = [System.Drawing.Drawing2D.LineJoin]::Round

    # Two VM blocks on top of a hypervisor layer
    # Top Left VM
    $vm1 = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundRect $vm1 (52 * $scale) (52 * $scale) (68 * $scale) (64 * $scale) (14 * $scale)
    $fillBrush1 = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 22, 27, 34))
    $g.FillPath($fillBrush1, $vm1)
    $g.DrawPath($nodePen, $vm1)
    
    # Active indicator on VM 1 (crisp emerald dot)
    $dotBrush1 = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 34, 197, 94))
    $g.FillEllipse($dotBrush1, [float](78 * $scale), [float](76 * $scale), [float](16 * $scale), [float](16 * $scale))

    # Top Right VM
    $vm2 = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundRect $vm2 (136 * $scale) (52 * $scale) (68 * $scale) (64 * $scale) (14 * $scale)
    $g.FillPath($fillBrush1, $vm2)
    $g.DrawPath($nodePen, $vm2)

    # Active indicator on VM 2 (crisp blue dot)
    $dotBrush2 = New-Object System.Drawing.SolidBrush([System.Drawing.Color]::FromArgb(255, 59, 130, 246))
    $g.FillEllipse($dotBrush2, [float](162 * $scale), [float](76 * $scale), [float](16 * $scale), [float](16 * $scale))

    # Hypervisor Foundation Layer (Bottom Wide Blade)
    $hv = New-Object System.Drawing.Drawing2D.GraphicsPath
    Add-RoundRect $hv (52 * $scale) (138 * $scale) (152 * $scale) (64 * $scale) (14 * $scale)
    $g.FillPath($fillBrush1, $hv)
    $g.DrawPath($nodePen, $hv)

    # Hypervisor activity bar - clean minimal dashes
    $accentPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(255, 59, 130, 246), [Math]::Max(1.0, 5.0 * $scale))
    $accentPen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $accentPen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $g.DrawLine($accentPen, [float](78 * $scale), [float](170 * $scale), [float](118 * $scale), [float](170 * $scale))

    $whitePen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(180, 240, 246, 252), [Math]::Max(1.0, 5.0 * $scale))
    $whitePen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
    $whitePen.EndCap = [System.Drawing.Drawing2D.LineCap]::Round
    $g.DrawLine($whitePen, [float](132 * $scale), [float](170 * $scale), [float](178 * $scale), [float](170 * $scale))

    # Linking connection lines between hypervisor and VMs (minimal vertical pins)
    $linkPen = New-Object System.Drawing.Pen([System.Drawing.Color]::FromArgb(160, 59, 130, 246), [Math]::Max(1.0, 3.5 * $scale))
    $g.DrawLine($linkPen, [float](86 * $scale), [float](116 * $scale), [float](86 * $scale), [float](138 * $scale))
    $g.DrawLine($linkPen, [float](170 * $scale), [float](116 * $scale), [float](170 * $scale), [float](138 * $scale))

    $g.Dispose()
    return $bmp
}

$bmp256 = Create-AppBitmap 256
$bmp256.Save("C:\tools\hv-monitor\icon.png", [System.Drawing.Imaging.ImageFormat]::Png)

# Save multi-size ICO
$hIcon = $bmp256.GetHicon()
$icon = [System.Drawing.Icon]::FromHandle($hIcon)
$icoStream = [System.IO.File]::OpenWrite("C:\tools\hv-monitor\icon.ico")
$icon.Save($icoStream)
$icoStream.Close()
$bmp256.Dispose()

Write-Output "Minimalist icon generated successfully!"
