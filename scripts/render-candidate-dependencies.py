#!/usr/bin/env python3
"""Render/verify an offline, opt-in dependency qualification bundle; never apply it."""
from __future__ import annotations

import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tarfile
import tempfile

import yaml

ROOT = Path(__file__).resolve().parents[1]
CANDIDATE = ROOT / 'deploy/experimental/envoy-ai-gateway'
NAMESPACE = 'envoy-ai-gateway-system'
GATEWAY_NAMESPACE = 'inferscale-gateway'
ACCOUNT = 'ai-gateway-controller'
PREFIX = 'inferscale-ai-extension'
WEBHOOK = f'envoy-ai-gateway-gateway-pod-mutator.{NAMESPACE}'
STAGES = ('00-namespaces.yaml', '10-crds.yaml', '20-support.yaml', '30-addon.yaml', '40-controllers.yaml')
READ = ['get', 'list', 'watch']
IMAGE = re.compile(r'[^\s@]+@sha256:[0-9a-f]{64}')


def checksum(data):
    return hashlib.sha256(data).hexdigest()


def require_image(value):
    if not IMAGE.fullmatch(value):
        raise ValueError('candidate images must be immutable registry references with @sha256:<64 lowercase hex>')
    return value


def checked_file(path, expected):
    data = path.read_bytes()
    if checksum(data) != expected:
        raise ValueError(f'checksum mismatch: {path}')
    return data


def objects(data):
    return [item for item in yaml.safe_load_all(data) if item]


def dump(items):
    return yaml.safe_dump_all(items, sort_keys=False).encode()


def one(items, kind, name=None):
    matches = [d for d in items if d.get('kind') == kind and (name is None or d['metadata']['name'] == name)]
    if len(matches) != 1:
        raise ValueError(f'expected one {kind}/{name}, found {len(matches)}')
    return matches[0]


def resource(kind, name, namespace=None, **fields):
    api = 'rbac.authorization.k8s.io/v1' if 'Role' in kind else 'v1'
    obj = {'apiVersion': api, 'kind': kind, 'metadata': {'name': name}, **fields}
    if namespace:
        obj['metadata']['namespace'] = namespace
    return obj


def binding(name, role_kind, namespace=None):
    return resource('RoleBinding' if namespace else 'ClusterRoleBinding', name, namespace,
                    roleRef={'apiGroup': 'rbac.authorization.k8s.io', 'kind': role_kind, 'name': name},
                    subjects=[{'kind': 'ServiceAccount', 'name': ACCOUNT, 'namespace': NAMESPACE}])


def addon_rbac():
    # Pools and route metadata in newly created tenant namespaces are discovered
    # automatically. Only the two trusted namespaces enter the Secret cache.
    rules = [
        {'apiGroups': [''], 'resources': ['pods', 'services'], 'verbs': READ},
        {'apiGroups': ['apps'], 'resources': ['deployments', 'daemonsets'], 'verbs': READ},
        {'apiGroups': ['gateway.networking.k8s.io'], 'resources': ['gateways', 'gatewayclasses', 'httproutes', 'referencegrants'], 'verbs': READ},
        {'apiGroups': ['gateway.envoyproxy.io'], 'resources': ['backends', 'httproutefilters', 'securitypolicies', 'backendtrafficpolicies', 'envoyproxies'], 'verbs': READ},
        {'apiGroups': ['aigateway.envoyproxy.io'], 'resources': ['aigatewayroutes', 'aiservicebackends', 'backendsecuritypolicies', 'mcproutes', 'gatewayconfigs', 'quotapolicies'], 'verbs': READ},
        {'apiGroups': ['inference.networking.k8s.io'], 'resources': ['inferencepools'], 'verbs': READ},
        {'apiGroups': ['inference.networking.k8s.io'], 'resources': ['inferencepools/status'], 'verbs': ['update', 'patch']},
        {'apiGroups': [''], 'resources': ['events'], 'verbs': ['create', 'patch']},
        {'apiGroups': ['apiextensions.k8s.io'], 'resources': ['customresourcedefinitions'], 'resourceNames': ['inferencepools.inference.networking.k8s.io'], 'verbs': ['get']},
        {'apiGroups': ['admissionregistration.k8s.io'], 'resources': ['mutatingwebhookconfigurations'], 'resourceNames': [WEBHOOK], 'verbs': ['get', 'update', 'patch']},
    ]
    result = [resource('ServiceAccount', ACCOUNT, NAMESPACE), resource('ClusterRole', PREFIX, rules=rules), binding(PREFIX, 'ClusterRole')]
    for namespace in (NAMESPACE, GATEWAY_NAMESPACE):
        scoped = [{'apiGroups': [''], 'resources': ['secrets'], 'verbs': READ}]
        if namespace == NAMESPACE:
            scoped.append({'apiGroups': ['coordination.k8s.io'], 'resources': ['leases'], 'verbs': READ + ['create', 'update', 'patch']})
        else:
            scoped.extend([
                {'apiGroups': [''], 'resources': ['secrets'], 'verbs': ['create', 'update', 'patch', 'delete']},
                {'apiGroups': [''], 'resources': ['pods'], 'verbs': ['patch']},
            ])
        result.extend([resource('Role', PREFIX, namespace, rules=scoped), binding(PREFIX, 'Role', namespace)])
    return result


