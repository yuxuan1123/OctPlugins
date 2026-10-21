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
#                               browser-panel.js + vendor\chrome-tabs +
#                               build\*.js + node_modules\ws
#       kernel\kerneld.exe      Go kernel daemon
#       plugins\<id>\           plugin folders copied from plugins\
#                               (__pycache__ excluded)
#       sdk\                    oct_sdk (injected into python plugins via
#                               PYTHONPATH=<root>/sdk/python, node via _resolve)
#       config\                 user-settings.json (plugin prefs, e.g. the
#                               capability-gateway provider chains) +
#                               models-root.json (模型存储根, 默认 D:\AI_Model)
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
# Python env policy (offline first):
#   Default: ship every python plugin whose venv already exists in the source
#   tree (.\runtime, or <ResourcesSrc>\runtime). Everything is copied locally -
#   no interpreter download, no pip install. The kernel resolves it from
#   <root>\runtime\<id>\venv (see kernel/internal/runtime.VenvPython), and
#   pyvenv.cfg `home` is rewritten to the bundled interpreter so the package
#   works on a machine that never had uv/a CPython install.
#   -AllPyEnv: force-ship every python plugin env (fails if a venv is missing
#              and no uv is available to build it)
#   -NoPyEnv : ship no venv at all (developer mode only: the kernel installer is
#              still a skeleton, so python plugins would have no interpreter)
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
#   .\package.ps1                                   build into .\dist (slim)
#   .\package.ps1 -OutDir E:\Other\Dir
#   .\package.ps1 -OutDir D:\pyex\OctPlugins -ResourcesSrc D:\oct_src -AllPyEnv
#   .\package.ps1 -SkipUI                 skip tsc (reuse ui\build as-is)
#   .\package.ps1 -ResourcesSrc <dir>     reuse plugins/runtime/config/state/bin from a
#                                         previously packaged resources dir: everything is
#                                         copied locally, nothing is downloaded
#   .\package.ps1 -ForcePyReinstall       rebuild python venvs with uv
#                                         instead of reusing .\runtime
#   .\package.ps1 -AllPyEnv               ship every python venv (offline)
#   .\package.ps1 -NoPyEnv                ship no python venv at all
# ===========================================================================
param(
    [string]$OutDir = "",
    [string]$ResourcesSrc = "",
    [switch]$SkipUI,
    [switch]$ForcePyReinstall,
    [switch]$AllPyEnv,
    [switch]$NoPyEnv
)

$ErrorActionPreference = "Stop"
$root         = Split-Path -Parent $MyInvocation.MyCommand.Path
$uiDir        = Join-Path $root "ui"
$electronDist = Join-Path $uiDir "node_modules\electron\dist"
$sharedResSrc = Join-Path $root "resources"
$sdkSrc       = Join-Path $root "sdk"
$kernelExe    = Join-Path $root "kernel\kerneld.exe"
$csc          = Join-Path $env:WINDIR "Microsoft.NET\Framework64\v4.0.30319\csc.exe"

