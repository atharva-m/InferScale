#!/usr/bin/env python3
"""Offline regressions for candidate identity, privilege scope and apply ordering."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('candidate_dependencies', ROOT / 'scripts/render-candidate-dependencies.py')
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)


class CandidateDependenciesTest(unittest.TestCase):
    def image(self, name):
        return f'registry.invalid/{name}@sha256:' + 'a' * 64

    def bundle(self, directory):
        directory.mkdir()
        def deployment(name, namespace, image):
            return {
                'apiVersion': 'apps/v1', 'kind': 'Deployment',
                'metadata': {'name': name, 'namespace': namespace},
                'spec': {'template': {'spec': {'containers': [{'name': name, 'image': image}]}}},
            }
        docs = {
            '00-namespaces.yaml': [candidate.resource('Namespace', candidate.NAMESPACE)],
            '10-crds.yaml': [{'apiVersion': 'apiextensions.k8s.io/v1', 'kind': 'CustomResourceDefinition', 'metadata': {'name': 'inferencepools.inference.networking.k8s.io'}}],
            '20-support.yaml': candidate.addon_rbac(),
            '30-addon.yaml': [deployment('ai-gateway-controller', candidate.NAMESPACE, self.image('addon'))],
            '40-controllers.yaml': [deployment('envoy-gateway', 'envoy-gateway-system', self.image('envoy'))],
        }
        metadata = {
            'schemaVersion': 1, 'qualification': 'pending',
            'images': {f'{name}_image': self.image(name) for name in ('envoy', 'addon', 'epp')},
            'files': {},
        }
        for name, objects in docs.items():
            data = candidate.dump(objects)
            (directory / name).write_bytes(data)
            metadata['files'][name] = candidate.checksum(data)
        (directory / 'candidate-bundle.json').write_text(json.dumps(metadata))
        return metadata

    def test_strict_identity_and_bundle_tamper_rejection(self):
        for value in ('image:latest', self.image('epp') + '\n', self.image('epp').upper()):
            with self.assertRaises(ValueError):
                candidate.require_image(value)
        with tempfile.TemporaryDirectory() as temp:
            directory = Path(temp) / 'bundle'
            metadata = self.bundle(directory)
            self.assertEqual(candidate.verify_bundle(directory)['images']['epp_image'], self.image('epp'))
            metadata['images']['envoy_image'] = self.image('different-envoy')
            (directory / 'candidate-bundle.json').write_text(json.dumps(metadata))
            with self.assertRaisesRegex(ValueError, 'image inventory'):
                candidate.verify_bundle(directory)
            metadata['images']['envoy_image'] = self.image('envoy')
            (directory / 'candidate-bundle.json').write_text(json.dumps(metadata))
            with (directory / '20-support.yaml').open('a') as stream:
                stream.write('\n# modified after review\n')
            with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
                candidate.verify_bundle(directory)

    def test_future_namespaces_only_gain_pool_status_write(self):
        resources = candidate.addon_rbac()
        role = candidate.one(resources, 'ClusterRole', candidate.PREFIX)
        for rule in role['rules']:
            self.assertNotIn('*', rule['resources'])
            self.assertNotIn('*', rule['verbs'])
            self.assertNotIn('secrets', rule['resources'])
            if set(rule['verbs']) - set(candidate.READ):
                self.assertIn(rule['resources'], [['inferencepools/status'], ['events'], ['mutatingwebhookconfigurations']])
        scoped_roles = [d for d in resources if d['kind'] == 'Role']
        self.assertEqual({d['metadata']['namespace'] for d in scoped_roles}, {candidate.NAMESPACE, 'inferscale-gateway'})
        for role in scoped_roles:
            for rule in role['rules']:
                self.assertFalse(set(rule['apiGroups']) & {'gateway.envoyproxy.io', 'gateway.networking.k8s.io'})
        self.assertEqual([r for r in role['rules'] if r['resources'] == ['pods']][0]['verbs'], ['patch'])

    def test_extension_hooks_fail_closed_and_config_rolls_gateway(self):
        def gateway():
            return [
                {'kind': 'ConfigMap', 'metadata': {'name': 'envoy-gateway-config'}, 'data': {'envoy-gateway.yaml': yaml.safe_dump({'provider': {'kubernetes': {'shutdownManager': {}, 'rateLimitDeployment': {'container': {}}}}})}},
                {'kind': 'Deployment', 'metadata': {'name': 'envoy-gateway'}, 'spec': {'template': {}}},
            ]
        docs = gateway()
        candidate.configure_gateway(docs, self.image('envoy'), self.image('ratelimit'))
        config = yaml.safe_load(docs[0]['data']['envoy-gateway.yaml'])
        self.assertFalse(config['extensionManager']['failOpen'])
        self.assertEqual(config['extensionManager']['hooks']['xdsTranslator']['post'], ['Translation', 'Cluster', 'Route'])
        self.assertEqual(config['provider']['kubernetes']['deploy']['type'], 'GatewayNamespace')
        old_hash = docs[1]['spec']['template']['metadata']['annotations']['inferscale.io/dependency-config-sha256']
        candidate.configure_gateway(docs, self.image('new-envoy'), self.image('ratelimit'))
        self.assertNotEqual(old_hash, docs[1]['spec']['template']['metadata']['annotations']['inferscale.io/dependency-config-sha256'])

    def test_network_policy_only_allows_gateway_and_private_api(self):
        for value in ('0.0.0.0/0', '10.0.0.0/8', '8.8.8.8/32', '10.0.0.4/24'):
            with self.assertRaises(ValueError):
                candidate.addon_network_policy([value], 6443)
        policy = candidate.addon_network_policy(['10.43.0.1/32', '172.18.0.2/32'], 6443)['spec']
        self.assertEqual(set(policy['policyTypes']), {'Ingress', 'Egress'})
        peer = policy['ingress'][0]['from'][0]
        self.assertEqual(peer['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'], 'envoy-gateway-system')
        self.assertEqual(peer['podSelector']['matchLabels'], {'control-plane': 'envoy-gateway'})
        self.assertEqual(policy['ingress'][0]['ports'], [{'protocol': 'TCP', 'port': 1063}])

    def run_installer(self, temp, bundle, fail='', context=None):
        bin_dir = temp / 'bin'
        bin_dir.mkdir(exist_ok=True)
        fake = bin_dir / 'kubectl'
        fake.write_text('#!/usr/bin/env bash\nprintf "%s\\n" "$*" >> "$TEST_KUBECTL_LOG"\nif [[ -n "${TEST_FAIL:-}" && "$*" == *"${TEST_FAIL}"* ]]; then exit 1; fi\n')
        fake.chmod(0o755)
        log = temp / 'kubectl.log'
        command = ['bash', str(ROOT / 'scripts/install-platform-dependencies.sh'), '--candidate-bundle', str(bundle)]
        if context is not None:
            command.extend(['--context', context])
        result = subprocess.run(command, text=True, capture_output=True,
                                env={**os.environ, 'PATH': str(bin_dir) + ':' + os.environ['PATH'], 'TEST_KUBECTL_LOG': str(log), 'TEST_FAIL': fail})
        return result, log.read_text().splitlines() if log.exists() else []

    def test_crds_and_addon_readiness_precede_gateway_start(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            result, commands = self.run_installer(temp, bundle)
            self.assertEqual(result.returncode, 0, result.stderr)
            def index(text):
                return next(i for i, command in enumerate(commands) if text in command)
            self.assertLess(index('10-crds.yaml'), index('--for=condition=Established'))
            self.assertLess(index('--for=condition=Established'), index('30-addon.yaml'))
            self.assertLess(index('deployment/ai-gateway-controller'), index('40-controllers.yaml'))
            self.assertTrue(any('rollout status' in c and 'deployment/envoy-gateway' in c for c in commands))
            self.assertFalse(any('curl' in c or '--force' in c for c in commands))

    def test_no_gateway_apply_after_addon_failure(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            result, commands = self.run_installer(temp, bundle, fail='deployment/ai-gateway-controller')
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(any('40-controllers.yaml' in command for command in commands))

    def test_crd_failure_stops_before_any_controller_apply(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            result, commands = self.run_installer(temp, bundle, fail='--for=condition=Established')
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(any('20-support.yaml' in command or '30-addon.yaml' in command or '40-controllers.yaml' in command for command in commands))

    def test_explicit_context_is_used_for_every_cluster_operation(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            result, commands = self.run_installer(temp, bundle, context='candidate-cluster')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(commands)
            self.assertTrue(all(command.startswith('--context candidate-cluster ') for command in commands))

    def test_bad_bundle_never_mutates_cluster(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            (bundle / '10-crds.yaml').write_text('tampered')
            result, commands = self.run_installer(temp, bundle)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(commands, [])

    def test_remote_platform_uses_verified_candidate_picker(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            bundle = temp / 'bundle'
            self.bundle(bundle)
            bin_dir = temp / 'bin'
            bin_dir.mkdir()
            kubectl = bin_dir / 'kubectl'
            kubectl.write_text('#!/usr/bin/env bash\n[[ "$1" == kustomize ]] || exit 1\ncat "$TEST_TEMPLATE"\n')
            kubectl.chmod(0o755)
            template = temp / 'template.yaml'
            docs = [{'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'inferscale-config'}, 'data': {'INFERSCALE_EPP_IMAGE': self.image('old-epp')}}]
            for name in ('api', 'controller', 'admission', 'migrate'):
                docs.append({'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': name}, 'spec': {'template': {'spec': {'containers': [{'name': name, 'image': f'registry.invalid/inferscale/{name}:dev'}]}}}})
            template.write_bytes(candidate.dump(docs))
            env = {
                **os.environ, 'PATH': str(bin_dir) + ':' + os.environ['PATH'],
                'TEST_TEMPLATE': str(template), 'INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE': str(bundle),
                'PUBLIC_BASE_URL': 'https://inference.example.com',
                'INFERSCALE_BENCHMARK_PROVIDER': 'vast',
                'INFERSCALE_BENCHMARK_INFERENCE_BASE_URL': 'https://inference.example.com',
                'INFERSCALE_BENCHMARK_PROMETHEUS_URL': 'http://prometheus:9090',
                'INFERSCALE_BENCHMARK_DRIVER_VERSION': '580.82.07',
                'INFERSCALE_BENCHMARK_CUDA_VERSION': '13.0',
            }
            for name in ('API', 'CONTROLLER', 'ADMISSION', 'MIGRATE'):
                env[name + '_IMAGE'] = self.image(name.lower())
            result = subprocess.run(['bash', str(ROOT / 'scripts/render-remote-release.sh'), 'remote'], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            config = candidate.one(candidate.objects(result.stdout), 'ConfigMap', 'inferscale-config')
            self.assertEqual(config['data']['INFERSCALE_EPP_IMAGE'], self.image('epp'))
            self.assertEqual(config['metadata']['annotations']['inferscale.io/dependency-qualification'], 'pending')
            (bundle / '30-addon.yaml').write_text('modified after review')
            rejected = subprocess.run(['bash', str(ROOT / 'scripts/render-remote-release.sh'), 'remote'], env=env, text=True, capture_output=True)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertEqual(rejected.stdout, '')


if __name__ == '__main__':
    unittest.main()
