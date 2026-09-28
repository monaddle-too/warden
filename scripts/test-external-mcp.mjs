// Real external MCP client. Test credentials stay in memory, never logged.
// WARDEN_MCP_SDK_ROOT points at an installed @modelcontextprotocol/sdk package.
// With WARDEN_TEST_SESSION/CSRF/ORGANIZATION, exercises discovery, registration,
// PKCE, consent and an actual loopback callback. Otherwise uses an access token.
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
import {createServer} from 'node:http';
import {randomUUID} from 'node:crypto';
const root=process.env.WARDEN_MCP_SDK_ROOT;
if(!root) throw new Error('WARDEN_MCP_SDK_ROOT is required');
const {Client}=await import(pathToFileURL(`${root}/dist/esm/client/index.js`));
const {StreamableHTTPClientTransport}=await import(pathToFileURL(`${root}/dist/esm/client/streamableHttp.js`));
const {UnauthorizedError}=await import(pathToFileURL(`${root}/dist/esm/client/auth.js`));
const endpoint=new URL(process.env.WARDEN_MCP_URL);
let provider, callbackServer, callbackCode;
if(process.env.WARDEN_TEST_SESSION){
 let clientInfo,tokens,verifier;
 const state=randomUUID();
 callbackServer=createServer((req,res)=>{const url=new URL(req.url,'http://localhost');assert.equal(url.searchParams.get('state'),state);callbackCode=url.searchParams.get('code');res.end('Connected');});
 await new Promise(resolve=>callbackServer.listen(0,'127.0.0.1',resolve));
 const callback=`http://127.0.0.1:${callbackServer.address().port}/callback`;
 provider={redirectUrl:callback,clientMetadata:{client_name:'External MCP verification',redirect_uris:[callback],grant_types:['authorization_code','refresh_token'],response_types:['code'],token_endpoint_auth_method:'none'},state:()=>state,clientInformation:()=>clientInfo,saveClientInformation:v=>{clientInfo=v},tokens:()=>tokens,saveTokens:v=>{tokens=v},saveCodeVerifier:v=>{verifier=v},codeVerifier:()=>verifier,
 async redirectToAuthorization(url){
  const start=await fetch(url,{redirect:'manual'});assert.equal(start.status,303);
  const consent=new URL(start.headers.get('location'),endpoint);
  const request=consent.searchParams.get('request');assert(request);
  const response=await fetch(new URL(`/auth/agent-authorization/${request}`,endpoint),{method:'POST',headers:{'Content-Type':'application/json',Origin:endpoint.origin,Cookie:`__Host-warden-session=${process.env.WARDEN_TEST_SESSION}`,'X-Warden-CSRF':process.env.WARDEN_TEST_CSRF},body:JSON.stringify({allow:true,organizationId:process.env.WARDEN_TEST_ORGANIZATION})});
  assert.equal(response.status,200,await response.clone().text());
  const {redirect}=await response.json();await fetch(redirect);assert(callbackCode);
 }};
}
let client,transport;
const open=async()=>{
 client=new Client({name:'warden-external-verification',version:'1.0.0'});
 transport=new StreamableHTTPClientTransport(endpoint,provider ? {authProvider:provider} : {requestInit:{headers:{Authorization:`Bearer ${process.env.WARDEN_MCP_ACCESS_TOKEN}`}}});
 await client.connect(transport);
};
try {
 try{await open();}catch(e){if(!(e instanceof UnauthorizedError) || !callbackCode)throw e;await transport.finishAuth(callbackCode);await open();}
 const {tools}=await client.listTools();assert.equal(tools.length,9);
 for(const name of ['panta_read_document','panta_create_document','panta_edit_document','panta_propose_document_edit','warden_share_conversation'])assert(tools.some(t=>t.name===name));
 const call=async(name,args={})=>{const v=await client.callTool({name,arguments:args});assert(!v.isError,JSON.stringify(v));return JSON.parse(v.content[0].text);};
 await call('panta_list_documents');await client.ping();
 if(process.env.WARDEN_MCP_FULL_TEST==='1'){
  const createArgs={operationId:randomUUID(),title:'External agent verification',content:'# Verification\n\nOriginal paragraph.'};
  const doc=await call('panta_create_document',createArgs);
  const retry=await call('panta_create_document',createArgs);assert.equal(retry.id,doc.id);
  let read=await call('panta_read_document',{documentId:doc.id});
  const paragraph=read.blocks.find(b=>b.text==='Original paragraph.');assert(paragraph);
  await call('panta_edit_document',{documentId:doc.id,operationId:randomUUID(),revision:read.revision,edits:[{from:paragraph.from,to:paragraph.to,text:'Edited by an external agent.'}]});
  const stale=await client.callTool({name:'panta_edit_document',arguments:{documentId:doc.id,operationId:randomUUID(),revision:read.revision,edits:[{from:paragraph.from,to:paragraph.to,text:'Stale edit'}]}});assert(stale.isError);
  read=await call('panta_read_document',{documentId:doc.id});assert(read.blocks.some(b=>b.text==='Edited by an external agent.'));
  await call('panta_add_comment',{documentId:doc.id,operationId:randomUUID(),revision:read.revision,text:'External comment verification'});
  const comments=await call('panta_list_comments',{documentId:doc.id});assert(JSON.stringify(comments).includes('External comment verification'));
  const proposal=await call('panta_propose_document_edit',{documentId:doc.id,operationId:randomUUID(),revision:read.revision,content:'# Verification\n\nSuggested by an external agent.',reason:'Verify human review'});
  const suggestions=await call('panta_list_suggestions',{documentId:doc.id});assert(JSON.stringify(suggestions).includes('Verify human review'));
  read=await call('panta_read_document',{documentId:doc.id});assert(read.blocks.some(b=>b.text==='Edited by an external agent.'));
  const upload={operationId:randomUUID(),title:'External conversation verification',messages:[{role:'user',content:'Please review our organization document.'},{role:'assistant',content:'I edited the document and left a **suggestion** for review.\n\n- Comments are supported.\n- This conversation is shared only with our organization.'}]};
  const shared=await call('warden_share_conversation',upload);assert.equal((await call('warden_share_conversation',upload)).id,shared.id);
  console.log(JSON.stringify({documentId:doc.id,conversationUrl:shared.url,suggestionPending:true}));
 }
 if(provider){
  const old=provider.tokens();provider.saveTokens({...old,access_token:'expired-test-token'});
  await call('panta_list_documents');assert.notEqual(provider.tokens().refresh_token,old.refresh_token);
  const revoked=await fetch(new URL('/oauth/revoke',endpoint),{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:new URLSearchParams({client_id:provider.clientInformation().client_id,token:provider.tokens().access_token})});assert.equal(revoked.status,200);
  const denied=await fetch(endpoint,{method:'POST',headers:{Authorization:`Bearer ${provider.tokens().access_token}`,'Content-Type':'application/json'},body:JSON.stringify({jsonrpc:'2.0',id:99,method:'ping'})});assert.equal(denied.status,401);
 }
 console.log('MCP SDK passed: discovery, callback/PKCE (when configured), initialize, nine tools, calls, refresh/revoke (when configured), ping.');
}finally{await client?.close();if(callbackServer)await new Promise(resolve=>callbackServer.close(resolve));}