# 素材来源：
#   - plugins / runtime / bin：默认取仓库；-ResourcesSrc 指向一个已打包好的 resources
#     目录时，改为从该目录本地复制（离线、不重建任何环境）。
#   - config / state：永远取仓库（用户态配置与授权）。绝不能来自旧包——旧包的
#     user-settings 可能把 provider 链指向本机不可用的引擎（例如 CPU 跑不动的 hy-mt），
#     表现为"开发态能翻译、打包后崩溃/超时"。
$srcRes           = if ($ResourcesSrc) { [System.IO.Path]::GetFullPath($ResourcesSrc).TrimEnd('\') } else { "" }
$pluginSrc        = if ($srcRes) { Join-Path $srcRes "plugins" } else { Join-Path $root "plugins" }
$devRuntime       = if ($srcRes) { Join-Path $srcRes "runtime" } else { Join-Path $root "runtime" }
$binSrc           = if ($srcRes) { Join-Path $srcRes "bin" } else { Join-Path $root "bin" }
$cfgSrc           = Join-Path $root "config\user-settings.json"
$modelsRootCfgSrc = Join-Path $root "config\models-root.json"
$permsSrc         = Join-Path $root "state\perms.json"
$overridesSrc     = Join-Path $root "state\overrides.json"
$geoSrc           = Join-Path $root "state\plugins\proxy"
if (-not (Test-Path $geoSrc) -and $srcRes) { $geoSrc = Join-Path $srcRes "state\plugins\proxy" }
if (-not (Test-Path $geoSrc)) { $geoSrc = Join-Path $root "state\plugins\proxy" }

if (-not $OutDir) { $OutDir = Join-Path $root "dist" }

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
# 目录删除统一走 .NET：某些环境把 Remove-Item 接管为"移到回收站"（safe-delete），
# 大目录/只读文件会在这步失败。这里直接物理删除，失败才报错。
function Remove-Tree([string]$path) {
    if (-not (Test-Path $path)) { return }
    try { [System.IO.Directory]::Delete($path, $true) }
    catch { Die ("cannot remove directory: " + $path + " -> " + $_.Exception.Message) }
}

# Step 3 会整目录删除 OutDir —— 禁止指向仓库根或它的父目录（防误删源码）。
$rootFull = [System.IO.Path]::GetFullPath($root).TrimEnd('\')
$outFull  = [System.IO.Path]::GetFullPath($OutDir).TrimEnd('\')
if ($outFull -eq $rootFull -or $rootFull.StartsWith($outFull + '\')) {
    Die "OutDir 不能是项目根或它的父目录：$OutDir"
}

# ---------------------------------------------------------------------------
Step "1/8" "Preflight checks"
if (-not (Test-Path (Join-Path $electronDist "electron.exe"))) {
    Die "electron runtime missing: $electronDist\electron.exe (run 'npm install' in ui\ first)"
}
if (-not (Test-Path $kernelExe)) { Die "kernel\kerneld.exe missing" }
if (-not (Test-Path $csc))        { Die "csc.exe (.NET Framework) missing: $csc" }
# node/npm 只在需要重新编译 TS 时必需（-SkipUI 复用 ui\build 时不需要）。
$nodeCmd = Get-Command node -ErrorAction SilentlyContinue
$nodeExe = if ($nodeCmd) { $nodeCmd.Source } else { "" }
if (-not $SkipUI) {
    if (-not $nodeCmd) { Die "node not found in PATH (init fnm first, or pass -SkipUI)" }
    if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { Die "npm not found in PATH (init fnm first, or pass -SkipUI)" }
}
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
if (-not $uvExe -and (Test-Path (Join-Path $binSrc "uv.exe"))) { $uvExe = Join-Path $binSrc "uv.exe" }
if (-not $uvExe) {
    Warn "uv.exe not found: only pre-built venvs / bundled interpreter can be shipped (nothing will be created)"
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
Write-Host "node    : $(if ($nodeExe) { & $nodeExe -v } else { '<not found>' })"
Write-Host "uv      : $(if ($uvExe) { $uvExe } else { '<not found>' })"
Write-Host "src     : $srcBase"
Write-Host "plugins : $($pluginDirs.Count)"
Write-Host "out     : $OutDir"

# ---------------------------------------------------------------------------
if (-not $SkipUI) {
    Step "2/8" "Compile host UI (npm run build / tsc)"
    Push-Location $uiDir
    try {
        # npm 会把镜像源等告警写到 stderr，PS 5.1 在 $ErrorActionPreference='Stop'
        # 下会把原生命令的 stderr 变成终止错误 —— 故临时改为 Continue 并只看退出码。
        $prevEap = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try { npm run build 2>&1 | Out-String | Write-Host } finally { $ErrorActionPreference = $prevEap }
        Assert-Native "tsc"
    } finally { Pop-Location }
    if (-not (Test-Path (Join-Path $uiDir "build\main.js"))) { Die "ui\build\main.js missing after tsc" }
} else {
    Step "2/8" "Compile host UI skipped (-SkipUI)"
}

# ---------------------------------------------------------------------------
Step "3/8" "Prepare clean output folder + mirror Electron runtime"
Remove-Tree $OutDir
New-Item -ItemType Directory -Path $OutDir -Force | Out-Null

Copy-Item (Join-Path $electronDist "*") -Destination $OutDir -Recurse -Force
Move-Item (Join-Path $OutDir "electron.exe") (Join-Path $OutDir "OctPluginsHost.exe")
$defaultApp = Join-Path $OutDir "resources\default_app.asar"
if (Test-Path $defaultApp) { try { [System.IO.File]::Delete($defaultApp) } catch { } }

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
# 内嵌浏览器面板：index.html 直接用 <script src="browser-panel.js"> 管理 <webview>，
# 加上 chrome-tabs 前端资源，二者都必须随包分发，否则打包后静态请求 404（面板/标签栏全废）。
$panelJs = Join-Path $uiDir "browser-panel.js"
if (-not (Test-Path $panelJs)) { Die "ui\browser-panel.js missing" }
Copy-Item $panelJs (Join-Path $appDir "browser-panel.js") -Force
$vendorDir = Join-Path $uiDir "vendor"
if (-not (Test-Path $vendorDir)) { Die "ui\vendor missing (chrome-tabs)" }
Copy-Item $vendorDir (Join-Path $appDir "vendor") -Recurse -Force
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
        Remove-Tree $cache.FullName
    }
}

if (Test-Path $sharedResSrc) {
    $sharedDst = Join-Path $resRoot "resources"
    New-Item -ItemType Directory -Path $sharedDst -Force | Out-Null
    Copy-Item (Join-Path $sharedResSrc "*") -Destination $sharedDst -Recurse -Force
}

$binDst = Join-Path $resRoot "bin"
if ($uvExe) {
    New-Item -ItemType Directory -Path $binDst -Force | Out-Null
    Copy-Item $uvExe (Join-Path $binDst "uv.exe") -Force
    Ok "bin\uv.exe bundled from $uvExe"
} else {
    Warn "uv.exe not found - python envs can only be reused, not built"
}
# node: Node 类型插件的解释器。打包后目标机未必装 node，故把本机 node.exe 一并分发
# （内核 NodeInterp 优先用 <root>\bin\node.exe，其次才是 PATH）。
if ($nodeExe -and ([System.IO.Path]::GetFileName($nodeExe) -ieq "node.exe")) {
    New-Item -ItemType Directory -Path $binDst -Force | Out-Null
    Copy-Item $nodeExe (Join-Path $binDst "node.exe") -Force
    Ok "bin\node.exe bundled from $nodeExe"
} else {
    Warn "node.exe not found (or not a real exe) - Node plugins will need node in PATH"
}

# ---------------------------------------------------------------------------
Step "6/8" "Seed SDK / config / state (required at runtime)"
# 1) sdk: python plugins inject oct_sdk from <root>/sdk/python via PYTHONPATH.
$sdkDst = Join-Path $resRoot "sdk"
New-Item -ItemType Directory -Path $sdkDst -Force | Out-Null
Copy-Item (Join-Path $sdkSrc "*") -Destination $sdkDst -Recurse -Force
foreach ($c in @(Get-ChildItem $sdkDst -Recurse -Directory -Filter "__pycache__" -ErrorAction SilentlyContinue)) {
    Remove-Tree $c.FullName
}
Ok "sdk -> resources\sdk (oct_sdk.py python + node)"

# 2) config: host-side plugin preferences (capability-gateway provider chains).
$cfgDst = Join-Path $resRoot "config"
New-Item -ItemType Directory -Path $cfgDst -Force | Out-Null
if (Test-Path $cfgSrc) {
    Copy-Item $cfgSrc (Join-Path $cfgDst "user-settings.json") -Force
    Ok "config -> resources\config\user-settings.json"
} else {
    # 缺失也要落一个可用文件：能力网关的 provider 链、UI 默认页都从它读。
    $minimalCfg = @'
{
  "schemaVersion": 1,
  "plugins": {}
}
'@
    [System.IO.File]::WriteAllText((Join-Path $cfgDst "user-settings.json"), $minimalCfg,
        (New-Object System.Text.UTF8Encoding($false)))
    Warn "config\user-settings.json not found - wrote a minimal default"
}

# 2b) config/models-root.json：模型存储根（用户自行改盘符/目录，默认 D:\AI_Model）。
#     内核优先级：user-settings.json.modelsRoot > 本文件 root > 内置 DefaultModelsRoot。
$modelsRootDst = Join-Path $cfgDst "models-root.json"
if (Test-Path $modelsRootCfgSrc) {
    Copy-Item $modelsRootCfgSrc $modelsRootDst -Force
    Ok "config -> resources\config\models-root.json (reused user value)"
} else {
    $modelsRootDefault = @'
{
  "root": "D:\\AI_Model"
}
'@
    [System.IO.File]::WriteAllText($modelsRootDst, $modelsRootDefault,
        (New-Object System.Text.UTF8Encoding($false)))
    Ok "config -> resources\config\models-root.json (default D:\AI_Model)"
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

# 3b) state/overrides.json: 用户态生命周期覆盖（load_mode / 心跳 / 请求超时等）。
#     开发态能跑通的策略若写在这里，必须随包带走，否则打包后行为会悄悄变化。
if (Test-Path $overridesSrc) {
    Copy-Item $overridesSrc (Join-Path $stateDst "overrides.json") -Force
    Ok "state -> resources\state\overrides.json"
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
    # 默认（离线优先）：把来源目录里"已有可复用 venv"的 python 插件全部随包分发；
    # 只有拿不到现成环境的插件才需要首启联网安装。原 slim 策略仍可用 -NoPyEnv 收窄。
    $reusable = @($pyIds | Where-Object { Test-Path (Join-Path $devRuntime ($_ + "\venv\Scripts\python.exe")) })
    $shipEnv  = @($reusable)
    $lazyEnv  = @($pyIds | Where-Object { $shipEnv -notcontains $_ })
    Warn ("shipping " + $(if ($shipEnv.Count) { $shipEnv -join ", " } else { "<none>" }) +
          "; no local env (would install on 1st call): " + $(if ($lazyEnv.Count) { $lazyEnv -join ", " } else { "<none>" }))
}

# Bundle the uv-managed CPython 3.12 interpreter itself. Two reasons:
#   1) newly built venvs land here (UV_PYTHON_INSTALL_DIR is pointed at this
#      dir by the kernel) so no interpreter download is needed;
#   2) shipped venvs get their pyvenv.cfg `home` rewritten to this dir, so
#      they work on a machine that never had a uv-managed 3.12.
$bundledPyDir = $null
if (-not $NoPyEnv) {
    $srcPy = ""
    # 1) 优先复用来源目录里已有的随包解释器（<src>\runtime\python\<impl>\python.exe）：
    #    纯本地复制，不联网、不下载。
    foreach ($d in @(Get-ChildItem (Join-Path $devRuntime "python") -Directory -ErrorAction SilentlyContinue)) {
        $cand = Join-Path $d.FullName "python.exe"
        if (Test-Path $cand) { $srcPy = $cand; break }
    }
    # 2) 其次用本机已有的 uv 托管 3.12（仍是本地文件）
    if (-not $srcPy -and $uvExe) {
        $found = @(Capture-Quiet $uvExe @("python", "find", "3.12") | Where-Object { $_ -match 'python\.exe\s*$' })
        if ($LASTEXITCODE -eq 0 -and $found.Count -gt 0) { $srcPy = $found[0].ToString().Trim() }
    }
    if (-not $srcPy -or -not (Test-Path $srcPy)) {
        Die "cannot locate a CPython 3.12 to bundle (no $devRuntime\python, and 'uv python find 3.12' failed)"
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
    # 无 uv 时不替换启动器：来源 venv 若是 uv/venv 正常创建的，其 Scripts\python.exe
    # 已经是遵循 pyvenv.cfg `home` 的 venv launcher，改写 home 后即可用。
    $launcherDir = $null
    if ($uvExe) {
        $scratch = Join-Path $env:TEMP ("octpkg_venv_" + $PID)
        Remove-Tree $scratch
        Run-Quiet $uvExe @("venv", "--python", $pyExePath, $scratch)
        Assert-Native "uv venv (launcher scratch)"
        $launcherDir = Join-Path $scratch "Scripts"
    } else {
        Warn "uv not available - keeping the original venv launchers (pyvenv.cfg home is still rewritten)"
    }
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
        Ok ("{0,-16} reused from {1}" -f $id, $devVenv)
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
if ($scratch) { Remove-Tree $scratch }

# ---------------------------------------------------------------------------
Step "8/8" "Build admin launcher (OctPlugins.exe) with embedded UAC manifest"
$tmpDir = Join-Path $env:TEMP ("octpkg_" + $PID)
Remove-Tree $tmpDir
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
Remove-Tree $tmpDir

# ---------------------------------------------------------------------------
Write-Host ""
Write-Host "=== Verification ===" -ForegroundColor Cyan
$mustExist = @(
    "OctPlugins.exe",
    "OctPluginsHost.exe",
    "resources\app\package.json",
    "resources\app\index.html",
    "resources\app\browser-panel.js",
    "resources\app\vendor\chrome-tabs\chrome-tabs.js",
    "resources\app\build\main.js",
    "resources\app\build\renderer.js",
    "resources\app\node_modules\ws\package.json",
    "resources\kernel\kerneld.exe",
    "resources\sdk\python\oct_sdk\__init__.py",
    "resources\config\user-settings.json",
    "resources\config\models-root.json",
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
