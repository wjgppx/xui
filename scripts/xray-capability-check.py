#!/usr/bin/env python3
"""Independent loopback-only VLESS/VMess TCP capability experiment; no production credentials."""
import argparse
import json
import os
from pathlib import Path
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
import uuid

parser = argparse.ArgumentParser()
parser.add_argument('--protocol', choices=['vless', 'vmess'], default='vless')
protocol = parser.parse_args().protocol
xray = os.environ.get('XUI_XRAY_BIN', '/workspace/shared/tools/xray-26.10.21/xray')
def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]
def recv_exact(s, size):
    data = b''
    while len(data) < size:
        chunk = s.recv(size - len(data))
        if not chunk:
            raise ConnectionError('connection closed')
        data += chunk
    return data
class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        self.request.settimeout(15)
        try:
            while data := self.request.recv(65536):
                self.request.sendall(data)
        except OSError:
            pass
class EchoServer(socketserver.ThreadingTCPServer):
    daemon_threads = True

processes = []
active = None
with tempfile.TemporaryDirectory(prefix='xui-xray-capability-') as directory, EchoServer(('127.0.0.1', 0), Echo) as echo:
    thread = threading.Thread(target=echo.serve_forever, daemon=True)
    thread.start()
    root = Path(directory)
    api_port, node_port, socks_port = free_port(), free_port(), free_port()
    while len({api_port, node_port, socks_port}) != 3:
        api_port, node_port, socks_port = free_port(), free_port(), free_port()
    identity, subject = str(uuid.uuid4()), 'capability-subject@xui.test'
    server_config = {
        'log': {'loglevel': 'debug'},
        'api': {'tag': 'management', 'services': ['HandlerService', 'StatsService']},
        'stats': {}, 'policy': {'levels': {'0': {'statsUserUplink': True, 'statsUserDownlink': True}}},
        'inbounds': [
            {'tag': 'management', 'listen': '127.0.0.1', 'port': api_port, 'protocol': 'dokodemo-door', 'settings': {'address': '127.0.0.1'}},
            {'tag': 'capability-inbound', 'listen': '127.0.0.1', 'port': node_port, 'protocol': protocol, 'settings': {'clients': [], **({'decryption': 'none'} if protocol == 'vless' else {})}},
        ],
        'outbounds': [{'protocol': 'freedom', 'settings': {'finalRules': [{'action': 'allow', 'network': 'tcp', 'ip': ['127.0.0.1/32'], 'port': echo.server_address[1]}]}}],
        'routing': {'rules': [{'type': 'field', 'inboundTag': ['management'], 'outboundTag': 'management'}]},
    }
    client_config = {
        'log': {'loglevel': 'debug'},
        'inbounds': [{'listen': '127.0.0.1', 'port': socks_port, 'protocol': 'socks', 'settings': {'auth': 'noauth', 'udp': False}}],
        'outbounds': [{'protocol': protocol, 'settings': {'vnext': [{'address': '127.0.0.1', 'port': node_port, 'users': [{'id': identity, **({'encryption': 'none'} if protocol == 'vless' else {'security': 'auto'})}]}]}}],
    }
    def api(command, *args):
        result = subprocess.run([xray, 'api', command, f'--server=127.0.0.1:{api_port}', *args], capture_output=True, text=True, timeout=8)
        if result.returncode:
            raise RuntimeError(f'Xray API {command} failed: {result.stderr}')
        return result.stdout
    def start(name, config):
        path = root / f'{name}.json'
        path.write_text(json.dumps(config))
        path.chmod(0o600)
        subprocess.run([xray, 'run', '-test', '-config', str(path)], capture_output=True, check=True, timeout=10)
        log = open(root / f'{name}.log', 'wb')
        p = subprocess.Popen([xray, 'run', '-config', str(path)], stdout=log, stderr=log)
        log.close()
        processes.append(p)
    def connect():
        s = socket.create_connection(('127.0.0.1', socks_port), timeout=10)
        try:
            s.sendall(b'\x05\x01\x00')
            if recv_exact(s, 2) != b'\x05\x00':
                raise ConnectionError('SOCKS negotiation failed')
            s.sendall(b'\x05\x01\x00\x01' + socket.inet_aton('127.0.0.1') + struct.pack('!H', echo.server_address[1]))
            response = recv_exact(s, 4)
            if response[1] != 0:
                raise ConnectionError('SOCKS connect rejected')
            length = {1: 4, 4: 16}.get(response[3])
            if length is None:
                length = recv_exact(s, 1)[0]
            recv_exact(s, length + 2)
            return s
        except BaseException:
            s.close()
            raise
    def transfer(s, payload):
        s.sendall(payload)
        if recv_exact(s, len(payload)) != payload:
            raise AssertionError('proxied payload corrupted')
    def counters():
        raw = api('statsquery', '-pattern', 'user>>>' + subject)
        result = json.loads(raw[raw.index('{'):])
        return {stat['name']: int(stat.get('value', 0)) for stat in result.get('stat', [])}
    try:
        start('server', server_config)
        start('client', client_config)
        for attempt in range(40):
            try:
                raw = api('inboundusercount', '-tag=capability-inbound')
                break
            except (RuntimeError, subprocess.TimeoutExpired):
                if attempt == 39:
                    raise
                time.sleep(.1)
        add = root / 'add-user.json'
        add.write_text(json.dumps({'inbounds': [{'tag': 'capability-inbound', 'listen': '127.0.0.1', 'port': node_port, 'protocol': protocol, 'settings': {'clients': [{'id': identity, 'email': subject, 'level': 0}], **({'decryption': 'none'} if protocol == 'vless' else {})}}]}))
        api('adu', str(add))
        count = json.loads(api('inboundusercount', '-tag=capability-inbound'))
        assert int(count.get('count', 0)) == 1, 'user addition did not take effect'
        active = connect()
        transfer(active, b'xui-independent-capability-test' * 1024)
        first = counters()
        second = counters()
        assert len(first) >= 2 and all(v > 0 for v in first.values()), 'missing live per-user traffic counters'
        assert all(second.get(k, 0) >= v for k, v in first.items()), 'default stats query reset counters'
        api('rmu', '-tag=capability-inbound', subject)
        try:
            transfer(active, b'check-existing-session-after-user-removal')
            existing_survives = True
        except OSError:
            existing_survives = False
        new_denied = False
        try:
            with connect() as new:
                transfer(new, b'new-session-must-be-rejected')
        except OSError:
            new_denied = True
        assert new_denied, 'removed credential still allowed a new connection'
        version = subprocess.check_output([xray, 'version'], text=True).splitlines()[0]
        print(json.dumps({'core': version, 'scope': f'loopback {protocol} TCP; no TLS/WS or throughput claims', 'hot_add_and_proxy_payload': True, 'non_resetting_positive_counters': True, 'new_connection_rejected_after_removal': new_denied, 'existing_connection_survives_removal': existing_survives}, ensure_ascii=False, indent=2))
    except BaseException:
        for log in root.glob('*.log'):
            print(log.name, log.read_text())
        raise
    finally:
        if active:
            active.close()
        for p in processes:
            p.terminate()
        for p in processes:
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill(); p.wait()
        echo.shutdown()
