# Builds openvpn-aws.exe and tapctl.exe on Windows: upstream OpenVPN with
# Hostbay's AWS Client VPN patch (openvpn-2.7.9-aws.patch), built the way
# OpenVPN's README.cmake.md describes (MSVC, CMake and vcpkg) but with the
# x64-windows-static triplet and the static C runtime, so neither program
# needs a DLL beside it. tapctl.exe creates the ovpn-dco adapter.
#
# Usage: build.ps1 [-SourceArchive <file.zip>] <output dir>
#
# Run it from a Developer PowerShell for Visual Studio 2022 or newer (x64) with CMake
# 3.21 or newer, Git and Python 3 on PATH. The OpenVPN tarball is checked
# against versions.env before it is unpacked. OpenSSL, LZO, LZ4 and the
# TAP-Windows headers come from vcpkg at the commit versions.env pins.
# -SourceArchive also writes a zip of the OpenVPN tarball, the library
# sources and ports vcpkg used, the patch and these build files.
param(
    [Parameter(Mandatory = $true, Position = 0)][string]$OutDir,
    [string]$SourceArchive
)
$ErrorActionPreference = 'Stop'

# Windows PowerShell 5.1 does not stop on a failing program, so every
# program runs through Invoke-Checked. Arguments that start with a dash are
# quoted where PowerShell could read them as its own parameters.
function Invoke-Checked {
    $exe, $rest = $args
    & $exe @rest
    if ($LASTEXITCODE -ne 0) { throw "$exe exited with $LASTEXITCODE" }
}

