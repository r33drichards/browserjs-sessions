#!/usr/bin/env python3
"""Exercise the release's failure paths without a production cluster."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
OLD = 'registry/site@sha256:' + '1' * 64
NEW = 'registry/site@sha256:' + '2' * 64

STUB = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
state = Path(os.environ['STUB_STATE'])
if args[:2] == ['-n', 'browserjs-sessions']: args = args[2:]
with open(os.environ['STUB_LOG'], 'a') as f: f.write(' '.join(args) + '\n')
if args[:2] == ['get', 'rollouts.argoproj.io'] and os.environ.get('NO_ROLLOUT'): sys.exit(1)
if args[:2] == ['get', 'deployment']:
    if os.environ.get('API_FAILURE'): sys.exit(1)
    if os.environ.get('MISSING'): sys.exit(0)
    print(json.dumps({'spec': {'template': {'spec': {'containers': [{'name': 'site', 'image': state.read_text()}]}}}}))
elif args[:2] == ['set', 'image']: state.write_text(args[3].split('=', 1)[1])
elif args[:2] == ['get', 'pods']:
    digest = state.read_text().split('@')[1]
    if os.environ.get('WRONG_PODS'): digest = 'sha256:' + '0' * 64
    print(json.dumps({'items': [{'metadata': {}, 'status': {'phase': 'Running', 'containerStatuses': [{'name': 'site', 'imageID': 'repo@' + digest}]}}]}))
elif args[:2] == ['create', 'configmap']: print('{}')
elif args[:1] == ['apply']: sys.stdin.read()
'''
RELEASE = r'''#!/usr/bin/env bash
if [ "$1" = pinned ]; then
  echo "site registry/site@sha256:1111111111111111111111111111111111111111111111111111111111111111"
else
  echo "rollout $*" >> "$STUB_LOG"
  if [ -n "${FAIL_NEW:-}" ] && [[ "$(cat "$STUB_STATE")" == *@sha256:222* ]]; then exit 1; fi
fi
'''


class Release(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for directory in ['hack', 'bin', 'deploy/gke']:
            (self.root / directory).mkdir(parents=True)
        shutil.copy(ROOT / 'hack/site-release.sh', self.root / 'hack/site-release.sh')
        for path, content in [('bin/kubectl', STUB), ('hack/release.sh', RELEASE)]:
            target = self.root / path
            target.write_text(content)
            target.chmod(0o755)
        (self.root / 'state').write_text(OLD)
        self.manifest = self.root / 'deploy/gke/kustomization.yaml'
        self.manifest.write_text('images:\n  - name: browserjs/site\n    newName: registry/site\n    digest: sha256:' + '0' * 64 + '\n  - name: browserjs/backend\n    newName: registry/backend\n    digest: sha256:' + '3' * 64 + '\n')
        self.env = {**os.environ, 'PATH': str(self.root / 'bin') + ':' + os.environ['PATH'],
                    'STUB_STATE': str(self.root / 'state'), 'STUB_LOG': str(self.root / 'calls'),
                    'GITHUB_SHA': 'a' * 40}

    def run_release(self, *args, **env):
        return subprocess.run(['bash', 'hack/site-release.sh', *args], cwd=self.root,
                              env={**self.env, **env}, text=True, capture_output=True)

    def test_preserve_does_not_revert_site_or_change_backend(self):
        result = self.run_release('preserve')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('digest: sha256:' + '1' * 64, self.manifest.read_text())
        self.assertIn('digest: sha256:' + '3' * 64, self.manifest.read_text())

    def test_preserve_supports_rollback_tree(self):
        target = self.root / 'rollback/deploy/gke'
        target.mkdir(parents=True)
        shutil.copy(self.manifest, target / 'kustomization.yaml')
        self.assertEqual(self.run_release('preserve', 'rollback').returncode, 0)
        self.assertIn('digest: sha256:' + '1' * 64, (target / 'kustomization.yaml').read_text())
        self.assertIn('digest: sha256:' + '0' * 64, self.manifest.read_text())

    def test_initial_deployment_is_allowed_but_api_failure_is_not(self):
        before = self.manifest.read_text()
        self.assertEqual(self.run_release('preserve', MISSING='1').returncode, 0)
        self.assertNotEqual(self.run_release('preserve', API_FAILURE='1').returncode, 0)
        self.assertEqual(self.manifest.read_text(), before)

    def test_success_changes_only_site_and_records_digest(self):
        result = self.run_release('deploy', NEW)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((self.root / 'state').read_text(), NEW)
        calls = (self.root / 'calls').read_text()
        self.assertIn('create configmap site-release', calls)
        self.assertIn('set image deployment/site', calls)
        self.assertNotIn('deployment/backend', calls)

    def test_missing_canary_prevents_mutation(self):
        result = self.run_release('deploy', NEW, NO_ROLLOUT='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / 'state').read_text(), OLD)
        self.assertNotIn('set image', (self.root / 'calls').read_text())

    def test_failed_canary_restores_previous_image_and_fails_run(self):
        result = self.run_release('deploy', NEW, FAIL_NEW='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / 'state').read_text(), OLD)
        self.assertNotIn('create configmap', (self.root / 'calls').read_text())

    def test_wrong_serving_digest_is_rolled_back(self):
        result = self.run_release('deploy', NEW, WRONG_PODS='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / 'state').read_text(), OLD)

    def test_unpinned_or_wrong_repository_is_rejected_before_cluster_access(self):
        for image in ['registry/site:main', 'other/site@sha256:' + '2' * 64]:
            self.assertNotEqual(self.run_release('deploy', image).returncode, 0)
        self.assertFalse((self.root / 'calls').exists())


if __name__ == '__main__':
    unittest.main()
