param([string]$Source, [string]$OutDir)
$ErrorActionPreference = 'Stop'
# Load only the two pure functions; never invoke the build/start main body.
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($Source, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw $errors[0] }
foreach ($name in @('To-JsonString', 'Set-DevConfigValue')) {
  $fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name }, $true)
  if (-not $fn) { throw "Missing function: $name" }
  . ([scriptblock]::Create($fn.Extent.Text))
}
$cases = @(
  @{ name='control-path'; text='{}' },
  @{ name='empty'; text='{}' },
  @{ name='nested-only'; text='{"nested":{"dataDir":"keep-data","workspace":"keep-ws"},"unknown":[1,true,null]}' },
  @{ name='top-and-nested'; text='{"dataDir":"old","nested":{"dataDir":"keep-data","workspace":"keep-ws"},"workspace":"old-ws","tail":42}' },
  @{ name='non-string-top'; text='{"dataDir":{"old":[1,2]},"workspace":null,"tail":42}' },
  @{ name='escaped-key'; text='{"data\u0044ir":"old","workspace":"old-ws","tail":42}' },
  @{ name='braces-in-string'; text='{"note":"comma, brace } and quote \"","dataDir":"old"}' }
)
$failed = 0
foreach ($case in $cases) {
  Write-Output "=== RUN   L16/$($case.name)"
  try {
    $data = 'C:\dev\data'
    if ($case.name -eq 'control-path') { $data = "C:\dev\data`nline`tend" }
    $ws = 'C:\dev\workspace'
    $fixed = Set-DevConfigValue (Set-DevConfigValue $case.text 'dataDir' $data) 'workspace' $ws
    # A separate strict JSON parser checks the exact output after this process exits.
    $fixed | Set-Content -LiteralPath (Join-Path $OutDir ($case.name + '.json')) -Encoding UTF8
    $obj = ConvertFrom-Json -InputObject $fixed
    if ($obj.dataDir -cne $data -or $obj.workspace -cne $ws) { throw 'Top-level paths were not updated exactly' }
    if ($obj.nested -and ($obj.nested.dataDir -cne 'keep-data' -or $obj.nested.workspace -cne 'keep-ws')) { throw 'Nested keys were modified' }
    if ($case.text.Contains('"tail":42') -and -not $fixed.Contains('"tail":42')) { throw 'Unknown field text was changed' }
    if ($case.text.Contains('"unknown":[1,true,null]') -and -not $fixed.Contains('"unknown":[1,true,null]')) { throw 'Unknown array text was changed' }
    if ($case.text.Contains('"nested"') -and $fixed.IndexOf('"nested"') -gt $fixed.IndexOf('"tail"') -and $obj.tail) { throw 'Field order changed' }
    if ((Set-DevConfigValue (Set-DevConfigValue $fixed 'dataDir' $data) 'workspace' $ws) -cne $fixed) { throw 'Update must be idempotent' }
    Write-Output "--- PASS: L16/$($case.name)"
  } catch {
    $failed++
    Write-Output "--- FAIL: L16/$($case.name): $($_.Exception.Message)"
  }
}
if ($failed) { exit 1 }
exit 0