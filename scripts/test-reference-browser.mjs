// Local-only acceptance fixture: actual Panta persistence and collaboration,
// deterministic Warden conversations. Run with NODE_ENV=test and PANTA_ROOT.
import {createServer,request} from 'node:http';
import {readFileSync} from 'node:fs';
import {resolve} from 'node:path';
import {randomBytes,randomUUID} from 'node:crypto';
if(process.env.NODE_ENV!=='test' || !process.env.PANTA_ROOT || !process.env.PANTA_TEST_POSTGRES_URL) throw new Error('Test environment, private Panta checkout and test PostgreSQL required');
const panta=resolve(process.env.PANTA_ROOT),root=resolve('chat/web/dist');
const {openDatabase}=await import(panta+'/apps/docs/server/database.mjs');
const {start}=await import(panta+'/apps/docs/server/index.mjs');
const {WorkspaceDocuments}=await import(panta+'/apps/docs/server/workspace-documents.mjs');
const database=await openDatabase({url:process.env.PANTA_TEST_POSTGRES_URL,singleServer:false});
const org=randomBytes(16).toString('hex'),key=randomBytes(32).toString('hex');
const service=await start({database,policy:{wardenOrganizations:true},organizationMode:true,serviceKey:key,port:0});
const credentials={organization:org,subject:'fixture-user',email:'reader@example.com'};
const docs=[];for(const name of ['Annual planning','Reference acceptance']) docs.push(await service.documents.create(credentials,{kind:'document',name,operationId:randomUUID()}));
const workspace=new WorkspaceDocuments(service.documents);
const chat={id:'reference-chat',title:'Annual planning',sandboxID:'fixture',repository:'',status:'idle',archived:false,provider:'codex',approvals:[],conversation:{entries:[{id:'entry',role:'assistant',text:`See [Annual planning](http://127.0.0.1:5290/documents/${docs[0].id})`,detail:'',createdAt:1,isStreaming:false,delivery:''}]}};
const shared={id:'a'.repeat(32),title:'Annual planning',author:'Example Author',agent:'Example Agent',organizationName:'Reference testing',createdAt:new Date().toISOString(),messages:[{role:'user',content:'Shared conversation acceptance fixture.'}]};
const otherRefs=[{kind:'chat',id:chat.id,title:chat.title,url:'/?chat='+chat.id,subtitle:'Chat'},{kind:'shared_conversation',id:shared.id,title:shared.title,url:'/shared-conversations/'+shared.id,subtitle:'Shared conversation'}];
const json=(res,data,status=200)=>{res.writeHead(status,{'Content-Type':'application/json'});res.end(JSON.stringify(data));};
function headers(req){return {...req.headers,'x-panta-key':key,'x-panta-organization':org,'x-panta-subject':credentials.subject,'x-panta-email':credentials.email};}
const server=createServer(async(req,res)=>{
 const url=new URL(req.url,'http://localhost'),path=url.pathname;
 if(path==='/auth/session')return json(res,{enabled:true,mode:'email',authenticated:true,email:credentials.email,csrf:'fixture-csrf',organizationId:org,organizations:[{id:org,name:'Reference testing'}],fullAdmin:true,user:{sub:credentials.subject,email:credentials.email,name:'Example Reader',role:'admin'}});
 if(path==='/api/events'){res.writeHead(200,{'Content-Type':'text/event-stream'});res.write('data: '+JSON.stringify({version:1,chats:[chat]})+'\n\n');return;}
 if(path==='/api/state')return json(res,{version:1,chats:[chat]});
 if(path==='/api/references/search'){const q=url.searchParams.get('q')||'';const result=await workspace.references({query:q},org);return json(res,{items:[...result.items,...otherRefs.filter(r=>r.title.toLowerCase().includes(q.toLowerCase()))]});}
 if(path==='/api/references/resolve'){const chunks=[];for await(const chunk of req)chunks.push(chunk);const {references}=JSON.parse(Buffer.concat(chunks));const all=[...(await workspace.references({resolve:true,ids:references.filter(r=>r.kind==='document').map(r=>r.id)},org)).items,...otherRefs];return json(res,{items:all.filter(r=>references.some(k=>k.kind===r.kind&&k.id===r.id))});}
 if(path==='/auth/shared-conversations/'+shared.id)return json(res,shared);
 if(path==='/auth/shared-conversations')return json(res,{items:[shared],next:''});
 if(path==='/api/environments')return json(res,[]);
 if(path.startsWith('/api/chats/'))return json(res,{});
 if(path.startsWith('/api/')){const up=request({host:'127.0.0.1',port:service.port,path:req.url,method:req.method,headers:headers(req)},r=>{res.writeHead(r.statusCode,r.headers);r.pipe(res);});req.pipe(up);up.on('error',()=>{res.writeHead(502);res.end();});return;}
 let file;if(path.startsWith('/docs-app/'))file=panta+'/apps/docs/dist/index.html';else if(path.startsWith('/assets/')){try{readFileSync(root+path);file=root+path;}catch{file=panta+'/apps/docs/dist'+path;}}else file=root+'/index.html';
 try{res.setHeader('Content-Type',file.endsWith('.js')?'text/javascript':file.endsWith('.css')?'text/css':file.endsWith('.ttf')?'font/ttf':'text/html');res.end(readFileSync(file));}catch{res.writeHead(404);res.end();}
});
server.on('upgrade',(req,socket,head)=>{const up=request({host:'127.0.0.1',port:service.port,path:req.url,headers:headers(req)});up.on('upgrade',(r,peer,ph)=>{socket.write('HTTP/1.1 101 Switching Protocols\r\n'+Object.entries(r.headers).map(([k,v])=>`${k}: ${v}`).join('\r\n')+'\r\n\r\n');if(ph.length)socket.write(ph);if(head.length)peer.write(head);socket.pipe(peer);peer.pipe(socket);socket.on('error',()=>peer.destroy());peer.on('error',()=>socket.destroy());});up.on('error',()=>socket.destroy());up.end();});
server.listen(5290,'127.0.0.1',()=>console.log('Reference browser fixture ready at http://127.0.0.1:5290'));
let closing=false;async function stop(){if(closing)return;closing=true;server.close();server.closeAllConnections();await service.close();await database.close();const pg=await import(panta+'/apps/docs/node_modules/pg/lib/index.js');const pool=new pg.default.Pool({connectionString:process.env.PANTA_TEST_POSTGRES_URL});await pool.query(`DROP SCHEMA IF EXISTS "panta_org_${org}" CASCADE`);await pool.end();process.exit(0);}
process.on('SIGTERM',stop);process.on('SIGINT',stop);
