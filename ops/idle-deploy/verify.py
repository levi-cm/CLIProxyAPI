"""Small real HTTP/SSE/WS probes; opaque continuation stays in private release state."""
import asyncio
import hashlib
import inspect as pyinspect
import json
from pathlib import Path
import time
import uuid

from common import ROOT,api,atomic,key,owners,prefix_digest,probe


def model():
    available={m['id'] for m in json.loads(api('/v1/models')[2])['data']}
    for name in ('gpt-6.1-sol','gpt-6-sol','gpt-6-astra','gpt-5.4'):
        if name in available:return name
    raise RuntimeError('no supported verification model currently advertised')


def bound_owner(session,chosen):
    hashed=hashlib.sha256(('codex:'+session).encode()).hexdigest()
    matches=[v for k,v in owners(ROOT/'account-policy-state').items() if json.loads(k)['session_hash']==hashed and json.loads(k)['model']==chosen]
    if len(matches)!=1:raise RuntimeError('verification conversation has no unique durable owner')
    return matches[0]


def before(directory):
    chosen=model();session='idle-release-'+uuid.uuid4().hex
    headers={'Session_id':session,'User-Agent':'codex_cli_rs/idle-deploy'}
    body={'model':chosen,'input':[{'role':'user','content':'Compute 197 times 239 internally, then reply only with the integer.'}],'include':['reasoning.encrypted_content'],'reasoning':{'effort':'medium'},'store':False}
    status,_,raw=api('/v1/responses',body=body,headers=headers,timeout=180);data=json.loads(raw)
    if status!=200 or data.get('status')!='completed' or not any(v.get('encrypted_content') for v in data.get('output',[])):raise RuntimeError('encrypted pre-deployment proof unavailable')
    atomic(Path(directory)/'continuity.json',{'body':body,'headers':headers,'output':data['output'],'owner':bound_owner(session,chosen)})
    return {'predeployment_encrypted_proof':True,'model':chosen}


def after(directory,commit):
    result=probe(commit)
    for item in json.loads((Path(directory)/'history-before.json').read_text()):
        if prefix_digest(ROOT/'account-policy-state/history'/item['name'],item['size'])!=item['sha256']:raise RuntimeError('retained historical bytes changed')
    result['retained_history_prefixes_verified']=True
    record=json.loads((Path(directory)/'continuity.json').read_text());body=record['body'];session=record['headers']['Session_id'];chosen=body['model']
    if bound_owner(session,chosen)!=record['owner']:raise RuntimeError('predeployment conversation owner changed')
    body['input']+=record['output']+[{'role':'user','content':'Add 17 to the previous result and reply only the integer.'}];body['stream']=True
    status,_,raw=api('/v1/responses',body=body,headers=record['headers'],timeout=180)
    if status!=200 or b'"type":"response.completed"' not in raw.replace(b' ',b''):raise RuntimeError('encrypted continuation SSE did not complete')
    if bound_owner(session,chosen)!=record['owner']:raise RuntimeError('encrypted continuation migrated account')
    result.update(encrypted_continuation_same_owner=True,complete_sse=True)
    # A standard client must receive public output without private model metadata.
    plain={'model':chosen,'input':'Reply only READY.','stream':True,'store':False}
    status,_,data=api('/v1/responses',body=plain,headers={'Session_id':'idle-standard-'+uuid.uuid4().hex,'User-Agent':'idle-verifier/1'},timeout=180)
    if status!=200 or b'response.completed' not in data or b'event: response.metadata' in data or b'codex.response.metadata' in data:raise RuntimeError('standard SSE verification failed')
    result['standard_sse']=True

    async def ws():
        import websockets
        headers={'Authorization':'Bearer '+key('CLI_PROXY_API_KEY'),'Session_id':'idle-ws-'+uuid.uuid4().hex,'User-Agent':'codex_cli_rs/idle-deploy'}
        keyword='additional_headers' if 'additional_headers' in pyinspect.signature(websockets.connect).parameters else 'extra_headers'
        async with asyncio.timeout(180):
            async with websockets.connect('ws://100.82.251.30:8317/v1/responses',**{keyword:headers},ping_interval=None,open_timeout=10,close_timeout=2,max_size=16<<20) as conn:
                first=None
                for turn in range(2):
                    req={'type':'response.create','model':chosen,'input':[{'role':'user','content':'Reply only READY.'}],'store':False}
                    if first:req['previous_response_id']=first
                    await conn.send(json.dumps(req))
                    while True:
                        event=json.loads(await conn.recv())
                        if event.get('type') in ('error','response.failed'):raise RuntimeError('WebSocket verification turn failed')
                        if event.get('type')=='response.completed':first=event['response']['id'];break
    asyncio.run(ws());result['two_websocket_turns']=True
    return result
