# ===========================================================================
# OctPlugins packager  (Windows PowerShell 5.1+, ASCII only)
#
# Builds a portable, ready-to-run Electron app folder:
#
#   <OutDir>\
#     OctPlugins.exe         1) admin launcher (embedded requireAdministrator
#                               manifest -> UAC prompt on every double click)
#     OctPluginsHost.exe     2) renamed electron.exe, the real host GUI
#     *.dll *.pak locales\      Chromium/Electron runtime (from ui node_modules)
#     resources\
#       app\                    Electron app: package.json + index.html +
#                               build\*.js + node_modules\ws
#       kernel\kerneld.exe      Go kernel daemon
#       plugins\<id>\           plugin folders copied from plugins\
#                               (__pycache__ excluded)
#       sdk\                    oct_sdk (injected into python plugins via
#                               PYTHONPATH=<root>/sdk/python, node via _resolve)
#       config\                 user-settings.json (plugin prefs, e.g. the
#                               capability-gateway provider chains)
#       state\perms.json        first-party permission grants (seeded)
#       state\plugins\proxy\    mihomo GeoIP data (geoip.metadb), seeded from the
#                               dev tree - see note below
#       runtime\<id>\venv       Python plugin venvs (slim: only the envs that
#                               CANNOT be rebuilt on first run, see below)
#       runtime\python\         bundled uv-managed CPython 3.12 interpreter
#                               (so first-run venv builds need no interpreter
#                               download, and shipped venvs work on machines
#                               without a uv-managed 3.12)
#       resources\              shared theme / logo / icons / shanhe12
#       bin\uv.exe              uv bootstrap (copied if found in PATH)
#
# Runtime-root logic: build\main.js computes __dirname\..\.. = resources\ ;
# the kernel derives its root from kerneld.exe\.. = resources\ as well.
#
# Python env policy (why slim ships exactly one venv):
#   Default ("essential"): ship only hy-mt-server's venv. Its dependency
#   llama-cpp-python publishes ONLY an sdist on PyPI (no Windows wheel), so a
#   first-run `uv pip install` would have to compile it with MSVC/CMake - not
#   something an end user can be expected to have. The other four python
#   plugins (opus-mt-server, doc-convert, sherpa-onnx, moss-tts-onnx) have
#   prebuilt wheels for every dependency, so the kernel installs them on
#   demand at first call (network required) - see kernel
#   lifecycle/GetOrStart -> deps install gating (E_DEPS_MISSING +
#   installing=true while it runs, then the plugin starts itself).
#   -AllPyEnv: ship every venv (offline-ready, ~1.1 GB extra, old behavior)
#   -NoPyEnv : ship no venv at all (leanest; needs network for most plugins)
#
# Why sdk/config/state must ship:
#   - sdk\    : python plugins do `import oct_sdk`; without it they die at
#               import time and the kernel only reports "plugin crashed".
#   - config\ : capability-gateway reads its provider chain from
#               user-settings.json; without it every invoke fails.
#   - state\  : the host auto-grants only FIRST_PARTY_PLUGINS (translate);
#               capability-gateway needs local_model for gate.settings_get,
#               otherwise every invoke fails with a misleading
#               E_PLUGIN_MISSING. Seeding perms.json pre-authorizes it.
#
# Usage:
#   .\package.ps1                         build into default output dir (slim)
#   .\package.ps1 -OutDir E:\Other\Dir
#   .\package.ps1 -SkipUI                 skip tsc (reuse ui\build as-is)
#   .\package.ps1 -ForcePyReinstall       rebuild python venvs with uv
#                                         instead of reusing .\runtime
#   .\package.ps1 -AllPyEnv               ship every python venv (offline)
#   .\package.ps1 -NoPyEnv                ship no python venv at all
# ===========================================================================
param(
    [string]$OutDir = "D:\Project\HalfProject\OctPlugins",
    [switch]$SkipUI,
    [switch]$ForcePyReinstall,
    [switch]$AllPyEnv,
    [switch]$NoPyEnv
)

$ErrorActionPreference = "Stop"
$root         = Split-Path -Parent $MyInvocation.MyCommand.Path
$uiDir        = Join-Path $root "ui"
$electronDist = Join-Path $uiDir "node_modules\electron\dist"
$pluginSrc    = Join-Path $root "plugins"
$sharedResSrc = Join-Path $root "resources"
$sdkSrc       = Join-Path $root "sdk"
$cfgSrc       = Join-Path $root "config\user-settings.json"
$permsSrc     = Join-Path $root "state\perms.json"
$devRuntime   = Join-Path $root "runtime"
$kernelExe    = Join-Path $root "kernel\kerneld.exe"
$csc          = Join-Path $env:WINDIR "Microsoft.NET\Framework64\v4.0.30319\csc.exe"