def addon_network_policy(cidrs, port):
    for cidr in cidrs:
        network = ipaddress.ip_network(cidr, strict=True)
        private = [ipaddress.ip_network(value) for value in ('10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', 'fc00::/7')]
        if network.prefixlen != network.max_prefixlen or not any(network.version == value.version and network.subnet_of(value) for value in private):
            raise ValueError('Kubernetes API endpoints must be exact private /32 or /128 CIDRs')
    if not cidrs or not 1 <= port <= 65535:
        raise ValueError('explicit Kubernetes API endpoints and a valid TCP port are required')
    return {
        'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
        'metadata': {'name': PREFIX, 'namespace': NAMESPACE},
        'spec': {
            'podSelector': {}, 'policyTypes': ['Ingress', 'Egress'],
            'ingress': [{'from': [{
                'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'envoy-gateway-system'}},
                'podSelector': {'matchLabels': {'control-plane': 'envoy-gateway'}},
            }], 'ports': [{'protocol': 'TCP', 'port': 1063}]}],
            'egress': [
                {'to': [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'kube-system'}},
                         'podSelector': {'matchLabels': {'k8s-app': 'kube-dns'}}}],
                 'ports': [{'protocol': protocol, 'port': 53} for protocol in ('TCP', 'UDP')]},
                {'to': [{'ipBlock': {'cidr': cidr}} for cidr in sorted(set(cidrs))],
                 'ports': [{'protocol': 'TCP', 'port': value} for value in sorted({443, port})]},
            ],
        },
    }


def configure_gateway(items, gateway_image, rate_limit_image):
    config = one(items, 'ConfigMap', 'envoy-gateway-config')
    settings = yaml.safe_load(config['data']['envoy-gateway.yaml'])
    provider = settings['provider']['kubernetes']
    provider['deploy'] = {'type': 'GatewayNamespace'}
    provider['shutdownManager']['image'] = gateway_image
    provider['rateLimitDeployment']['container']['image'] = rate_limit_image
    settings.setdefault('extensionApis', {})['enableBackend'] = True
    settings['extensionManager'] = {
        'failOpen': False,
        'backendResources': [{'group': 'inference.networking.k8s.io', 'kind': 'InferencePool', 'version': 'v1'}],
        'hooks': {'xdsTranslator': {
            'translation': {name: {'includeAll': True} for name in ('listener', 'route', 'cluster', 'secret')},
            'post': ['Translation', 'Cluster', 'Route'],
        }},
        'service': {'fqdn': {'hostname': f'{ACCOUNT}.{NAMESPACE}.svc.cluster.local', 'port': 1063}},
    }
    config['data']['envoy-gateway.yaml'] = yaml.safe_dump(settings, sort_keys=False)
    # ConfigMap changes must restart the controller; EnvoyGateway config is read
    # at startup, and an already-Available old Deployment is not readiness proof.
    pod = one(items, 'Deployment', 'envoy-gateway')['spec']['template']
    pod.setdefault('metadata', {}).setdefault('annotations', {})['inferscale.io/dependency-config-sha256'] = checksum(config['data']['envoy-gateway.yaml'].encode())


