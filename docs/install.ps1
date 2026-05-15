# radii5 installer - short URL entry point
# Usage: irm https://ohcass.github.io/music/install.ps1 | iex
$raw = "https://raw.githubusercontent.com/ohcass/music/main/scripts"
if ($PSVersionTable.PSVersion.Major -le 5) {
    Invoke-Expression (Invoke-RestMethod "$raw/install-ps5.ps1")
} else {
    Invoke-Expression (Invoke-RestMethod "$raw/install.ps1")
}