function Die($msg) { Write-Host "[FAIL] $msg" -ForegroundColor Red; exit 1 }
function Step($n, $msg) { Write-Host ""; Write-Host "=== [$n] $msg ===" -ForegroundColor Cyan }
function Warn($msg) { Write-Host "  [WARN] $msg" -ForegroundColor Yellow }
function Ok($msg)   { Write-Host "  OK   $msg" -ForegroundColor Green }
function Assert-Native($what) {
    if ($LASTEXITCODE -ne 0) { Die "$what failed (exit $LASTEXITCODE)" }
}
# Native tools (uv / robocopy / python) write progress to stderr. Under
# $ErrorActionPreference='Stop' PS 5.1 turns that into a terminating
# NativeCommandError, so every noisy native call goes through these helpers.
function Run-Quiet([string]$exe, [string[]]$argv) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try { & $exe @argv 2>&1 | Out-Null } finally { $ErrorActionPreference = $prev }
}
function Capture-Quiet([string]$exe, [string[]]$argv) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try { return @(& $exe @argv 2>&1) } finally { $ErrorActionPreference = $prev }
}
function Read-Manifest($dir) {
    Get-Content (Join-Path $dir "manifest.json") -Raw -Encoding UTF8 | ConvertFrom-Json
}

# ---------------------------------------------------------------------------
Step "1/8" "Preflight checks"
if (-not (Test-Path (Join-Path $electronDist "electron.exe"))) {
    Die "electron runtime missing: $electronDist\electron.exe (run 'npm install' in ui\ first)"
}
if (-not (Test-Path $kernelExe)) { Die "kernel\kerneld.exe missing" }
if (-not (Test-Path $csc))        { Die "csc.exe (.NET Framework) missing: $csc" }
if (-not (Get-Command node -ErrorAction SilentlyContinue)) { Die "node not found in PATH" }
if (-not $SkipUI -and -not (Get-Command npm -ErrorAction SilentlyContinue)) { Die "npm not found in PATH" }
if (-not (Test-Path (Join-Path $sdkSrc "python\oct_sdk\__init__.py"))) {
    Die "sdk\python\oct_sdk\__init__.py missing - python plugins would fail to import oct_sdk"
}

$pluginDirs = @(Get-ChildItem $pluginSrc -Directory)
if ($pluginDirs.Count -eq 0) { Die "no plugin folders under $pluginSrc" }
$manifests = @{}
foreach ($p in $pluginDirs) {
    if (-not (Test-Path (Join-Path $p.FullName "manifest.json"))) {
        Die "plugin folder without manifest.json: $($p.FullName)"
    }
    $manifests[$p.Name] = Read-Manifest $p.FullName
}

$uvCmd = Get-Command uv.exe -ErrorAction SilentlyContinue
$uvExe = if ($uvCmd) { $uvCmd.Source } else { "" }
if (-not $uvExe -and -not $AllPyEnv) {
    Die "uv.exe not found in PATH: slim packaging needs it to bundle the CPython interpreter, and the kernel needs resources\bin\uv.exe to install plugin dependencies on first run"
}

if (Get-Process OctPluginsHost -ErrorAction SilentlyContinue) {
    Die "OctPluginsHost.exe is currently running - close it before packaging"
}
if (Test-Path $OutDir) {
    $locked = Join-Path $OutDir "OctPluginsHost.exe"
    if (Test-Path $locked) {
        try { $fs = [System.IO.File]::Open($locked, "Open", "Read", "None"); $fs.Close() }
        catch { Die "target file is locked (app running?): $locked" }
    }
}
Write-Host "node    : $(node -v)"
Write-Host "uv      : $(if ($uvExe) { $uvExe } else { '<not found>' })"
Write-Host "plugins : $($pluginDirs.Count)"
Write-Host "out     : $OutDir"

