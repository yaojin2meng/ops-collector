# 选择最快的 PyPI 镜像（中国大陆网络环境）。
# 用法：powershell -ExecutionPolicy Bypass -File scripts/choose-mirror.ps1
# 输出：最快的镜像 URL（写入 stdout），全部失败时回退官方源。
$mirrors = @(
    "https://mirrors.aliyun.com/pypi/simple/",
    "https://mirrors.cloud.tencent.com/pypi/simple/",
    "https://pypi.tuna.tsinghua.edu.cn/simple/",
    "https://mirrors.bfsu.edu.cn/pypi/simple/",
    "https://pypi.org/simple/"
)
$probe = "httpx/"
$best = $null
$bestMs = [int]::MaxValue
foreach ($m in $mirrors) {
    $url = $m + $probe
    try {
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $r = Invoke-WebRequest -Uri $url -Method Head -TimeoutSec 6 -UseBasicParsing -ErrorAction Stop
        $sw.Stop()
        if ($r.StatusCode -eq 200 -and $sw.ElapsedMilliseconds -lt $bestMs) {
            $best = $m
            $bestMs = $sw.ElapsedMilliseconds
        }
        Write-Host ("OK   {0}  {1}ms" -f $m, $sw.ElapsedMilliseconds) -ForegroundColor DarkGray
    }
    catch {
        $code = $null
        try { $code = [int]$_.Exception.Response.StatusCode } catch {}
        Write-Host ("FAIL {0}  HTTP {1}" -f $m, $code) -ForegroundColor DarkYellow
    }
}
if (-not $best) { $best = $mirrors[-1]; Write-Host "全部镜像失败，回退官方源" }
Write-Host ("SELECTED {0} ({1}ms)" -f $best, $bestMs)
Write-Output $best
