#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
case "${1-}" in ''|--release) ;; *) echo 'usage: release-check.sh [--release]' >&2; exit 2;; esac
python3 - <<'PY'
import json,pathlib,re
root=pathlib.Path('.')
manifest=json.loads((root/'plugins/harvester/.claude-plugin/plugin.json').read_text())
version=(root/'VERSION').read_text().strip()
assert version and version==manifest['version'], 'VERSION and plugin.json disagree'
for path in ['plugins/harvester/.codex-plugin/plugin.json', 'plugins/harvester/package.json']:
    assert json.loads((root/path).read_text())['version']==version, f'VERSION and {path} disagree'
assert manifest.get('mcpServers', {}).get('harvester') == {'type':'stdio','command':'${CLAUDE_PLUGIN_ROOT}/libexec/harvester','args':['mcp']}, 'plugin.json must register the standalone harvester MCP door'
assert re.fullmatch(r'[0-9A-Za-z._+-]+',version) and version not in ('.','..'), 'unsafe version'
sums=(root/'plugins/harvester/libexec/SHA256SUMS').read_text().splitlines()
expected={f'harvester_{version}_{os}_{arch}' for os in ['linux','darwin'] for arch in ['amd64','arm64']}
parsed={}
for line in sums:
    checksum,asset=line.split()
    assert re.fullmatch(r'[0-9a-f]{64}',checksum),f'invalid checksum for {asset}'
    assert asset not in parsed,f'duplicate checksum for {asset}'
    parsed[asset]=checksum
assert set(parsed)==expected,'SHA256SUMS does not contain exactly the four versioned release assets'
print(f'release-check: version/metadata PASS {version}')
PY
if [[ "${1-}" == --release ]]; then
  temporary=$(mktemp -d)
  trap 'rm -rf "$temporary"' EXIT
  scripts/build-release.sh --out "$temporary"
  diff -u plugins/harvester/libexec/SHA256SUMS "$temporary/SHA256SUMS"
fi
