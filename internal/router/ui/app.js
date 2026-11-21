(function(){
  let status={models:[],telemetry:{},peers:{}};
  const $=id=>document.getElementById(id);
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  // Skips the write when the markup is unchanged, so a poll does not rebuild nodes (flicker, lost hover, lost selection).
  const setHTML=(el,html)=>{if(el._h!==html){el._h=html;el.innerHTML=html}};
  const icon=s=>{const paths={model:'<path d="M6 4.5 12 2l6 2.5v6L12 13l-6-2.5v-6Z"/><path d="m6 10.5 6 2.5 6-2.5M12 13v5"/>',peer:'<circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="m8.2 10.8 7.5-3.6M8.2 13.2l7.5 3.6"/>'};return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">'+(paths[s]||paths.model)+'</svg>'};
  const pageCopy={dashboard:['Fleet node','Overview','Live capacity and model activity for this router.'],playground:['Operator tools','Playground','Try a streaming chat, an image, an embeddings, a rerank, a speech, a transcription request or a load test against this router.'],activity:['Observability','Activity','Generation, audio, video and realtime requests on this node, with token or media stats and captured bodies.'],logs:['Observability','Logs','Live router, upstream and mesh output.']};
  document.querySelectorAll('.nav button').forEach(b=>b.onclick=()=>{const view=b.dataset.view;document.querySelectorAll('.nav button').forEach(x=>{x.classList.remove('active');x.removeAttribute('aria-current')});const wasLogs=$('logs').classList.contains('active');document.querySelectorAll('.view').forEach(x=>x.classList.remove('active'));b.classList.add('active');b.setAttribute('aria-current','page');$(view).classList.add('active');$('eyebrow').textContent=pageCopy[view][0];$('page-title').textContent=pageCopy[view][1];$('page-subtitle').textContent=pageCopy[view][2];if(view!=='logs')logStop();else if(!wasLogs)logStart()});
  async function json(url,opts){const r=await fetch(url,opts);const text=await r.text();let data;try{data=JSON.parse(text)}catch(e){data={error:text}}if(!r.ok)throw new Error(data.error||text||('HTTP '+r.status));return data}
  function setHealth(ok,message){$('health').textContent=message;$('side-health').textContent=ok?'Status updates every 3 seconds':'Unable to reach status endpoint';$('health-dot').className='status-dot '+(ok?'':'off');$('side-dot').className='status-dot '+(ok?'':'off')}
  window.refresh=async function(){try{status=await json('/_router/status');const node=status.node||'unknown';const now=new Date();$('side-node').textContent=node;$('updated').textContent='Updated '+now.toLocaleTimeString();setHealth(true,'Router healthy');renderSummary();renderGPUs();renderModels();renderPeers();fillSelectors()}catch(e){setHealth(false,e.message);$('updated').textContent='Snapshot unavailable'}};
  function renderSummary(){const gs=status.telemetry&&status.telemetry.gpus||[];const models=status.models||[];const running=models.filter(m=>m.state==='running').length;const peers=status.peers||{};$('stat-gpus').textContent=gs.length;$('stat-running').textContent=running;$('stat-models').textContent=models.length;$('stat-peers').textContent=Object.keys(peers).length;$('model-summary').textContent=models.length+' entries · '+running+' online'}
  function renderGPUs(){const gs=status.telemetry&&status.telemetry.gpus||[];const lm=status.telemetry&&status.telemetry.loaded_models||[];setHTML($('gpu-cards'),gs.map(g=>{const on=lm.filter(m=>(m.gpus&&m.gpus.length?m.gpus:[m.gpu]).includes(g.index)).map(m=>'<li><span class="mono">'+esc(m.instance||m.id)+'</span>'+(m.slots?'<span class="subtle">'+esc(m.in_flight)+' in flight, '+esc(m.slots)+' slots</span>':'')+'</li>');const total=Number(g.total_mb)||0,free=Number(g.free_mb)||0,stale=Number(g.free_if_stale_evicted_mb)||free,idle=Number(g.free_if_idle_evicted_mb)||stale;const used=total?Math.max(0,Math.min(100,((total-free)/total)*100)):0;return '<article class="panel gpu-card"><div class="gpu-title"><span class="gpu-name">'+esc(g.name||'GPU '+g.index)+'</span><span class="gpu-index">GPU '+esc(g.index)+'</span></div><div class="gpu-memory"><span class="gpu-free">'+esc(free)+'</span><span class="gpu-unit">MB free</span></div><div class="meter" role="progressbar" aria-label="GPU memory used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="'+Math.round(used)+'"><div class="meter-fill" style="width:'+used+'%"></div></div><div class="gpu-footer"><span>'+esc(total)+' MB total</span><span>'+esc(idle)+' MB if idle evicted</span></div><ul class="gpu-models" aria-label="Models on this GPU">'+(on.join('')||'<li class="subtle">No models</li>')+'</ul></article>'}).join('')||'<div class="panel empty">No GPU telemetry available yet.</div>')}
  function renderModels(){const rows=(status.models||[]).map(m=>{const origin=m.origin||'local';const freshness=m.freshness||'';const state=freshness||m.state||(origin==='remote'?'available':'stopped');const can=origin==='local';return '<tr><td><div class="model-cell"><span class="model-glyph">'+icon(origin==='remote'?'peer':'model')+'</span><div><span class="model-id">'+esc(m.id)+'</span>'+(m.name?'<span class="model-name">'+esc(m.name)+'</span>':'')+'</div></div></td><td><span class="state '+esc(origin)+'">'+esc(origin==='remote'?'remote':'local')+'</span></td><td><span class="state '+esc(state)+'">'+esc(state)+'</span></td><td class="mono">'+(m.vram_mb?esc(m.vram_mb)+' MB':'—')+'</td><td>'+(can?'<div class="actions"><a class="ghost" href="/upstream/'+encodeURIComponent(m.id)+'/" target="_blank" rel="noopener">Open UI</a><button data-act="load" data-id="'+esc(m.id)+'">Load</button><button class="danger" data-act="unload" data-id="'+esc(m.id)+'">Unload</button></div>':'<span class="subtle">Mesh routed</span>')+'</td></tr>'}).join('');setHTML($('models'),rows||'<tr><td colspan="5" class="empty">No models in the catalog.</td></tr>')}
  function renderPeers(){const ps=status.peers||{};const cards=Object.keys(ps).sort().map(n=>{const p=ps[n]||{};const gs=p.gpus||[];const loaded=(p.loaded_models||[]).map(m=>typeof m==='string'?m:((m.instance||m.id||'')+(m.gpu>=0?' @gpu'+(m.gpus&&m.gpus.length?m.gpus.join(','):m.gpu):'')+(m.freshness?' '+m.freshness:'')+(m.slots?(' '+m.in_flight+'/'+m.slots):'')));return '<article class="panel peer-card"><div class="peer-head"><div><div class="peer-name">'+esc(n)+'</div><div class="subtle mono">'+(p.timestamp?'Last seen '+new Date(p.timestamp*1000).toLocaleTimeString():'No timestamp')+'</div></div><span class="peer-state">'+(gs.length?'Fresh telemetry':'No telemetry')+'</span></div><div class="peer-detail"><div><div class="peer-label">GPU memory</div><div class="peer-value">'+esc(gs.map(g=>g.free_mb+' MB free'+(g.free_if_stale_evicted_mb&&g.free_if_stale_evicted_mb!==g.free_mb?' / '+g.free_if_stale_evicted_mb+' if stale evicted':'')).join(' · ')||'—')+'</div></div><div><div class="peer-label">Loaded models</div><div class="peer-value">'+esc(loaded.join(', ')||'None reported')+'</div></div></div></article>'}).join('');setHTML($('peers'),cards||'<div class="panel empty">No fresh peer telemetry.</div>')}
  function sameOptions(sel,ids){return sel.options.length===ids.length&&ids.every((id,i)=>sel.options[i].value===id)}
  function fillSelect(sel,ids,html){const prev=sel.value;if(sameOptions(sel,ids))return;sel.innerHTML=html;if([...sel.options].some(o=>o.value===prev))sel.value=prev}
  function fillSelectors(){const local=(status.models||[]).filter(m=>(m.origin||'local')==='local').map(m=>m.id);fillSelect($('log-model'),[''].concat(local),'<option value="">Model…</option>'+local.map(id=>'<option value="'+esc(id)+'">'+esc(id)+'</option>').join(''));const models=(status.models||[]).filter(m=>m.api_type!=='image');const ids=models.map(m=>m.id);fillSelect($('chat-model'),ids.length?ids:[''],ids.length?models.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No models available</option>');const embs=(status.models||[]).filter(m=>m.api_type==='embedding');fillSelect($('embed-model'),embs.length?embs.map(m=>m.id):[''],embs.length?embs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No embedding models available</option>');const rrs=(status.models||[]).filter(m=>m.api_type==='rerank');fillSelect($('rerank-model'),rrs.length?rrs.map(m=>m.id):[''],rrs.length?rrs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No rerank models available</option>');const sps=(status.models||[]).filter(m=>m.api_type==='speech');fillSelect($('speech-model'),sps.length?sps.map(m=>m.id):[''],sps.length?sps.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No speech models available</option>');const trs=(status.models||[]).filter(m=>m.api_type==='transcription');fillSelect($('transcribe-model'),trs.length?trs.map(m=>m.id):[''],trs.length?trs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No transcription models available</option>');fillSelect($('load-model'),ids.length?ids:[''],ids.length?models.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No models available</option>');const rts=(status.models||[]).filter(m=>m.api_type==='realtime');fillSelect($('rt-model'),rts.length?rts.map(m=>m.id):[''],rts.length?rts.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No realtime models available</option>');const imgs=(status.models||[]).filter(m=>m.api_type==='image');fillSelect($('image-model'),[''].concat(imgs.map(m=>m.id)),'<option value="">Path default</option>'+imgs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+'</option>').join(''))}
  window.loadModel=async function(id){try{await json('/_router/load',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model_id:id})});await refresh()}catch(e){alert('Load failed: '+e.message)}};
  window.unloadModel=async function(id){try{await json('/_router/unload',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model_id:id})});await refresh()}catch(e){alert('Unload failed: '+e.message)}};
  $('models').onclick=e=>{const b=e.target.closest('[data-act]');if(!b)return;if(b.dataset.act==='load')loadModel(b.dataset.id);else if(b.dataset.act==='unload')unloadModel(b.dataset.id)};

  const ALLOWED_HTML=new Set(['a','b','blockquote','br','code','del','details','div','em','h1','h2','h3','h4','h5','h6','hr','i','img','li','ol','p','pre','span','strong','sub','summary','sup','table','tbody','td','th','thead','tr','ul']);
  const VOID_HTML=new Set(['br','hr','img']);
  const THINK_BLOCK=/<\s*(think(?:ing)?)\s*>([\s\S]*?)<\s*\/\s*\1\s*>/gi;
  const THINK_OPEN=/<\s*(think(?:ing)?)\s*>/i;

  function slotToken(kind,n){return '\0'+kind+n+'\0'}
  function fillSlots(s,kind,slots){return s.replace(new RegExp('\\0'+kind+'(\\d+)\\0','g'),(_,n)=>slots[+n]??'')}
  function safeUrl(v){return /^(https?:|mailto:|\/|#|data:image\/)/i.test(v||'')}
  function sanitizeTag(raw){
    const m=String(raw).match(/^<\/?([a-zA-Z][a-zA-Z0-9]*)\b([^>]*)\/?>$/);
    if(!m)return '';
    const name=m[1].toLowerCase();
    if(!ALLOWED_HTML.has(name))return '';
    if(raw.startsWith('</'))return '</'+name+'>';
    const attrs=[];
    const attrRe=/([a-zA-Z:_][a-zA-Z0-9:._-]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))/g;
    let a;
    while((a=attrRe.exec(m[2]||''))){
      const key=a[1].toLowerCase();
      const val=a[3]??a[4]??a[5]??'';
      if(key.startsWith('on'))continue;
      if((key==='href'||key==='src')&&safeUrl(val))attrs.push(key+'="'+esc(val)+'"');
      else if(key==='alt'||key==='title'||key==='colspan'||key==='rowspan')attrs.push(key+'="'+esc(val)+'"');
    }
    return '<'+name+(attrs.length?' '+attrs.join(' '):'')+(VOID_HTML.has(name)?' /':'')+'>';
  }
  function pullFences(src,slots){
    return src.replace(/```([^\n`]*)\n?([\s\S]*?)(```|$)/g,(_,lang,code,close)=>{
      const cls=String(lang||'').trim();
      slots.push('<pre><code'+(cls?' class="language-'+esc(cls)+'"':'')+'>'+esc(code.replace(/\n$/,''))+'</code></pre>');
      return slotToken('b',slots.length-1);
    });
  }
  function pullHtml(src,slots){
    src=src.replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi,'').replace(/<style\b[^>]*>[\s\S]*?<\/style>/gi,'');
    return src.replace(/<\/?([a-zA-Z][a-zA-Z0-9]*)\b[^>]*>/g,(full,name)=>{
      if(!ALLOWED_HTML.has(name.toLowerCase()))return '';
      const clean=sanitizeTag(full);
      if(!clean)return '';
      slots.push(clean);
      return slotToken('b',slots.length-1);
    });
  }
  function inlineMarkdown(s){
    const slots=[];
    s=s.replace(/`([^`]+)`/g,(_,code)=>{slots.push('<code>'+esc(code)+'</code>');return slotToken('c',slots.length-1)});
    s=esc(s);
    s=s.replace(/\*\*([\s\S]+?)\*\*/g,'<strong>$1</strong>').replace(/__([\s\S]+?)__/g,'<strong>$1</strong>');
    s=s.replace(/~~([\s\S]+?)~~/g,'<del>$1</del>');
    s=s.replace(/(^|[^\*])\*([^*\n]+)\*(?!\*)/g,'$1<em>$2</em>').replace(/(^|[^_])_([^_\n]+)_(?!_)/g,'$1<em>$2</em>');
    s=s.replace(/!\[([^\]]*)\]\((https?:\/\/[^)\s]+)\)/g,'<img alt="$1" src="$2">');
    s=s.replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g,'<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    return fillSlots(s,'c',slots);
  }
  function isFence(line){return /^```/.test(line)}
  function isHeading(line){return /^#{1,6} /.test(line)}
  function isRule(line){return /^(---|\*\*\*|___)$/.test(line.trim())}
  function isQuote(line){return /^>\s?/.test(line)}
  function isUl(line){return /^\s*[-*+] /.test(line)}
  function isOl(line){return /^\s*\d+[.)] /.test(line)}
  function isTableSep(line){return /^\s*\|?(\s*:?-+:?\s*\|)+\s*:?-+:?\s*\|?\s*$/.test(line)}
  function isTableRow(line){return /^\s*\|/.test(line)}
  function splitCells(line){return line.replace(/^\s*\||\|\s*$/g,'').split('|').map(c=>c.trim())}
  function renderMarkdown(raw,nested){
    const slots=[];
    let src=String(raw??'').replace(/\r\n?/g,'\n');
    if(!nested)src=pullHtml(pullFences(src,slots),slots);
    const lines=src.split('\n');
    const out=[];
    let i=0;
    while(i<lines.length){
      const line=lines[i];
      if(!line.trim()){i++;continue}
      if(/^\0b\d+\0$/.test(line)){out.push(line);i++;continue}
      if(isHeading(line)){
        const d=line.match(/^(#{1,6}) /)[1].length;
        out.push('<h'+d+'>'+inlineMarkdown(line.slice(d+1))+'</h'+d+'>');
        i++;continue;
      }
      if(isRule(line)){out.push('<hr>');i++;continue}
      if(isQuote(line)){
        const buf=[];
        while(i<lines.length&&isQuote(lines[i]))buf.push(lines[i++].replace(/^>\s?/,''));
        out.push('<blockquote>'+renderMarkdown(buf.join('\n'),true)+'</blockquote>');
        continue;
      }
      if(isTableRow(line)&&i+1<lines.length&&isTableSep(lines[i+1])){
        const heads=splitCells(line);
        i+=2;
        const rows=[];
        while(i<lines.length&&isTableRow(lines[i]))rows.push(splitCells(lines[i++]));
        out.push('<table><thead><tr>'+heads.map(h=>'<th>'+inlineMarkdown(h)+'</th>').join('')+'</tr></thead><tbody>'+rows.map(r=>'<tr>'+r.map(c=>'<td>'+inlineMarkdown(c)+'</td>').join('')+'</tr>').join('')+'</tbody></table>');
        continue;
      }
      if(isUl(line)||isOl(line)){
        const ordered=isOl(line);
        const re=ordered?/^\s*\d+[.)] /:/^\s*[-*+] /;
        const tag=ordered?'ol':'ul';
        const items=[];
        while(i<lines.length&&re.test(lines[i])){
          let item=lines[i++].replace(re,'');
          while(i<lines.length&&/^\s{2,}\S/.test(lines[i])&&!re.test(lines[i])&&!isHeading(lines[i])&&!isFence(lines[i]))item+=' '+lines[i++].trim();
          items.push(item);
        }
        out.push('<'+tag+'>'+items.map(x=>'<li>'+inlineMarkdown(x)+'</li>').join('')+'</'+tag+'>');
        continue;
      }
      const buf=[];
      while(i<lines.length&&lines[i].trim()&&!isHeading(lines[i])&&!isRule(lines[i])&&!isQuote(lines[i])&&!isUl(lines[i])&&!isOl(lines[i])&&!isFence(lines[i])&&!(isTableRow(lines[i])&&i+1<lines.length&&isTableSep(lines[i+1])))buf.push(lines[i++]);
      out.push('<p>'+buf.map(inlineMarkdown).join('<br>')+'</p>');
    }
    return nested?out.join(''):fillSlots(out.join(''),'b',slots);
  }
  function splitThink(text){
    const parts=[];
    const re=new RegExp(THINK_BLOCK.source,'gi');
    let last=0,m;
    while((m=re.exec(text))){
      if(m.index>last)parts.push({kind:'md',text:text.slice(last,m.index)});
      parts.push({kind:'think',text:m[2],pending:false});
      last=re.lastIndex;
    }
    const rest=text.slice(last);
    const open=rest.search(THINK_OPEN);
    if(open!==-1){
      if(open>0)parts.push({kind:'md',text:rest.slice(0,open)});
      parts.push({kind:'think',text:rest.slice(open).replace(THINK_OPEN,''),pending:true});
    }else if(rest)parts.push({kind:'md',text:rest});
    return parts;
  }
  function thinkBlock(text,pending){
    if(!String(text||'').trim()&&!pending)return '';
    return '<details class="chat-think" open><summary>'+(pending?'Thinking':'Thought')+'</summary><div class="chat-md">'+renderMarkdown(text)+'</div></details>';
  }
  function paintChat(el,reasoning,content,streaming){
    const chunks=[];
    if(reasoning)chunks.push({kind:'think',text:reasoning,pending:streaming&&!content});
    splitThink(content).forEach(p=>chunks.push(p));
    const html=chunks.map(p=>p.kind==='think'?thinkBlock(p.text,p.pending):('<div class="chat-md">'+renderMarkdown(p.text)+'</div>')).join('');
    if(!html){
      el.innerHTML=streaming?'':'Request completed with no text output.';
      if(!streaming)el.classList.add('muted');
    }else{
      el.classList.remove('muted');
      el.innerHTML=html;
    }
    if(streaming)el.scrollIntoView({block:'end',inline:'nearest'});
  }

  // Playground tabs only toggle visibility, so a stream or result survives a tab switch.
  const pgTabs=$('playground').querySelector('[role=tablist]');
  pgTabs.addEventListener('click',ev=>{
    const t=ev.target.closest('[data-tab]');
    if(!t)return;
    pgTabs.querySelectorAll('[data-tab]').forEach(x=>{
      const on=x===t;
      x.classList.toggle('active',on);
      x.setAttribute('aria-selected',on);
      x.tabIndex=on?0:-1;
      $(x.getAttribute('aria-controls')).hidden=!on;
    });
  });
  pgTabs.addEventListener('keydown',ev=>{
    if(ev.key!=='ArrowLeft'&&ev.key!=='ArrowRight')return;
    const tabs=[...pgTabs.querySelectorAll('[data-tab]')];
    const next=tabs[(tabs.indexOf(document.activeElement)+(ev.key==='ArrowRight'?1:tabs.length-1))%tabs.length];
    next.focus();next.click();
  });

  window.sendChat=async function(){
    const out=$('chat-output');
    out.className='output chat-output';
    out.innerHTML='';
    const body={model:$('chat-model').value,messages:[{role:'user',content:$('chat-prompt').value}],max_tokens:Number($('chat-tokens').value)||256,temperature:Number($('chat-temp').value)||0.7,stream:true};
    let reasoning='',content='',raf=0;
    const schedule=()=>{if(raf)return;raf=requestAnimationFrame(()=>{raf=0;paintChat(out,reasoning,content,true)})};
    try{
      const r=await fetch('/v1/chat/completions',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      if(!r.ok)throw new Error(await r.text());
      const reader=r.body.getReader(),dec=new TextDecoder();
      let buf='';
      window.scrollTo(0,out.getBoundingClientRect().top+window.scrollY-24);
      while(true){
        const x=await reader.read();
        if(x.done)break;
        buf+=dec.decode(x.value,{stream:true});
        const lines=buf.split('\n');
        buf=lines.pop();
        for(const line of lines){
          if(!line.startsWith('data:'))continue;
          const raw=line.slice(5).trim();
          if(raw==='[DONE]')continue;
          try{
            const d=JSON.parse(raw),c=d.choices&&d.choices[0]&&d.choices[0].delta||{};
            if(c.reasoning_content)reasoning+=c.reasoning_content;
            if(c.content)content+=c.content;
            schedule();
          }catch(e){}
        }
      }
      if(raf){cancelAnimationFrame(raf);raf=0}
      paintChat(out,reasoning,content,false);
    }catch(e){
      if(raf)cancelAnimationFrame(raf);
      out.className='output chat-output error';
      out.textContent=e.message;
    }
  };
  // shortcut: fixed 3 minute client timeout and no progress text, make it a field if slow image models need longer.
  let imageCtl;
  window.cancelImage=()=>{if(imageCtl)imageCtl.abort()};
  window.generateImage=async function(){
    if(imageCtl)return;
    const out=$('image-output'),go=$('image-generate'),stop=$('image-cancel');
    out.className='image-result';out.textContent='Generating…';
    const body={prompt:$('image-prompt').value,negative_prompt:$('image-negative').value,width:Number($('image-width').value),height:Number($('image-height').value),steps:Number($('image-steps').value),seed:Number($('image-seed').value)};
    if($('image-model').value)body.model=$('image-model').value;
    const ctl=imageCtl=new AbortController();
    let timedOut=false;
    const timer=setTimeout(()=>{timedOut=true;ctl.abort()},180000);
    go.disabled=true;stop.disabled=false;
    try{
      const r=await fetch('/sdapi/v1/txt2img',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body),signal:ctl.signal});
      const text=await r.text();
      if(!r.ok){const wait=r.headers.get('Retry-After');throw new Error('HTTP '+r.status+': '+(text.trim()||r.statusText)+(wait?' (retry in '+wait+' s)':''))}
      let d;try{d=JSON.parse(text)}catch(e){throw new Error('The response was not JSON.')}
      const imgs=d.images||[];
      out.innerHTML=imgs.length?imgs.map(x=>'<img alt="Generated image" src="data:image/png;base64,'+x+'">').join(''):'<span class="muted">No images returned.</span>'
    }catch(e){
      out.className='image-result error';
      out.textContent=ctl.signal.aborted?(timedOut?'Timed out after 3 minutes with no answer.':'Cancelled.'):e.message
    }finally{clearTimeout(timer);imageCtl=null;go.disabled=false;stop.disabled=true}
  };

  // shortcut: shows the first 8 values only and no copy button, add a full-vector download if operators ask.
  window.sendEmbedding=async function(){
    const out=$('embed-output'),model=$('embed-model').value;
    const fail=m=>{out.className='output error';out.textContent=m};
    if(!model)return fail('No embedding model is available. Add a catalog entry with api_type: embedding.');
    out.className='output muted';out.textContent='Embedding…';
    try{
      const r=await fetch('/v1/embeddings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model,input:$('embed-input').value})});
      const text=await r.text();
      let d;try{d=JSON.parse(text)}catch(e){d=null}
      if(!r.ok){const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      const v=d&&d.data&&d.data[0]&&d.data[0].embedding;
      if(!Array.isArray(v))throw new Error('The response had no embedding vector.');
      const head=v.slice(0,8).map(x=>Number(x).toPrecision(5)).join(', ');
      out.className='output';
      out.innerHTML='<div><strong>Dimension:</strong> <span class="mono">'+v.length+'</span></div><div><strong>Model:</strong> <span class="mono">'+esc(d.model||model)+'</span></div><div><strong>Preview:</strong> <span class="mono">['+esc(head+(v.length>8?', … ('+(v.length-8)+' more)':''))+']</span></div>';
    }catch(e){fail(e.message)}
  };

  // shortcut: no top_n control and the document text comes from the input lines, add both if backends return trimmed or reordered documents.
  window.sendRerank=async function(){
    const out=$('rerank-output'),model=$('rerank-model').value;
    const fail=m=>{out.className='output error';out.textContent=m};
    const docs=$('rerank-docs').value.split('\n').map(x=>x.trim()).filter(Boolean);
    if(!model)return fail('No rerank model is available. Add a catalog entry with api_type: rerank.');
    if(!$('rerank-query').value.trim())return fail('Enter a query.');
    if(!docs.length)return fail('Enter at least one document, one per line.');
    out.className='output muted';out.textContent='Reranking…';
    try{
      const r=await fetch('/v1/rerank',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model,query:$('rerank-query').value,documents:docs})});
      const text=await r.text();
      let d;try{d=JSON.parse(text)}catch(e){d=null}
      if(!r.ok){const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      const rs=d&&(d.results||d.data);
      if(!Array.isArray(rs))throw new Error('The response had no rerank results.');
      const score=x=>Number(x.relevance_score??x.score)||0;
      const rows=[...rs].sort((a,b)=>score(b)-score(a)).map(x=>'<li><span class="mono">'+score(x).toFixed(4)+'</span> '+esc(docs[x.index]??'(document '+x.index+')')+'</li>').join('');
      out.className='output';
      out.innerHTML='<div><strong>Model:</strong> <span class="mono">'+esc(d.model||model)+'</span></div><ol>'+rows+'</ol>';
    }catch(e){fail(e.message)}
  };

  // shortcut: speech uses the backend's default audio format and the voice is free text, add a format picker and a voice list if backends need them.
  let speechURL;
  window.sendSpeech=async function(){
    const out=$('speech-output'),model=$('speech-model').value,input=$('speech-input').value.trim(),voice=$('speech-voice').value.trim();
    const fail=m=>{out.className='output error';out.textContent=m};
    if(!model)return fail('No speech model is available. Add a catalog entry with api_type: speech.');
    if(!input)return fail('Enter some text to speak.');
    out.className='output muted';out.textContent='Generating audio…';
    try{
      const body={model,input};if(voice)body.voice=voice;
      const r=await fetch('/v1/audio/speech',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      if(!r.ok){const text=await r.text();let d;try{d=JSON.parse(text)}catch(e){d=null}const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      const blob=await r.blob();
      if(!blob.size)throw new Error('The response had no audio.');
      if(speechURL)URL.revokeObjectURL(speechURL);
      speechURL=URL.createObjectURL(blob);
      const ext=(blob.type.split('/')[1]||'mpeg').split(';')[0].replace('mpeg','mp3').replace('x-wav','wav');
      out.className='output';
      out.innerHTML='<audio controls src="'+esc(speechURL)+'" style="width:100%"></audio><p><a href="'+esc(speechURL)+'" download="speech.'+esc(ext)+'">Download audio</a></p>';
    }catch(e){fail(e.message)}
  };

  window.sendTranscription=async function(){
    const out=$('transcribe-output'),model=$('transcribe-model').value,file=$('transcribe-file').files[0];
    const fail=m=>{out.className='output error';out.textContent=m};
    if(!model)return fail('No transcription model is available. Add a catalog entry with api_type: transcription.');
    if(!file)return fail('Choose an audio file.');
    out.className='output muted';out.textContent='Transcribing…';
    try{
      const fd=new FormData();fd.append('model',model);fd.append('file',file);
      const r=await fetch('/v1/audio/transcriptions',{method:'POST',body:fd});
      const text=await r.text();
      let d;try{d=JSON.parse(text)}catch(e){d=null}
      if(!r.ok){const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      out.className='output';
      out.textContent=d&&typeof d.text==='string'?d.text:text;
    }catch(e){fail(e.message)}
  };

  // Load test: the browser drives /v1/chat/completions like any other client, so nothing here touches backends directly.
  // State is module-level, so a run keeps going while the operator is on another tab.
  // shortcut: fixed request count, latency is whole-response time (non-streaming), add a duration mode and TTFT if operators ask, p95 needs 20+ samples.
  let load=null;
  const pct=(sorted,p)=>sorted[Math.max(0,Math.ceil(p*sorted.length)-1)];
  function paintLoad(final){
    const s=load,n=s.lat.length,secs=((final?s.end:performance.now())-s.t0)/1000,sorted=s.lat.slice().sort((a,b)=>a-b);
    const rows=[['Completed',n+' / '+s.total],['Failed',s.failed],['Average latency',n?fmtDur(Math.round(s.lat.reduce((a,b)=>a+b,0)/n)):'—']];
    if(final){rows.push(['Throughput',n&&secs>0?(n/secs).toFixed(2)+' req/s':'—'],['p95 latency',n>=20?fmtDur(Math.round(pct(sorted,0.95))):'Needs 20 or more completed requests'],['Elapsed',fmtDur(Math.round(secs*1000))])}
    $('load-output').innerHTML=rows.map(r=>'<div><strong>'+esc(r[0])+':</strong> '+esc(r[1])+'</div>').join('')
  }
  window.startLoad=async function(){
    if(load)return;
    const out=$('load-output'),status=$('load-status'),model=$('load-model').value,prompt=$('load-prompt').value.trim();
    const num=(id,max)=>Math.min(max,Math.max(1,Math.floor(Number($(id).value))||1));
    const conc=num('load-conc',64),total=num('load-count',1000),maxTokens=num('load-tokens',4096);
    if(!model){out.className='output error';out.textContent='No model is available.';return}
    if(!prompt){out.className='output error';out.textContent='Enter a prompt.';return}
    const ctl=new AbortController();
    load={total,failed:0,lat:[],t0:performance.now(),end:0};
    const run=load;
    $('load-start').disabled=true;$('load-cancel').disabled=false;out.className='output';status.textContent='Running…';
    const body=JSON.stringify({model,messages:[{role:'user',content:prompt}],max_tokens:maxTokens,stream:false});
    let next=0;
    const worker=async()=>{
      while(next<total&&!ctl.signal.aborted){
        next++;const t=performance.now();
        try{
          const r=await fetch('/v1/chat/completions',{method:'POST',headers:{'Content-Type':'application/json'},body,signal:ctl.signal});
          await r.arrayBuffer();
          if(!r.ok)throw new Error('HTTP '+r.status);
          run.lat.push(performance.now()-t)
        }catch(e){if(ctl.signal.aborted)return;run.failed++}
        paintLoad(false)
      }
    };
    window.cancelLoad=()=>ctl.abort();
    await Promise.all(Array.from({length:conc},worker));
    run.end=performance.now();paintLoad(true);
    status.textContent=ctl.signal.aborted?'Cancelled.':'Finished.';
    $('load-start').disabled=false;$('load-cancel').disabled=true;load=null
  };

  // shortcut: text only, the model goes in the URL query (not a first session.update), no audio capture; add audio when a realtime backend is configured for it.
  let rt=null;
  const rtOut=$('rt-output');
  function rtLine(text,cls){const d=document.createElement('div');if(cls)d.className=cls;d.textContent=text;rtOut.appendChild(d);rtOut.scrollTop=rtOut.scrollHeight;return d}
  function rtButtons(live,ready){$('rt-open').disabled=live;$('rt-model').disabled=live;$('rt-close').disabled=!live;$('rt-send').disabled=!ready}
  window.openRealtime=function(){
    if(rt)return;
    const model=$('rt-model').value;
    rtOut.className='output';rtOut.textContent='';
    if(!model){rtOut.className='output error';rtOut.textContent='No realtime model is available. Add a catalog entry with api_type: realtime.';return}
    // A browser sends its cached router key (UI sign-in) on the handshake, same as for fetch.
    const ws=new WebSocket((location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/v1/realtime?model='+encodeURIComponent(model));
    let opened=false,reply=null;
    rt=ws;rtButtons(true,false);$('rt-status').textContent='Connecting…';
    ws.onopen=()=>{opened=true;rtButtons(true,true);$('rt-status').textContent='Open';rtLine('Session open.','muted')};
    ws.onmessage=m=>{
      let ev;try{ev=JSON.parse(m.data)}catch(e){rtLine(String(m.data).slice(0,500),'muted');return}
      const t=String(ev.type||'');
      if(/^response\.(output_)?(text|audio_transcript)\.delta$/.test(t)){
        if(!reply)reply=rtLine('Model: ');
        reply.textContent+=ev.delta||'';rtOut.scrollTop=rtOut.scrollHeight;
      }else if(t==='error'){rtLine('Error: '+((ev.error&&ev.error.message)||JSON.stringify(ev.error||ev)),'error')}
      else{
        if(t==='response.done'){reply=null;const r=ev.response||{};if(r.status==='failed')rtLine('Response failed: '+JSON.stringify(r.status_details||{}),'error')}
        rtLine(t||'event','muted');
      }
    };
    ws.onclose=ev=>{
      if(!opened)rtLine('Could not open a session. Check that the model is available and, from a public address, that you have signed in with a router key.','error');
      else if(ev.code===1000)rtLine('Session closed.','muted');
      else rtLine('Disconnected (code '+ev.code+(ev.reason?': '+ev.reason:'')+').','error');
      rt=null;rtButtons(false,false);$('rt-status').textContent='Closed';
    };
  };
  window.closeRealtime=function(){if(rt)rt.close(1000)};
  window.sendRealtime=function(){
    const text=$('rt-input').value.trim();
    if(!rt||rt.readyState!==WebSocket.OPEN||!text)return;
    rt.send(JSON.stringify({type:'conversation.item.create',item:{type:'message',role:'user',content:[{type:'input_text',text}]}}));
    rt.send(JSON.stringify({type:'response.create',response:{modalities:['text']}}));
    rtLine('You: '+text);
  };

  function fmtDur(ms){ms=Number(ms)||0;if(ms<1000)return ms+' ms';if(ms<60000)return (ms/1000).toFixed(2)+' s';return Math.floor(ms/60000)+'m '+((ms/1000)%60).toFixed(0)+'s'}
  function fmtSpeed(v){v=Number(v);return v>0?v.toFixed(v>=100?0:1)+' t/s':'—'}
  function fmtTok(v){v=Number(v)||0;return v>0?v.toLocaleString():'—'}
  function fmtBytes(v){v=Number(v)||0;if(!v)return '—';if(v<1024)return v+' B';if(v<1048576)return (v/1024).toFixed(1)+' KB';if(v<1073741824)return (v/1048576).toFixed(1)+' MB';return (v/1073741824).toFixed(1)+' GB'}
  function fmtTime(ts){const d=new Date(ts);return isNaN(d)?String(ts||''):d.toLocaleTimeString()}
  function prettyBody(raw){
    const s=String(raw??'');
    if(!s)return '(empty)';
    if(s.startsWith('data:'))return s;
    try{return JSON.stringify(JSON.parse(s),null,2)}catch(e){return s}
  }
  function prettyHeaders(h){
    const lines=[];
    Object.keys(h||{}).sort().forEach(k=>(h[k]||[]).forEach(v=>lines.push(k+': '+v)));
    return lines.join('\n');
  }
  // The svg children are built once and then only have attributes changed, so a poll morphs the line
  // (CSS transition on the path's d) instead of replacing it. A change in sample count snaps, because
  // paths with different segment counts cannot interpolate; browsers without CSS d just snap too.
  function spark(el, values, color){
    const w=320,h=96,pad=8;
    let s=el._s;
    if(!s){
      el.innerHTML='<text class="spark-none" x="160" y="52" text-anchor="middle" fill="#5d6c7f" font-size="11">No samples yet</text><path class="spark-line" fill="none" stroke-width="2" stroke-linejoin="round"/><circle class="spark-dot" r="3"/><text class="spark-last" x="'+(w-pad)+'" y="14" text-anchor="end" font-size="11"></text>';
      const c=el.children;
      s=el._s={none:c[0],line:c[1],dot:c[2],last:c[3],n:-1};
    }
    const has=values.length>0;
    s.none.style.display=has?'none':'';
    [s.line,s.dot,s.last].forEach(n=>{n.style.display=has?'':'none'});
    if(!has){s.n=-1;return}
    const min=Math.min(...values), max=Math.max(...values);
    const span=max-min||1;
    const pts=values.map((v,i)=>[pad+(i/Math.max(values.length-1,1))*(w-pad*2),h-pad-((v-min)/span)*(h-pad*2)]);
    const d='M'+pts.map(p=>p[0].toFixed(1)+' '+p[1].toFixed(1)).join(' L');
    const tip=pts[pts.length-1],last=values[values.length-1];
    s.line.setAttribute('stroke',color);s.dot.setAttribute('fill',color);s.last.setAttribute('fill',color);
    const snap=s.n!==values.length;
    if(snap)[s.line,s.dot].forEach(n=>{n.style.transition='none'});
    s.line.setAttribute('d',d);s.line.style.d='path("'+d+'")';
    s.dot.style.cx=tip[0].toFixed(1)+'px';s.dot.style.cy=tip[1].toFixed(1)+'px';
    s.dot.setAttribute('cx',tip[0].toFixed(1));s.dot.setAttribute('cy',tip[1].toFixed(1));
    s.last.textContent=last.toFixed(last>=100?0:1);
    if(snap){s.line.getBoundingClientRect();[s.line,s.dot].forEach(n=>{n.style.transition=''})}
    s.n=values.length;
  }
  async function activity(){
    try{
      const d=await json('/_router/activity');
      const rows=d.entries||[];
      $('act-count').textContent=rows.length;
      const gens=rows.map(e=>Number(e.tokens&&e.tokens.tokens_per_second)||0).filter(v=>v>0);
      const durs=rows.map(e=>Number(e.duration_ms)||0).filter(v=>v>0);
      const errs=rows.filter(e=>e.status>=400).length;
      $('act-avg-gen').innerHTML=gens.length?(gens.reduce((a,b)=>a+b,0)/gens.length).toFixed(1)+'<small> tok/s</small>':'—<small> tok/s</small>';
      $('act-avg-dur').textContent=durs.length?fmtDur(durs.reduce((a,b)=>a+b,0)/durs.length):'—';
      $('act-errors').textContent=errs;
      const chrono=rows.slice().reverse();
      spark($('chart-gen'), chrono.map(e=>Number(e.tokens&&e.tokens.tokens_per_second)||0).filter(v=>v>0), '#63dda1');
      spark($('chart-prompt'), chrono.map(e=>Number(e.tokens&&e.tokens.prompt_per_second)||0).filter(v=>v>0), '#58d6e6');
      spark($('chart-dur'), chrono.map(e=>Number(e.duration_ms)||0), '#8db8ff');
      spark($('chart-ttft'), chrono.map(e=>Number(e.ttft_ms)||0).filter(v=>v>0), '#f1bb62');
      $('chart-gen-label').textContent=gens.length?'tok/s':'tok/s';
      acts=new Map(rows.map(e=>[String(e.id),e]));
      thumbs.forEach((_,id)=>{if(!acts.has(id))thumbs.delete(id)});
      // Rows only animate in once the first paint is done and the tab is showing.
      syncRows(rows,!actFirst&&$('activity').classList.contains('active'));
      actFirst=false;
      $('activity-hint').textContent='In-memory on this node';
    }catch(e){
      // Keep the rows already on screen; a failed poll should not wipe the table.
      $('activity-hint').textContent=e.message;
      if(!$('activity-rows').querySelector('tr[data-id]'))setNote(e.message);
    }
  }
  let actFirst=true,acts=new Map();

  // Rows are matched by id and updated in place; see syncRows.
  const IMG_PATH=/\/(images\/(generations|edits|variations)|txt2img|img2img)$/;
  const ACT_NOTE='No generation requests yet. Send one from the Playground or any client.';
  function setNote(text){$('activity-rows').querySelectorAll('.act-note').forEach(n=>n.remove());$('activity-rows').insertAdjacentHTML('beforeend','<tr class="act-note"><td colspan="10" class="empty">'+esc(text)+'</td></tr>')}
  function actCells(e){
    const tok=e.tokens||{};
    const thumb=IMG_PATH.test(e.path||'')?'<span class="thumb" aria-hidden="true" data-id="'+esc(e.id)+'" data-state="'+(e.has_capture?'wait':'none')+'"'+(e.thumb_issue?' title="'+esc(e.thumb_issue)+'"':'')+'></span>':'';
    const io=e.media?mediaCells(e.media,e).map(esc):[fmtTok(tok.prompt_tokens)+(tok.cached_tokens?' <span class="subtle">('+fmtTok(tok.cached_tokens)+' cached)</span>':''),fmtTok(tok.completion_tokens)];
    return [
      ['mono',esc(fmtTime(e.timestamp))],
      ['','<div class="model-line">'+thumb+'<div><div class="model-id">'+esc(e.model||'—')+'</div><div class="subtle">'+esc(e.target||'')+'</div></div></div>'],
      ['mono',esc(e.method||'')+' '+esc(e.path||'')],
      [e.status>=400?'status-bad':'status-ok',esc(e.status)],
      ['mono',io[0]],
      ['mono',io[1]],
      ['mono',esc(fmtSpeed(tok.prompt_per_second))],
      ['mono',esc(fmtSpeed(tok.tokens_per_second))],
      ['mono',esc(e.ttft_ms?fmtDur(e.ttft_ms):'—')],
      ['mono',esc(fmtDur(e.duration_ms))]
    ];
  }
  // mediaCells fills the Prompt and Generated columns of an audio, video, realtime or voice row.
  const fmtSec=v=>Number(v)>0?Number(v).toFixed(1)+' s':'—';
  function mediaCells(m,e){
    switch(m.kind){
      case 'speech':return [(m.input_chars||0)+' chars'+(m.voice?' · '+m.voice:''),fmtSec(m.seconds)+' · '+fmtBytes(e.resp_bytes)];
      case 'transcription':return [(m.file_name||'upload')+' · '+fmtBytes(m.input_bytes),(m.output_chars||0)+' chars'+(m.language?' · '+m.language:'')];
      case 'video':case 'audio_gen':return ['job '+(m.job_id||'—'),(m.state||'—')+' · '+fmtSec(m.seconds)];
      case 'realtime':return [(m.frames_in||0)+' frames in',(m.frames_out||0)+' out · close '+(m.close_code||'—')];
      default:return ['voice library','—'];
    }
  }
  function paintRow(tr,e){
    tr.toggleAttribute('data-empty',!e.has_capture);
    actCells(e).forEach((c,i)=>{
      const td=tr.cells[i];
      if(td.className!==c[0])td.className=c[0];
      if(tr._c[i]!==c[1]){tr._c[i]=c[1];td.innerHTML=c[1]}
    });
    tr.querySelectorAll('.thumb[data-state=wait]').forEach(hydrateThumb);
  }
  const liveRow=n=>{while(n&&n.classList.contains('leaving'))n=n.nextElementSibling;return n};
  function syncRows(entries,animate){
    const tb=$('activity-rows'),have=new Map(),keep=new Set(entries.map(e=>String(e.id)));
    tb.querySelectorAll('.act-note').forEach(n=>n.remove());
    [...tb.children].forEach(tr=>{if(tr.dataset.id&&!tr.classList.contains('leaving'))have.set(tr.dataset.id,tr)});
    have.forEach((tr,id)=>{
      if(keep.has(id))return;
      tr.classList.add('leaving');tr.removeAttribute('data-id');tr.removeAttribute('tabindex');
      setTimeout(()=>tr.remove(),240);
    });
    let ref=liveRow(tb.firstElementChild);
    entries.forEach(e=>{
      const id=String(e.id);
      let tr=have.get(id);
      if(!tr){
        tr=document.createElement('tr');tr.tabIndex=0;tr.dataset.id=id;tr._c=[];
        for(let i=0;i<10;i++)tr.appendChild(document.createElement('td'));
        if(animate)tr.classList.add('row-in');
      }
      paintRow(tr,e);
      if(tr===ref)ref=liveRow(ref.nextElementSibling);else tb.insertBefore(tr,ref);
    });
    if(!entries.length)setNote(ACT_NOTE);
  }

  // Thumbnails: image rows get a placeholder at once; the image is fetched from the
  // capture only when the row scrolls into view. The server decodes and downscales the
  // first image it saw in the response; rows captured without one fall back to the body.
  const thumbs=new Map();
  const thumbIO=window.IntersectionObserver&&new IntersectionObserver(es=>es.forEach(x=>{if(x.isIntersecting){thumbIO.unobserve(x.target);loadThumb(x.target)}}),{rootMargin:'160px'});
  function hydrateThumb(el){if(thumbIO&&!thumbs.has(el.dataset.id))thumbIO.observe(el);else loadThumb(el)}
  function loadThumb(el){
    const id=el.dataset.id;
    if(!thumbs.has(id))thumbs.set(id,thumbSrc(id));
    thumbs.get(id).then(src=>src?showThumb(el,src,()=>captureSrc(id)):el.dataset.state='none');
  }
  // The thumb probe and the <img> GET are separate requests, so the row state flips
  // only once the bytes decode; a failed GET retries the captured body, then gives up.
  function showThumb(el,src,retry){
    if(!src){el.dataset.state='none';return}
    const im=new Image();im.alt='';im.decoding='async';
    im.onload=()=>{el.replaceChildren(im);el.dataset.state='done'};
    im.onerror=()=>retry?retry().then(fb=>showThumb(el,fb===src?'':fb,null)):el.dataset.state='none';
    im.src=src;
  }
  // The thumb endpoint 404s when the response carried no image; the captured body still
  // holds one for responses small enough to fit the capture limit.
  function thumbSrc(id){
    const url='/_router/activity/'+id+'/thumb';
    return fetch(url).then(r=>r.ok?url:captureSrc(id)).catch(()=>captureSrc(id));
  }
  function captureSrc(id){
    return json('/_router/activity/'+id).then(c=>imgSrcs(parseJSON(c.resp_body))[0]||'').catch(()=>{thumbs.delete(id);return ''});
  }

  const parseJSON=s=>{try{return JSON.parse(s)}catch(e){return null}};
  // imgSrcs reads image data from an OpenAI-style (data[]) or sd.cpp-style (images[]) response.
  function imgSrcs(d){
    const list=d&&(Array.isArray(d.data)?d.data:Array.isArray(d.images)?d.images:[])||[];
    return list.map(x=>{
      const v=typeof x==='string'?x:(x&&(x.b64_json||x.url))||'';
      if(!v)return '';
      if(/^(https?:|data:image\/)/i.test(v))return v;
      return 'data:image/'+(v.startsWith('/9j/')?'jpeg':v.startsWith('UklG')?'webp':'png')+';base64,'+v;
    }).filter(Boolean);
  }
  // chatReply reads a chat completion, whole or streamed as SSE. It returns null when the body is neither.
  function chatReply(body){
    const out={text:'',think:'',finish:''};
    const take=d=>{
      const c=d&&Array.isArray(d.choices)&&d.choices[0];
      if(!c)return false;
      const m=c.message||c.delta||{};
      if(typeof m.content==='string')out.text+=m.content;
      if(typeof m.reasoning_content==='string')out.think+=m.reasoning_content;
      if(c.finish_reason)out.finish=c.finish_reason;
      return true;
    };
    const s=String(body||'');
    const whole=parseJSON(s);
    if(whole)return take(whole)?out:null;
    let ok=false;
    s.split('\n').forEach(l=>{if(l.startsWith('data:')){const d=parseJSON(l.slice(5).trim());if(d&&take(d))ok=true}});
    return ok?out:null;
  }
  const msgText=c=>typeof c==='string'?c:Array.isArray(c)?c.map(p=>p&&p.type==='text'?p.text:'['+((p&&p.type)||'part')+']').join('\n'):c==null?'':JSON.stringify(c);
  const capBlock=(title,inner)=>'<section class="cap-block"><h3>'+title+'</h3>'+inner+'</section>';
  const capStat=(k,v)=>'<div class="cap-stat"><dt>'+esc(k)+'</dt><dd>'+esc(v)+'</dd></div>';
  const capText=t=>'<div class="cap-text">'+esc(t)+'</div>';
  // capParsed builds the parsed dialog view. ok is false when the bodies were not recognised, and the
  // dialog then opens on the raw view.
  function capParsed(e,cap){
    const tok=e.tokens||{},req=parseJSON(cap.req_body);
    let html='<dl class="cap-summary">'+capStat('Model',e.model||'—')+capStat('Target',e.target||'—')+capStat('Request',(e.method||'')+' '+(e.path||''))+capStat('Status',e.status)+capStat('Duration',fmtDur(e.duration_ms))+capStat('Time to first token',e.ttft_ms?fmtDur(e.ttft_ms):'—')+capStat('Tokens',fmtTok(tok.prompt_tokens)+' in, '+fmtTok(tok.completion_tokens)+' out'+(tok.cached_tokens?', '+fmtTok(tok.cached_tokens)+' cached':''))+capStat('Speed',fmtSpeed(tok.prompt_per_second)+' prompt, '+fmtSpeed(tok.tokens_per_second)+' generation')+capStat('Content type',e.content_type||'—')+capStat('Size',fmtBytes(e.resp_bytes))+'</dl>';
    let ok=false;
    if(e.media){
      ok=true;
      html+=capMedia(e,e.media,req);
    }else if(IMG_PATH.test(e.path||'')){
      const resp=parseJSON(cap.resp_body),imgs=imgSrcs(resp),r=req||{};
      ok=!!(req||imgs.length);
      const size=r.size||(r.width&&r.height?r.width+'×'+r.height:'—');
      html+=capBlock('Image request','<dl class="cap-summary">'+capStat('Prompt',r.prompt||'—')+capStat('Size',size)+capStat('Count',r.n||imgs.length||'—')+'</dl>');
      html+=capBlock('Result',imgs.length?imgs.map(s=>'<img class="cap-img" alt="Generated image" src="'+esc(s)+'">').join(''):'<p class="subtle">No image in the response body. It may be clipped or not JSON; the raw view has the bytes.</p>');
    }else if(req&&Array.isArray(req.messages)){
      const reply=chatReply(cap.resp_body);
      ok=true;
      const stop=req.stop==null?'—':Array.isArray(req.stop)?req.stop.join(', '):String(req.stop);
      html+=capBlock('Parameters','<dl class="cap-summary">'+capStat('Temperature',req.temperature??'—')+capStat('Max tokens',req.max_tokens??req.max_completion_tokens??'—')+capStat('Stream',req.stream??'—')+capStat('Stop',stop)+'</dl>');
      html+=capBlock('Messages',req.messages.map(m=>'<div class="cap-msg"><div class="cap-role">'+esc((m&&m.role)||'?')+'</div>'+capText(msgText(m&&m.content))+'</div>').join('')||'<p class="subtle">No messages.</p>');
      html+=capBlock('Response',reply?(reply.think?'<details class="cap-think"><summary>Reasoning</summary>'+capText(reply.think)+'</details>':'')+(reply.text?capText(reply.text):'<p class="subtle">No text in the response.</p>')+'<p class="subtle">Finish reason: '+esc(reply.finish||'—')+'</p>':'<p class="subtle">The response is not a chat completion; the raw view has the body.</p>');
    }
    return {html,ok};
  }
  // capMedia renders one media row: a player for captured speech, the transcript preview, the
  // job with a link to its finished content, or the realtime session.
  function capMedia(e,m,req){
    const dl=s=>'<dl class="cap-summary">'+s+'</dl>';
    switch(m.kind){
      case 'speech':{
        const player=m.playable?'<audio controls preload="none" aria-label="Speech response" src="/_router/activity/'+esc(e.id)+'/audio"></audio>':'<p class="subtle">'+esc(fmtBytes(e.resp_bytes))+', not captured.</p>';
        return capBlock('Speech',dl(capStat('Voice',m.voice||'—')+capStat('Characters',m.input_chars||0)+capStat('Length',fmtSec(m.seconds))+capStat('Size',fmtBytes(e.resp_bytes)))+(req&&typeof req.input==='string'?capText(req.input):'')+player);
      }
      case 'transcription':
        return capBlock('Transcription',dl(capStat('File',m.file_name||'—')+capStat('Upload',fmtBytes(m.input_bytes))+capStat('Characters',m.output_chars||0)+capStat('Language',m.language||'—')+capStat('Length',fmtSec(m.seconds)))+(m.text?capText(m.text+(m.output_chars>m.text.length?'…':'')):'<p class="subtle">No transcript text.</p>'));
      case 'video':case 'audio_gen':{
        const done=m.state==='completed'&&m.job_id;
        return capBlock('Job',dl(capStat('Job id',m.job_id||'—')+capStat('State',m.state||'—')+capStat('Duration',fmtSec(m.seconds)))+(done?'<p><a href="'+esc(e.path+'/'+encodeURIComponent(m.job_id)+'/content')+'" target="_blank" rel="noopener">Open the finished '+(m.kind==='video'?'video':'audio')+'</a></p>':'<p class="subtle">This is the submit response; the job finishes on the backend.</p>'));
      }
      case 'realtime':
        return capBlock('Realtime session',dl(capStat('Opened',fmtTime(e.timestamp))+capStat('Duration',fmtDur(e.duration_ms))+capStat('Frames in',m.frames_in||0)+capStat('Frames out',m.frames_out||0)+capStat('Close code',m.close_code||'—')));
      default:
        return capBlock('Voice library',dl(capStat('Change',(e.method||'')+' '+(e.path||''))));
    }
  }

  let capView='parsed';
  function setCapView(v){
    capView=v;
    $('capture-parsed').hidden=v!=='parsed';
    $('capture-raw').hidden=v!=='raw';
    $('capture-toggle').textContent=v==='parsed'?'Show raw':'Show parsed';
  }
  $('capture-toggle').onclick=()=>setCapView(capView==='parsed'?'raw':'parsed');
  async function openCapture(id, row){
    const dlg=$('activity-dialog'),e=acts.get(String(id))||{};
    $('capture-title').textContent='Request '+id;
    $('capture-meta').textContent='Loading capture…';
    $('capture-parsed').innerHTML='';
    ['capture-req','capture-resp','capture-req-h','capture-resp-h'].forEach(k=>{$(k).textContent=''});
    $('capture-toggle').disabled=true;
    setCapView('raw');
    if(row)row.setAttribute('aria-busy','true');
    if(!dlg.open)dlg.showModal();
    try{
      if(!e.has_capture){
        // Nothing was stored for this row (a media response with no request
        // body, or a capture over the ring budget): show the row summary.
        $('capture-meta').textContent='No captured body.';
        $('capture-parsed').innerHTML=capParsed(e,{req_body:'',resp_body:''}).html;
        setCapView('parsed');
        return;
      }
      const cap=await json('/_router/activity/'+id);
      const p=capParsed(e,cap);
      const notes=(cap.notes||[]).length?'Pipeline: '+cap.notes.join('; ')+'. ':'';
      $('capture-meta').textContent=notes+(cap.truncated?'Bodies truncated to 256 KB. ':'')+(p.ok?'':'Not a recognised chat or image body, showing the raw view. ');
      $('capture-parsed').innerHTML=p.html;
      $('capture-req').textContent=prettyBody(cap.req_body);
      $('capture-resp').textContent=prettyBody(cap.resp_body);
      $('capture-req-h').textContent=prettyHeaders(cap.req_headers);
      $('capture-resp-h').textContent=prettyHeaders(cap.resp_headers);
      $('capture-toggle').disabled=false;
      setCapView(p.ok?'parsed':'raw');
    }catch(err){
      $('capture-meta').textContent='Capture unavailable';
      $('capture-req').textContent=err.message;
    }finally{
      if(row)row.removeAttribute('aria-busy');
    }
  }
  $('activity-rows').addEventListener('click',e=>{
    const row=e.target.closest('tr[data-id]');
    if(!row)return;
    openCapture(row.dataset.id, row);
  });
  $('activity-rows').addEventListener('keydown',e=>{
    if(e.key!=='Enter'&&e.key!==' ')return;
    const row=e.target.closest('tr[data-id]');
    if(!row)return;
    e.preventDefault();
    openCapture(row.dataset.id, row);
  });
  $('capture-close').onclick=()=>$('activity-dialog').close();

  // ---- Logs ----
  // One live transcript fed by GET /logs/stream[/...]. The stream is open only
  // while the Logs view is showing.
  const LEVEL_CLASS={error:'lvl-error',warn:'lvl-warn',debug:'lvl-debug',mesh:'lvl-mesh',upstream:'lvl-upstream',info:''};
  const LOG_MAX=2000;
  const logOut=$('log-output');
  let logSrc='',logQuery='',logPause=false,logConn='',logAbort=null,logTimer=0,logRaf=0,logLines=[],logPending=[];

  // splitTs peels the HH:MM:SS.mmm prefix the logger writes.
  function splitTs(line){
    const m=/^(\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)\s+([\s\S]*)$/.exec(String(line||''));
    return m?{ts:m[1],msg:m[2]}:{ts:'',msg:String(line||'')};
  }

  // classify maps a line to a severity for colouring. The logger encodes
  // level in the prefix; upstream lines are raw backend stdout so we look for
  // the words backends actually emit on failure.
  function classify(msg){
    if(msg.startsWith('[router:error]')||msg.startsWith('[error]'))return 'error';
    if(msg.startsWith('[router:warn]')||msg.startsWith('[warn]'))return 'warn';
    if(msg.startsWith('[router:debug]'))return 'debug';
    if(msg.startsWith('[mesh]'))return 'mesh';
    if(msg.startsWith('[upstream]'))return /\b(error|failed|fatal|panic|exception)\b/i.test(msg)?'error':(/\bwarn/i.test(msg)?'warn':'upstream');
    return /\b(error|failed|fatal|panic|exception)\b/i.test(msg)?'error':'info';
  }

  // highlight escapes first, then wraps search hits so the markup cannot be
  // injected through a crafted log line.
  function highlight(s){
    const safe=esc(s);
    if(!logQuery)return safe;
    const q=logQuery.replace(/[.*+?^${}()|[\]\\]/g,'\\$&');
    return safe.replace(new RegExp(q,'gi'),m=>'<mark>'+m+'</mark>');
  }

  const logMatch=line=>!logQuery||line.toLowerCase().includes(logQuery);
  function logRow(line){
    const {ts,msg}=splitTs(line);
    return '<div class="log-line '+LEVEL_CLASS[classify(msg)]+'"><span class="log-ts">'+esc(ts)+'</span><span class="log-msg">'+highlight(msg)+'</span></div>';
  }
  function logStatus(t){
    if(t!==undefined)logConn=t;
    $('log-state').textContent=logPause?'Paused · '+logConn:logConn;
  }
  // logPaint redraws everything held in memory (filter change, resume).
  function logPaint(){
    logPending=[];
    logOut.innerHTML=logLines.filter(logMatch).map(logRow).join('');
    logOut.scrollTop=logOut.scrollHeight;
  }
  // logAppend adds newly streamed lines once per frame. It only follows the
  // tail when the reader was already at the bottom.
  function logAppend(){
    logRaf=0;
    const rows=logPending.splice(0).filter(logMatch);
    if(!rows.length)return;
    const near=logOut.scrollTop+logOut.clientHeight>=logOut.scrollHeight-8;
    logOut.insertAdjacentHTML('beforeend',rows.map(logRow).join(''));
    while(logOut.childElementCount>LOG_MAX)logOut.firstElementChild.remove();
    if(near)logOut.scrollTop=logOut.scrollHeight;
  }
  function logPush(line){
    logLines.push(line);
    if(logLines.length>LOG_MAX)logLines.shift();
    if(logPause)return;
    logPending.push(line);
    if(!logRaf)logRaf=requestAnimationFrame(logAppend);
  }

  function logStop(){
    clearTimeout(logTimer);
    if(logAbort)logAbort.abort();
    logAbort=null;
    if(logRaf)cancelAnimationFrame(logRaf);
    logRaf=0;
  }
  // Some proxies hold a chunked text/plain response until it ends, so the
  // stream never answers. When no headers arrive in time, poll the JSON
  // snapshot (which such proxies do pass) instead of sitting on "Connecting…".
  const LOG_STREAM_WAIT=5000,LOG_POLL_MS=2000;
  async function logPoll(ac){
    // The combined snapshot carries no per-model lines, so a model source
    // polls its own snapshot.
    const model=['','proxy','upstream','mesh'].includes(logSrc)?'':logSrc;
    let prev=new Map();
    const tick=async()=>{
      try{
        const url=model?'/_router/logs/'+model.split('/').map(encodeURIComponent).join('/'):'/_router/logs';
        const r=await fetch(url,{signal:ac.signal});
        if(!r.ok)throw new Error('HTTP '+r.status);
        const j=await r.json();
        const key={proxy:'entries',upstream:'upstream',mesh:'mesh'}[logSrc];
        // shortcut: the combined view orders by the HH:MM:SS prefix, so lines either side of midnight can swap, upgrade if the snapshot carries a sequence number.
        const lines=model?(j.lines||[]):key?(j[key]||[]):[...(j.entries||[]),...(j.upstream||[]),...(j.mesh||[])].sort((a,b)=>a.slice(0,12)<b.slice(0,12)?-1:a.slice(0,12)>b.slice(0,12)?1:0);
        const seen=new Map();
        lines.forEach(l=>{
          const n=(seen.get(l)||0)+1;
          seen.set(l,n);
          if(n>(prev.get(l)||0))logPush(l);
        });
        prev=seen;
        logStatus('Polling every 2 s · live stream blocked');
      }catch(e){
        if(ac.signal.aborted)return;
        logStatus('Polling failed · retrying');
      }
      logTimer=setTimeout(tick,LOG_POLL_MS);
    };
    tick();
  }
  async function logStart(){
    logStop();
    logLines=[];logPending=[];logOut.innerHTML='';
    const ac=logAbort=new AbortController();
    logStatus('Connecting…');
    let slow=false;
    const wait=setTimeout(()=>{slow=true;ac.abort()},LOG_STREAM_WAIT);
    try{
      const url='/logs/stream'+(logSrc?'/'+logSrc.split('/').map(encodeURIComponent).join('/'):'');
      const r=await fetch(url,{signal:ac.signal});
      clearTimeout(wait);
      if(!r.ok){logStatus((await r.text()).trim()||'HTTP '+r.status);return}
      logStatus('Live');
      const reader=r.body.getReader(),dec=new TextDecoder();
      let buf='';
      for(;;){
        const x=await reader.read();
        if(x.done)break;
        buf+=dec.decode(x.value,{stream:true});
        const parts=buf.split('\n');
        buf=parts.pop();
        parts.forEach(l=>{if(l)logPush(l)});
      }
      throw new Error('stream closed');
    }catch(e){
      clearTimeout(wait);
      if(slow&&logAbort===ac){
        const next=logAbort=new AbortController();
        logPoll(next);
        return;
      }
      if(ac.signal.aborted)return;
      logStatus('Disconnected · retrying');
      logTimer=setTimeout(logStart,2000);
    }
  }

  const logModelEl=$('log-model');
  const logTabs=$('log-tabs');
  function logSelect(src,tab){
    logSrc=src;
    logTabs.querySelectorAll('button').forEach(x=>{x.classList.toggle('active',x===tab);x.setAttribute('aria-pressed',x===tab?'true':'false')});
    logModelEl.classList.toggle('active',!tab);
    if(tab)logModelEl.value='';
    logStart();
  }
  logTabs.addEventListener('click',ev=>{const b=ev.target.closest('button[data-src]');if(b)logSelect(b.dataset.src,b)});
  logModelEl.addEventListener('change',()=>{if(logModelEl.value)logSelect(logModelEl.value,null)});
  const searchEl=$('log-search');
  let searchTimer=0;
  searchEl.addEventListener('input',()=>{
    clearTimeout(searchTimer);
    searchTimer=setTimeout(()=>{logQuery=searchEl.value.trim().toLowerCase();logPaint()},140);
  });
  $('log-pause').addEventListener('change',ev=>{
    logPause=ev.target.checked;
    logStatus();
    if(!logPause)logPaint();
  });

  refresh();activity();setInterval(refresh,3000);setInterval(activity,3000);
})();
