#!/usr/bin/env python3
"""Opt-in three-node check for GitHub #268; requires Docker and a built test image.

Three nodes start together from one config that holds no certificates. Each
generates and publishes its own at start, and every publish is the same config
version, so the cluster converges on one origin's config. Before the fix that
config carried only that origin's certificate, permanently. Passes when every
node's config on disk carries every node's certificate.

All containers and networks this creates are removed in `finally`. Daemon logs
and the final configs stay in the printed temporary directory.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import uuid

p = argparse.ArgumentParser()
p.add_argument('--image', required=True)
p.add_argument('--settle-seconds', type=int, default=60)
a = p.parse_args()
root = Path(tempfile.mkdtemp(prefix='pulseha-certs-'))
prefix = 'phc-' + uuid.uuid4().hex[:8]
names = [prefix + '-' + str(i) for i in range(1, 4)]
network = prefix + '-net'
ids = ['00000000-0000-4000-8000-%012d' % i for i in range(1, 4)]
created, created_nets = [], []
print('Evidence:', root, flush=True)


def docker(*args, check=True):
    r = subprocess.run(['docker', *args], text=True, capture_output=True, timeout=90)
    if check and r.returncode:
        raise RuntimeError('docker ' + ' '.join(args) + ': ' + r.stderr + r.stdout)
    return r.stdout


def config_of(name):
    return json.loads(docker('exec', name, 'cat', '/etc/pulseha/config.json'))


try:
    docker('network', 'create', '--subnet', '10.188.0.0/24', network)
    created_nets.append(network)
    for i, name in enumerate(names, 1):
        docker('run', '-d', '--name', name, '--hostname', 'node' + str(i),
               '--cap-add', 'NET_ADMIN', '--cap-add', 'NET_RAW',
               '--network', network, '--ip', f'10.188.0.{i + 10}',
               a.image, 'sleep', 'infinity')
        created.append(name)
    nodes = {ids[i - 1]: {'hostname': 'node' + str(i), 'bind_address': f'10.188.0.{i + 10}',
                          'bind_port': '8080', 'group_assignments': {}}
             for i in range(1, 4)}
    for i, name in enumerate(names, 1):
        cfg = {'pulseha': {'local_node': ids[i - 1], 'cluster_token': 'cert-convergence-token',
                           'mode': 'active-passive', 'hcs_interval': 1000, 'fos_interval': 5000,
                           'fo_limit': 10000, 'logging_level': 'info', 'log_to_file': False},
               'nodes': nodes, 'floating_ip_groups': {}}
        path = root / f'config{i}.json'
        path.write_text(json.dumps(cfg))
        docker('cp', str(path), name + ':/etc/pulseha/config.json')

    # Together, which is the case: every node mints the same version for its publish.
    with ThreadPoolExecutor(len(names)) as pool:
        list(pool.map(lambda n: docker('exec', '-d', n, 'sh', '-c', 'exec pulseha > /tmp/daemon.log 2>&1'), names))
    time.sleep(a.settle_seconds)

    ok = True
    for i, name in enumerate(names, 1):
        cfg = config_of(name)
        (root / f'final-config{i}.json').write_text(json.dumps(cfg, indent=2))
        carried = sorted('node' + id_[-1] for id_, n in cfg.get('nodes', {}).items() if n.get('tls_cert'))
        stamp = cfg.get('config_stamp', {})
        print(f'node{i}: certificates carried {len(carried)}/3 {carried} '
              f'stamp=v{stamp.get("version")}/{str(stamp.get("origin", ""))[-2:]}', flush=True)
        ok = ok and len(carried) == 3
    print('PASS: every node carries every certificate' if ok else
          'FAIL: some node is missing a peer\'s certificate', flush=True)
    sys.exit(0 if ok else 1)
finally:
    for i, name in enumerate(created, 1):
        (root / f'node{i}.log').write_text(docker('exec', name, 'cat', '/tmp/daemon.log', check=False))
        docker('rm', '-f', name, check=False)
    for net in created_nets:
        docker('network', 'rm', net, check=False)