def replace_images(items, replacements):
    counts = {source: 0 for source in replacements}
    for document in items:
        if document.get('kind') not in {'Deployment', 'StatefulSet', 'DaemonSet', 'Job'}:
            continue
        pod = document['spec']['template']['spec']
        for container in pod.get('containers', []) + pod.get('initContainers', []):
            image = container.get('image', '')
            if image in replacements:
                container['image'] = replacements[image]
                counts[image] += 1
            require_image(container['image'])
    return counts


def render_addon(archive, expected_hash, helm, addon_image, extproc_image):
    checked_file(archive, expected_hash)
    with tempfile.TemporaryDirectory(prefix='inferscale-addon-chart-') as directory:
        directory = Path(directory)
        # Only extract regular chart files from the authenticated source archive.
        with tarfile.open(archive) as tar:
            for member in tar.getmembers():
                parts = Path(member.name).parts
                if len(parts) < 5 or parts[1:3] != ('manifests', 'charts') or parts[3] not in {'ai-gateway-helm', 'ai-gateway-crds-helm'}:
                    continue
                if not member.isfile():
                    continue
                relative = Path(*parts[3:])
                if '..' in relative.parts:
                    raise ValueError('invalid archive path')
                destination = directory / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_bytes(tar.extractfile(member).read())
        values = yaml.safe_load((CANDIDATE / 'candidate-values.yaml').read_text())
        # The chart appends ':tag'; use a temporary pinned value then replace the
        # exact rendered references, supporting registries with port numbers.
        for section, image in ((values['controller'], addon_image), (values['extProc'], extproc_image)):
            repository, digest = image.split('@')
            section['image'] = {'repository': repository, 'tag': f'candidate@{digest}'}
        values['controller']['mcp'] = {'sessionEncryption': {'seed': secrets.token_hex(32)}}
        values_file = directory / 'values.yaml'
        values_file.write_text(yaml.safe_dump(values))
        result = subprocess.run([helm, 'template', PREFIX, str(directory / 'ai-gateway-helm'), '--namespace', NAMESPACE, '--values', str(values_file)], check=True, text=True, capture_output=True)
        addon = objects(result.stdout)
        deployment = one(addon, 'Deployment', ACCOUNT)
        container = deployment['spec']['template']['spec']['containers'][0]
        container['image'] = addon_image
        container['args'] = [f'--extProcImage={extproc_image}' if arg.startswith('--extProcImage=') else arg for arg in container['args']]
        if '--watchNamespaces=' not in container['args']:
            raise ValueError('addon chart must watch future tenant namespaces')
        container['args'].append(f'--secretNamespaces={NAMESPACE},{GATEWAY_NAMESPACE}')
        secret = one(addon, 'Secret')
        hook = one(addon, 'MutatingWebhookConfiguration', WEBHOOK)['webhooks'][0]
        hook['clientConfig']['caBundle'] = secret['data']['ca.crt']
        expressions = hook['objectSelector']['matchExpressions']
        if len(expressions) != 2 or {v['operator'] for v in expressions} != {'Exists', 'DoesNotExist'} or len({v['key'] for v in expressions}) != 1:
            raise ValueError('sidecar injection selector must remain unsatisfiable')
        deployment['spec']['template'].setdefault('metadata', {}).setdefault('annotations', {})['inferscale.io/webhook-ca-sha256'] = checksum(secret['data']['ca.crt'].encode())
        crds = []
        for path in sorted((directory / 'ai-gateway-crds-helm/templates').glob('*.yaml')):
            crds.extend(objects(path.read_text()))
        if len(crds) != 6 or any(d['kind'] != 'CustomResourceDefinition' for d in crds):
            raise ValueError('expected all six AI Gateway CRDs')
        return addon_rbac() + addon, crds


