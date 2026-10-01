#!/usr/bin/env bash
# Packages plugin/ (manifest, MCP pointer, skills, assets) into the ZIP uploaded to
# ChatGPT (Plugins → Personal → upload) or to the plugin submission portal.
#
# Usage: deploy/build-plugin.sh   → dist/nuanu-odoo-plugin-<version>.zip
set -euo pipefail
cd "$(dirname "$0")/.."
python - <<'PY'
import json, pathlib, zipfile
root = pathlib.Path("plugin")
version = json.loads((root / "plugin.json").read_text(encoding="utf-8"))["version"]
out = pathlib.Path("dist") / f"nuanu-odoo-plugin-{version}.zip"
out.parent.mkdir(exist_ok=True)
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for f in sorted(root.rglob("*")):
        if f.is_file():
            z.write(f, f.relative_to(root).as_posix())  # plugin.json at the ZIP root
print(out.as_posix())
PY
