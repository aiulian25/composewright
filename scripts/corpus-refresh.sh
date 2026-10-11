#!/usr/bin/env bash
# Re-download the pinned corpus snapshots listed in testdata/corpus/corpus.tsv.
# Needs network; run by hand when a pin changes. Usage: scripts/corpus-refresh.sh [slug...]
set -euo pipefail
cd "$(dirname "$0")/.."
go build -o bin/composewright ./cmd/composewright
while IFS=$'\t' read -r slug repository commit registries verdict; do
	[[ -z "$slug" || "$slug" == \#* ]] && continue
	if [[ $# -gt 0 && ! " $* " == *" $slug "* ]]; then continue; fi
	target="testdata/corpus/$slug"
	rm -rf "$target/files"
	mkdir -p "$target/files"
	curl -fsSL --proto '=https' "https://codeload.github.com/$repository/tar.gz/$commit" |
		bin/composewright debug untar --out "$target/files"
	owner="${repository%%/*}"
	printf '{\n  "url": "https://github.com/%s",\n  "owner": "%s",\n  "commit": "%s",\n  "allow_registries": "%s",\n  "verdict": "%s"\n}\n' \
		"$repository" "$owner" "$commit" "$registries" "$verdict" >"$target/source.json"
done <testdata/corpus/corpus.tsv