# ---------------------------------------------------------------------------
if (-not $SkipUI) {
    Step "2/8" "Compile host UI (npm run build / tsc)"
    Push-Location $uiDir
    try {
        npm run build
        Assert-Native "tsc"
    } finally { Pop-Location }
    if (-not (Test-Path (Join-Path $uiDir "build\main.js"))) { Die "ui\build\main.js missing after tsc" }
} else {
    Step "2/8" "Compile host UI skipped (-SkipUI)"
}

# ---------------------------------------------------------------------------
Step "3/8" "Prepare clean output folder + mirror Electron runtime"
if (Test-Path $OutDir) { Remove-Item $OutDir -Recurse -Force }
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null

Copy-Item (Join-Path $electronDist "*") -Destination $OutDir -Recurse -Force
Move-Item (Join-Path $OutDir "electron.exe") (Join-Path $OutDir "OctPluginsHost.exe")
$defaultApp = Join-Path $OutDir "resources\default_app.asar"
if (Test-Path $defaultApp) { Remove-Item $defaultApp -Force }

# ---------------------------------------------------------------------------
Step "4/8" "Assemble resources\app (Electron application)"
$appDir = Join-Path $OutDir "resources\app"
New-Item -ItemType Directory -Path $appDir -Force | Out-Null

$appPkg = @'
{
  "name": "octplugins-host",
  "version": "0.1.0",
  "main": "build/main.js"
}
'@
Set-Content -Path (Join-Path $appDir "package.json") -Value $appPkg -Encoding ASCII -NoNewline

$indexHtml = Join-Path $uiDir "index.html"
if (-not (Test-Path $indexHtml)) { Die "ui\index.html missing" }
Copy-Item $indexHtml (Join-Path $appDir "index.html")
Copy-Item (Join-Path $uiDir "build") (Join-Path $appDir "build") -Recurse -Force

# ws is the single runtime npm dependency (zero transitive deps).
$wsSrc = Join-Path $uiDir "node_modules\ws"
if (-not (Test-Path (Join-Path $wsSrc "package.json"))) { Die "ui\node_modules\ws missing (npm install in ui\ first)" }
New-Item -ItemType Directory -Path (Join-Path $appDir "node_modules") -Force | Out-Null
Copy-Item $wsSrc (Join-Path $appDir "node_modules\ws") -Recurse -Force

# ---------------------------------------------------------------------------
Step "5/8" "Assemble kernel / plugins / shared resources / bin"
$resRoot = Join-Path $OutDir "resources"

New-Item -ItemType Directory -Path (Join-Path $resRoot "kernel") -Force | Out-Null
Copy-Item $kernelExe (Join-Path $resRoot "kernel\kerneld.exe") -Force