$v = @{}
foreach ($line in Get-Content (Join-Path $PSScriptRoot 'versions.env')) {
    if ($line -match '^([A-Z0-9_]+)=(.*)$') { $v[$Matches[1]] = $Matches[2] }
}
$ver = $v['OPENVPN_VERSION']

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path
if ($SourceArchive) { $SourceArchive = [IO.Path]::GetFullPath($SourceArchive) }
$work = Join-Path ([IO.Path]::GetTempPath()) ("openvpn-aws-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null

try {
    # The tarball is checked before it is unpacked, so a tampered or
    # replaced upstream file never reaches tar or the compiler.
    $tarball = Join-Path $work ([IO.Path]::GetFileName($v['OPENVPN_URL']))
    Invoke-Checked curl.exe -fsSL --retry 3 -o $tarball $v['OPENVPN_URL']
    $got = (Get-FileHash -Algorithm SHA256 $tarball).Hash.ToLower()
    if ($got -ne $v['OPENVPN_SHA256']) {
        throw "openvpn-$ver.tar.gz has SHA-256 $got but versions.env pins $($v['OPENVPN_SHA256']); not unpacking it"
    }
    Invoke-Checked tar.exe -xzf $tarball -C $work
    $src = Join-Path $work "openvpn-$ver"

    # Git may have checked the patch out with CRLF line ends; the sources
    # use LF, so the patch is applied from an LF copy. git apply refuses a
    # hunk whose context does not match exactly, so a point release that
    # moves the patched code stops the build.
    $patch = Join-Path $work 'aws.patch'
    $text = (Get-Content -Raw (Join-Path $PSScriptRoot "openvpn-$ver-aws.patch")) -replace "`r`n", "`n"
    [IO.File]::WriteAllText($patch, $text)
    Invoke-Checked git -C $src apply -p1 $patch

    # vcpkg at the pinned commit, fetched without its history.
    $vcpkg = Join-Path $work 'vcpkg'
    Invoke-Checked git init -q $vcpkg
    Invoke-Checked git -C $vcpkg fetch -q --depth 1 https://github.com/microsoft/vcpkg.git $v['VCPKG_COMMIT']
    Invoke-Checked git -C $vcpkg checkout -q FETCH_HEAD
    Invoke-Checked (Join-Path $vcpkg 'bootstrap-vcpkg.bat') -disableMetrics

    # OpenVPN's own manifest (contrib/vcpkg-manifests/windows) also pulls in
    # pkcs11-helper and cmocka, which this build leaves out.
    $manifest = Join-Path $work 'manifest'
    New-Item -ItemType Directory -Path $manifest | Out-Null
    [IO.File]::WriteAllText((Join-Path $manifest 'vcpkg.json'), @'
{
  "name": "openvpn-aws",
  "version-string": "1",
  "dependencies": [
    "openssl",
    "lzo",
    "lz4",
    "tap-windows6",
    { "name": "pkgconf", "host": true }
  ]
}
'@)

    # OpenVPN's CMakeLists asks for CMake 3.14, which predates
    # CMAKE_MSVC_RUNTIME_LIBRARY, so policy CMP0091 is switched on here.
    # struct user_pass grows to 256 KB with the patch and several sit on the
    # stack; MSVC's default 1 MB stack is raised to the 8 MB macOS and Linux
    # give their main thread.
    # vcpkg must build every library from the source it downloads here:
    # a binary cache would skip the download and leave the source archive
    # without it.
    $env:VCPKG_BINARY_SOURCES = 'clear'
    $build = Join-Path $work 'build'
    # No -G: CMake picks the newest Visual Studio installed, which GitHub's
    # runners move ahead of a pinned version.
    Invoke-Checked cmake -S $src -B $build -A x64 `
        "-DCMAKE_TOOLCHAIN_FILE=$vcpkg\scripts\buildsystems\vcpkg.cmake" `
        "-DVCPKG_MANIFEST_DIR=$manifest" `
        "-DVCPKG_INSTALLED_DIR=$work\vcpkg_installed" `
        '-DVCPKG_TARGET_TRIPLET=x64-windows-static' `
        '-DVCPKG_HOST_TRIPLET=x64-windows' `
        '-DCMAKE_POLICY_DEFAULT_CMP0091=NEW' `
        '-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded$<$<CONFIG:Debug>:Debug>' `
        '-DCMAKE_EXE_LINKER_FLAGS=/STACK:8388608' `
        '-DENABLE_PKCS11=OFF' `
        '-DBUILD_TESTING=OFF' `
        '-DUSE_WERROR=OFF'
    Invoke-Checked cmake --build $build --config Release --target openvpn tapctl

    Copy-Item (Join-Path $build 'Release\openvpn.exe') (Join-Path $OutDir 'openvpn-aws.exe')
    Copy-Item (Join-Path $build 'src\tapctl\Release\tapctl.exe') (Join-Path $OutDir 'tapctl.exe')

    # Smoke checks: the right version, and no DLL that is not Windows' own.
    $exe = Join-Path $OutDir 'openvpn-aws.exe'
    $first = (& $exe --version | Select-Object -First 1)
    if ($first -notlike "OpenVPN $ver *") { throw "openvpn-aws.exe --version printed: $first" }
    Write-Output $first
    # OpenSSL reads openssl.cnf, providers and engines from the paths
    # compiled into it, and the helper runs openvpn-aws.exe as SYSTEM. vcpkg
    # leaves OpenSSL's defaults, which are under C:\Program Files, where only
    # administrators can write; a path elsewhere would let a user plant one.
    $strings = [Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes($exe))
    $dirs = [regex]::Matches($strings, '(OPENSSLDIR|MODULESDIR|ENGINESDIR): "([^"]*)"') |
        ForEach-Object { $_.Groups[1].Value + '=' + $_.Groups[2].Value } | Sort-Object -Unique
    $safe = @($dirs | Where-Object { $_ -like '*=C:\Program Files\*' })
    if ($dirs.Count -ne 3 -or $safe.Count -ne 3) {
        throw "OpenSSL's built-in paths in openvpn-aws.exe are not all under C:\Program Files (check by hand): $($dirs -join ', ')"
    }
    foreach ($bin in @($exe, (Join-Path $OutDir 'tapctl.exe'))) {
        $deps = Invoke-Checked dumpbin.exe /nologo /dependents $bin
        $foreign = $deps | Where-Object { $_ -match '^\s+\S+\.dll\s*$' -and $_ -match '(?i)libcrypto|libssl|lzo|lz4|vcruntime|msvcp|ucrtbase|api-ms-win-crt' }
        if ($foreign) { throw "$bin links DLLs that are not Windows' own: $($foreign -join ', ')" }
    }

    if ($SourceArchive) {
        $bundle = Join-Path $work "openvpn-aws-$ver-source"
        New-Item -ItemType Directory -Path $bundle, "$bundle\vcpkg-downloads", "$bundle\vcpkg-ports" | Out-Null
        Copy-Item $tarball, (Join-Path $PSScriptRoot "openvpn-$ver-aws.patch"), `
            (Join-Path $PSScriptRoot 'versions.env'), (Join-Path $PSScriptRoot 'build.sh'), `
            $PSCommandPath, (Join-Path $PSScriptRoot 'README.md') $bundle
        # The library sources as vcpkg downloaded them, and the ports whose
        # portfiles and patches it applied to them.
        $libs = Get-ChildItem (Join-Path $vcpkg 'downloads') -File |
            Where-Object { $_.Name -match '^(openssl|lzo|lz4|tap-windows6|OpenVPN-tap-windows6)' }
        foreach ($lib in 'openssl', 'lzo', 'lz4') {
            if (-not ($libs | Where-Object Name -like "$lib*")) { throw "vcpkg downloaded no $lib source to archive" }
        }
        $libs | Copy-Item -Destination "$bundle\vcpkg-downloads"
        foreach ($port in 'openssl', 'lzo', 'lz4', 'tap-windows6') {
            Copy-Item -Recurse (Join-Path $vcpkg "ports\$port") "$bundle\vcpkg-ports\$port"
        }
        Compress-Archive -Force -Path $bundle -DestinationPath $SourceArchive
        Write-Output "wrote $SourceArchive"
    }
    Write-Output "wrote $OutDir\openvpn-aws.exe and $OutDir\tapctl.exe"
}
finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $work
}
