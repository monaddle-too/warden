#!/usr/bin/env python3
"""Regenerate the embedded HTTP/WebSocket contracts from shared message schemas."""
import json
from pathlib import Path
root=Path(__file__).resolve().parents[1]/'chat/internal/recordings/docs'
def obj(properties,required=()):
    return dict(type='object',properties=properties,required=list(required),additionalProperties=False)
S={'type':'string'};N={'type':'integer','minimum':0}
create=obj(dict(operationId=dict(type='string',minLength=8,maxLength=128),title=dict(type='string',minLength=1,maxLength=200),mode={'enum':['stream','upload']},encoding={'const':'pcm_s16le'},sampleRate={'const':16000},channels={'const':1},language={'enum':['en-US'],'default':'en-US'}),['operationId','title','mode','encoding','sampleRate','channels'])
segment=obj(dict(offsetBytes=N,size=N,text=S,state={'enum':['waiting','complete','failed']}),['offsetBytes','size','text','state'])
record=obj({**dict.fromkeys(['id','title','deviceId','deviceName','language','mode','provisional','error'],S),'status':{'enum':['live','uploading','interrupted','finished']},'transcription':{'enum':['waiting','transcribing','complete','failed']},**dict.fromkeys(['bytes','nextSeq','archivedBytes'],N),**dict.fromkeys(['createdAt','updatedAt'],{'type':'string','format':'date-time'}),'segments':{'type':'array','items':segment}},['id','title','status','transcription','bytes','nextSeq','archivedBytes'])
error=obj({'error':S},['error']);finish=obj({'nextSeq':N},['nextSeq'])
spec={'openapi':'3.1.0','info':{'title':'Warden device recordings','version':'1.0.0','description':'Organization-scoped audio capture. See /recording-api/ for framing, recovery, limits and example client.'},'servers':[{'url':'/'}],'security':[{'deviceKey':[]}],'paths':{},'components':{'securitySchemes':{'deviceKey':{'type':'http','scheme':'bearer','bearerFormat':'wrd_ device secret'},'browserSession':{'type':'apiKey','in':'cookie','name':'__Host-warden-session'}},'schemas':{'CreateRecording':create,'Recording':record,'Error':error,'Finish':finish}}}
def response(schema,description='Success',media='application/json'):
    return {'description':description,'content':{media:{'schema':schema}}}
def endpoint(path,method,summary,result=record,body=None,browser=False,media='application/json',code='200'):
    responses={code:response(result)}
    for status,description in [(400,'Invalid input'),(401,'Invalid/revoked credentials'),(403,'Access/CSRF refused'),(404,'Not found in this scope'),(409,'Sequence, metadata or lifecycle conflict'),(413,'Size limit exceeded'),(429,'Backpressure/quota; retry after five seconds'),(503,'Temporarily unavailable')]:
        responses[str(status)]=response(error,description)
    op={'summary':summary,'operationId':method+'_'+path.replace('/','_').replace('{','').replace('}',''),'responses':responses}
    params=[{'name':name,'in':'path','required':True,'schema':N if name=='seq' else S} for name in ['id','seq'] if '{'+name+'}' in path]
    if browser:
        op['security']=[{'browserSession':[]}]
        if method!='get':params += [{'name':name,'in':'header','required':True,'schema':S} for name in ['X-Warden-CSRF','Origin']]
    if params:op['parameters']=params
    if body is not None:op['requestBody']={'required':True,'content':{media:{'schema':body}}}
    spec['paths'].setdefault(path,{})[method]=op
