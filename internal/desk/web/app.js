'use strict';
let state, inspectRun=null, refreshBusy=false;
const $=id=>document.getElementById(id);
const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const bytes=n=>n<0?'unknown':n<1024?`${n} B`:n<1048576?`${(n/1024).toFixed(1)} KiB`:n<1073741824?`${(n/1048576).toFixed(1)} MiB`:`${(n/1073741824).toFixed(2)} GiB`;
const empty=(title,detail)=>`<div class="empty"><strong>${title}</strong>${detail}</div>`;
async function api(path,method='GET',body){const r=await fetch('/api/'+path,{method,headers:body?{'Content-Type':'application/json'}:{},body:body?JSON.stringify(body):undefined});const data=await r.json();if(!r.ok)throw Error(data.error||`HTTP ${r.status}`);return data;}
function showError(e){$('error').textContent=e.message;$('error').hidden=false;}
function button(label,action,id,kind='quiet'){return `<button class="${kind}" data-action="${action}" data-id="${esc(id)}">${label}</button>`;}
async function refresh(){if(refreshBusy)return;refreshBusy=true;try{
 state=await api('state');$('connection').textContent='Connected';
 $('settings').innerHTML=`<dt>Model directory</dt><dd>${esc(state.settings.models)}</dd><dt>llama-server</dt><dd>${esc(state.settings.binary)}</dd><dt>Parallel downloads</dt><dd>${state.settings.parallel_downloads}</dd>`;
 const active=state.jobs.filter(j=>['queued','downloading'].includes(j.status)).length;
 $('download-count').textContent=active?`${active} active`:'No active transfers';
 $('downloads').innerHTML=state.jobs.length?state.jobs.map(j=>`<div class="row"><div class="row-content"><div class="filename">${esc(j.file)}</div><small>${esc(j.status)} · ${bytes(j.bytes)}${j.total>0?' / '+bytes(j.total):''}${j.bytes_per_second?' · '+bytes(j.bytes_per_second)+'/s':''}</small>${['queued','downloading'].includes(j.status)?`<progress ${j.total>0?`max="${j.total}" value="${j.bytes}"`:''}></progress>`:''}${j.error?`<small class="error-detail">${esc(j.error)}</small>`:''}</div><div class="actions">${['queued','downloading'].includes(j.status)?button('Cancel','cancel',j.id):j.status!=='completed'?button('Resume','resume',j.id):'<span class="badge">Saved</span>'}</div></div>`).join(''):'';
 $('model-count').textContent=`${state.models.length} files · ${bytes(state.models.reduce((n,m)=>n+m.size,0))}`;
 $('models').innerHTML=state.models.length?state.models.map(m=>`<div class="row"><div class="row-content"><div class="filename">${esc(m.path.split('/').pop())}</div><small>${bytes(m.size)} · ${esc(m.path)}</small></div><div class="actions">${button('Inspect','inspect',m.path)}${button('Run →','run',m.path,'primary')}${button('Delete','delete',m.path)}</div></div>`).join(''):empty('No models yet','Download a GGUF above, or place files in your model directory.');
 $('run-count').textContent=`${state.runs.filter(r=>r.status==='running').length} running`;
 $('runs').innerHTML=state.runs.length?state.runs.map(r=>`<div class="row"><div class="row-content"><div class="filename">${esc(r.launch.model.split('/').pop())}</div><small>${esc(r.status)} · PID ${r.pid} · 127.0.0.1:${r.launch.port}</small>${r.error?`<small class="error-detail">${esc(r.error)}</small>`:''}</div><div class="actions">${button('Logs','logs',r.id)}${button('Health','health',r.id)}${['running','stopping'].includes(r.status)?button('Stop','stop',r.id,'danger'):button('Run again','again',r.id)}</div></div>`).join(''):empty('Nothing running','Choose a model and click Run.');
 if(inspectRun&&$('inspect-dialog').open){const data=await api(`runs/${inspectRun}/logs`);$('inspect-body').textContent=data.logs||'Waiting for output…';}
 }catch(e){$('connection').textContent='Disconnected';showError(e);}finally{refreshBusy=false;}}
