#!/usr/bin/env python3
"""Reuse original, authenticated native release artifacts without rebuilding C++."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import time
import zipfile

ROOT = Path(__file__).resolve().parents[1]
CONSTRUCTION_INPUTS = (
    'installer/install.go', 'tools/native_payload.py', 'tools/package.py',
    'go.mod', 'go.sum', 'installer/models.json', 'native/macos/SystemInfo.swift',
    'LICENSE', 'tools/smoke.py', '.github/workflows/ci.yml',
)
NATIVE_SPECS = {
    'linux-amd64': ('native / linux (ubuntu-24.04, linux-amd64)', (
        'Run python3 tools/native_payload.py --target linux-amd64 --output native-payloads --jobs 2', 'Verify native RHEL9 CPU inference')),
    'linux-arm64': ('native / linux (ubuntu-24.04-arm, linux-arm64)', (
        'Run python3 tools/native_payload.py --target linux-arm64 --output native-payloads --jobs 2', 'Verify native RHEL9 CPU inference')),
    'darwin-amd64': ('native / desktop (macos-15-intel, darwin-amd64, cpu,metal)', (
        'Build on the actual target architecture', 'Native macOS compiler-free CPU smoke')),
    'darwin-arm64': ('native / desktop (macos-15, darwin-arm64, cpu,metal)', (
        'Build on the actual target architecture', 'Native macOS compiler-free CPU smoke')),
    'windows-amd64': ('native / desktop (windows-2022, windows-amd64, cpu)', (
        'Build on the actual target architecture', 'Native Windows compiler-free CPU smoke')),
    'windows-arm64': ('native / desktop (windows-11-arm, windows-arm64, cpu)', (
        'Build on the actual target architecture', 'Native Windows compiler-free CPU smoke')),
}
SOURCE_JOBS = tuple('validate / test (' + runner + ')' for runner in ('ubuntu-24.04', 'macos-15', 'windows-2022')) + ('validate / source-installer-development',)


def load_module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'tools' / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


NATIVE = load_module('reuse_native', 'native_payload.py')
PACKAGE = load_module('reuse_package', 'package.py')
SMOKE = load_module('reuse_smoke', 'smoke.py')


def sha256(path):
    return NATIVE.sha256(path)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify_run(run, jobs, repository, run_id):
    require(str(run.get('id')) == str(run_id), 'Producer run ID mismatch')
    repo, head = run.get('repository', {}), run.get('head_repository', {})
    require(repo.get('full_name') == repository and head.get('full_name') == repository and
            repo.get('id') == head.get('id') and type(repo.get('id')) is int, 'Producer must originate in the same repository, not a fork')
    require(run.get('event') == 'push' and run.get('path') == '.github/workflows/release.yml', 'Producer must be an original release workflow tag push')
    require(re.fullmatch(r'v0\.1\.0(?:-rc\.[0-9]+)?', run.get('head_branch', '')), 'Producer must have a versioned release tag')
    require(re.fullmatch(r'[0-9a-f]{40}', run.get('head_sha', '')), 'Invalid producer source commit')
    require(run.get('run_attempt') == 1, 'Reuse accepts only original first-attempt runs, avoiding mixed-attempt artifacts')
    require(run.get('status') == 'completed' and run.get('conclusion') in ('success', 'failure'), 'Producer run is not complete')
    approved = {}
    for name, required_steps in list(NATIVE_SPECS.values()) + [(name, ()) for name in SOURCE_JOBS]:
        matches = [job for job in jobs if job.get('name') == name]
        require(len(matches) == 1, 'Missing or ambiguous required producer job: ' + name)
        job = matches[0]
        require(job.get('run_id') == run['id'] and job.get('head_sha') == run['head_sha'] and job.get('run_attempt') == 1,
                'Producer job source/run mismatch: ' + name)
        require(job.get('status') == 'completed' and job.get('conclusion') == 'success', 'Producer job did not pass: ' + name)
        for step_name in required_steps:
            steps = [step for step in job.get('steps', []) if step.get('name') == step_name]
            require(len(steps) == 1 and steps[0].get('status') == 'completed' and steps[0].get('conclusion') == 'success',
                    'Required native build/smoke step did not pass: ' + step_name)
        approved[name] = job['id']
    return {'producer_run_id': run['id'], 'producer_commit': run['head_sha'], 'producer_tag': run['head_branch'],
            'producer_run_attempt': 1, 'producer_conclusion': run['conclusion'], 'verified_jobs': approved}


def git(root, *arguments):
    result = subprocess.run(['git', '-C', str(root), *arguments], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    require(result.returncode == 0, 'Git source proof is missing or unrelated: ' + ' '.join(arguments))
    return result.stdout


def native_job_blocks(data):
    """Compare unchanged build jobs while permitting only the new reuse guard."""
    text = data.decode('utf-8')
    blocks = {}
    for name in ('linux', 'desktop'):
        match = re.search(r'^  ' + name + r':\n(.*?)(?=^  [A-Za-z][A-Za-z0-9_-]*:|\Z)', text, re.M | re.S)
        require(match is not None, 'Missing native construction job: ' + name)
        blocks[name] = match.group(1).replace("    if: inputs.reuse_run_id == ''\n", '')
    return blocks


def verify_sources(root, producer_commit):
    root = Path(root)
    require(re.fullmatch(r'[0-9a-f]{40}', producer_commit), 'Invalid producer commit')
    current = git(root, 'rev-parse', 'HEAD').decode().strip()
    git(root, 'merge-base', '--is-ancestor', producer_commit, current)
    snapshot = NATIVE.source_state(root)
    dynamic = set(snapshot['runtime_source_files']) | set(snapshot['native_source_files'])
    inputs = set(CONSTRUCTION_INPUTS) | dynamic
    recorded = {}
    for name in sorted(inputs):
        path = root / name
        require(path.is_file() and not path.is_symlink(), 'Construction input must be a regular file: ' + name)
        original = git(root, 'show', producer_commit + ':' + name)
        committed = git(root, 'show', current + ':' + name)
        require(original == committed == path.read_bytes(), 'Changed construction/runtime input: ' + name)
        recorded[name] = hashlib.sha256(original).hexdigest()
    producer_names = set(git(root, 'ls-tree', '-r', '--name-only', producer_commit).decode().splitlines())
    producer_runtime = {name for name in producer_names if (name.startswith(('cmd/fabrics/', 'internal/')) and name.endswith('.go') and not name.endswith('_test.go')) or (name.startswith('native/') and name.endswith('.swift'))}
    require(producer_runtime == dynamic, 'Runtime/native source file set changed')
    # Build flags, platform matrix, compiler setup and frozen build image must
    # remain identical even though artifact download/reuse wiring can change.
    workflow = '.github/workflows/native.yml'
    original_workflow = git(root, 'show', producer_commit + ':' + workflow)
    current_workflow = (root / workflow).read_bytes()
    require(native_job_blocks(original_workflow) == native_job_blocks(current_workflow), 'Native construction jobs changed')
    crlf = {}
    for key, value in snapshot.items():
        if isinstance(value, dict):
            crlf[key] = {name: hashlib.sha256((root / name).read_bytes().replace(b'\n', b'\r\n')).hexdigest() for name in value}
        else:
            name = {'go_mod_sha256': 'go.mod', 'go_sum_sha256': 'go.sum', 'model_catalog_sha256': 'installer/models.json'}[key]
            crlf[key] = hashlib.sha256((root / name).read_bytes().replace(b'\n', b'\r\n')).hexdigest()
    return {'current_commit': current, 'source_state': snapshot, 'windows_crlf_state': crlf,
            'construction_input_sha256': recorded,
            'native_build_job_sha256': {name: hashlib.sha256(value.encode()).hexdigest() for name, value in native_job_blocks(original_workflow).items()}}


def extract_artifact(zip_path, destination, target, expected_digest):
    require(re.fullmatch(r'[0-9a-f]{64}', expected_digest or ''), 'Artifact ZIP digest is missing or invalid')
    require(sha256(zip_path) == expected_digest, 'Artifact ZIP SHA256 mismatch')
    expected = {'native-payloads/' + target + '.json', 'native-payloads/' + target + '.tar.gz', 'native-smoke-' + target + '.json'}
    destination = Path(destination)
    require(not destination.exists(), 'Preserving existing artifact output directory')
    with zipfile.ZipFile(zip_path) as archive:
        files = [entry for entry in archive.infolist() if not entry.is_dir()]
        require(len(files) == 3 and {entry.filename for entry in files} == expected, 'Artifact must contain exactly the original payload, manifest and smoke report')
        for entry in archive.infolist():
            require(not stat.S_ISLNK(entry.external_attr >> 16), 'Artifact ZIP contains a link')
            if entry.is_dir():
                require(entry.filename == 'native-payloads/', 'Unexpected artifact ZIP directory')
            else:
                limit = 2 << 30 if entry.filename.endswith('.tar.gz') else 64 << 20
                require(0 < entry.file_size <= limit, 'Artifact ZIP entry exceeds expected size bounds')
        for entry in files:
            path = destination / entry.filename
            path.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(entry) as source, path.open('xb') as output:
                shutil.copyfileobj(source, output)


def verify_payload(directory, target, expected_state):
    directory = Path(directory)
    archive, raw = PACKAGE.load_native_payload(directory / 'native-payloads', target)
    manifest = json.loads(raw)
    for name, expected in expected_state.items():
        require(manifest.get(name) == expected, 'Native source snapshot mismatch: ' + target + ' / ' + name)
    require(manifest.get('llama_commit') == NATIVE.LLAMA_COMMIT and manifest.get('llama_source_sha256') == NATIVE.LLAMA_SHA256, 'Pinned llama source mismatch')
    require(manifest.get('go_version') == 'go version ' + NATIVE.GO_VERSION + ' ' + target.replace('-', '/'), 'Pinned native Go toolchain/host mismatch')
    backends = ['cpu', 'metal'] if target.startswith('darwin-') else ['cpu']
    require(manifest.get('backends') == backends and manifest.get('default_backend') == 'cpu', 'Unexpected native release backends')
    suffix = '.exe' if target.startswith('windows-') else ''
    require(manifest['backend_paths']['cpu'] == 'backends/cpu/llama-server' + suffix, 'Unexpected CPU backend path')
    if target.startswith('linux-'):
        version = manifest.get('glibc_minimum')
        require(isinstance(version, str) and re.fullmatch(r'[0-9]+(?:\.[0-9]+)+', version) and tuple(map(int, version.split('.'))) <= (2, 34), 'Linux payload exceeds glibc baseline')
    if target == 'linux-amd64':
        require(manifest.get('minimum_cpu_isa') == 'x86-64-v2', 'Linux amd64 CPU minimum is missing or wrong')
        # Recheck the original binary ELF notes without executing its code.
        with tempfile.TemporaryDirectory(prefix='reuse-elf-') as temporary:
            binaries = []
            with tarfile.open(directory / 'native-payloads' / (target + '.tar.gz'), 'r:gz') as content:
                for entry in content:
                    if entry.isfile() and entry.name.startswith('native/backends/cpu/'):
                        path = Path(temporary) / Path(entry.name).name
                        with content.extractfile(entry) as source, path.open('xb') as output:
                            shutil.copyfileobj(source, output)
                        binaries.append(path)
            require(NATIVE.verify_elf_cpu_isa(binaries) == manifest.get('elf_cpu_isa', {}).get('cpu'), 'ELF CPU requirement evidence mismatch')
    if target.startswith('darwin-'):
        require(manifest.get('macos_minimum') == '13.0' and 'bin/fabrics-system-info' in manifest['files'], 'macOS deployment/helper proof is missing')
    smoke_path = directory / ('native-smoke-' + target + '.json')
    report = json.loads(smoke_path.read_text(encoding='utf-8-sig'))
    require(report.get('status') in ('passed', 'passed_with_model_warnings') and report.get('runtime_checks') == 'passed' and report.get('mode') == 'balanced' and report.get('backend') == 'cpu', 'Original native CPU smoke did not pass')
    evidence = report.get('backend_evidence', {})
    system, arch = target.split('-')
    require(evidence.get('host_os') == {'linux': 'Linux', 'darwin': 'Darwin', 'windows': 'Windows'}[system] and evidence.get('host_arch') in ({'amd64': ('x86_64', 'AMD64'), 'arm64': ('arm64', 'aarch64', 'ARM64')}[arch]), 'Original smoke was not on the actual target OS/architecture')
    observed = SMOKE.backend_evidence({'backend': 'cpu'}, '\n'.join(evidence.get('startup_evidence_lines', [])), 'cpu')
    require(evidence.get('configured_backend') == 'cpu' and evidence.get('expected_backend') == 'cpu' and evidence.get('offload_confirmed') is False and evidence.get('offloaded_layers') == 0 and observed['offload_confirmed'] is False, 'Original CPU load evidence is missing or inconsistent')
    SMOKE.check_arithmetic(report['arithmetic'], 'balanced')
    SMOKE.check_result(report['recall'], 'balanced')
    require('neptune' in report['recall']['answer'].lower(), 'Original smoke recall answer failed')
    persisted = report['persisted_turn']
    require(persisted['answer'] == report['recall']['answer'] and json.loads(persisted['reflection']) == report['recall']['assessment'], 'Original smoke persistence proof differs from answer/assessment')
    require(report.get('reopen') == 'persisted turn unchanged' and 'parent exited' in report.get('shutdown', '') and 'listeners closed' in report.get('shutdown', ''), 'Original reopen/shutdown proof is missing')
    catalog = json.loads((ROOT / 'installer/models.json').read_text())
    pinned = next(model for model in catalog if model['name'] == 'qwen2.5-1.5b')
    require(report.get('model') == pinned['name'] + '-' + pinned['sha256'] + '.gguf', 'Original smoke did not use the pinned release model')
    return {'archive_sha256': hashlib.sha256(archive).hexdigest(), 'manifest_sha256': hashlib.sha256(raw).hexdigest(),
            'smoke_sha256': sha256(smoke_path), 'smoke_status': report['status'], 'model': report['model']}


class GitHub:
    def __init__(self, repository, executable='gh'):
        require(re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository), 'Invalid repository name')
        self.prefix, self.executable = 'repos/' + repository + '/', executable

    def json(self, endpoint):
        result = subprocess.run([self.executable, 'api', self.prefix + endpoint], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        require(result.returncode == 0, 'Authenticated GitHub API request failed: ' + endpoint.split('?')[0])
        return json.loads(result.stdout)

    def list(self, endpoint, key):
        records, page = [], 1
        while True:
            separator = '&' if '?' in endpoint else '?'
            data = self.json(endpoint + separator + 'per_page=100&page=' + str(page))
            records.extend(data[key])
            if len(data[key]) < 100:
                return records
            page += 1

    def download(self, artifact_id, path):
        require(not Path(path).exists(), 'Preserving existing artifact download')
        # GitHub's artifact redirect/CDN can transiently fail just after upload.
        # Retry transport failures only; ZIP/payload digest failures never retry.
        for attempt in range(3):
            with Path(path).open('wb') as output:
                result = subprocess.run([self.executable, 'api', self.prefix + 'actions/artifacts/' + str(artifact_id) + '/zip'], stdout=output, stderr=subprocess.PIPE)
            if result.returncode == 0:
                break
            status = re.search(rb'HTTP\s+[0-9]{3}', result.stderr)
            diagnostic = status.group().decode() if status else 'artifact transport error'
            require(attempt < 2, 'Authenticated artifact download failed: ' + diagnostic)
            print('Retrying GitHub artifact transport:', diagnostic, flush=True)
            time.sleep(attempt + 1)
        require(Path(path).stat().st_size <= 2 << 30, 'Artifact ZIP is too large')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repository', required=True)
    parser.add_argument('--run-id', required=True)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--report', required=True, type=Path)
    parser.add_argument('--gh', default='gh', help='Existing authenticated GitHub CLI; no credentials are extracted')
    args = parser.parse_args()
    require(re.fullmatch(r'[1-9][0-9]*', args.run_id), 'Invalid workflow run ID')
    require(not args.output.exists() and not args.report.exists(), 'Preserving existing reuse output/report')
    api = GitHub(args.repository, args.gh)
    run = api.json('actions/runs/' + args.run_id)
    jobs = api.list('actions/runs/' + args.run_id + '/jobs?filter=latest', 'jobs')
    report = verify_run(run, jobs, args.repository, args.run_id)
    tag = api.json('git/ref/tags/' + run['head_branch'])['object']
    for _ in range(5):
        if tag.get('type') == 'commit':
            break
        require(tag.get('type') == 'tag' and re.fullmatch(r'[0-9a-f]{40}', tag.get('sha', '')), 'Invalid producer tag object')
        tag = api.json('git/tags/' + tag['sha'])['object']
    require(tag.get('type') == 'commit' and tag.get('sha') == run['head_sha'], 'Producer tag no longer names the recorded commit')
    # fetch-depth:0 normally supplies this ancestor. Fetch its exact validated
    # hash only when absent, using the checkout's existing Git authentication.
    if subprocess.run(['git', '-C', str(ROOT), 'cat-file', '-e', run['head_sha'] + '^{commit}'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode:
        subprocess.run(['git', '-C', str(ROOT), 'fetch', '--no-tags', 'origin', run['head_sha']], check=True)
    sources = verify_sources(ROOT, run['head_sha'])
    artifacts = api.list('actions/runs/' + args.run_id + '/artifacts', 'artifacts')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    checked = {}
    with tempfile.TemporaryDirectory(prefix='.native-reuse-', dir=args.output.parent) as temporary:
        staging = Path(temporary)
        for target in NATIVE_SPECS:
            name = 'native-payload-' + target
            matches = [item for item in artifacts if item.get('name') == name]
            require(len(matches) == 1, 'Missing or ambiguous native artifact: ' + name)
            artifact = matches[0]
            provenance = artifact.get('workflow_run', {})
            require(not artifact.get('expired', True) and provenance.get('id') == run['id'] and provenance.get('head_sha') == run['head_sha'] and provenance.get('repository_id') == run['repository']['id'] and provenance.get('head_repository_id') == run['repository']['id'], 'Artifact producer provenance mismatch')
            require(re.fullmatch(r'sha256:[0-9a-f]{64}', artifact.get('digest', '')), 'GitHub artifact has no authoritative SHA256 digest')
            archive_path = staging / (target + '.zip')
            api.download(artifact['id'], archive_path)
            destination = staging / name
            extract_artifact(archive_path, destination, target, artifact['digest'][7:])
            state = sources['windows_crlf_state'] if target.startswith('windows-') else sources['source_state']
            checked[target] = verify_payload(destination, target, state)
            checked[target].update(artifact_id=artifact['id'], artifact_zip_sha256=artifact['digest'][7:],
                                   source_rendering='Windows CRLF' if target.startswith('windows-') else 'Git LF')
            print('Verified original native artifact:', target, flush=True)
        args.output.mkdir()
        for target in NATIVE_SPECS:
            shutil.move(str(staging / ('native-payload-' + target)), args.output)
    report.update(status='verified', repository=args.repository, current_commit=sources['current_commit'],
                  construction_input_sha256=sources['construction_input_sha256'], native_build_job_sha256=sources['native_build_job_sha256'],
                  payloads=checked, clean_installation='Must rerun Ubuntu22.04 and Alma9 smoke on both architectures in the current workflow')
    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2, sort_keys=True) + '\n', encoding='utf-8')
    print('All six native payloads verified; fresh clean-install gates are still required.', flush=True)


if __name__ == '__main__':
    main()
