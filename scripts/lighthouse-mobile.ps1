param([int]$Runs = 3, [string]$Url = 'http://host.docker.internal:8080/')
$ErrorActionPreference = 'Stop'
$out = Join-Path (Get-Location) 'outputs\benchmarks\lighthouse'
New-Item -ItemType Directory -Force $out | Out-Null
for ($i=1; $i -le $Runs; $i++) {
  docker run --rm --add-host=host.docker.internal:host-gateway -v "${out}:/reports" node:22-bookworm bash -lc "apt-get update -qq && apt-get install -y -qq chromium >/dev/null && npx --yes lighthouse $Url --output=json --output-path=/reports/run-$i.json --chrome-path=/usr/bin/chromium --chrome-flags='--headless --no-sandbox' --preset=perf --screenEmulation.disabled=false --emulated-form-factor=mobile --throttling.cpuSlowdownMultiplier=4 --throttling.rttMs=150 --throttling.throughputKbps=1600 --viewport.width=390 --viewport.height=844"
}
Write-Output "Lighthouse reports written to $out"
