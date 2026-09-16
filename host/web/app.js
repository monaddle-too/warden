'use strict';
const $=id=>document.getElementById(id);
let linkedRequest=null;
function consumeSession(){const fragment=new URLSearchParams(location.hash.slice(1));if(fragment.has('request'))linkedRequest=fragment.get('request');if(fragment.has('session')){sessionStorage.setItem('warden-session',fragment.get('session'));history.replaceState(null,'','/');}}
consumeSession();
window.addEventListener('hashchange',()=>{consumeSession();message('');refresh();});
let state=null, selectedRequest=null, policyLoaded=false, connectionError=false, refreshGeneration=0;
function message(text){$('message').textContent=text;$('message').hidden=!text;}
async function api(path,value,signal){const headers={Authorization:'Bearer '+(sessionStorage.getItem('warden-session')||'')};const options={headers,cache:'no-store',signal};if(value!==undefined){options.method='POST';headers['Content-Type']='application/json';options.body=JSON.stringify(value);}const response=await fetch('/api/'+path,options);const result=await response.json();if(!response.ok){const error=Error(result.error||'Request failed');error.status=response.status;throw error;}return result;}
function el(tag,text,cls){const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(cls)e.className=cls;return e;}
function empty(target,title,detail){const box=el('div',undefined,'empty');box.append(el('strong',title),el('span',detail));target.append(box);}
function renderRequests(){const target=$('request-list');target.replaceChildren();const pending=state.requests.filter(r=>r.status==='pending');$('count').textContent=state.pending_count??pending.length;if(!pending.length){empty(target,'Nothing waiting for permission','GitHub, Figma, and Google Docs requests appear here before they can proceed.');return;}if((state.pending_count??pending.length)>pending.length)target.append(el('p','Showing the newest 25 pending requests. Use Request history → Awaiting approval to review older requests.'));for(const request of pending){const card=el('article',undefined,'card');const top=el('div',undefined,'request-top');top.append(el('div',request.summary.operation,'operation'),el('span','AWAITING APPROVAL','pill'));card.append(top,el('div',request.summary.method+' '+request.summary.host+request.summary.path,'path'),el('small',new Date(request.created*1000).toLocaleString()+' · '+request.id));const details=el('details');details.append(el('summary','Inspect request metadata and headers'),el('pre',JSON.stringify(request.summary,null,2)));card.append(details);const actions=el('div',undefined,'actions');const deny=el('button','Deny');deny.onclick=()=>act(()=>api('deny',{request_id:request.id}));const approve=el('button','Review & approve','primary');approve.onclick=()=>openApproval(request);actions.append(deny,approve);card.append(actions);target.append(card);}}
async function openApproval(request){
  selectedRequest=request;
  const operation=request.summary.operation, read=operation==='git/read', push=operation==='git/push', preview=push||operation==='pulls/create';
  $('approval-summary').textContent=read?'Read session for '+request.summary.repository+' — clone, fetch, and inspect remote refs. No push permission.':push?'Publish one branch in '+request.summary.repository:request.summary.method+' '+request.summary.path;
  $('kind').value=read?'scoped':'exact';$('kind').disabled=push;
  $('kind').options[1].textContent=read?'Read this repository repeatedly':'Same operation and URL, repeated';
  $('ttl').value=read?'3600':'300';$('predicates').value='{}';$('predicate-fields').hidden=true;
  $('git-review').hidden=!preview;$('git-review-text').textContent=preview?'Verifying review…':'';
  $('approve-submit').disabled=preview;$('approval').showModal();
  if(preview){
    try{
      const review=await api('request-review',{request_id:request.id});
      if(selectedRequest?.id!==request.id||!$('approval').open)return;
      $('git-review-text').textContent=review.pull_request?JSON.stringify(review.pull_request,null,2):[review.update.ref,review.update.old+' → '+review.update.new,'Review base: '+review.base,review.stat,review.commits,review.patch].join('\n\n');
      $('approve-submit').disabled=false;
    }catch(error){$('git-review-text').textContent='Review unavailable: '+error.message+'. Retry the push to regenerate it.';}
  }
}
$('approval').addEventListener('close',()=>{$('git-review-text').textContent='';selectedRequest=null;});
const historyStatuses={
  pending:['Awaiting approval','Waiting for a decision on the host.'],
  approved:['Approved','Permission was granted. This does not confirm that the request completed; permissions may since have expired or been revoked.'],
  executed:['Dispatched','Authorized for dispatch to the provider. This does not confirm a successful response.'],
  denied:['Denied','The approval request was denied on the host.'],
  stale:['Stale after restart','The controller restarted before approval. Retry the command to create a new request.']
};
const pageViews={history:{},traffic:{}};
for(const view of Object.values(pageViews))Object.assign(view,{data:null,cursor:null,previous:[],page:1,controller:null,generation:0});
function clearPage(name){
  const view=pageViews[name];view.controller?.abort();view.generation++;
  view.data=null;view.cursor=null;view.previous=[];view.page=1;
  $(name+'-list').replaceChildren();$(name+'-page').textContent='';
  $(name+'-summary').textContent='Reconnect to the host to load this page.';
  $(name+'-prev').disabled=true;$(name+'-next').disabled=true;
}
function renderHistory(rows){
  const target=$('history-list');target.replaceChildren();
  for(const request of rows){
    const [label,explanation]=historyStatuses[request.status]||[request.status,''];
    const card=el('article',undefined,'card'), top=el('div',undefined,'request-top');
    top.append(el('div',request.summary.operation||'GitHub request','operation'),el('span',label,'pill history-'+request.status));
    card.append(top,el('div',[request.summary.method,request.summary.host,request.summary.path].filter(Boolean).join(' '),'path'),
      el('small',new Date(request.created*1000).toLocaleString()+' · '+request.id),el('p',explanation));
    const details=el('details');
    details.append(el('summary','Inspect request metadata and headers'),el('pre',JSON.stringify(request.summary,null,2)));
    card.append(details);
    if(request.status==='pending'){
      const actions=el('div',undefined,'actions'),deny=el('button','Deny'),approve=el('button','Review & approve','primary');
      deny.onclick=()=>act(()=>api('deny',{request_id:request.id}));
      approve.onclick=()=>openApproval(request);
      actions.append(deny,approve);card.append(actions);
    }
    target.append(card);
  }
}
function renderTraffic(rows){
  const target=$('traffic-list');target.replaceChildren();
  for(const event of rows){
    const card=el('article',undefined,'card'),top=el('div',undefined,'request-top');
    top.append(el('div',event.event_type,'operation'));
    if(event.status!==undefined)top.append(el('span','HTTP '+event.status,'pill'));
    const req=event.request||{}, destination=event.destination||{};
    card.append(top,el('div',[req.method,event.hostname||req.host||destination.DST,req.path].filter(Boolean).join(' '),'path'),
      el('small',new Date(event.time).toLocaleString()+' · '+(event.request_id||event.event_id||'')));
    if(event.reason)card.append(el('p',event.reason));
    const details=el('details');details.append(el('summary','Inspect event metadata'),el('pre',JSON.stringify(event,null,2)));
    card.append(details);target.append(card);
  }
}
async function loadPage(name,direction='latest'){
  const view=pageViews[name];view.controller?.abort();
  const generation=++view.generation;view.controller=new AbortController();
  let cursor=view.cursor,previous=[...view.previous],page=view.page;
  if(direction==='latest'){cursor=null;previous=[];page=1;}
  else if(direction==='next'){
    if(!view.data?.next_cursor)return;
    previous.push(cursor);if(previous.length>20)previous.shift();
    cursor=view.data.next_cursor;page++;
  }else if(direction==='prev'){
    if(!previous.length)return;cursor=previous.pop();page--;
  }
  const params=new URLSearchParams({q:$(name+'-search').value.trim()});
  if(name==='history')params.set('status',$('history-status').value);
  else params.set('type',$('traffic-type').value);
  if(cursor)params.set('cursor',cursor);
  view.data=null;$(name+'-list').replaceChildren();
  $(name+'-summary').textContent='Loading page…';$(name+'-prev').disabled=true;$(name+'-next').disabled=true;
  try{
    const data=await api(name+'?'+params,undefined,view.controller.signal);
    if(generation!==view.generation)return;
    Object.assign(view,{data,cursor,previous,page});
    (name==='history'?renderHistory:renderTraffic)(data.items);
    const more=data.next_cursor?'Older records are available.':'End of history.';
    $(name+'-summary').textContent=`${data.items.length} records on this page. ${more}`+(name==='traffic'?` Scanned ${Math.ceil(data.scanned_bytes/1024)} KiB of the log.${data.skipped_records?' Oversized or malformed records were omitted.':''}`:'');
    if(!data.items.length)empty($(name+'-list'),'No matching records on this page',data.next_cursor?'Continue to older events, or change the filter.':'Try a different filter or refresh to latest.');
    $(name+'-page').textContent='Page '+page;
    $(name+'-prev').disabled=!previous.length;$(name+'-next').disabled=!data.next_cursor;
  }catch(error){
    if(generation!==view.generation||error.name==='AbortError')return;
    $(name+'-summary').textContent=error.status===401?'Session expired. Run ./warden open on the host.':error.message;
    empty($(name+'-list'),'Page unavailable',error.status===409?'The log changed. Choose Refresh to latest.':'Reconnect or choose Refresh to latest to retry.');
    $(name+'-page').textContent='';
  }
}
function renderGrants(){const target=$('grant-list');target.replaceChildren();const grants=state.grants.filter(g=>g.active);if(!grants.length){empty(target,'No active permissions','Permissions expire automatically and are revoked on control-plane restart.');return;}for(const grant of grants){const card=el('article',undefined,'card');card.append(el('div',grant.operation,'operation'),el('div',grant.path,'path'),el('p',(grant.kind==='exact'?'Exact request · single use':'Repeated requests · constrained scope')+' · '+Math.max(0,Math.ceil((grant.expires-state.time)/60))+' min remaining'));if(grant.kind==='scoped')card.append(el('pre',grant.predicates));const button=el('button','Revoke access');button.onclick=()=>act(()=>api('revoke',{grant_id:grant.id}));card.append(button);target.append(card);}}
function unavailable(error){
  renderResources(null);
  state=null;selectedRequest=null;connectionError=true;
  if($('approval').open)$('approval').close();
  const auth=error.status===401||error.status===403;
  $('connection').textContent=auth?'Dashboard sign-in required':'Host disconnected';
  $('count').textContent='—';$('proxy').textContent='Unknown';
  $('catalog').textContent='Unavailable until connected';
  $('token-state').textContent='Unavailable until connected';
  $('figma-state').textContent='Sign in to Warden to manage the Figma connection.';
  $('figma-connect').disabled=true;$('figma-disconnect').disabled=true;
  $('google_docs-state').textContent='Sign in to Warden to manage the Google Docs connection.';
  $('google_docs-connect').disabled=true;$('google_docs-disconnect').disabled=true;
  $('request-list').replaceChildren();$('grant-list').replaceChildren();
  const title=auth?'Sign in to view permission requests':'Permission requests unavailable';
  const detail=auth?'Run ./warden open on the host to reopen this dashboard with a current session. Your requests are still queued.':'Cannot reach the host control plane. This does not mean the approval queue is empty.';
  empty($('request-list'),title,detail);empty($('grant-list'),'Permissions unavailable',detail);clearPage('history');clearPage('traffic');
  message(auth?'Dashboard session missing or expired. Run ./warden open in the repository on the host.':error.message);
}
function renderResources(resources){
  for(const name of ['macos','proxy']){
    const value=resources?.[name];
    const fresh=value?.status==='ok'&&Number.isFinite(value.sampled_at)&&Date.now()/1000-value.sampled_at>=-2&&Date.now()/1000-value.sampled_at<=20;
    const bar=$(name+'-memory-bar');bar.hidden=!fresh;
    $(name+'-cpu').textContent=fresh?'CPU '+value.cpu_percent.toFixed(1)+'%':'CPU —';
    $(name+'-memory').textContent=fresh?'RAM '+(value.memory_used_bytes/1073741824).toFixed(1)+' / '+(value.memory_total_bytes/1073741824).toFixed(1)+' GiB':'RAM —';
    if(fresh)bar.value=value.memory_used_bytes/value.memory_total_bytes*100;
    $(name+'-resource-status').textContent=fresh?value.vcpu_count+' vCPUs · Sampled '+new Date(value.sampled_at*1000).toLocaleTimeString():value?.status==='sampling'?'Sampling…':'Unavailable · VM stopped or management unreachable';
  }
}
async function refresh(){
  const generation=++refreshGeneration;
  try{
    const next=await api('state');if(generation!==refreshGeneration)return;
    state=next;if(connectionError){message('');connectionError=false;}
    $('connection').textContent='● Host connected';$('proxy').textContent=state.proxy_ready?'Enforcing':'Offline';
    $('catalog').textContent=state.catalog_operations.toLocaleString()+' individually identified REST operations';
    const figma=state.figma||{};
    $('figma-state').textContent=figma.connected?'Connected to Figma account '+figma.account_id+'.':figma.app_configured?'App configured. Connect your Figma account.':'Configure a Figma OAuth app to connect.';
    $('figma-connect').disabled=!figma.app_configured;
    $('figma-disconnect').disabled=!figma.connected;
    const google_docs=state.google_docs||{};
    $('google_docs-state').textContent=google_docs.connected?'Google Docs connected. Each document request requires approval.':google_docs.app_configured?'App configured. Connect your Google Docs account.':'Configure a Google Docs OAuth app to connect.';
    $('google_docs-connect').disabled=!google_docs.app_configured;
    $('google_docs-disconnect').disabled=!google_docs.connected;
    $('token-form').hidden=!!state.github_app;
    $('token-state').textContent=state.github_app?'GitHub App connected for '+state.github_app.owner+'. Credentials are issued automatically after approval.':state.token_configured?'Token configured for this session.':'No GitHub token configured.';
    $('audit-path').textContent=state.audit_path;
    if(!policyLoaded){$('policy').value=JSON.stringify(state.policy,null,2);policyLoaded=true;}
    $('network-status').textContent=state.network_enabled===false?(state.network_applied===false?'Guest network disconnected.':'Disconnect requested; awaiting appliance acknowledgement.'):'Network enabled · '+(state.policy.egress?.mode||'public')+' destinations';
    $('network-toggle').textContent=state.network_enabled===false?'Reconnect network':'Disconnect network';
    renderRequests();renderGrants();renderResources(state.resources);
    if(linkedRequest){const id=linkedRequest;linkedRequest=null;try{const request=await api('request/'+encodeURIComponent(id));if(request.status==='pending')await openApproval(request);else message('This request is already '+request.status+'.');}catch(error){message(error.message);}}
  }catch(error){if(generation===refreshGeneration)unavailable(error);}
}
async function act(fn){try{await fn();message('');await refresh();if(!$('history').hidden)await loadPage('history','same');}catch(error){message(error.message);}}
document.querySelectorAll('[data-panel]').forEach(button=>button.onclick=()=>{document.querySelectorAll('.panel').forEach(panel=>panel.hidden=panel.id!==button.dataset.panel);for(const name of Object.keys(pageViews))if(name!==button.dataset.panel)clearPage(name);document.querySelectorAll('nav button').forEach(b=>b.classList.toggle('selected',b===button));$('heading').textContent=button.textContent.replace(/\s*\d+$/,'');if(pageViews[button.dataset.panel]&&!pageViews[button.dataset.panel].data)loadPage(button.dataset.panel);});
let historySearchTimer;
$('history-search').oninput=()=>{clearTimeout(historySearchTimer);historySearchTimer=setTimeout(()=>loadPage('history'),300);};
$('history-status').onchange=()=>loadPage('history');
$('traffic-filter').onsubmit=event=>{event.preventDefault();loadPage('traffic');};
$('traffic-type').onchange=()=>loadPage('traffic');
for(const name of ['history','traffic']){
  $(name+'-latest').onclick=()=>loadPage(name);
  $(name+'-prev').onclick=()=>loadPage(name,'prev');
  $(name+'-next').onclick=()=>loadPage(name,'next');
}
$('kind').onchange=()=>{$('predicate-fields').hidden=$('kind').value!=='scoped'||selectedRequest?.summary.operation==='git/read';};
$('cancel-approval').onclick=()=>$('approval').close();
$('approval-form').onsubmit=event=>{event.preventDefault();act(async()=>{await api('approve',{request_id:selectedRequest.id,kind:$('kind').value,ttl:Number($('ttl').value),predicates:$('kind').value==='scoped'?JSON.parse($('predicates').value):{}});$('approval').close();});};
$('token-form').onsubmit=event=>{event.preventDefault();const token=$('token').value;$('token').value='';act(()=>api('token',{token}));};
$('save-policy').onclick=()=>act(async()=>{await api('policy',JSON.parse($('policy').value));policyLoaded=false;});
refresh();setInterval(refresh,3000);

