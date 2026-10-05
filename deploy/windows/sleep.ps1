# Puts the machine into S3 sleep with wake events armed (third argument must stay $false).
Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Application]::SetSuspendState('Suspend', $false, $false) | Out-Null
