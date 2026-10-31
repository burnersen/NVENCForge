# NVENCForge (https://github.com/burnersen/NVENCForge)
# Copyright (C) 2026 burnersen
# SPDX-License-Identifier: GPL-3.0-only
#
# Checks the Windows files a release is about to sign or publish:
#
#   - every file exists,
#   - its file properties carry exactly the product name and product version
#     that the SignPath artifact configuration enforces (product-name,
#     product-version). SignPath refuses a signing request whose files do not
#     match, so this check runs on every release build, signed or not: a
#     mismatch shows up long before the first signed release.
#   - with -Signed: every file carries a valid Authenticode signature.
#
# Usage (from the release workflow):
#   ./.signpath/check-files.ps1 -ProductName X -ProductVersion 1.2.3 -Path a.exe, b.exe [-Signed]

param(
    [Parameter(Mandatory)] [string[]] $Path,
    [Parameter(Mandatory)] [string] $ProductName,
    [Parameter(Mandatory)] [string] $ProductVersion,
    [switch] $Signed
)

$ErrorActionPreference = 'Stop'
$failed = 0

foreach ($file in $Path) {
    if (-not (Test-Path -LiteralPath $file -PathType Leaf)) {
        Write-Host "::error::$file is missing"
        $failed++
        continue
    }
    $info = (Get-Item -LiteralPath $file).VersionInfo
    $name = "$($info.ProductName)".Trim()
    $version = "$($info.ProductVersion)".Trim()
    if ($name -ne $ProductName) {
        Write-Host "::error::$file has product name '$name', expected '$ProductName'"
        $failed++
    }
    if ($version -ne $ProductVersion) {
        Write-Host "::error::$file has product version '$version', expected '$ProductVersion'"
        $failed++
    }
    $line = "{0}: {1} {2}" -f $file, $name, $version
    if ($Signed) {
        $signature = Get-AuthenticodeSignature -LiteralPath $file
        if ($signature.Status -ne 'Valid') {
            Write-Host "::error::$file is not validly signed ($($signature.Status): $($signature.StatusMessage))"
            $failed++
        } else {
            $line += ", signed by $($signature.SignerCertificate.Subject)"
        }
    }
    Write-Host $line
}

if ($failed -gt 0) {
    Write-Host "::error::$failed problem(s) with the release files"
    exit 1
}
