#Requires -Version 7.4
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$buildNames = @('GOOS','GOARCH','CGO_ENABLED','TMP','TEMP','GOMAXPROCS','GOFLAGS','GOMEMLIMIT')
$previous = @{}
foreach ($name in $buildNames) { $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
Push-Location $PSScriptRoot
try {
    $env:GOMAXPROCS = '2'
    $env:GOFLAGS = '-p=1'
    $env:GOMEMLIMIT = '1GiB'
    go test ./...
    go vet ./...
    pnpm --dir web install --frozen-lockfile
    pnpm --dir web build
    $env:CGO_ENABLED = '0'
    foreach ($platform in @('linux/amd64','linux/arm64','darwin/amd64','darwin/arm64','windows/amd64')) {
        $env:GOOS, $env:GOARCH = $platform.Split('/')
        $binaryDirectory = Join-Path $PSScriptRoot "dist/$platform/bin"
        New-Item -ItemType Directory -Path $binaryDirectory -Force | Out-Null
        $binaryName = if ($env:GOOS -eq 'windows') { 'plugin.exe' } else { 'plugin' }
        go build -trimpath -ldflags='-s -w' -o (Join-Path $binaryDirectory $binaryName) ./cmd/plugin
    }
    $env:GOOS = $previous['GOOS']
    $env:GOARCH = $previous['GOARCH']
    $packageTemp = Join-Path $PSScriptRoot 'dist/tmp'
    New-Item -ItemType Directory -Path $packageTemp -Force | Out-Null
    $env:TMP = $packageTemp
    $env:TEMP = $packageTemp
    go tool trplugin pack --manifest plugin.json --frontend dist/frontend --bin-dir dist --out release
    foreach ($package in Get-ChildItem -LiteralPath release -Filter '*.tar.gz') {
        go tool trplugin validate $package.FullName
    }
    go list -m -json github.com/TokenFlux/TokenRouter/plugin-sdk/go
} finally {
    foreach ($name in $buildNames) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    Pop-Location
}
