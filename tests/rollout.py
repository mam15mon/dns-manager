import argparse, base64, json, os, pathlib, shutil, socket, ssl, subprocess as sub, tempfile, time, urllib.request
parser = argparse.ArgumentParser()
parser.add_argument('--coredns', type=lambda s: pathlib.Path(s).resolve(), required=True)
parser.add_argument('--node', type=lambda s: pathlib.Path(s).resolve(), required=True)
parser.add_argument('--controller', type=lambda s: pathlib.Path(s).resolve(), required=True)
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parents[1]
def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0))
        return s.getsockname()[1]
def wait(f, timeout=23):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        try:
            if f(): return
        except (OSError, subprocess_error): pass
        time.sleep(.2)
    raise AssertionError('condition timed out')
subprocess_error=sub.SubprocessError
with tempfile.TemporaryDirectory() as tmp:
    d=pathlib.Path(tmp)
    cert,key=d/'cert',d/'key'
    sub.run(['openssl','req','-x509','-nodes','-newkey','rsa:2048','-days','1','-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost,IP:127.0.0.1','-keyout',key,'-out',cert],stdout=sub.DEVNULL,stderr=sub.DEVNULL,check=True)
    ports=set()
    while len(ports)<5:
        ports.add(port())
    first,second,first_api,second_api,cp=ports
    p=[first,second]
    api=[first_api,second_api]
    processes=[]
    token='test-token-012345678901234567890123456'
    password='controller-test-password'
    env={**os.environ,'DNS_NODE_TOKEN':token,'DNS_ADMIN_PASSWORD':password,'TEST_TOKEN':token}
    ctx=ssl.create_default_context(cafile=str(cert))
    for i in range(2):
        (d/str(i)).mkdir()
        shutil.copyfile(root/'examples/zone.db',d/str(i)/'zone.db')
        (d/str(i)/'Corefile').write_text((root/'examples/Corefile.example').read_text().replace('1053',str(p[i])))
    cfg=json.loads((root/'examples/controller.json').read_text())
    z=cfg['Zones'][0]
    for i,t in enumerate(z['Targets']):
        t.update(URL=f'https://127.0.0.1:{api[i]}/v1/zone',TokenEnv='TEST_TOKEN')
    (d/'settings').write_text(json.dumps(cfg))
    def launch(args,cwd=d):
        proc=sub.Popen(args,cwd=cwd,env=env,stdout=sub.DEVNULL)
        processes.append(proc)
        return proc
    def start_dns(i):
        return launch([args.coredns,'-conf','Corefile'],d/str(i))
    def start_node(i):
        return launch([args.node,'-listen',f'127.0.0.1:{api[i]}','-zone',z['Name'],'-file',d/str(i)/'zone.db','-dns',f'127.0.0.1:{p[i]}','-tls-cert',cert,'-tls-key',key,'-client-ca',cert])
    def start_controller():
        return launch([args.controller,'-listen',f'127.0.0.1:{cp}','-config','settings','-state','releases','-node-ca',cert,'-tls-cert',cert,'-tls-key',key,'-node-client-cert',cert,'-node-client-key',key])
    def request(path='/api/releases', data=None):
        auth=base64.b64encode(('admin:'+password).encode()).decode()
        req=urllib.request.Request(f'https://localhost:{cp}'+path,data=None if data is None else json.dumps(data).encode(),headers={'Authorization':'Basic '+auth,'Content-Type':'application/json'})
        with urllib.request.urlopen(req,context=ctx,timeout=3) as resp: return resp.read()
    def history():
        return json.loads(request())[0]['History']
    def publish(address=None,rollback=0):
        records=[dict(r) for r in z['InitialRecords']]
        records[1]['Value']=address
        return json.loads(request(data={'Zone':z['Name'],'Records':records,'Rollback':rollback}))['serial']
    def query(i,tcp=False):
        return sub.check_output(['dig','@127.0.0.1','-p',str(p[i]),'git.infra.example.com','A','+short','+time=1','+tries=1']+(['+tcp'] if tcp else []),text=True).strip()
    def stop(proc):
        proc.terminate()
        proc.wait(timeout=5)
    try:
        ds=[start_dns(i) for i in range(2)]
        n0=start_node(0)
        controller=start_controller()
        wait(lambda: '内网 DNS 管理' in request('/').decode())
        assert publish('192.0.2.21')==2
        wait(lambda: history()[-1]['Nodes'].get('dns-a2',{}).get('Error'))
        sub.run([root/'deploy/check-coredns', z['Name'], '127.0.0.1', str(p[0])], check=True)
        assert query(0)=='192.0.2.21' and query(1)=='192.0.2.20'
        n1=start_node(1)
        wait(lambda: history()[-1]['Nodes'].get('dns-a2',{}).get('Applied'))
        assert publish('192.0.2.22')==3
        wait(lambda: all(query(i)=='192.0.2.22' for i in range(2)))
        assert publish(rollback=2)==4
        wait(lambda: len([n for n in history()[-1]['Nodes'].values() if n['Applied']])==2)
        stop(controller)
        stop(n0)
        stop(n1)
        for i in range(2):
            stop(ds[i])
            ds[i]=start_dns(i)
        wait(lambda: all(query(i)==query(i,True)=='192.0.2.21' for i in range(2)))
        stop(ds[0])
        assert sub.run([root/'deploy/check-coredns', z['Name'], '127.0.0.1', str(p[0])],
                       stdout=sub.DEVNULL).returncode != 0
        assert query(1)==query(1,True)=='192.0.2.21'
        controller=start_controller()
        wait(lambda: history()[-1]['Serial']==4)
        print('PASS: HTTPS/mTLS rollout, per-node failure/retry, rollback, controller state persistence, two offline CoreDNS restarts (UDP/TCP)')
    finally:
        for proc in reversed(processes):
            if proc.poll() is None:
                stop(proc)
