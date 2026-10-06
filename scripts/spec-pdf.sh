#!/usr/bin/env bash
# Renders SPEC.md to SPEC.pdf (A4, 10pt body text, 15 mm margins) with headless
# Chrome and reports the page count; the brief asks for two pages. Needs
# python3 with the `markdown` module, and Google Chrome (override its path with
# CHROME=...).
set -euo pipefail
cd "$(dirname "$0")/.."
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
TMP="$(mktemp -d)"
CHROME_PID=""
cleanup() {
  if [ -n "$CHROME_PID" ]; then kill "$CHROME_PID" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

python3 - SPEC.md "$TMP/spec.html" <<'EOF'
import pathlib, sys, markdown
html = markdown.markdown(pathlib.Path(sys.argv[1]).read_text(), extensions=["tables", "fenced_code"])
css = """
@page { size: A4; margin: 15mm 15mm; }
body { font-family: 'Helvetica Neue', Arial, sans-serif; font-size: 10pt; line-height: 1.3; color: #111; }
h1 { font-size: 15pt; margin: 0 0 4pt; }
h2 { font-size: 10.5pt; margin: 8pt 0 2pt; }
p { margin: 2pt 0 4pt; }
ul, ol { padding-left: 14pt; margin: 2pt 0 4pt; }
li { margin: 0; }
pre { font-size: 8pt; line-height: 1.18; margin: 3pt 0; background: #f4f4f4; padding: 3pt 5pt; }
code { font-family: Menlo, monospace; font-size: 8.8pt; }
pre code { font-size: 8pt; }
table { border-collapse: collapse; margin: 3pt 0 5pt; font-size: 9.2pt; }
th, td { border: 0.5pt solid #999; padding: 1.2pt 4pt; text-align: left; vertical-align: top; }
"""
pathlib.Path(sys.argv[2]).write_text(
    f"<!doctype html><meta charset='utf-8'><title>BH/1</title><style>{css}</style>{html}")
EOF

rm -f SPEC.pdf
# Its own profile, and cleanup by PID only: never touch the user's Chrome.
"$CHROME" --headless=new --disable-gpu --no-pdf-header-footer --no-first-run \
  --user-data-dir="$TMP/profile" --print-to-pdf="$PWD/SPEC.pdf" "file://$TMP/spec.html" >/dev/null 2>&1 &
CHROME_PID=$!
for _ in $(seq 120); do
  [ -s SPEC.pdf ] && break
  sleep 0.5
done
sleep 1
kill "$CHROME_PID" 2>/dev/null || true
sleep 1
kill -9 "$CHROME_PID" 2>/dev/null || true
CHROME_PID=""

[ -s SPEC.pdf ] || { echo "Chrome produced no PDF" >&2; exit 1; }
python3 -c 'import re; print("SPEC.pdf:", len(re.findall(rb"/Type\s*/Page[^s]", open("SPEC.pdf", "rb").read())), "pages")'