def dependency_sources(lock, candidate):
    components = lock['components']
    result = {'envoy-gateway.yaml': candidate['envoyGateway']}
    for filename, component, prefix in (
        ('inference-extension.yaml', 'gatewayAPIInferenceExtension', 'manifest'),
        ('llmd-objective.yaml', 'llmd', 'objectiveCRD'),
        ('keda.yaml', 'keda', 'manifest'),
        ('service-monitor.yaml', 'prometheusOperator', 'serviceMonitorCRD'),
        ('pod-monitor.yaml', 'prometheusOperator', 'podMonitorCRD'),
    ):
        result[filename] = {'manifestURL': components[component][prefix + 'URL'], 'manifestSHA256': components[component][prefix + 'SHA256']}
    return result


def make_bundle(args):
    images = {name: require_image(getattr(args, name)) for name in ('envoy_image', 'addon_image', 'epp_image')}
    network_policy = addon_network_policy(args.kubernetes_api_cidr, args.kubernetes_api_port)
    dns_label = r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?'
    if args.image_pull_secret and (len(args.image_pull_secret) > 253 or not re.fullmatch(dns_label + r'(?:\.' + dns_label + r')*', args.image_pull_secret)):
        raise ValueError('image pull secret must be a Kubernetes Secret name')
    candidate = json.loads((CANDIDATE / 'candidate-dependencies.json').read_text())
    lock = yaml.safe_load((ROOT / 'versions.lock.yaml').read_text())
    if lock['components']['gatewayAPI']['version'] != 'v1.5.1':
        raise ValueError('candidate EG bundle requires reviewed Gateway API v1.5.1 CRDs')
    sources = dependency_sources(lock, candidate)
    manifests = {name: objects(checked_file(args.manifest_dir / name, source['manifestSHA256'])) for name, source in sources.items()}
    eg = lock['components']['envoyGateway']
    keda = lock['components']['keda']
    envoy = manifests['envoy-gateway.yaml']
    counts = replace_images(envoy, {candidate['envoyGateway']['manifestControllerImage']: images['envoy_image']})
    if list(counts.values()) != [2]:
        raise ValueError('expected EG controller and certificate job images')
    configure_gateway(envoy, images['envoy_image'], require_image(eg['rateLimitImage'] + '@' + eg['rateLimitImageDigest']))
    # Existing certgen Jobs have immutable pod templates. Use a candidate-image
    # name so applying a new controller image can create its own certificate job.
    job = one(envoy, 'Job', 'eg-gateway-helm-certgen')
    job['metadata']['name'] = 'inferscale-envoy-certgen-' + checksum((images['envoy_image'] + args.image_pull_secret).encode())[:12]
    counts = replace_images(manifests['keda.yaml'], {
        keda['manifestOperatorImage']: require_image(keda['image'] + '@' + keda['imageDigest']),
        keda['manifestMetricsServerImage']: require_image(keda['metricsServerImage'] + '@' + keda['metricsServerImageDigest']),
    })
    if sorted(counts.values()) != [1, 1]:
        raise ValueError('expected exactly one KEDA operator and metrics image')
    addon, addon_crds = render_addon(args.ai_archive, candidate['aiGateway']['archiveSHA256'], args.helm, images['addon_image'], candidate['aiGateway']['extProcImage'])
    combined = [d for docs in manifests.values() for d in docs]
    combined += objects((ROOT / 'deploy/base/gateway/namespace.yaml').read_text())
    combined += objects((ROOT / 'deploy/base/gateway/controller-rbac.yaml').read_text())
    combined += [resource('Namespace', NAMESPACE), network_policy] + addon + addon_crds
    if args.image_pull_secret:
        for document in combined:
            if document.get('kind') in {'Deployment', 'Job'} and document['metadata'].get('namespace') in {NAMESPACE, 'envoy-gateway-system'}:
                document['spec']['template']['spec']['imagePullSecrets'] = [{'name': args.image_pull_secret}]
    stages = {name: [] for name in STAGES}
    identities = {}
    for document in combined:
        key = (document['apiVersion'], document['kind'], document['metadata'].get('namespace'), document['metadata']['name'])
        if key in identities:
            if identities[key] != document:
                raise ValueError(f'conflicting dependency resources: {key}')
            continue
        identities[key] = document
        kind = document['kind']
        if kind == 'Namespace':
            stage = STAGES[0]
        elif kind == 'CustomResourceDefinition':
            stage = STAGES[1]
        elif kind in {'Deployment', 'DaemonSet', 'StatefulSet', 'Job'}:
            stage = STAGES[3] if document['metadata'].get('namespace') == NAMESPACE else STAGES[4]
        else:
            stage = STAGES[2]
        stages[stage].append(document)
    # This release uses the complete experimental Gateway API bundle already in
    # the authenticated EG v1.8.1 install. Never downgrade it with standard CRDs.
    gateway_crds = [d for d in stages[STAGES[1]] if d['spec']['group'] == 'gateway.networking.k8s.io']
    if len(gateway_crds) != 10 or any(d['metadata']['annotations']['gateway.networking.k8s.io/bundle-version'] != 'v1.5.1' for d in gateway_crds):
        raise ValueError('unexpected bundled Gateway API CRD inventory/version')
    for stage in stages.values():
        replace_images(stage, {})
    # No writes until every source and transform passes validation.
    args.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    metadata = {
        'schemaVersion': 1, 'qualification': 'pending', 'images': images,
        'sources': sources, 'aiArchiveSHA256': candidate['aiGateway']['archiveSHA256'],
        'candidateSourceProvenanceSHA256': checksum((CANDIDATE / 'weighted-canary.provenance.json').read_bytes()),
        'pickerSourceProvenanceSHA256': checksum((ROOT / 'deploy/experimental/llm-d/round-robin-picker.provenance.json').read_bytes()),
        'crds': sorted(d['metadata']['name'] for d in stages[STAGES[1]]),
        'files': {},
    }
    for name, docs in stages.items():
        data = dump(docs)
        path = args.output / name
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'wb') as stream:
            stream.write(data)
        metadata['files'][name] = checksum(data)
    (args.output / 'candidate-bundle.json').write_text(json.dumps(metadata, indent=2) + '\n')
    os.chmod(args.output / 'candidate-bundle.json', 0o600)
    print(f'Rendered pending qualification bundle: {args.output}; includes private webhook TLS material (0600).')


