#!/usr/bin/env bash
# No colour outside web/static/tokens.css: templates, stylesheets and scripts
# use the semantic tokens, so both themes stay in step and the contrast test
# sees every colour. Inline styles are refused as well, because the CSP
# forbids them. The favicon keeps the logo's own colours: the browser draws
# it outside the page, where tokens do not reach.
set -euo pipefail
cd "$(dirname "$0")/.."

roots=()
for root in web internal/web; do
  [[ -d "$root" ]] && roots+=("$root")
done
[[ ${#roots[@]} -eq 0 ]] && exit 0

literal='(?<![\w&])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b'
functions='\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\s*\(|\bcolor\s*\('
named=':\s*[^;{}]*\b(?:white|black|red|green|blue|yellow|orange|purple|gray|grey|silver|navy|teal|maroon|olive|lime|aqua|fuchsia|pink|brown|gold|cyan|magenta)\b'
inline='\sstyle\s*=|<style\b'

pattern="${literal}|${functions}|${named}|${inline}"

# Fragment links such as href="#add" look like hex colours; they are removed
# before the second match, so a colour elsewhere on the same line still counts.
findings=$(grep -rnP --include='*.html' --include='*.css' --include='*.js' --include='*.svg' "$pattern" "${roots[@]}" |
  grep -v '^web/static/tokens\.css:' | grep -v '^web/static/favicon\.svg:' | sed -E 's/(xlink:)?href="#[A-Za-z0-9_-]+"//g' | grep -P "$pattern" || true)

if [[ -n "$findings" ]]; then
  echo "colour literals or inline styles outside web/static/tokens.css:" >&2
  echo "$findings" >&2
  exit 1
fi
