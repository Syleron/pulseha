#!/usr/bin/env python3
"""Opt-in Witness rig (END-2631); requires Docker and a built test image.

Two serving nodes and a Witness, each on a control network (the heartbeat path,
bind_address) and a service network (witness_address and the floating IP). The
Witness container doubles as the client whose ARP cache is checked.

Phases, in order, each asserted on the kernel -- `ip addr` on the nodes and
`ip neigh` on the client -- never on the daemons' own claims:

  tier-1      cut the control link between the nodes with the Witness up: the
              incumbent stays sole holder, the other never promotes.
  control     the same cut with the Witness frozen: both nodes end up holding,
              which is today's behaviour and shows the Witness is what made the
              difference in tier-1.
  ruling      thaw the Witness with the split in place: the node it did not last
              see serving stands down, and the client's ARP entry ends on the
              keeper's MAC -- the re-announcement after the release.
  sigstop     heal, then freeze the incumbent's daemon. Its address stays in
              `ip addr` and answers nobody; the Witness must report it not
              visible, and the survivor must take the address.

All containers and networks this creates are removed in finally. Logs, samples
and the arping announcement logs stay in the printed directory.
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
p.add_argument('--image', default='pulseha-witness')
p.add_argument('--cut-seconds', type=int, default=35)
a = p.parse_args()
root = Path(tempfile.mkdtemp(prefix='pulseha-witness-'))
prefix = 'phw-' + uuid.uuid4().hex[:8]
names = [prefix + '-' + n for n in ('node1', 'node2', 'witness')]
networks = [prefix + '-control', prefix + '-service']
ids = ['00000000-0000-4000-8000-%012d' % i for i in range(1, 4)]
control = lambda i: f'10.196.0.{i+10}'
service = lambda i: f'10.197.0.{i+10}'
fip = '10.197.0.100'
WITNESS = 3
created, created_nets = [], []
print('Evidence:', root, flush=True)


def docker(*args, check=True):
    r = subprocess.run(['docker', *args], text=True, capture_output=True, timeout=90)
    if check and r.returncode:
        raise RuntimeError('docker ' + ' '.join(args) + ': ' + r.stderr + r.stdout)
    return r.stdout


def execute(i, *args, check=True):
    return docker('exec', names[i-1], *args, check=check)


def addresses(i):
    return json.loads(execute(i, 'ip', '-j', '-4', 'addr', 'show'))


def owns(i):
    return any(x['local'] == fip for iface in addresses(i) for x in iface['addr_info'])


def holders(phase):
    with ThreadPoolExecutor(max_workers=2) as pool:
        held = list(pool.map(owns, (1, 2)))
    row = {'time': time.time(), 'phase': phase, 'holders': [i+1 for i, v in enumerate(held) if v]}
    with (root/'samples.jsonl').open('a') as f:
        f.write(json.dumps(row)+'\n')
    return row['holders']


def hold(phase, seconds, expected):
    end, count = time.monotonic()+seconds, 0
    while time.monotonic() < end:
        got = holders(phase)
        if got != expected:
            raise AssertionError(f'{phase}: holders={got}, expected={expected}')
        count += 1
        time.sleep(.7)
    print(f'{phase}: {count} samples, holders {expected} throughout', flush=True)


def reach(phase, seconds, expected):
    end = time.monotonic()+seconds
    while time.monotonic() < end:
        got = holders(phase)
        if got == expected:
            print(f'{phase}: holders reached {expected}', flush=True)
            return
        time.sleep(.7)
    raise AssertionError(f'{phase}: holders never reached {expected} (last {got})')


def cut(action):
    for i, other in [(1, 2), (2, 1)]:
        for chain, direction in [('INPUT', '-s'), ('OUTPUT', '-d')]:
            execute(i, 'iptables', action, chain, direction, control(other), '-j', 'DROP')


def signal(i, sig):
    execute(i, 'pkill', '-' + sig, '-x', 'pulseha')


def mac(i, iface):
    return next(x['address'] for x in json.loads(execute(i, 'ip', '-j', 'link', 'show', iface)))


def client_sees(phase):
    """The MAC the client's ARP cache holds for the floating IP, after traffic."""
    execute(WITNESS, 'ping', '-c', '1', '-W', '1', fip, check=False)
    neigh = json.loads(execute(WITNESS, 'ip', '-j', 'neigh', 'show', fip))
    got = neigh[0].get('lladdr') if neigh else None
    with (root/'arp.jsonl').open('a') as f:
        f.write(json.dumps({'time': time.time(), 'phase': phase, 'lladdr': got})+'\n')
    return got


