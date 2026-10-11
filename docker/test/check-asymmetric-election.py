#!/usr/bin/env python3
"""Opt-in four-node kernel test; requires Docker and a built test image.

Only node1 <-> node2 loses the cluster link; nodes3/4 still reach node2.
Samples actual interface addresses, not the daemon's ownership claims.
All containers/networks created by this invocation are removed in finally.
Logs and JSON samples remain in the printed temporary directory.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

p = argparse.ArgumentParser()
p.add_argument('--image', default='pulseha-pr263-review')
p.add_argument('--rounds', type=int, default=3)
p.add_argument('--cut-seconds', type=int, default=35)
p.add_argument('--heal-seconds', type=int, default=20)
a = p.parse_args()
root = Path(tempfile.mkdtemp(prefix='pulseha-asymmetric-'))
prefix = 'pha-' + uuid.uuid4().hex[:8]
names = [prefix + '-' + str(i) for i in range(1, 5)]
networks = [prefix + '-control', prefix + '-service']
ids = ['00000000-0000-4000-8000-%012d' % i for i in range(1, 5)]
fip = '10.187.0.100'
created = []
created_nets = []
samples = []
print('Evidence:', root, flush=True)

def docker(*args, check=True):
    r = subprocess.run(['docker', *args], text=True, capture_output=True, timeout=90)
    if check and r.returncode:
        raise RuntimeError('docker ' + ' '.join(args) + ': ' + r.stderr + r.stdout)
    return r.stdout

def execute(i, *args):
    return docker('exec', names[i-1], *args)

def addresses(i):
    return json.loads(execute(i, 'ip', '-j', '-4', 'addr', 'show'))

def owns(i):
    return any(x['local'] == fip for iface in addresses(i) for x in iface['addr_info'])

def sample(phase):
    with ThreadPoolExecutor(max_workers=4) as pool:
        held = list(pool.map(owns, range(1, 5)))
    row = {'time': time.time(), 'phase': phase, 'holders': [i+1 for i, v in enumerate(held) if v]}
    samples.append(row)
    with (root/'samples.jsonl').open('a') as f:
        f.write(json.dumps(row)+'\n')
    return row['holders']

def measure(phase, seconds, expected):
    end = time.monotonic() + seconds
    count = 0
    while time.monotonic() < end:
        holders = sample(phase)
        if holders != expected:
            raise AssertionError(f'{phase}: holders={holders}, expected={expected}')
        count += 1
        time.sleep(.7)
    print(f'{phase}: {count} samples, sole holder {expected}', flush=True)

def cut(action):
    for i, other in [(1, 2), (2, 1)]:
        for chain, direction in [('INPUT', '-s'), ('OUTPUT', '-d')]:
            execute(i, 'iptables', action, chain, direction, f'10.186.0.{other+10}', '-j', 'DROP')

try:
    for name, subnet in zip(networks, ['10.186.0.0/24', '10.187.0.0/24']):
        docker('network', 'create', '--subnet', subnet, name)
        created_nets.append(name)
    interfaces = {}
    for i, name in enumerate(names, 1):
        docker('run', '-d', '--name', name, '--hostname', 'node'+str(i),
               '--cap-add', 'NET_ADMIN', '--cap-add', 'NET_RAW',
               '--network', networks[0], '--ip', f'10.186.0.{i+10}',
               a.image, 'sleep', 'infinity')
        created.append(name)
        docker('network', 'connect', '--ip', f'10.187.0.{i+10}', networks[1], name)
        interfaces[i] = next(iface['ifname'] for iface in addresses(i)
                             if any(x['local'] == f'10.187.0.{i+10}' for x in iface['addr_info']))
    nodes = {ids[i-1]: {'hostname': 'node'+str(i), 'bind_address': f'10.186.0.{i+10}',
             'bind_port': '8080', 'group_assignments': {interfaces[i]: ['test']}}
             for i in range(1, 5)}
    # Bootstrap node2 as incumbent without racing an explicit ownership handoff.
    # Node1 rejoins eligibility before any baseline or fault samples.
    nodes[ids[0]]['maintenance'] = True
    for i, name in enumerate(names, 1):
        cfg = {'pulseha': {'local_node': ids[i-1], 'cluster_token': 'isolated-regression-token',
                'mode': 'active-passive', 'hcs_interval': 1000, 'fos_interval': 5000,
                'fo_limit': 10000, 'auto_failback': True, 'logging_level': 'debug',
                'log_to_file': False}, 'nodes': nodes, 'floating_ip_groups': {'test': [fip+'/24']}}
        path = root/f'config{i}.json'
        path.write_text(json.dumps(cfg))
        docker('cp', str(path), name+':/etc/pulseha/config.json')
    for name in names:
        docker('exec', '-d', name, 'sh', '-c', 'exec pulseha > /tmp/daemon.log 2>&1')
    time.sleep(15)
    deadline = time.monotonic()+90
    consecutive = 0
    while time.monotonic() < deadline:
        consecutive = consecutive+1 if sample('settle') == [2] else 0
        if consecutive >= 10:
            break
        time.sleep(1)
    else:
        raise AssertionError('node2 never became the stable sole holder')
    print(execute(1, 'pulsectl', 'node', 'maintenance', '--disable'), flush=True)
    measure('coordinator-eligible', 15, [2])
    for r in range(1, a.rounds+1):
        measure(f'baseline-{r}', 5, [2])
        cut('-I')
        try:
            measure(f'cut-{r}', a.cut_seconds, [2])
        finally:
            cut('-D')
        measure(f'heal-{r}', a.heal_seconds, [2])
    # Availability control: a stopped incumbent with its addresses explicitly
    # removed must be replaced by the surviving configured majority.
    execute(2, 'pkill', '-KILL', '-x', 'pulseha')
    execute(2, 'ip', 'addr', 'del', fip+'/24', 'dev', interfaces[2])
    deadline = time.monotonic()+60
    successor = None
    while time.monotonic() < deadline:
        held = sample('incumbent-stopped')
        if len(held) == 1 and held != [2]:
            successor = held
            break
        time.sleep(1)
    if successor is None:
        raise AssertionError('majority did not replace stopped incumbent')
    measure('replacement-stable', 10, successor)
    print('PASS: asymmetric cuts/heals and stopped-incumbent failover', flush=True)
finally:
    for i, name in enumerate(created, 1):
        try:
            (root/f'node{i}.log').write_text(docker('exec', name, 'cat', '/tmp/daemon.log', check=False))
        finally:
            docker('rm', '-f', name, check=False)
    for name in reversed(created_nets):
        docker('network', 'rm', name, check=False)
