#!/usr/bin/env python3
"""Exercise release refusal and failed-run cleanup without a Docker daemon."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
BINARIES = ["usr/bin/composewright", "usr/local/bin/docker",
            "usr/local/libexec/docker/cli-plugins/docker-compose"]


class Gates(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.folder = Path(self.temp.name)
        self.env = dict(os.environ, PATH=f"{self.folder}:{os.environ['PATH']}",
                        FIXTURE=str(self.folder / "fixture.json"),
                        STATE=str(self.folder / "state"),
                        CALLS=str(self.folder / "calls"))

    def executable(self, name, source):
        path = self.folder / name
        path.write_text(source)
        path.chmod(0o755)

    def scan(self, report):
        (self.folder / "fixture.json").write_text(json.dumps(report))
        self.executable("trivy", """#!/usr/bin/env python3
import os,sys,shutil
if '--version' in sys.argv: print('fixture scanner'); sys.exit(0)
if os.environ.get('FAIL_SCAN'): sys.exit(3)
out=sys.argv[sys.argv.index('--output')+1]
shutil.copyfile(os.environ['FIXTURE'],out)
""")
        return subprocess.run(["bash", "scripts/scan-candidate.sh", "candidate.tar",
                               str(self.folder / "evidence")], cwd=ROOT,
                              env=self.env, capture_output=True)

    def report(self, severity=None):
        results = [{"Target": binary + " (gobinary)", "Type": "gobinary",
                    "Packages": [{"Name": "stdlib", "Version": "test"}],
                    "Vulnerabilities": []} for binary in BINARIES]
        if severity:
            results[0]["Vulnerabilities"] = [{"Severity": severity}]
        return {"SchemaVersion": 2, "Results": results}

    def test_scan_accepts_complete_clean_evidence(self):
        for severity in (None, "LOW", "MEDIUM"):
            with self.subTest(severity=severity):
                self.assertEqual(self.scan(self.report(severity)).returncode, 0)
                self.assertTrue((self.folder / "evidence/sbom.cdx.json").exists())

    def test_scan_refuses_high_and_critical_including_unfixed(self):
        for severity in ("HIGH", "CRITICAL"):
            with self.subTest(severity=severity):
                self.assertNotEqual(self.scan(self.report(severity)).returncode, 0)
                self.assertTrue((self.folder / "evidence/vulnerabilities.json").exists())

    def test_scan_refuses_missing_binary_package_versions_or_schema(self):
        incomplete = self.report()
        incomplete["Results"].pop()
        no_versions = self.report()
        no_versions["Results"][1]["Packages"] = []
        for report in ({}, {"SchemaVersion": 2, "Results": []}, incomplete, no_versions):
            with self.subTest(report=report):
                self.assertNotEqual(self.scan(report).returncode, 0)

    def test_scanner_failure_is_not_a_pass(self):
        self.env["FAIL_SCAN"] = "1"
        self.assertNotEqual(self.scan(self.report()).returncode, 0)

    def test_failed_daemon_creation_cleans_up_and_retains_logs(self):
        # Simulate an API failure after creating the outer daemon. The trap
        # must remove that daemon without any host-wide resource operation.
        self.executable("docker", """#!/usr/bin/env python3
import os,sys
from pathlib import Path
args=sys.argv[1:]; state=Path(os.environ['STATE'])
with open(os.environ['CALLS'],'a') as calls: calls.write(' '.join(args)+'\\n')
if args[0]=='run': state.touch(); sys.exit(1)
if args[0]=='inspect': sys.exit(0 if state.exists() else 1)
if args[0]=='logs': print('retained daemon failure')
if args[0]=='rm': state.unlink(missing_ok=True)
""")
        logs = self.folder / "logs"
        self.env["COMPOSEWRIGHT_TEST_LOGS"] = str(logs)
        result = subprocess.run(["bash", "scripts/container-smoke.sh", "candidate:test"],
                                cwd=ROOT, env=self.env, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.folder / "state").exists())
        calls = (self.folder / "calls").read_text()
        self.assertIn("rm -fv composewright-check-", calls)
        self.assertNotIn("prune", calls)
        self.assertIn("retained daemon failure", (logs / "daemon.log").read_text())
        for kind in ("containers", "volumes", "networks"):
            self.assertTrue((logs / f"final-{kind}.log").exists())


if __name__ == "__main__":
    unittest.main()
