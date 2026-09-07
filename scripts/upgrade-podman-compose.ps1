# Installs podman-compose from upstream main.
#
# Released podman-compose 1.6.0 mis-detects a Windows build context ("C:\src")
# as a git URL, so it never passes -f to `podman build` and every build fails
# with "no Containerfile or Dockerfile specified or found in context directory".
# Upstream main fixes this; the fix is not in any release yet, so this script
# installs from git. Remove it once a release > 1.6.0 ships the fix.
#
#   pwsh ./scripts/upgrade-podman-compose.ps1

[CmdletBinding()]
param(
    [string]$Ref = 'main'
)

$ErrorActionPreference = 'Stop'

foreach ($tool in 'python', 'git') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool is required and was not found on PATH."
    }
}

# --force-reinstall is required: upstream main still reports version 1.6.0, so
# pip would otherwise treat the installed release as already satisfying it.
python -m pip install --user --force-reinstall --no-deps `
    "git+https://github.com/containers/podman-compose@$Ref"
if ($LASTEXITCODE -ne 0) { throw "pip install failed with exit code $LASTEXITCODE." }

$check = python -c "import podman_compose as p; print(p.is_context_git_url(r'C:\x'))"
if ($check.Trim() -ne 'False') {
    throw "podman-compose still treats Windows paths as git URLs. Check that '$((Get-Command podman-compose).Source)' resolves to the interpreter just upgraded."
}

Write-Host "podman-compose upgraded. Windows build contexts are handled correctly."
