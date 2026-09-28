#!/usr/bin/env python3
"""Warden device reference client. Python 3.10+; pip install websockets==15.0.1.
The secret is read from WARDEN_DEVICE_KEY or a hidden prompt, never logged.
Audio stays on your machine; only the selected WAV is sent to --server.
"""
import argparse
import getpass
import json
import os
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import wave

# Filled in when copied from the device registration page.
DEVICE_KEY = ""
SERVER_URL = ""

class APIError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status

class Client:
    def __init__(self, server, key):
        self.server, self.key = server.rstrip('/'), key
    def request(self, method, path, data=None, content_type='application/json'):
        if data is not None and not isinstance(data, bytes):
            data = json.dumps(data).encode()
        req = urllib.request.Request(self.server+'/v1/recordings'+path, data=data, method=method,
            headers={'Authorization':'Bearer '+self.key, 'Content-Type':content_type})
        try:
            with urllib.request.urlopen(req, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            try: message = json.load(error).get('error', 'Request failed')
            except Exception: message = 'Request failed'
            raise APIError(error.code, message) from None
    def retry(self, method, path, data=None):
        for attempt in range(12):
            try: return self.request(method, path, data)
            except APIError as error:
                if error.status not in (429, 502, 503, 504): raise
            except (urllib.error.URLError, TimeoutError): pass
            time.sleep(min(2**attempt, 10))
        raise RuntimeError('Server remains unavailable. Resume with the printed recording ID.')

def read_audio(path):
    with wave.open(path, 'rb') as wav:
        if (wav.getnchannels(), wav.getsampwidth(), wav.getframerate(), wav.getcomptype()) != (1, 2, 16000, 'NONE'):
            raise ValueError('Use 16 kHz, mono, 16-bit PCM WAV.')
        if wav.getnframes() > 16000*7200:
            raise ValueError('Maximum recording length is two hours.')
        return wav.readframes(wav.getnframes())

def stream(client, recording, audio, args):
    try:
        from websockets.sync.client import connect
        from websockets.exceptions import ConnectionClosed, InvalidStatus
    except ImportError:
        raise RuntimeError('Install streaming support: python3 -m pip install websockets==15.0.1') from None
    address = client.server.replace('https://', 'wss://', 1).replace('http://', 'ws://', 1)
    injected, failures, shown = False, 0, ''
    while True:
        state = client.retry('GET', '/'+recording)
        offset, seq = state['bytes'], state['nextSeq']
        if offset > len(audio): raise ValueError('Server has more audio than this file. Use the original WAV.')
        if state['status']=='finished':
            if offset != len(audio): raise ValueError('Recording was finished before the whole file was uploaded.')
            return
        try:
            with connect(address+'/v1/recordings/'+recording+'/stream', additional_headers={'Authorization':'Bearer '+client.key}, compression=None, open_timeout=15, close_timeout=2) as ws:
                hello = json.loads(ws.recv(timeout=20))
                if hello.get('type')!='status': raise RuntimeError('Unexpected server handshake.')
                offset, seq = hello['recording']['bytes'], hello['recording']['nextSeq']
                while offset < len(audio):
                    start = time.monotonic()
                    chunk = audio[offset:offset+3200]
                    ws.send(struct.pack('>Q',seq)+chunk)
                    while True:
                        message = json.loads(ws.recv(timeout=20))
                        if message['type']=='ack':
                            if message['nextSeq']!=seq+1: raise RuntimeError('Concurrent writer detected. Use one connection per recording.')
                            break
                        if message['type']=='error': raise APIError(message['status'],message['error'])
                        if message['type']=='status':
                            rec = message['recording']
                            text = ' '.join(s['text'] for s in rec.get('segments',[]) if s['state']=='complete')
                            if rec.get('provisional'): text += ' ['+rec['provisional']+' — provisional]'
                            if text and text!=shown: print(text,flush=True); shown=text
                    offset += len(chunk); seq += 1; failures=0
                    if args.reconnect_after and not injected and offset/32000>=args.reconnect_after:
                        injected=True
                        print('Deliberate disconnect; resuming from durable acknowledgement.',flush=True)
                        break
                    if not args.fast: time.sleep(max(0,len(chunk)/32000-(time.monotonic()-start)))
                if offset==len(audio):
                    ws.send(json.dumps({'type':'finish','nextSeq':seq}))
                    # HTTP confirmation handles a dropped final WebSocket response.
                    client.retry('POST','/'+recording+'/finish',{'nextSeq':seq})
                    return
        except APIError as error:
            if error.status not in (429,502,503,504): raise
            failures+=1; time.sleep(5)
        except InvalidStatus as error:
            if error.response.status_code in (401,403,404): raise RuntimeError('Device key was revoked or recording is unavailable.') from None
            failures+=1;time.sleep(min(2**min(failures,4),10))
        except (OSError,TimeoutError,ConnectionClosed):
            failures+=1;time.sleep(min(2**min(failures,4),10))
        if failures>=12: raise RuntimeError('Repeated connection failures. Resume with --recording-id '+recording)

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('mode',choices=['upload','stream'])
    p.add_argument('wav',help='16 kHz mono PCM16 WAV file')
    p.add_argument('--server',default=os.environ.get('WARDEN_URL') or SERVER_URL,required=not (os.environ.get('WARDEN_URL') or SERVER_URL))
    p.add_argument('--title',default='Device recording')
    p.add_argument('--recording-id',help='Resume an existing recording with the same original file')
    p.add_argument('--operation-id',default=str(uuid.uuid4()),help='Reuse this value when retrying recording creation')
    p.add_argument('--reconnect-after',type=float,default=0,metavar='SECONDS',help='Deliberately reconnect once during streaming')
    p.add_argument('--fast',action='store_true',help='Send faster than real time')
    p.add_argument('--wait',type=int,default=180,help='Seconds to wait for final transcription; 0 skips waiting')
    args=p.parse_args()
    parsed=urllib.parse.urlsplit(args.server)
    if parsed.scheme!='https' and not(parsed.scheme=='http' and parsed.hostname in ('127.0.0.1','localhost')):
        p.error('--server must use HTTPS (HTTP is allowed only for localhost tests).')
    if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ('','/'):
        p.error('--server must be a plain origin, without credentials or path.')
    audio=read_audio(args.wav)
    key=os.environ.get('WARDEN_DEVICE_KEY') or DEVICE_KEY or getpass.getpass('Device key: ')
    client=Client(args.server,key.strip())
    print('Operation ID:',args.operation_id,flush=True)
    if args.recording_id:
        recording=args.recording_id
        state=client.retry('GET','/'+recording)
        if state['mode']!=args.mode: raise ValueError('Resume with the original recording mode.')
    else:
        state=client.retry('POST','',{'title':args.title,'operationId':args.operation_id,'mode':args.mode,'language':'en-US','encoding':'pcm_s16le','sampleRate':16000,'channels':1})
        recording=state['id']
    print('Recording:',recording,flush=True)
    print('View:',client.server+'/recordings/'+recording,flush=True)
    if args.mode=='stream': stream(client,recording,audio,args)
    else:
        # Resumable HTTP upload of decoded WAV samples; bounded requests avoid
        # retransmitting a whole file after a connection failure.
        state=client.retry('GET','/'+recording)
        offset,seq=state['bytes'],state['nextSeq']
        if offset>len(audio): raise ValueError('Use the original WAV to resume.')
        while offset<len(audio):
            chunk=audio[offset:offset+32000]
            result=client.retry('PUT',f'/{recording}/chunks/{seq}',chunk)
            if result['nextSeq']!=seq+1: raise RuntimeError('Concurrent writer detected.')
            seq+=1;offset+=len(chunk)
        client.retry('POST','/'+recording+'/finish',{'nextSeq':seq})
    print('Audio saved; waiting for transcription.',flush=True)
    deadline=time.monotonic()+args.wait
    while time.monotonic()<deadline:
        state=client.retry('GET','/'+recording)
        if state['transcription'] in ('complete','failed'):
            for segment in state.get('segments',[]): print(segment['text'] or ('[No speech detected]' if segment['state']=='complete' else '[Transcription failed; retry in web UI]'))
            print('Transcription:',state['transcription'],flush=True)
            return 0 if state['transcription']=='complete' else 2
        time.sleep(2)
    print('Still processing. Follow progress in the web UI.',flush=True)
    return 0
if __name__=='__main__':
    try: sys.exit(main())
    except (APIError,ValueError,RuntimeError,OSError) as e:
        print('Error:',e,file=sys.stderr);sys.exit(1)
    except KeyboardInterrupt:
        print('\nInterrupted. Resume with --recording-id and the same WAV.',file=sys.stderr);sys.exit(130)
