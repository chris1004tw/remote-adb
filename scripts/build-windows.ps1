param(
    [ValidateSet("amd64", "arm64")]
    [string]$GoArch = "amd64",
    [string]$Output = "radb.exe",
    [string]$Target = "./cmd/radb",
    [string]$Ldflags = "",
    [switch]$ForceKillRunning
)

$ErrorActionPreference = "Stop"
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

function Get-GoMinorVersion {
    $versionText = go version
    if ($versionText -match "go version go1\.(\d+)") {
        return [int]$Matches[1]
    }
    throw "failed to parse Go version from: $versionText"
}

function Resolve-RepoPath([string]$Path) {
    if ([System.IO.Path]::IsPathRooted($Path)) {
        return [System.IO.Path]::GetFullPath($Path)
    }
    return [System.IO.Path]::GetFullPath((Join-Path $repoRoot $Path))
}

function Get-TargetProcesses([string]$ExecutablePath) {
    $resolvedExe = [System.IO.Path]::GetFullPath($ExecutablePath)
    $processName = [System.IO.Path]::GetFileNameWithoutExtension($resolvedExe)
    $matches = @()

    foreach ($proc in (Get-Process -Name $processName -ErrorAction SilentlyContinue)) {
        try {
            if ($proc.Path -and [System.IO.Path]::GetFullPath($proc.Path) -eq $resolvedExe) {
                $matches += $proc
            }
        } catch {
            continue
        }
    }

    if ($matches.Count -eq 0) {
        $candidates = Get-CimInstance Win32_Process -Filter "Name = '$processName.exe'" -ErrorAction SilentlyContinue
        foreach ($proc in $candidates) {
            if ($proc.ExecutablePath -and [System.IO.Path]::GetFullPath($proc.ExecutablePath) -eq $resolvedExe) {
                $matches += $proc
            }
        }
    }

    return @($matches | Sort-Object Id -Unique)
}

function Stop-TargetProcesses([string]$ExecutablePath) {
    $targets = @(Get-TargetProcesses $ExecutablePath)
    if ($targets.Count -eq 0) {
        return
    }

    $pids = @($targets | ForEach-Object { $_.Id })
    Write-Host "Stopping running target process(es): $($pids -join ', ')"

    foreach ($proc in $targets) {
        try {
            if ($proc -is [System.Diagnostics.Process]) {
                Stop-Process -Id $proc.Id -Force -ErrorAction Stop
            } else {
                Invoke-CimMethod -InputObject $proc -MethodName Terminate | Out-Null
            }
        } catch {
            throw "failed to stop running process $($proc.Id) for '$ExecutablePath': $($_.Exception.Message)"
        }
    }

    Start-Sleep -Milliseconds 300
}

$originalGoos = $env:GOOS
$originalGoarch = $env:GOARCH
$originalGoexperiment = $env:GOEXPERIMENT
$originalLocation = Get-Location

try {
    Set-Location $repoRoot
    $env:GOOS = "windows"
    $env:GOARCH = $GoArch

    $goMinor = Get-GoMinorVersion
    if ($goMinor -ge 26) {
        if ([string]::IsNullOrWhiteSpace($env:GOEXPERIMENT)) {
            $env:GOEXPERIMENT = "nogreenteagc"
        } elseif ($env:GOEXPERIMENT -notmatch "(^|,)nogreenteagc($|,)") {
            $env:GOEXPERIMENT = "$($env:GOEXPERIMENT),nogreenteagc"
        }
        Write-Host "GOEXPERIMENT=$($env:GOEXPERIMENT) (enabled for Go 1.$goMinor Windows build)"
    }

    $resolvedOutput = Resolve-RepoPath $Output
    $resolvedTarget = Resolve-RepoPath $Target
    $outputDir = Split-Path -Parent $resolvedOutput
    if (-not [string]::IsNullOrWhiteSpace($outputDir)) {
        New-Item -ItemType Directory -Force -Path $outputDir | Out-Null
    }

    $runningTargets = @(Get-TargetProcesses $resolvedOutput)
    if ($runningTargets.Count -gt 0) {
        if ($ForceKillRunning) {
            Stop-TargetProcesses $resolvedOutput
        } else {
            $pids = @($runningTargets | ForEach-Object { $_.Id })
            Write-Warning "Target executable is currently running (PID: $($pids -join ', ')). Re-run with -ForceKillRunning to stop it before build."
        }
    }

    $buildArgs = @("build", "-trimpath")
    if (-not [string]::IsNullOrWhiteSpace($Ldflags)) {
        $buildArgs += "-ldflags=$Ldflags"
    }
    $buildArgs += @("-o", $resolvedOutput, $resolvedTarget)

    & go @buildArgs
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
    Write-Host "Built: $resolvedOutput"
} finally {
    Set-Location $originalLocation
    $env:GOOS = $originalGoos
    $env:GOARCH = $originalGoarch
    $env:GOEXPERIMENT = $originalGoexperiment
}
