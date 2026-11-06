#!/usr/bin/env bash
# Scan the immutable OCI archive, never a mutable registry tag or a rebuild.
set -euo pipefail
archive="${1:?usage: scan-candidate.sh OCI_ARCHIVE EVIDENCE_DIRECTORY}"
evidence="${2:?evidence directory required}"
mkdir -p "$evidence"
trivy --version >"$evidence/scanner-version.log"
# No inherited config/waivers: all findings remain visible in the retained
# report; HIGH/CRITICAL (including unfixed) stop publication.
export TRIVY_CONFIG=/dev/null
trivy image --input "$archive" --scanners vuln --ignorefile .trivyignore.yaml \
	--list-all-pkgs --format json --output "$evidence/vulnerabilities.json" --exit-code 0
trivy image --input "$archive" --scanners vuln --ignorefile .trivyignore.yaml \
	--format cyclonedx --output "$evidence/sbom.cdx.json" --exit-code 0
# A successful scanner invocation that missed a shipped executable is not
# sufficient evidence. Keep installed package versions for applicability review.
jq -e '.SchemaVersion == 2 and (.Results | type == "array")' "$evidence/vulnerabilities.json" >/dev/null
for binary in usr/bin/composewright usr/local/bin/docker usr/local/libexec/docker/cli-plugins/docker-compose; do
	jq -e --arg binary "$binary" 'any(.Results[]; .Type == "gobinary" and
		((.Target | split(" (")[0] | ltrimstr("/")) == $binary) and (.Packages | length > 0))' \
		"$evidence/vulnerabilities.json" >/dev/null
done
jq -e '[.Results[]?.Vulnerabilities[]? | select(.Severity == "HIGH" or .Severity == "CRITICAL")] | length == 0' \
	"$evidence/vulnerabilities.json"