function launchSpec(){const env={};for(const line of $('env').value.split('\n').filter(s=>s.trim())){const index=line.indexOf('=');if(index<1)throw Error('Use NAME=value for each environment variable.');env[line.slice(0,index).trim()]=line.slice(index+1);}return{model:$('model-path').value,context:Number($('context').value),gpu_layers:Number($('gpu').value),port:Number($('port').value),args:$('args').value.split('\n').filter(s=>s.trim()),env};}
function preview(){try{const s=launchSpec();const command=[state.settings.binary,'--model',state.settings.models+'/'+s.model,'--ctx-size',String(s.context),'--n-gpu-layers',String(s.gpu_layers),...s.args,'--host','127.0.0.1','--port',String(s.port)];$('preview').textContent=command.map(s=>/^[a-zA-Z0-9_./:=+-]+$/.test(s)?s:"'"+s.replaceAll("'","'\\''")+"'").join(' ');}catch(e){$('preview').textContent=e.message;}}
function openLaunch(model,spec){spec=spec||state.profiles[model]||{};$('model-path').value=model;$('launch-model').textContent=model;$('context').value=spec.context||8192;$('gpu').value=spec.gpu_layers??99;$('port').value=spec.port||8080;$('args').value=(spec.args||[]).join('\n');$('env').value=Object.entries(spec.env||{}).map(([k,v])=>`${k}=${v}`).join('\n');preview();$('launch-dialog').showModal();}
function inspect(title,body){inspectRun=null;$('inspect-title').textContent=title;$('inspect-body').textContent=body;$('inspect-dialog').showModal();}
$('download-form').addEventListener('submit',async e=>{e.preventDefault();const b=e.submitter;b.disabled=true;try{await api('downloads','POST',{urls:$('urls').value.split('\n').map(s=>s.trim()).filter(Boolean)});$('urls').value='';$('error').hidden=true;await refresh();}catch(e){showError(e);}finally{b.disabled=false;}});
$('launch-form').addEventListener('input',preview);
$('launch-form').addEventListener('submit',async e=>{e.preventDefault();e.submitter.disabled=true;try{await api('runs','POST',launchSpec());$('launch-dialog').close();$('error').hidden=true;await refresh();}catch(e){$('launch-dialog').close();showError(e);}finally{e.submitter.disabled=false;}});
$('close-dialog').onclick=()=>$('launch-dialog').close();$('close-inspect').onclick=()=>{inspectRun=null;$('inspect-dialog').close();};
document.addEventListener('click',async event=>{const b=event.target.closest('[data-action]');if(!b)return;const id=b.dataset.id;b.disabled=true;try{switch(b.dataset.action){
 case'run':openLaunch(id);break;
 case'again':{const r=state.runs.find(r=>r.id===id);openLaunch(r.launch.model,r.launch);break;}
 case'inspect':{const m=state.models.find(m=>m.path===id);inspect('Model file',JSON.stringify({absolute_path:state.settings.models+'/'+m.path,size_bytes:m.size,modified:m.modified,saved_launch:state.profiles[m.path]||null},null,2));break;}
 case'delete':if(confirm(`Permanently delete ${id}?`))await api('models?path='+encodeURIComponent(id),'DELETE');break;
 case'cancel':case'resume':await api(`downloads/${id}/${b.dataset.action}`,'POST',{});break;
 case'stop':await api(`runs/${id}/stop`,'POST',{});break;
 case'logs':{const data=await api(`runs/${id}/logs`);const r=state.runs.find(r=>r.id===id);inspect('Server logs',data.logs||'Waiting for output…');inspectRun=id;break;}
 case'health':{const data=await api(`runs/${id}/health`);const r=state.runs.find(r=>r.id===id);inspect('Server health',JSON.stringify({...data,command:r.command,environment:r.launch.env},null,2));break;}
 }await refresh();}catch(e){showError(e);}finally{b.disabled=false;}});
refresh();setInterval(refresh,2000);