New-Item -ItemType Directory -Path (Join-Path $resRoot "plugins") -Force | Out-Null
foreach ($p in $pluginDirs) {
    $dst = Join-Path $resRoot ("plugins\" + $p.Name)
    Copy-Item $p.FullName $dst -Recurse -Force
    # runtime caches must not ship inside the package
    foreach ($cache in @(Get-ChildItem $dst -Recurse -Directory -Filter "__pycache__" -ErrorAction SilentlyContinue)) {
        Remove-Item $cache.FullName -Recurse -Force
    }
}

if (Test-Path $sharedResSrc) {
    $sharedDst = Join-Path $resRoot "resources"
    New-Item -ItemType Directory -Path $sharedDst -Force | Out-Null
    Copy-Item (Join-Path $sharedResSrc "*") -Destination $sharedDst -Recurse -Force
}

if ($uvExe) {
    New-Item -ItemType Directory -Path (Join-Path $resRoot "bin") -Force | Out-Null
    Copy-Item $uvExe (Join-Path $resRoot "bin\uv.exe") -Force
    Ok "bin\uv.exe bundled from $uvExe"
} else {
    Warn "uv.exe not found in PATH - python envs can only be reused, not built"
}

# ---------------------------------------------------------------------------
Step "6/8" "Seed SDK / config / state (required at runtime)"
# 1) sdk: python plugins inject oct_sdk from <root>/sdk/python via PYTHONPATH.
$sdkDst = Join-Path $resRoot "sdk"
New-Item -ItemType Directory -Path $sdkDst -Force | Out-Null
Copy-Item (Join-Path $sdkSrc "*") -Destination $sdkDst -Recurse -Force
foreach ($c in @(Get-ChildItem $sdkDst -Recurse -Directory -Filter "__pycache__" -ErrorAction SilentlyContinue)) {
    Remove-Item $c.FullName -Recurse -Force
}
Ok "sdk -> resources\sdk (oct_sdk.py python + node)"

# 2) config: host-side plugin preferences (capability-gateway provider chains).
if (Test-Path $cfgSrc) {
    $cfgDst = Join-Path $resRoot "config"
    New-Item -ItemType Directory -Path $cfgDst -Force | Out-Null
    Copy-Item $cfgSrc (Join-Path $cfgDst "user-settings.json") -Force
    Ok "config -> resources\config\user-settings.json"
} else {
    Warn "config\user-settings.json not found - capability-gateway will have no provider chain"
}

# 3) state/perms.json: seed the permission grants that the host does NOT
#    auto-grant (its FIRST_PARTY_PLUGINS list only covers translate). Any
#    plugin declaring local_model would otherwise be denied gate.settings_get.
$stateDst = Join-Path $resRoot "state"
New-Item -ItemType Directory -Path $stateDst -Force | Out-Null
$seed = @{}
if (Test-Path $permsSrc) {
    $src = Get-Content $permsSrc -Raw -Encoding UTF8 | ConvertFrom-Json
    foreach ($prop in $src.PSObject.Properties) {
        $d = @{}; $g = @{}
        if ($prop.Value.declared) { foreach ($k in $prop.Value.declared.PSObject.Properties)   { $d[$k.Name] = $true } }
        if ($prop.Value.granted)  { foreach ($k in $prop.Value.granted.PSObject.Properties)    { $g[$k.Name] = $true } }
        $seed[$prop.Name] = @{ declared = $d; granted = $g }
    }
}
# union in every plugin that declares local_model and is not seeded yet, so the
# seed stays correct even when the dev state file is absent or stale.
$seedAdded = @()
foreach ($name in $manifests.Keys) {
    $mf = $manifests[$name]
    if (-not $mf.permissions) { continue }
    if (-not ($mf.permissions -contains "local_model")) { continue }
    if ($seed.ContainsKey($mf.id)) { continue }
    $d = @{}
    foreach ($perm in $mf.permissions) { $d[$perm] = $true }
    $seed[$mf.id] = @{ declared = $d; granted = $d }
    $seedAdded += $mf.id
}
if ($seed.Count -gt 0) {
    # Write WITHOUT a BOM: Go's json.Unmarshal rejects a leading BOM, which
    # would make the kernel silently load an empty grant set and then overwrite
    # this file with "{}". PS 5.1's `Set-Content -Encoding UTF8` adds a BOM.
    $seedJson = $seed | ConvertTo-Json -Depth 6
    [System.IO.File]::WriteAllText(
        (Join-Path $stateDst "perms.json"), $seedJson, (New-Object System.Text.UTF8Encoding($false)))
    Ok ("state -> resources\state\perms.json (" + $seed.Count + " plugin(s)" + $(if ($seedAdded.Count) { "; derived: " + ($seedAdded -join ",") } else { "" }) + ")")
} else {
    Warn "no permission grant could be seeded"
}

# 4) state/plugins/proxy: mihomo's GeoIP data (MMDB) is NOT optional. proxy.exe
#    runs the core with -d <OCT_PLUGIN_DATA> (= state\plugins\proxy) and mihomo
#    downloads geoip.metadb from GitHub into that dir on first start. In networks
#    where GitHub is unreachable the download times out (90s) and the core dies:
#      "can't initial GeoIP: can't download MMDB: context deadline exceeded"
#    -> proxy.exe reports "Mihomo config check failed; verify the profile is
#       compatible with this core build and that GeoIP/GeoSite data is available".
#    The dev tree already has that file warmed up (state\plugins\proxy\geoip.metadb),
#    which is the only reason the dev build "just works". Ship it as a seed so the
#    packaged app needs no network for the default profile.
$geoFiles = @("geoip.metadb", "Country.mmdb", "geoip.dat", "geosite.dat")
$geoSrc   = Join-Path $root "state\plugins\proxy"
$geoDst   = Join-Path $stateDst "plugins\proxy"
$geoCopied = @()
if (Test-Path $geoSrc) {
    foreach ($f in $geoFiles) {
        $src = Join-Path $geoSrc $f
        if (Test-Path $src) {
            New-Item -ItemType Directory -Path $geoDst -Force | Out-Null
            Copy-Item $src (Join-Path $geoDst $f) -Force
            $geoCopied += $f
        }
    }
}
if ($geoCopied.Count -gt 0) {
    Ok ("state -> resources\state\plugins\proxy\ (" + ($geoCopied -join ", ") + ")")
} else {
    Warn "no mihomo geo data in state\plugins\proxy - the proxy core will try to download GeoIP from GitHub at first start"
}

# ---------------------------------------------------------------------------
Step "7/8" "Prepare Python plugin environments"

# Which venvs ship? Slim default ships only the envs that CANNOT be rebuilt
# from PyPI wheels on first run (llama-cpp-python has no Windows wheel -> it
# would need MSVC). Everything else is installed on demand by the kernel.
$essentialEnv = @("hy-mt-server")
$pyIds = @()
foreach ($name in ($manifests.Keys | Sort-Object)) {
    $mf = $manifests[$name]
    if (-not $mf.type -or $mf.type.ToLower() -ne "python") { continue }
    $pyIds += $mf.id
}
if ($NoPyEnv) {
    $shipEnv = @()
    Warn "no python venv shipped (-NoPyEnv): every python plugin installs on first call (needs uv + network)"
} elseif ($AllPyEnv) {
    $shipEnv = $pyIds
} else {
    $shipEnv = @($pyIds | Where-Object { $essentialEnv -contains $_ })
    $lazyEnv = @($pyIds | Where-Object { $shipEnv -notcontains $_ })
    Warn ("slim mode: shipping " + $(if ($shipEnv.Count) { $shipEnv -join ", " } else { "<none>" }) +
          "; installed on 1st call: " + $(if ($lazyEnv.Count) { $lazyEnv -join ", " } else { "<none>" }))
}

# Bundle the uv-managed CPython 3.12 interpreter itself. Two reasons:
#   1) newly built venvs land here (UV_PYTHON_INSTALL_DIR is pointed at this
#      dir by the kernel) so no interpreter download is needed;
#   2) shipped venvs get their pyvenv.cfg `home` rewritten to this dir, so
#      they work on a machine that never had a uv-managed 3.12.
$bundledPyDir = $null
if (-not $NoPyEnv) {
    $srcPy = ""
    if ($uvExe) {
        $found = @(Capture-Quiet $uvExe @("python", "find", "3.12") | Where-Object { $_ -match 'python\.exe\s*$' })
        if ($LASTEXITCODE -eq 0 -and $found.Count -gt 0) { $srcPy = $found[0].ToString().Trim() }
    }
    if (-not $srcPy -or -not (Test-Path $srcPy)) {
        Die "cannot locate a uv-managed CPython 3.12 to bundle (uv python find 3.12 failed)"
    }
    $srcPyRoot = Split-Path -Parent $srcPy
    $pyName    = Split-Path -Leaf $srcPyRoot
    $dstPyRoot = Join-Path $resRoot ("runtime\python\" + $pyName)
    New-Item -ItemType Directory -Path $dstPyRoot -Force | Out-Null
    Run-Quiet "robocopy" @($srcPyRoot, $dstPyRoot, "/E", "/NFL", "/NDL", "/NJH", "/NJS", "/NP", "/XD", "__pycache__")
    if ($LASTEXITCODE -ge 8) { Die "robocopy bundled python failed (exit $LASTEXITCODE)" }
    $bundledPyDir = Join-Path $resRoot "runtime\python"
    $pyExePath    = Join-Path $dstPyRoot "python.exe"
    Ok ("{0,-16} bundled interpreter ({1})" -f "python", $pyName)

    # A freshly created venv holds a real CPython venv launcher in
    # Scripts\python.exe, i.e. one that honours pyvenv.cfg `home`. Venvs reused
    # from .\runtime were built by an older uv whose python.exe stub resolves
    # its base interpreter some other way and silently ignores `home` - so the
    # launcher is swapped in below (otherwise the shipped venv stays bound to
    # this machine's %APPDATA%\uv\python and dies on any other machine).
    $scratch    = Join-Path $env:TEMP ("octpkg_venv_" + $PID)
    if (Test-Path $scratch) { Remove-Item $scratch -Recurse -Force }
    Run-Quiet $uvExe @("venv", "--python", $pyExePath, $scratch)
    Assert-Native "uv venv (launcher scratch)"
    $launcherDir = Join-Path $scratch "Scripts"
}

foreach ($id in $shipEnv) {
    $req     = Join-Path (Join-Path $pluginSrc $id) "requirements.txt"
    $dstVenv = Join-Path $resRoot ("runtime\" + $id + "\venv")
    $dstPy   = Join-Path $dstVenv "Scripts\python.exe"
    $devVenv = Join-Path $devRuntime ($id + "\venv")
    $devPy   = Join-Path $devVenv "Scripts\python.exe"

    if ((-not $ForcePyReinstall) -and (Test-Path $devPy)) {
        # reuse the already-populated venv: no download, no rebuild
        New-Item -ItemType Directory -Path $dstVenv -Force | Out-Null
        Run-Quiet "robocopy" @($devVenv, $dstVenv, "/E", "/NFL", "/NDL", "/NJH", "/NJS", "/NP", "/XD", "__pycache__")
        if ($LASTEXITCODE -ge 8) { Die "robocopy $id venv failed (exit $LASTEXITCODE)" }
        Ok ("{0,-16} reused from .\runtime" -f $id)
    } elseif (Test-Path $req) {
        if (-not $uvExe) { Die "uv is required to build the $id env (not in PATH)" }
        New-Item -ItemType Directory -Path $dstVenv -Force | Out-Null
        $pyArg = if ($pyExePath) { $pyExePath } else { "3.12" }
        Run-Quiet $uvExe @("venv", "--python", $pyArg, $dstVenv)
        Assert-Native "uv venv $id"
        Run-Quiet $uvExe @("pip", "install", "--python", $dstPy, "-r", $req)
        Assert-Native "uv pip install $id"
        Ok ("{0,-16} installed via uv" -f $id)
    } else {
        Warn ("{0,-16} no requirements.txt and no reusable venv" -f $id)
        continue
    }

    # Guard against the "venv directory exists but holds no packages" trap:
    # an empty-shell venv would silently be treated as ready and the plugin
    # would crash on import forever.
    $pkgRoot  = Join-Path $dstVenv "Lib\site-packages"
    $distInfo = @(Get-ChildItem $pkgRoot -Directory -Filter "*.dist-info" -ErrorAction SilentlyContinue)
    if ($distInfo.Count -eq 0) {
        Die "python env for $id has an empty site-packages (venv exists but no deps installed)"
    }

    # Point the venv at the bundled interpreter: rewrite pyvenv.cfg `home` and make
    # sure Scripts\python.exe is a launcher that actually honours it.
    if ($bundledPyDir) {
        $cfgPath  = Join-Path $dstVenv "pyvenv.cfg"
        $cfgLines = @(Get-Content $cfgPath -Encoding UTF8 | Where-Object { $_ -notmatch '^\s*home\s*=' })
        $cfgLines = @("home = $dstPyRoot") + $cfgLines
        [System.IO.File]::WriteAllLines($cfgPath, $cfgLines, (New-Object System.Text.ASCIIEncoding))
        if ($launcherDir) {
            foreach ($lexe in @("python.exe", "pythonw.exe")) {
                $srcLexe = Join-Path $launcherDir $lexe
                if (Test-Path $srcLexe) { Copy-Item $srcLexe (Join-Path $dstVenv ("Scripts\" + $lexe)) -Force }
            }
        }
        $probe = @(Capture-Quiet $dstPy @("-c", "import sys; print(sys.base_prefix)") | Select-Object -First 1)
        if ($probe.Count -gt 0) { $probe = $probe[0].ToString().Trim() } else { $probe = "" }
        if ($probe -ne $dstPyRoot) {
            Die ("$id venv base interpreter mismatch: got '" + $probe + "', want '" + $dstPyRoot + "'")
        }
    }
    Write-Host ("       " + $id + " site-packages dist-info = " + $distInfo.Count)
}
if ($pyIds.Count -eq 0) { Warn "no python plugins found" }
if ($scratch -and (Test-Path $scratch)) { Remove-Item $scratch -Recurse -Force }

# ---------------------------------------------------------------------------
Step "8/8" "Build admin launcher (OctPlugins.exe) with embedded UAC manifest"
$tmpDir = Join-Path $env:TEMP ("octpkg_" + $PID)
if (Test-Path $tmpDir) { Remove-Item $tmpDir -Recurse -Force }
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

$launcherCs = @'
using System;
using System.Diagnostics;
using System.IO;
using System.Text;
using System.Windows.Forms;

internal static class Launcher
{
    [STAThread]
    private static int Main(string[] args)
    {
        string baseDir = AppDomain.CurrentDomain.BaseDirectory;
        string host = Path.Combine(baseDir, "OctPluginsHost.exe");
        if (!File.Exists(host))
        {
            MessageBox.Show("OctPluginsHost.exe not found:\n" + host,
                "OctPlugins", MessageBoxButtons.OK, MessageBoxIcon.Error);
            return 2;
        }
        try
        {
            ProcessStartInfo psi = new ProcessStartInfo();
            psi.FileName = host;
            psi.WorkingDirectory = baseDir;
            psi.UseShellExecute = false;
            StringBuilder sb = new StringBuilder();
            foreach (string a in args)
            {
                if (sb.Length > 0) sb.Append(' ');
                sb.Append('"').Append(a.Replace("\"", "\\\"")).Append('"');
            }
            psi.Arguments = sb.ToString();
            Process.Start(psi);
            return 0;
        }
        catch (Exception ex)
        {
            MessageBox.Show(ex.Message, "OctPlugins",
                MessageBoxButtons.OK, MessageBoxIcon.Error);
            return 1;
        }
    }
}
'@
Set-Content -Path (Join-Path $tmpDir "launcher.cs") -Value $launcherCs -Encoding ASCII

$manifestXml = @'
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="0.1.0.0" name="OctPlugins.Launcher" type="win32"/>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
</assembly>
'@
Set-Content -Path (Join-Path $tmpDir "app.manifest") -Value $manifestXml -Encoding ASCII

$launcherExe = Join-Path $OutDir "OctPlugins.exe"
& $csc /nologo /target:winexe /platform:anycpu `
    /reference:System.Windows.Forms.dll `
    /win32manifest:$(Join-Path $tmpDir "app.manifest") `
    /out:$launcherExe (Join-Path $tmpDir "launcher.cs")
Assert-Native "csc launcher"
Remove-Item $tmpDir -Recurse -Force

# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "=== Verification ===" -ForegroundColor Cyan
$mustExist = @(
    "OctPlugins.exe",
    "OctPluginsHost.exe",
    "resources\app\package.json",
    "resources\app\index.html",
    "resources\app\build\main.js",
    "resources\app\node_modules\ws\package.json",
    "resources\kernel\kerneld.exe",
    "resources\sdk\python\oct_sdk\__init__.py",
    "resources\config\user-settings.json",
    "resources\state\perms.json",
    "resources\resources\theme\plugin.css"
)
$failed = @()
foreach ($rel in $mustExist) {
    if (Test-Path (Join-Path $OutDir $rel)) {
        Write-Host ("  OK   " + $rel) -ForegroundColor Green
    } else {
        Write-Host ("  MISS " + $rel) -ForegroundColor Red
        $failed += $rel
    }
}

# every shipped manifest entry must physically exist
$shippedPlugins = @(Get-ChildItem (Join-Path $resRoot "plugins") -Directory)
foreach ($p in $shippedPlugins) {
    $mf = Read-Manifest $p.FullName
    if ($mf.entry -and -not (Test-Path (Join-Path $p.FullName $mf.entry))) {
        $failed += "$($mf.id):entry"; Write-Host ("  MISS entry " + $mf.id + " -> " + $mf.entry) -ForegroundColor Red
    }
    if ($mf.ui.entry -and -not (Test-Path (Join-Path $p.FullName $mf.ui.entry))) {
        $failed += "$($mf.id):ui"; Write-Host ("  MISS ui " + $mf.id + " -> " + $mf.ui.entry) -ForegroundColor Red
    }
}
$leftoverCache = @(Get-ChildItem (Join-Path $resRoot "plugins") -Recurse -Directory -Filter "__pycache__" -ErrorAction SilentlyContinue)
if ($leftoverCache.Count -gt 0) { $failed += "pycache" }

# the capability-gateway must be pre-authorized, or every capability invoke
# fails with the misleading E_PLUGIN_MISSING
$permsDst = Join-Path $resRoot "state\perms.json"
if (Test-Path $permsDst) {
    $permBytes = [System.IO.File]::ReadAllBytes($permsDst)
    if ($permBytes.Length -ge 3 -and $permBytes[0] -eq 0xEF -and $permBytes[1] -eq 0xBB -and $permBytes[2] -eq 0xBF) {
        $failed += "perms-bom"
        Write-Host "  MISS perms.json starts with a UTF-8 BOM (Go would ignore it)" -ForegroundColor Red
    }
    $perms = Get-Content $permsDst -Raw -Encoding UTF8 | ConvertFrom-Json
    $gw = $perms.PSObject.Properties | Where-Object { $_.Name -eq "capability-gateway" }
    if ($gw -and $gw.Value.granted.local_model) {
        Write-Host "  OK   perms: capability-gateway granted local_model" -ForegroundColor Green
    } else {
        $failed += "perms-gateway"
        Write-Host "  MISS perms: capability-gateway local_model not granted" -ForegroundColor Red
    }
}

# python venvs must not be empty shells, and must point at the bundled interpreter
# (otherwise they only work on the machine that built them).
$emptyVenv = @()
foreach ($d in @(Get-ChildItem (Join-Path $resRoot "runtime") -Directory -ErrorAction SilentlyContinue)) {
    $sp = Join-Path $d.FullName "venv\Lib\site-packages"
    if (-not (Test-Path $sp)) { continue }
    if (@(Get-ChildItem $sp -Directory -Filter "*.dist-info" -ErrorAction SilentlyContinue).Count -eq 0) {
        $emptyVenv += $d.Name
    }
    $cfg = Join-Path $d.FullName "venv\pyvenv.cfg"
    if (Test-Path $cfg) {
        $homeLine = (Get-Content $cfg -Encoding UTF8 | Where-Object { $_ -match '^\s*home\s*=' } | Select-Object -First 1)
        $homePath = ($homeLine -replace '^\s*home\s*=\s*', '').Trim()
        if (-not (Test-Path (Join-Path $homePath "python.exe"))) {
            $failed += "venv-home"; Write-Host ("  MISS " + $d.Name + " venv home -> " + $homePath) -ForegroundColor Red
        }
    }
}
if ($emptyVenv.Count -gt 0) { $failed += "empty-venv"; Write-Host ("  MISS empty python env: " + ($emptyVenv -join ", ")) -ForegroundColor Red }

# bundled interpreter sanity: the kernel relies on it to build venvs offline
if (-not $NoPyEnv) {
    $bundledOk = $false
    foreach ($d in @(Get-ChildItem (Join-Path $resRoot "runtime\python") -Directory -ErrorAction SilentlyContinue)) {
        if (Test-Path (Join-Path $d.FullName "python.exe")) { $bundledOk = $true }
    }
    if ($bundledOk) {
        Write-Host "  OK   runtime\python bundled interpreter present" -ForegroundColor Green
    } else {
        $failed += "bundled-python"
        Write-Host "  MISS runtime\python bundled interpreter missing" -ForegroundColor Red
    }
}

# proxy core needs GeoIP data inside its -d dir, otherwise mihomo tries to fetch
# it from GitHub at first start and the config check fails offline.
$proxyGeoDst = Join-Path $resRoot "state\plugins\proxy"
if (-not (Test-Path $proxyGeoDst)) {
    Warn "no state\plugins\proxy seed - proxy core must download GeoIP on first start"
} else {
    $geoHit = @(Get-ChildItem $proxyGeoDst -File -ErrorAction SilentlyContinue | Where-Object { $geoFiles -contains $_.Name })
    if ($geoHit.Count -gt 0) {
        Write-Host ("  OK   proxy geo data seeded: " + (($geoHit | ForEach-Object { $_.Name }) -join ", ")) -ForegroundColor Green
    } else {
        Warn "state\plugins\proxy has no geo data - proxy core must download GeoIP on first start"
    }
}

$launcherBytes = [System.IO.File]::ReadAllBytes($launcherExe)
$launcherText  = [System.Text.Encoding]::ASCII.GetString($launcherBytes)
if ($launcherText.Contains("requireAdministrator")) {
    Write-Host "  OK   launcher manifest: requireAdministrator (UAC prompt)" -ForegroundColor Green
} else {
    $failed += "launcher-manifest"
    Write-Host "  MISS launcher manifest requireAdministrator" -ForegroundColor Red
}
if ($failed.Count -gt 0) { Die "package incomplete: $($failed -join ', ')" }

$sizeMB = [math]::Round(((Get-ChildItem $OutDir -Recurse -File | Measure-Object Length -Sum).Sum / 1MB), 1)
Write-Host ""
Write-Host "DONE." -ForegroundColor Green
Write-Host "  output : $OutDir"
Write-Host "  size   : $sizeMB MB"
Write-Host "  plugins: $($shippedPlugins.Count)"
Write-Host "  launch : double-click OctPlugins.exe (UAC prompt -> host starts elevated)"