def client_reaches(phase, seconds, want):
    end = time.monotonic()+seconds
    while time.monotonic() < end:
        if client_sees(phase) == want:
            print(f'{phase}: client ARP entry on {want}', flush=True)
            return
        time.sleep(1)
    raise AssertionError(f'{phase}: client ARP entry never reached {want} (last {client_sees(phase)})')


def witness_reading():
    status = json.loads(execute(1, 'pulsectl', 'status', '--json'))
    w = next(m for m in status['members'] if m.get('witness'))
    return w['status'], w.get('witness_trusted', False)


try:
    for name, subnet in zip(networks, ['10.196.0.0/24', '10.197.0.0/24']):
        docker('network', 'create', '--subnet', subnet, name)
        created_nets.append(name)
    ifaces = {}
    for i, name in enumerate(names, 1):
        docker('run', '-d', '--name', name, '--hostname', name.rsplit('-', 1)[1],
               '--cap-add', 'NET_ADMIN', '--cap-add', 'NET_RAW',
               '--network', networks[0], '--ip', control(i), a.image, 'sleep', 'infinity')
        created.append(name)
        docker('network', 'connect', '--ip', service(i), networks[1], name)
        ifaces[i] = next(x['ifname'] for x in addresses(i)
                         if any(y['local'] == service(i) for y in x['addr_info']))

    nodes = {}
    for i in (1, 2, 3):
        nodes[ids[i-1]] = {'hostname': names[i-1].rsplit('-', 1)[1], 'bind_address': control(i),
                           'bind_port': '8080', 'witness_address': f'{service(i)}:9084',
                           'group_assignments': {} if i == WITNESS else {ifaces[i]: ['test']}}
    nodes[ids[WITNESS-1]]['witness'] = True
    # node2 bootstraps as the incumbent; node1 rejoins eligibility afterwards.
    nodes[ids[0]]['maintenance'] = True
    def write_configs():
        for i, name in enumerate(names, 1):
            cfg = {'pulseha': {'local_node': ids[i-1], 'cluster_token': 'witness-rig-token',
                   'mode': 'active-passive', 'hcs_interval': 1000, 'fos_interval': 5000,
                   'fo_limit': 10000, 'auto_failback': True, 'logging_level': 'debug',
                   'log_to_file': False}, 'nodes': nodes, 'floating_ip_groups': {'test': [fip+'/24']}}
            path = root/f'config{i}.json'
            path.write_text(json.dumps(cfg))
            docker('cp', str(path), name+':/etc/pulseha/config.json')

    # Certificates are seeded rather than left to the daemons to publish. Each
    # daemon publishes its certificate into the cluster config at start, and
    # nodes that start from the same file each make that publish config version
    # 1 under their own origin; ConfigSync ignores an equal version from another
    # origin, so the certificates never converge and the Witness path is never
    # trusted. That is a ConfigSync defect that predates the Witness: #268.
    # Remove this seeding once it is fixed, and this rig becomes its regression.
    # Here each daemon runs once to mint its identity -- stable across restarts
    # (EnsureCertificates) -- and every config is then written already holding
    # all three, the state the publishes should have converged to.
    write_configs()
    for name in names:
        docker('exec', '-d', name, 'sh', '-c', 'exec pulseha > /tmp/mint.log 2>&1')
    time.sleep(6)
    for i, name in enumerate(names, 1):
        execute(i, 'pkill', '-KILL', '-x', 'pulseha', check=False)
    time.sleep(1)
    for i in (1, 2, 3):
        nodes[ids[i-1]]['tls_cert'] = execute(i, 'cat', '/root/.pulseha/certs/pulseha.crt').strip()
        execute(i, 'sh', '-c', 'ip addr del %s/24 dev %s 2>/dev/null; true' % (fip, ifaces[i]) if i != WITNESS else 'true')
    write_configs()
    for name in names:
        docker('exec', '-d', name, 'sh', '-c', 'exec pulseha > /tmp/daemon.log 2>&1')

    time.sleep(15)
    reach('settle', 90, [2])
    print(execute(1, 'pulsectl', 'node', 'maintenance', '--disable'), flush=True)
    deadline = time.monotonic()+90
    while time.monotonic() < deadline:
        st, trusted = witness_reading()
        if st == 'Witness' and trusted:
            break
        time.sleep(2)
    else:
        raise AssertionError(f'the Witness never read as arbitrating (last {witness_reading()})')
    print('Witness: arbitrating', flush=True)
    hold('baseline', 15, [2])

    # Tier 1: cut the heartbeat path only; the Witness sees both.
    cut('-I')
    try:
        hold('tier-1', a.cut_seconds, [2])

        # Control: the same split with the Witness frozen is today's both-Active.
        signal(WITNESS, 'STOP')
        reach('control', 60, [1, 2])
        hold('control-steady', 5, [1, 2])
        # node1 promoted last, so the client is expected to have followed it. If
        # it did not, the ruling phase cannot show a repair -- say so rather than
        # pass on an ARP entry that never moved.
        macs = {1: mac(1, ifaces[1]), 2: mac(2, ifaces[2])}
        print('MACs: node1', macs[1], 'node2', macs[2], flush=True)
        during = client_sees('control')
        print('control: client ARP entry on', during,
              '(node1 already)' if during == macs[1] else '(not node1; seeding it below)', flush=True)

        # While both hold the address both answer ARP requests, so the client
        # may never have followed node1 on its own. Put its cache where the
        # network ends up when node1 announced last -- the state ADR-0002
        # measured -- so the phase below tests that node2's re-announcement,
        # and nothing else, moves it back.
        if during != macs[1]:
            execute(WITNESS, 'ip', 'neigh', 'replace', fip, 'lladdr', macs[1], 'nud', 'reachable',
                    'dev', next(x['ifname'] for x in addresses(WITNESS)
                                if any(y['local'] == service(WITNESS) for y in x['addr_info'])))
            print('control: client ARP entry seeded to node1', flush=True)

        # Ruling: thaw the Witness with the split still in place.
        signal(WITNESS, 'CONT')
        reach('ruling', 60, [2])
        # Within a few seconds, well inside the entry's reachable time, so only
        # an announcement can have moved it -- not the kernel re-probing.
        client_reaches('ruling', 10, mac(2, ifaces[2]))
        hold('ruling-steady', 15, [2])
    finally:
        signal(WITNESS, 'CONT')
        cut('-D')
    reach('heal', 60, [2])
    hold('healed', 15, [2])

    # SIGSTOP the incumbent: still in `ip addr`, answering nobody.
    signal(2, 'STOP')
    try:
        end = time.monotonic()+60
        while time.monotonic() < end:
            if 1 in holders('sigstop'):
                break
            time.sleep(.7)
        else:
            raise AssertionError('sigstop: node1 never took the address from a frozen incumbent')
        print('sigstop: node1 took the address from the frozen incumbent', flush=True)
        client_reaches('sigstop', 30, mac(1, ifaces[1]))
    finally:
        signal(2, 'CONT')
    print('PASS: tier-1 hold, both-Active control, ruling with re-announcement, frozen incumbent', flush=True)
finally:
    for i, name in enumerate(created, 1):
        try:
            (root/f'{name.rsplit("-", 1)[1]}.log').write_text(docker('exec', name, 'cat', '/tmp/daemon.log', check=False))
            (root/f'{name.rsplit("-", 1)[1]}-arping.log').write_text(
                docker('exec', name, 'cat', '/var/log/pulseha/arping.log', check=False))
        finally:
            docker('rm', '-f', name, check=False)
    for name in reversed(created_nets):
        docker('network', 'rm', name, check=False)
