param(
  [int]$Generators = 5,
  [int]$RequestsPerGenerator = 20000,
  [int]$Concurrency = 400,
  [int]$DurationSeconds = 10,
  [int]$RequestTimeoutSeconds = 30
)

$ErrorActionPreference = 'Stop'
$network = 'hi-x20_default'
$image = 'flashsale-loadgen:latest'
$startAt = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() + 5000
$logDir = Join-Path (Get-Location) 'outputs\benchmarks'
New-Item -ItemType Directory -Force $logDir | Out-Null

for ($i = 1; $i -le $Generators; $i++) {
  docker rm -f "loadgen$i" 2>$null | Out-Null
  docker run -d --name "loadgen$i" --network $network -e "LOADGEN_ID=gen$i" `
    --entrypoint /loadtest $image `
    -url http://nginx/api/v1/checkout `
    -requests $RequestsPerGenerator `
    -launch-window "${DurationSeconds}s" `
    -start-at $startAt `
    -concurrency $Concurrency `
    -request-timeout "${RequestTimeoutSeconds}s" | Out-Null
}

$lines = @()
for ($i = 1; $i -le $Generators; $i++) {
  docker wait "loadgen$i" | Out-Null
  Write-Output "--- loadgen$i ---"
  $line = docker logs "loadgen$i" 2>&1 | Select-Object -Last 1
  $line | Tee-Object -FilePath (Join-Path $logDir "loadgen$i.txt")
  $lines += $line
}

$sum = @{}
foreach ($line in $lines) {
  foreach ($m in [regex]::Matches($line, '(?<k>[a-z0-9_]+)=(?<v>[0-9]+)')) {
    if (-not $sum.ContainsKey($m.Groups['k'].Value)) { $sum[$m.Groups['k'].Value] = 0 }
    $sum[$m.Groups['k'].Value] += [int64]$m.Groups['v'].Value
  }
}
Write-Output ("aggregate requested={0} launched={1} completed={2} accepted_202={3} throttled_429={4} failed={5} in_flight={6}" -f $sum.requested,$sum.launched,$sum.completed,$sum.accepted_202,$sum.throttled_429,$sum.failed,$sum.in_flight)