endpoint('/v1/recordings','post','Create or idempotently recover a recording',body=create,code='201')
endpoint('/v1/recordings/{id}','get','Read own device recording, resume offset and transcript')
endpoint('/v1/recordings/{id}/chunks/{seq}','put','Commit one PCM chunk; duplicates must match',result=finish,body={'type':'string','format':'binary','maxLength':32000},media='application/octet-stream')
endpoint('/v1/recordings/{id}/upload','put','Upload complete PCM WAV, maximum 32 MiB, then finish',body={'type':'string','format':'binary'},media='audio/wav')
endpoint('/v1/recordings/{id}/finish','post','Finish with matching nextSeq',result=obj({'status':{'const':'finished'}}),body=finish)
device=obj({**dict.fromkeys(['id','name','creator','userId','createdAt'],S),'lastSeenAt':{'type':['string','null']},'revoked':{'type':'boolean'},'canManage':{'type':'boolean'}})
endpoint('/api/devices','get','List devices in selected organization',result=obj({'items':{'type':'array','items':device}}),browser=True)
endpoint('/api/devices','post','Register device; secret shown only once',result=obj({'id':S,'secret':S}),body=obj({'name':S},['name']),browser=True,code='201')
for action in ['rotate','revoke']:endpoint('/api/devices/{id}/'+action,'post',action+' device (creator or organization admin)',result=obj({'secret':S}),body=obj({}),browser=True)
endpoint('/api/recordings','get','List latest 100 organization recordings',result=obj({'items':{'type':'array','items':record}}),browser=True)
endpoint('/api/recordings/{id}','get','Read organization recording',browser=True)
endpoint('/api/recordings/{id}/finish','post','Finish organization recording',result=obj({'status':{'const':'finished'}}),body=finish,browser=True)
endpoint('/api/recordings/{id}/retry','post','Retry failed transcription without reuploading',result=obj({'status':S}),body=obj({}),browser=True)
endpoint('/api/recordings/{id}/audio','get','Play finished WAV; supports one HTTP byte range',browser=True)
op=spec['paths']['/api/recordings/{id}/audio']['get'];op['responses']['200']=response({'type':'string','format':'binary'},'PCM WAV','audio/wav');op['responses']['206']=response({'type':'string','format':'binary'},'Requested byte range','audio/wav');op['parameters'].append({'name':'Range','in':'header','schema':S});op['responses']['416']={'description':'Invalid byte range'}
endpoint('/api/recordings/events','get','Live SSE full snapshots; reconnect receives latest state',browser=True)
op=spec['paths']['/api/recordings/events']['get'];op['parameters']=[{'name':'id','in':'query','schema':S,'description':'Optional recording ID; omit for organization list'}];op['responses']['200']=response({'type':'string'},'SSE data contains Recording or {items: Recording[]}; id is snapshot digest','text/event-stream')
(root/'openapi.json').write_text(json.dumps(spec,indent=2)+'\n')
asyncspec={'asyncapi':'3.0.0','info':{'title':'Warden PCM streaming','version':'1.0.0'},'servers':{'cloud':{'host':'{host}','protocol':'wss','variables':{'host':{'default':'your-warden-host.example'}},'security':[{'$ref':'#/components/securitySchemes/deviceKey'}]}},'channels':{'recording':{'address':'/v1/recordings/{id}/stream','parameters':{'id':{'description':'Recording ID returned by HTTP create'}},'messages':{}}},'operations':{},'components':{'securitySchemes':{'deviceKey':{'type':'http','scheme':'bearer'}},'messages':{}}}
msgs={'audio':{'name':'AudioChunk','contentType':'application/octet-stream','description':'Binary uint64 big-endian sequence, followed by 2–32000 bytes PCM16 little-endian mono 16000 Hz; 3200 recommended. Retain until ack. Resume from status recording.nextSeq and recording.bytes.','payload':{'type':'string','format':'binary'}},'finish':{'name':'Finish','payload':obj({'type':{'const':'finish'},'nextSeq':N},['type','nextSeq'])},'ack':{'name':'Acknowledgement','payload':obj({'type':{'const':'ack'},'nextSeq':N},['type','nextSeq'])},'status':{'name':'Status','payload':obj({'type':{'const':'status'},'recording':record},['type','recording'])},'error':{'name':'Error','payload':obj({'type':{'const':'error'},'status':N,'error':S},['type','status','error'])}}
for name,msg in msgs.items():
    if name!='audio':msg['contentType']='application/json'
    asyncspec['components']['messages'][name]=msg;asyncspec['channels']['recording']['messages'][name]={'$ref':'#/components/messages/'+name}
for action,names in [('receive',['audio','finish']),('send',['ack','status','error'])]:asyncspec['operations'][action]={'action':action,'channel':{'$ref':'#/channels/recording'},'messages':[{'$ref':'#/channels/recording/messages/'+n} for n in names]}
(root/'asyncapi.json').write_text(json.dumps(asyncspec,indent=2)+'\n')