$('network-toggle').onclick=()=>act(()=>api('network',{enabled:state?.network_enabled===false}));
$('revoke-all').onclick=()=>act(()=>api('revoke-all',{}));
$('readiness-refresh').onclick=async()=>{try{const data=await api('readiness');const target=$('readiness-list');target.replaceChildren();for(const check of data.checks){const row=el('article',undefined,'card');row.append(el('strong',(check.ready?'Ready · ':'Next · ')+check.name));if(check.next)row.append(el('pre',check.next));target.append(row);}}catch(error){message(error.message);}};

$('figma-callback').textContent=location.origin+'/oauth/figma/callback';
$('figma-configure').onsubmit=event=>{event.preventDefault();const client_id=$('figma-client-id').value,client_secret=$('figma-client-secret').value;$('figma-client-secret').value='';act(()=>api('figma/configure',{client_id,client_secret}));};
$('figma-connect').onclick=()=>act(async()=>{const result=await api('figma/connect',{});const url=new URL(result.authorization_url);if(url.origin!=='https://www.figma.com'||url.pathname!=='/oauth')throw Error('Invalid authorization URL');location.assign(url.href);});
$('figma-disconnect').onclick=()=>act(()=>api('figma/disconnect',{}));
$('google_docs-callback').textContent=location.origin+'/oauth/google_docs/callback';
$('google_docs-configure').onsubmit=event=>{event.preventDefault();const client_id=$('google_docs-client-id').value,client_secret=$('google_docs-client-secret').value;$('google_docs-client-secret').value='';act(()=>api('google_docs/configure',{client_id,client_secret}));};
$('google_docs-connect').onclick=()=>act(async()=>{const result=await api('google_docs/connect',{});const url=new URL(result.authorization_url);if(url.origin!=='https://accounts.google.com'||url.pathname!=='/o/oauth2/v2/auth')throw Error('Invalid authorization URL');location.assign(url.href);});
$('google_docs-disconnect').onclick=()=>act(()=>api('google_docs/disconnect',{}));