def verify_bundle(path):
    metadata = json.loads((path / 'candidate-bundle.json').read_text())
    if metadata.get('schemaVersion') != 1 or metadata.get('qualification') != 'pending' or set(metadata.get('files', {})) != set(STAGES):
        raise ValueError('not a complete pending candidate dependency bundle')
    for name, expected in metadata['files'].items():
        checked_file(path / name, expected)
    for name in ('envoy_image', 'addon_image', 'epp_image'):
        require_image(metadata['images'][name])
    # The digest record must agree with the actual rendered workloads.
    for stage, name, image_key in ((STAGES[3], ACCOUNT, 'addon_image'), (STAGES[4], 'envoy-gateway', 'envoy_image')):
        deployment = one(objects((path / stage).read_text()), 'Deployment', name)
        actual = deployment['spec']['template']['spec']['containers'][0]['image']
        if actual != metadata['images'][image_key]:
            raise ValueError(f'bundle image inventory disagrees with {name}')
    return metadata


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    render = commands.add_parser('render')
    render.add_argument('--manifest-dir', type=Path, required=True)
    render.add_argument('--ai-archive', type=Path, required=True)
    render.add_argument('--helm', default='helm')
    render.add_argument('--kubernetes-api-cidr', action='append', required=True)
    render.add_argument('--kubernetes-api-port', type=int, default=6443)
    render.add_argument('--image-pull-secret', default='')
    for name in ('envoy', 'addon', 'epp'):
        render.add_argument(f'--{name}-image', required=True)
    render.add_argument('--output', type=Path, required=True)
    verify = commands.add_parser('verify')
    verify.add_argument('bundle', type=Path)
    verify.add_argument('--print-epp-image', action='store_true')
    args = parser.parse_args()
    try:
        if args.command == 'render':
            make_bundle(args)
        else:
            metadata = verify_bundle(args.bundle)
            print(metadata['images']['epp_image'] if args.print_epp_image else 'Candidate bundle checksums verified; live qualification remains pending.')
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError, yaml.YAMLError, tarfile.TarError) as exc:
        parser.exit(1, f'error: {exc}\n')


if __name__ == '__main__':
    main()
