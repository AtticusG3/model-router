(function(){
  let status={models:[],telemetry:{},peers:{}};
  const $=id=>document.getElementById(id);
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  // Skips the write when the markup is unchanged, so a poll does not rebuild nodes (flicker, lost hover, lost selection).
  const setHTML=(el,html)=>{if(el._h!==html){el._h=html;el.innerHTML=html}};
  const icon=s=>{const paths={model:'<path d="M6 4.5 12 2l6 2.5v6L12 13l-6-2.5v-6Z"/><path d="m6 10.5 6 2.5 6-2.5M12 13v5"/>',peer:'<circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="m8.2 10.8 7.5-3.6M8.2 13.2l7.5 3.6"/>'};return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">'+(paths[s]||paths.model)+'</svg>'};
  // Family id -> SVG, filled from ui/families at build time.
  const FAMILY_ICONS=__EMBED_FAMILY_ICONS__;
  const pageCopy={dashboard:['Fleet node','Overview','Live capacity and model activity for this router.'],playground:['Operator tools','Playground','Try a streaming chat, an image, an embeddings, a rerank, a speech, a transcription, a video, a music or sound effect request, or a load test against this router.'],activity:['Observability','Activity','Generation, audio, video and realtime requests on this node, with token or media stats and captured bodies.'],logs:['Observability','Logs','Live router, upstream and mesh output.']};
  document.querySelectorAll('.nav button').forEach(b=>b.onclick=()=>{const view=b.dataset.view;document.querySelectorAll('.nav button').forEach(x=>{x.classList.remove('active');x.removeAttribute('aria-current')});const wasLogs=$('logs').classList.contains('active'),wasAct=$('activity').classList.contains('active');document.querySelectorAll('.view').forEach(x=>x.classList.remove('active'));b.classList.add('active');b.setAttribute('aria-current','page');$(view).classList.add('active');$('eyebrow').textContent=pageCopy[view][0];$('page-title').textContent=pageCopy[view][1];$('page-subtitle').textContent=pageCopy[view][2];if(view!=='logs')logStop();else if(!wasLogs)logStart();if(view!=='activity')actStop();else if(!wasAct)actStart()});
  async function json(url,opts){const r=await fetch(url,opts);const text=await r.text();let data;try{data=JSON.parse(text)}catch(e){data={error:text}}if(!r.ok){const e=new Error(data.error||text||('HTTP '+r.status));e.status=r.status;throw e}return data}
  function setHealth(ok,message){$('health').textContent=message;$('side-health').textContent=ok?'Status updates every 3 seconds':'Unable to reach status endpoint';$('health-dot').className='status-dot '+(ok?'':'off');$('side-dot').className='status-dot '+(ok?'':'off')}
  window.refresh=async function(){try{status=await json('/_router/status');const node=status.node||'unknown';const now=new Date();$('side-node').textContent=node;$('updated').textContent='Updated '+now.toLocaleTimeString();setHealth(true,'Router healthy');renderSummary();renderGPUs();renderModels();renderPeers();fillSelectors()}catch(e){setHealth(false,e.message);$('updated').textContent='Snapshot unavailable'}};
  function renderSummary(){const gs=status.telemetry&&status.telemetry.gpus||[];const models=status.models||[];const running=models.filter(m=>m.state==='running').length;const peers=status.peers||{};$('stat-gpus').textContent=gs.length;$('stat-running').textContent=running;$('stat-models').textContent=models.length;$('stat-peers').textContent=Object.keys(peers).length;$('model-summary').textContent=models.length+' entries · '+running+' online'}
  function renderGPUs(){const gs=status.telemetry&&status.telemetry.gpus||[];const lm=status.telemetry&&status.telemetry.loaded_models||[];setHTML($('gpu-cards'),gs.map(g=>{const on=lm.filter(m=>(m.gpus&&m.gpus.length?m.gpus:[m.gpu]).includes(g.index)).map(m=>'<li><span class="mono">'+esc(m.instance||m.id)+'</span>'+(m.slots?'<span class="subtle">'+esc(m.in_flight)+' in flight, '+esc(m.slots)+' slots</span>':'')+'</li>');const total=Number(g.total_mb)||0,free=Number(g.free_mb)||0,stale=Number(g.free_if_stale_evicted_mb)||free,idle=Number(g.free_if_idle_evicted_mb)||stale;const used=total?Math.max(0,Math.min(100,((total-free)/total)*100)):0;return '<article class="panel gpu-card"><div class="gpu-title"><span class="gpu-name">'+esc(g.name||'GPU '+g.index)+'</span><span class="gpu-index">GPU '+esc(g.index)+'</span></div><div class="gpu-memory"><span class="gpu-free">'+esc(free)+'</span><span class="gpu-unit">MB free</span></div><div class="meter" role="progressbar" aria-label="GPU memory used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="'+Math.round(used)+'"><div class="meter-fill" style="width:'+used+'%"></div></div><div class="gpu-footer"><span>'+esc(total)+' MB total</span><span>'+esc(idle)+' MB if idle evicted</span></div><ul class="gpu-models" aria-label="Models on this GPU">'+(on.join('')||'<li class="subtle">No models</li>')+'</ul></article>'}).join('')||'<div class="panel empty">No GPU telemetry available yet.</div>')}
  function renderModels(){const rows=(status.models||[]).map(m=>{const origin=m.origin||'local';const freshness=m.freshness||'';const state=freshness||m.state||(origin==='remote'?'available':'stopped');const can=origin==='local';const f=m.family,iu=m.info&&m.info.icon_url,fi=iu&&safeUrl(iu)?'<img src="'+esc(iu)+'" alt="" width="16" height="16" referrerpolicy="no-referrer">':f&&Object.hasOwn(FAMILY_ICONS,f.id)&&FAMILY_ICONS[f.id],fl=f?f.name+(f.owner&&f.owner!==f.name?' ('+f.owner+')':''):'';return '<tr><td><div class="model-cell"><span class="model-glyph'+(fi?' family':'')+'"'+(f?' role="img" aria-label="'+esc(fl)+'" title="'+esc(fl)+'"':'')+'>'+(fi||icon(origin==='remote'?'peer':'model'))+'</span><div><span class="model-id">'+esc(m.id)+'</span>'+(m.name?'<span class="model-name">'+esc(m.name)+'</span>':'')+infoChips(m.info)+'</div></div></td><td><span class="state '+esc(origin)+'">'+esc(origin==='remote'?'remote':'local')+'</span></td><td><span class="state '+esc(state)+'">'+esc(state)+'</span></td><td>'+peerChips(m.peers)+'</td><td class="mono">'+(m.vram_mb?esc(m.vram_mb)+' MB':'—')+'</td><td>'+(can?'<div class="actions"><a class="ghost" href="/upstream/'+encodeURIComponent(m.id)+'/" target="_blank" rel="noopener">Open UI</a><button data-act="load" data-id="'+esc(m.id)+'">Load</button><button class="danger" data-act="unload" data-id="'+esc(m.id)+'">Unload</button></div>':'<span class="subtle">—</span>')+'</td></tr>'}).join('');setHTML($('models'),rows||'<tr><td colspan="6" class="empty">No models in the catalog.</td></tr>')}
  // Owner, parameter count, then what the server knows for the model's type: context and quant, or image size and steps.
  function infoChips(i){if(!i)return '';const n=i.context_length,c=[i.owner,i.params&&i.params+' params',i.licence,n&&(n>=1024?Math.round(n/1024)+'k':n)+' ctx',i.modality,i.quant,i.size,i.steps&&i.steps+' steps'].filter(Boolean).map(x=>'<span class="chip">'+esc(x)+'</span>');if(i.hub&&safeUrl(i.hub_url))c.push('<a class="chip" href="'+esc(i.hub_url)+'" target="_blank" rel="noopener noreferrer">from '+esc(i.hub)+'</a>');return c.length?'<span class="model-meta">'+c.join('')+'</span>':''}
  // One chip per peer serving the model, e.g. "nodeb · loaded · gpu1 · 1/2"; last seen is the tooltip.
  function peerChips(ps){if(!ps||!ps.length)return '<span class="subtle">—</span>';return '<div class="peer-chips">'+ps.map(p=>{const bits=[p.name,p.state];if(p.gpus&&p.gpus.length)bits.push('gpu'+p.gpus.join(','));if(p.slots)bits.push((p.in_flight||0)+'/'+p.slots);return '<span class="state '+esc(p.state)+'" title="'+esc(p.last_seen?'Last seen '+new Date(p.last_seen*1000).toLocaleString():'No telemetry received')+'">'+esc(bits.join(' · '))+'</span>'}).join('')+'</div>'}
  function renderPeers(){const ps=status.peers||{};const disc=new Set(status.discovered||[]);const cards=Object.keys(ps).sort().map(n=>{const p=ps[n]||{};const gs=p.gpus||[];const loaded=(p.loaded_models||[]).map(m=>typeof m==='string'?m:((m.instance||m.id||'')+(m.gpu>=0?' @gpu'+(m.gpus&&m.gpus.length?m.gpus.join(','):m.gpu):'')+(m.freshness?' '+m.freshness:'')+(m.slots?(' '+m.in_flight+'/'+m.slots):'')));return '<article class="panel peer-card"><div class="peer-head"><div><div class="peer-name">'+esc(n)+(disc.has(n)?' <span class="subtle">discovered</span>':'')+'</div><div class="subtle mono">'+(p.timestamp?'Last seen '+new Date(p.timestamp*1000).toLocaleTimeString():'No timestamp')+'</div></div><span class="peer-state">'+(gs.length?'Fresh telemetry':'No telemetry')+'</span></div><div class="peer-detail"><div><div class="peer-label">GPU memory</div><div class="peer-value">'+esc(gs.map(g=>g.free_mb+' MB free'+(g.free_if_stale_evicted_mb&&g.free_if_stale_evicted_mb!==g.free_mb?' / '+g.free_if_stale_evicted_mb+' if stale evicted':'')).join(' · ')||'—')+'</div></div><div><div class="peer-label">Loaded models</div><div class="peer-value">'+esc(loaded.join(', ')||'None reported')+'</div></div></div></article>'}).join('');setHTML($('peers'),cards||'<div class="panel empty">No fresh peer telemetry.</div>')}
  function sameOptions(sel,ids){return sel.options.length===ids.length&&ids.every((id,i)=>sel.options[i].value===id)}
  function fillSelect(sel,ids,html){const prev=sel.value;if(sameOptions(sel,ids))return;sel.innerHTML=html;if([...sel.options].some(o=>o.value===prev))sel.value=prev}
  function fillSelectors(){const local=(status.models||[]).filter(m=>(m.origin||'local')==='local').map(m=>m.id);fillSelect($('log-model'),[''].concat(local),'<option value="">Model…</option>'+local.map(id=>'<option value="'+esc(id)+'">'+esc(id)+'</option>').join(''));const models=(status.models||[]).filter(m=>m.api_type!=='image');const ids=models.map(m=>m.id);fillSelect($('chat-model'),ids.length?ids:[''],ids.length?models.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No models available</option>');const embs=(status.models||[]).filter(m=>m.api_type==='embedding');fillSelect($('embed-model'),embs.length?embs.map(m=>m.id):[''],embs.length?embs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No embedding models available</option>');const rrs=(status.models||[]).filter(m=>m.api_type==='rerank');fillSelect($('rerank-model'),rrs.length?rrs.map(m=>m.id):[''],rrs.length?rrs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No rerank models available</option>');const sps=(status.models||[]).filter(m=>m.api_type==='speech');fillSelect($('speech-model'),sps.length?sps.map(m=>m.id):[''],sps.length?sps.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No speech models available</option>');const trs=(status.models||[]).filter(m=>m.api_type==='transcription');fillSelect($('transcribe-model'),trs.length?trs.map(m=>m.id):[''],trs.length?trs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No transcription models available</option>');fillSelect($('load-model'),ids.length?ids:[''],ids.length?models.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No models available</option>');const rts=(status.models||[]).filter(m=>m.api_type==='realtime');fillSelect($('rt-model'),rts.length?rts.map(m=>m.id):[''],rts.length?rts.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No realtime models available</option>');const imgs=(status.models||[]).filter(m=>m.api_type==='image');fillSelect($('image-model'),[''].concat(imgs.map(m=>m.id)),'<option value="">Path default</option>'+imgs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+'</option>').join(''));const vids=(status.models||[]).filter(m=>m.api_type==='video');fillSelect($('video-model'),vids.length?vids.map(m=>m.id):[''],vids.length?vids.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No video models available</option>');const ags=(status.models||[]).filter(m=>m.api_type==='audio_gen');fillSelect($('audiogen-model'),[''].concat(ags.map(m=>m.id)),'<option value="">Path default</option>'+ags.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+'</option>').join(''));videoOptions();actPeerFill()}
  // Duration, resolution and seed are opt-in per video model through its capabilities list.
  function videoOptions(){const m=(status.models||[]).find(x=>x.id===$('video-model').value)||{};const caps=m.capabilities||[];for(const k of ['duration','resolution','seed'])$('video-opt-'+k).hidden=!caps.includes(k)}
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
    if(t.dataset.tab==='speech')loadVoices();
  });
  pgTabs.addEventListener('keydown',ev=>{
    if(ev.key!=='ArrowLeft'&&ev.key!=='ArrowRight')return;
    const tabs=[...pgTabs.querySelectorAll('[data-tab]')];
    const next=tabs[(tabs.indexOf(document.activeElement)+(ev.key==='ArrowRight'?1:tabs.length-1))%tabs.length];
    next.focus();next.click();
  });
  $('video-model').addEventListener('change',videoOptions);

  // Playground history lives in these arrays only, never in browser storage, so a reload clears it.
  // Per tab: the output panel, the model field, then the other inputs a restore fills back in.
  const HIST={chat:['chat-output','chat-model','chat-prompt','chat-tokens','chat-temp'],image:['image-output','image-model','image-prompt','image-negative','image-width','image-height','image-steps','image-seed'],embeddings:['embed-output','embed-model','embed-input'],rerank:['rerank-output','rerank-model','rerank-query','rerank-docs'],speech:['speech-output','speech-model','speech-voice','speech-format','speech-input'],transcription:['transcribe-output','transcribe-model','transcribe-format','transcribe-language'],video:['video-output','video-model','video-prompt','video-seconds','video-resolution','video-seed'],audiogen:['audiogen-output','audiogen-model','audiogen-prompt','audiogen-lyrics','audiogen-seconds','audiogen-seed'],load:['load-output','load-model','load-prompt','load-conc','load-count','load-tokens'],realtime:['rt-output','rt-model']};
  const HIST_MAX=50,HIST_MEDIA=20,HIST_STATUS={ok:['Done','status-ok'],error:['Failed','status-bad'],cancelled:['Cancelled','subtle']};
  // Only these ids are selects: a restore checks their value still exists as an option.
  const HIST_SELECTS=new Set(['chat-model','image-model','embed-model','rerank-model','speech-model','speech-voice','speech-format','transcribe-model','transcribe-format','transcribe-language','video-model','video-resolution','audiogen-model','load-model','rt-model']);
  const hist={},histBlank={},histBusy={};
  let histSeq=0;
  for(const tab in HIST){
    hist[tab]=[];
    const o=$(HIST[tab][0]);histBlank[tab]=[o.className,o.innerHTML,$('hist-'+tab).innerHTML];
    $('hist-'+tab).onclick=ev=>{
      const b=ev.target.closest('[data-open],[data-reuse]'),e=b&&hist[tab].find(x=>String(x.id)===(b.dataset.open||b.dataset.reuse));
      if(!e)return;
      for(const id in e.inputs)$(id).value=e.inputs[id];
      if(tab==='video')videoOptions();
      // A select whose stored value has left the list goes blank, which for a model means the
      // path default. Say so; Speech reloads its voices for the restored model before checking.
      const check=()=>{
        const missing=[];
        for(const id in e.inputs){
          const el=$(id);
          if(HIST_SELECTS.has(id)&&![...el.options].some(x=>x.value===e.inputs[id]))missing.push(histLabel(el,id)+' "'+e.inputs[id]+'"');
        }
        histNote(tab,missing.length?'Not available: '+missing.join(', '):'');
      };
      if(tab==='speech')loadVoices(false,e.inputs['speech-voice']).then(check);else check();
      // Reuse fills the form only, a live realtime session keeps its own transcript,
      // and a running request keeps painting its own output.
      if(b.dataset.reuse||(tab==='realtime'&&rt)||histBusy[tab])return;
      o.className=e.cls;o.innerHTML=e.html;
      if(tab==='transcription')showTranscript(e.state);
    };
  }
  function histLabel(el,id){const l=el.closest&&el.closest('label'),s=l&&l.querySelector('span');return s&&s.textContent||id}
  function histNote(tab,text){
    let n=$('hist-note-'+tab);
    if(!n){n=document.createElement('p');n.id='hist-note-'+tab;n.className='subtle';n.setAttribute('role','status');n.setAttribute('aria-live','polite');const ul=$('hist-'+tab);if(ul.insertAdjacentElement)ul.insertAdjacentElement('afterend',n)}
    n.textContent=text;n.hidden=!text;
  }
  // histStart snapshots the inputs as a request is sent; note is the summary line (the prompt unless given).
  function histStart(tab,note){
    const f=HIST[tab],inputs={};
    f.slice(1).forEach(id=>{inputs[id]=$(id).value});
    histBusy[tab]=true;
    return {tab,time:Date.now(),model:inputs[f[1]]||'Path default',note:note??inputs[f[2]]??'',inputs};
  }
  // histEnd keeps the output panel as it now stands (or x.html when the panel plays a one-shot
  // stream URL). x.urls are the blob URLs the entry owns; they are revoked when it is dropped.
  function histEnd(h,status,x={}){
    if(!h)return;
    histBusy[h.tab]=false;
    const o=$(HIST[h.tab][0]),keep=[];
    Object.assign(h,{id:++histSeq,status,ms:Date.now()-h.time,cls:o.className,html:x.html??o.innerHTML,urls:x.urls||[],state:x.state});
    let media=0;
    for(const e of [h,...hist[h.tab]]){if(keep.length<HIST_MAX&&!(e.urls.length&&++media>HIST_MEDIA))keep.push(e);else histDrop(e)}
    hist[h.tab]=keep;
    histRender(h.tab);
  }
  // A dropped entry still on show would leave a dead player or link, so the panel goes back to its placeholder.
  function histDrop(e){
    const o=$(HIST[e.tab][0]);
    if(e.urls.some(u=>o.innerHTML.includes('"'+esc(u)+'"'))){o.className=histBlank[e.tab][0];o.innerHTML=histBlank[e.tab][1]}
    e.urls.forEach(u=>URL.revokeObjectURL(u));
  }
  function histRender(tab){
    $('hist-'+tab).innerHTML=hist[tab].map(e=>{const when=new Date(e.time).toLocaleTimeString(),st=HIST_STATUS[e.status];return '<li class="history-item"><button class="history-open" data-open="'+e.id+'"><span class="history-meta">'+esc(when)+' · <span class="mono">'+esc(e.model)+'</span> · <span class="'+st[1]+'">'+st[0]+'</span> · '+fmtDur(e.ms)+'</span><span class="history-note">'+esc(e.note||'(no input)')+'</span></button><button class="ghost" data-reuse="'+e.id+'" aria-label="Reuse the inputs from '+esc(when)+'">Reuse</button></li>'}).join('')||histBlank[tab][2];
  }
  window.clearHistory=tab=>{hist[tab].forEach(histDrop);hist[tab]=[];histRender(tab)};

  window.sendChat=async function(){
    const out=$('chat-output');
    out.className='output chat-output';
    out.innerHTML='';
    const body={model:$('chat-model').value,messages:[{role:'user',content:$('chat-prompt').value}],max_tokens:Number($('chat-tokens').value)||256,temperature:Number($('chat-temp').value)||0.7,stream:true};
    const h=histStart('chat');
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
      histEnd(h,'ok');
    }catch(e){
      if(raf)cancelAnimationFrame(raf);
      out.className='output chat-output error';
      out.textContent=e.message;
      histEnd(h,'error');
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
    const h=histStart('image');
    const ctl=imageCtl=new AbortController();
    let timedOut=false;
    const timer=setTimeout(()=>{timedOut=true;ctl.abort()},180000);
    go.disabled=true;stop.disabled=false;
    try{
      const r=await fetch('/sdapi/v1/txt2img',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body),signal:ctl.signal});
      const text=await r.text();
      if(!r.ok){const wait=r.headers.get('Retry-After');throw new Error('HTTP '+r.status+': '+(text.trim()||r.statusText)+(wait?' (retry in '+wait+' s)':''))}
      let d;try{d=JSON.parse(text)}catch(e){throw new Error('The response was not JSON.')}
      // Blob URLs, not data URLs, so up to 20 history entries hold bytes rather than base64 strings.
      const urls=(d.images||[]).map(x=>URL.createObjectURL(new Blob([Uint8Array.from(atob(x),c=>c.charCodeAt(0))],{type:'image/png'})));
      out.innerHTML=urls.length?urls.map(u=>'<img alt="Generated image" src="'+esc(u)+'">').join(''):'<span class="muted">No images returned.</span>';
      histEnd(h,'ok',{urls});
    }catch(e){
      out.className='image-result error';
      out.textContent=ctl.signal.aborted?(timedOut?'Timed out after 3 minutes with no answer.':'Cancelled.'):e.message;
      histEnd(h,ctl.signal.aborted&&!timedOut?'cancelled':'error');
    }finally{clearTimeout(timer);imageCtl=null;go.disabled=false;stop.disabled=true}
  };

  // shortcut: shows the first 8 values only and no copy button, add a full-vector download if operators ask.
  window.sendEmbedding=async function(){
    const out=$('embed-output'),model=$('embed-model').value;
    let h=null;
    const fail=m=>{out.className='output error';out.textContent=m;histEnd(h,'error')};
    if(!model)return fail('No embedding model is available. Add a catalog entry with api_type: embedding.');
    h=histStart('embeddings');
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
      histEnd(h,'ok');
    }catch(e){fail(e.message)}
  };

  // shortcut: no top_n control and the document text comes from the input lines, add both if backends return trimmed or reordered documents.
  window.sendRerank=async function(){
    const out=$('rerank-output'),model=$('rerank-model').value;
    let h=null;
    const fail=m=>{out.className='output error';out.textContent=m;histEnd(h,'error')};
    const docs=$('rerank-docs').value.split('\n').map(x=>x.trim()).filter(Boolean);
    if(!model)return fail('No rerank model is available. Add a catalog entry with api_type: rerank.');
    if(!$('rerank-query').value.trim())return fail('Enter a query.');
    if(!docs.length)return fail('Enter at least one document, one per line.');
    h=histStart('rerank');
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
      histEnd(h,'ok');
    }catch(e){fail(e.message)}
  };

  // Voices come from the selected model's backend. Only a running model is
  // asked (force after a generation has started it), so opening the tab never
  // starts or evicts a model just to fill the list.
  let voiceSeq=0;
  window.loadVoices=async function(force,want){
    const sel=$('speech-voice'),model=$('speech-model').value,prev=want===undefined?sel.value:want,seq=++voiceSeq;
    const m=(status.models||[]).find(x=>x.id===model),running=force===true||(!!m&&m.state==='running');
    let ids=[];
    if(model&&running){
      try{
        const r=await fetch('/v1/audio/voices?model='+encodeURIComponent(model));
        if(r.ok){const d=await r.json(),list=Array.isArray(d)?d:(d.data||d.voices||[]);ids=[...new Set(list.map(v=>typeof v==='string'?v:v&&(v.id||v.voice_id||v.name)).filter(Boolean))]}
      }catch(e){}
    }
    if(seq!==voiceSeq)return;
    // A restore names the voice it wants even when the model is stopped, so the select keeps it and Generate sends it again; the backend accepts or rejects it.
    if(model&&!running&&want)ids=[want];
    sel.innerHTML='<option value="">'+(model&&!running?'Backend default (voices list once the model runs)':'Backend default')+'</option>'+ids.map(id=>'<option value="'+esc(id)+'">'+esc(id)+'</option>').join('');
    if(ids.includes(prev))sel.value=prev;else sel.value='';
  };

  // streamInto feeds a response body into a MediaSource as chunks arrive, and
  // returns the whole body as a Blob for the download link.
  async function streamInto(ms,body,ct){
    await new Promise(res=>ms.addEventListener('sourceopen',res,{once:true}));
    const sb=ms.addSourceBuffer(ct),reader=body.getReader(),parts=[];
    const idle=()=>sb.updating?new Promise(res=>sb.addEventListener('updateend',res,{once:true})):null;
    for(;;){
      const {done,value}=await reader.read();
      if(done)break;
      parts.push(value);
      await idle();
      sb.appendBuffer(value);
    }
    await idle();
    ms.endOfStream();
    return new Blob(parts,{type:ct});
  }

  // Audio plays as it arrives through MediaSource when the browser can decode
  // the returned type that way (usually MP3 and AAC); anything else plays once
  // the whole file is in.
  // The MediaSource URL plays once, so history keeps the finished blob instead.
  let speechLive='';
  window.sendSpeech=async function(){
    const out=$('speech-output'),model=$('speech-model').value,input=$('speech-input').value.trim(),voice=$('speech-voice').value,format=$('speech-format').value;
    let h=null;
    const fail=m=>{out.className='output error';out.textContent=m;histEnd(h,'error')};
    if(!model)return fail('No speech model is available. Add a catalog entry with api_type: speech.');
    if(!input)return fail('Enter some text to speak.');
    h=histStart('speech',input);
    out.className='output muted';out.textContent='Generating audio…';
    try{
      const body={model,input,response_format:format};if(voice)body.voice=voice;
      const r=await fetch('/v1/audio/speech',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      if(!r.ok){const text=await r.text();let d;try{d=JSON.parse(text)}catch(e){d=null}const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      if(speechLive)URL.revokeObjectURL(speechLive);
      speechLive='';
      const ct=(r.headers.get('Content-Type')||'').split(';')[0].trim();
      const streamed=!!(r.body&&window.MediaSource&&ct&&MediaSource.isTypeSupported(ct));
      let blob;
      if(streamed){
        const ms=new MediaSource(),src=speechLive=URL.createObjectURL(ms);
        out.className='output';
        out.innerHTML='<audio controls autoplay aria-label="Generated speech" src="'+esc(src)+'" style="width:100%"></audio><p id="speech-dl" class="subtle">Playing as it arrives…</p>';
        blob=await streamInto(ms,r.body,ct);
      }else{
        blob=await r.blob();
      }
      if(!blob.size)throw new Error('The response had no audio.');
      const url=URL.createObjectURL(blob);
      const link='<a href="'+esc(url)+'" download="speech.'+esc(format)+'">Download audio</a>';
      const html=(format==='pcm'?'<p class="subtle">Raw PCM has no header, so the browser cannot play it. Download it instead.</p>':'<audio controls aria-label="Generated speech" src="'+esc(url)+'" style="width:100%"></audio>')+'<p>'+link+'</p>';
      if(streamed){
        $('speech-dl').innerHTML=link;
      }else{
        out.className='output';
        out.innerHTML=html;
      }
      histEnd(h,'ok',{urls:[url],html});
      loadVoices(true);
    }catch(e){fail(e.message)}
  };

  // Music and sound effects run as an async job: submit, poll the job every 2 s,
  // then fetch the finished audio once for playback and download.
  window.sendAudioGen=async function(){
    const out=$('audiogen-output'),btn=$('audiogen-submit'),model=$('audiogen-model').value,prompt=$('audiogen-prompt').value.trim(),lyrics=$('audiogen-lyrics').value.trim(),seconds=Number($('audiogen-seconds').value),seed=$('audiogen-seed').value;
    let h=null;
    const fail=m=>{out.className='output error';out.textContent=m;btn.disabled=false;histEnd(h,'error')};
    const httpErr=async r=>{const text=await r.text();let d;try{d=JSON.parse(text)}catch(x){d=null}const e=d&&d.error;return new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))};
    if(!prompt)return fail('Enter a prompt describing the music or sound.');
    h=histStart('audiogen');
    btn.disabled=true;out.className='output muted';out.textContent='Submitting job…';
    try{
      const body={prompt};if(model)body.model=model;if(lyrics)body.lyrics=lyrics;if(seconds>0)body.seconds=seconds;if(seed!==''&&Number(seed)>=0)body.seed=Number(seed);
      let r=await fetch('/v1/audio/generations',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      if(!r.ok)throw await httpErr(r);
      let job=await r.json();
      const path='/v1/audio/generations/'+encodeURIComponent(job.id);
      while(job.status!=='completed'){
        if(job.status&&job.status!=='queued'&&job.status!=='in_progress')throw new Error('Job '+job.id+' '+job.status+': '+((job.error&&job.error.message)||'no reason given'));
        out.textContent='Job '+job.id+': '+(job.status||'queued')+(job.progress?' · '+job.progress+'%':'')+'…';
        await new Promise(res=>setTimeout(res,2000));
        r=await fetch(path);if(!r.ok)throw await httpErr(r);job=await r.json();
      }
      out.textContent='Fetching audio…';
      r=await fetch(path+'/content');if(!r.ok)throw await httpErr(r);
      const blob=await r.blob();
      if(!blob.size)throw new Error('The job finished with no audio.');
      const url=URL.createObjectURL(blob);
      const ext=(blob.type.split(';')[0].split('/')[1]||'mp3').replace('mpeg','mp3');
      out.className='output';
      out.innerHTML='<audio controls aria-label="Generated audio" src="'+esc(url)+'" style="width:100%"></audio><p><a href="'+esc(url)+'" download="'+esc(job.id)+'.'+esc(ext)+'">Download audio</a></p>';
      btn.disabled=false;
      histEnd(h,'ok',{urls:[url]});
    }catch(e){fail(e.message)}
  };

  // Subtitle results (srt, vtt) keep their text and format here for the Copy and Download buttons.
  let transcript=null;
  function showTranscript(t){
    transcript=t||null;$('transcribe-actions').hidden=!t;
    if(t){$('transcribe-copy').textContent='Copy '+t.format.toUpperCase();$('transcribe-download').textContent='Download .'+t.format}
  }
  window.sendTranscription=async function(){
    const out=$('transcribe-output'),model=$('transcribe-model').value,file=$('transcribe-file').files[0],format=$('transcribe-format').value,language=$('transcribe-language').value;
    let h=null;
    const fail=m=>{out.className='output error';out.textContent=m;histEnd(h,'error')};
    showTranscript(null);
    if(!model)return fail('No transcription model is available. Add a catalog entry with api_type: transcription.');
    if(!file)return fail('Choose an audio file.');
    h=histStart('transcription',file.name);
    out.className='output muted';out.textContent='Transcribing…';
    try{
      const fd=new FormData();fd.append('model',model);fd.append('response_format',format);if(language)fd.append('language',language);fd.append('file',file);
      const r=await fetch('/v1/audio/transcriptions',{method:'POST',body:fd});
      const text=await r.text();
      let d;try{d=JSON.parse(text)}catch(e){d=null}
      if(!r.ok){const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      out.className='output';
      out.textContent=format==='verbose_json'&&d?JSON.stringify(d,null,2):d&&typeof d.text==='string'?d.text:text;
      if(format==='srt'||format==='vtt')showTranscript({text,format});
      histEnd(h,'ok',{state:transcript});
    }catch(e){fail(e.message)}
  };
  window.copyTranscript=async function(){
    const b=$('transcribe-copy'),label='Copy '+transcript.format.toUpperCase();
    // The clipboard API needs a secure context; over plain http the Download button still works.
    try{await navigator.clipboard.writeText(transcript.text);b.textContent='Copied'}catch(e){b.textContent='Copy failed'}
    setTimeout(()=>{b.textContent=label},1500);
  };
  window.downloadTranscript=function(){
    const url=URL.createObjectURL(new Blob([transcript.text],{type:transcript.format==='vtt'?'text/vtt':'application/x-subrip'}));
    const a=document.createElement('a');a.href=url;a.download='transcript.'+transcript.format;a.click();
    setTimeout(()=>URL.revokeObjectURL(url),1000);
  };

  // Video jobs outlive the tab and the page: the id stays in sessionStorage so a
  // reload resumes the same poll, and the router keeps the job running.
  const VIDEO_JOB_KEY='router.video.job';
  let videoJob=null,videoTimer=0,videoDelay=1000,videoHist=null;
  function videoDone(status,urls){histEnd(videoHist,status,{urls});videoHist=null}
  // A Cancel invalidates a status check already in flight so it cannot repaint or reschedule.
  let videoCheckEpoch=0,videoFailures=0;
  function videoError(d){
    const e=d&&d.error;
    return (e&&e.message)||(typeof e==='string'&&e)||(d&&(d.failure_reason||d.detail))||'The job failed without a message.';
  }
  // shortcut: progress is the backend's number when it sends one; add an indeterminate bar if a backend never reports it.
  function videoPaint(state,progress){
    videoFailures=0;
    const p=progress===null||progress===undefined?'':Number(progress);
    const out=$('video-output');
    out.className='output';
    out.innerHTML='<div><strong>State:</strong> <span class="mono">'+esc(state||'queued')+'</span>'+(p===''?'':' · <strong>'+esc(p)+'%</strong>')+'</div>';
    $('video-status').textContent=(state||'queued')+(p===''?'':' · '+p+'%');
  }
  function videoFail(message){
    clearTimeout(videoTimer);
    videoJob=null;sessionStorage.removeItem(VIDEO_JOB_KEY);
    $('video-generate').disabled=false;$('video-cancel').disabled=true;
    $('video-status').textContent='Failed';
    const out=$('video-output');out.className='output error';out.textContent=message;
    videoDone('error');
  }
  function videoCancelled(){
    clearTimeout(videoTimer);
    videoJob=null;sessionStorage.removeItem(VIDEO_JOB_KEY);
    $('video-generate').disabled=false;$('video-cancel').disabled=true;$('video-status').textContent='Cancelled';
    const out=$('video-output');out.className='output muted';out.textContent='Cancelled.';
    videoDone('cancelled');
  }
  async function videoShow(id){
    const r=await fetch('/v1/videos/'+encodeURIComponent(id)+'/content');
    if(!r.ok){const e=new Error('HTTP '+r.status+': '+(await r.text()).trim());e.status=r.status;throw e}
    const blob=await r.blob();
    const url=URL.createObjectURL(blob);
    const ext=(blob.type.split('/')[1]||'mp4').split(';')[0].replace('quicktime','mov').replace('x-matroska','mkv');
    const out=$('video-output');out.className='output';
    out.innerHTML='<video controls src="'+esc(url)+'" style="width:100%"></video><p><a href="'+esc(url)+'" download="video.'+esc(ext)+'">Download video</a></p>';
    clearTimeout(videoTimer);videoJob=null;sessionStorage.removeItem(VIDEO_JOB_KEY);
    $('video-generate').disabled=false;$('video-cancel').disabled=true;$('video-status').textContent='Completed';
    videoDone('ok',[url]);
  }
  async function videoCheck(){
    if(!videoJob)return;
    const id=videoJob,epoch=videoCheckEpoch;
    try{
      const d=await json('/v1/videos/'+encodeURIComponent(id));
      if(videoJob!==id||epoch!==videoCheckEpoch)return;
      if(d.status==='completed'){
        // Only the content download gives up after repeated errors; a status
        // poll that keeps failing never ends the job.
        try{await videoShow(id);return}catch(e){
          if(videoJob!==id||epoch!==videoCheckEpoch)return;
          if(++videoFailures>=10){videoFail(e.message);return}
          $('video-status').textContent='Retrying: '+e.message;
        }
      }else if(d.status==='cancelled'){videoCancelled();return}
      else if(d.status==='failed'){videoFail(videoError(d));return}
      else{
        const p=Number(d.progress);
        videoPaint(d.status||'queued',d.progress===undefined||d.progress===null||!Number.isFinite(p)?null:p);
      }
    }catch(e){
      if(videoJob!==id||epoch!==videoCheckEpoch)return;
      if(e.status===404){videoFail(e.message);return}
      // A slow, restarting or unreachable backend does not end the job: keep
      // polling until it reports a final state or the router forgets the id.
      $('video-status').textContent='Retrying: '+e.message;
    }
    videoDelay=Math.min(videoDelay*1.5,5000);
    clearTimeout(videoTimer);
    videoTimer=setTimeout(videoCheck,videoDelay);
  }
  window.sendVideo=async function(){
    if(videoJob)return;
    const out=$('video-output'),model=$('video-model').value,prompt=$('video-prompt').value.trim(),ref=$('video-reference').files[0];
    const fail=m=>{out.className='output error';out.textContent=m};
    if(!model)return fail('No video model is available. Add a catalog entry with api_type: video.');
    if(!prompt)return fail('Enter a prompt.');
    const body={model,prompt};
    if(!$('video-opt-duration').hidden)body.seconds=Number($('video-seconds').value)||4;
    if(!$('video-opt-resolution').hidden)body.size=$('video-resolution').value;
    if(!$('video-opt-seed').hidden){const seed=$('video-seed').value.trim();if(/^\d+$/.test(seed))body.seed=seed.length>15?seed:Number(seed)}
    out.className='output muted';out.textContent='Submitting…';
    videoHist=histStart('video');
    $('video-generate').disabled=true;$('video-cancel').disabled=true;$('video-status').textContent='Submitting…';
    try{
      let r;
      if(ref){
        const fd=new FormData();for(const k of Object.keys(body))fd.append(k,String(body[k]));fd.append('input_reference',ref);
        r=await fetch('/v1/videos',{method:'POST',body:fd});
      }else{
        r=await fetch('/v1/videos',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
      }
      const text=await r.text();let d;try{d=JSON.parse(text)}catch(e){d=null}
      if(!r.ok){const e=d&&d.error;throw new Error('HTTP '+r.status+': '+((e&&e.message)||e||text||r.statusText))}
      if(!d||!d.id)throw new Error('The response had no job id.');
      videoJob=d.id;videoDelay=1000;sessionStorage.setItem(VIDEO_JOB_KEY,d.id);
      $('video-cancel').disabled=false;
      videoPaint(d.status||'queued',d.progress===undefined?0:Number(d.progress)||0);
      videoTimer=setTimeout(videoCheck,videoDelay);
    }catch(e){
      $('video-generate').disabled=false;$('video-status').textContent='';
      fail(e.message);
      videoDone('error');
    }
  };
  window.cancelVideo=async function(){
    if(!videoJob)return;
    const id=videoJob;
    videoCheckEpoch++;
    clearTimeout(videoTimer);
    $('video-cancel').disabled=true;$('video-status').textContent='Cancelling…';
    try{
      await json('/v1/videos/'+encodeURIComponent(id),{method:'DELETE'});
    }catch(e){
      if(videoJob!==id)return;
      // The router ends a job only when DELETE succeeds, so keep it and poll on.
      $('video-cancel').disabled=false;$('video-status').textContent='Cancel failed';
      const out=$('video-output');out.className='output error';out.textContent='Cancel failed: '+e.message;
      videoDelay=Math.min(videoDelay*1.5,5000);
      clearTimeout(videoTimer);
      videoTimer=setTimeout(videoCheck,videoDelay);
      return;
    }
    if(videoJob!==id)return;
    videoCancelled();
  };
  function resumeVideo(){
    const id=sessionStorage.getItem(VIDEO_JOB_KEY);
    if(!id)return;
    videoJob=id;videoDelay=1000;
    // The form is back to its defaults after a reload, so a resumed job keeps no inputs to restore.
    videoHist=Object.assign(histStart('video'),{model:'—',note:'Resumed job '+id,inputs:{}});
    $('video-generate').disabled=true;$('video-cancel').disabled=false;
    const out=$('video-output');out.className='output muted';out.textContent='Resuming the job…';
    $('video-status').textContent='Resuming…';
    videoTimer=setTimeout(videoCheck,0);
  }

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
    const ctl=new AbortController(),h=histStart('load');
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
    histEnd(h,ctl.signal.aborted?'cancelled':'ok');
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
    const h=histStart('realtime','Text session');
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
      histEnd(h,opened&&ev.code===1000?'ok':'error');
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
  // Path suffix -> Activity filter group. The keys are generationPathSuffixes in activity.go plus
  // realtime (ui_test.go keeps them in step); the first suffix that matches wins.
  const ACT_PATH_GROUP={'/chat/completions':'chat','/txt2img':'sdapi','/img2img':'sdapi','/images/generations':'images','/images/edits':'images','/images/variations':'images','/embeddings':'embed','/embedding':'embed','/rerank':'embed','/reranking':'embed','/completions':'completions','/completion':'completions','/infill':'completions','/responses':'completions','/messages':'completions','/audio/speech':'audio','/audio/speech/batch':'audio','/audio/transcriptions':'audio','/audio/translations':'audio','/audio/generations':'audio','/videos':'video','/realtime':'realtime'};
  function pathGroup(p){p=String(p||'');const k=Object.keys(ACT_PATH_GROUP).find(s=>p.endsWith(s));return k?ACT_PATH_GROUP[k]:p.includes('/audio/voices')?'audio':'other'}
  // The filter lives in the URL hash (#paths=chat,sdapi) so a link keeps it; no hash means the default.
  const ACT_DEFAULT='chat,sdapi',actHashPaths=new URLSearchParams(location.hash.slice(1)).get('paths');
  const actGroupBtns=[...$('act-filter').querySelectorAll('[data-group]')];
  const actShow=new Set((actHashPaths??ACT_DEFAULT).split(',').filter(Boolean));
  function actFilterPaint(){
    actGroupBtns.forEach(b=>{const on=actShow.has(b.dataset.group);b.classList.toggle('active',on);b.setAttribute('aria-pressed',on)});
    const all=actGroupBtns.every(b=>actShow.has(b.dataset.group));
    $('act-all').classList.toggle('active',all);$('act-all').setAttribute('aria-pressed',all);
  }
  $('act-filter').addEventListener('click',ev=>{
    const b=ev.target.closest('button');
    if(!b)return;
    const g=b.dataset.group;
    if(!g)actGroupBtns.forEach(x=>actShow.add(x.dataset.group));
    else if(!actShow.delete(g))actShow.add(g);
    history.replaceState(null,'','#paths='+[...actShow].join(','));
    actFilterPaint();actRender();
  });
  // actPaint takes newest-first finished rows and, when given, the latest in-progress list.
  function actPaint(rows,live){
    actRows=rows;
    if(live)actProgress(live);
    actRender();
  }
  // actRender redraws the stat cards, sparklines and rows for the ticked path groups
  // and the selected peer filter, with the running rows above the finished ones.
  function actRender(){
    actPeerFill();
    let rows=actRows.filter(e=>actShow.has(pathGroup(e.path))).filter(actMatch),hid=actRows.length-rows.length;
    $('act-hidden').textContent=hid?hid+' hidden':'';
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
    const running=[...actLive.values()].filter(e=>actShow.has(pathGroup(e.path))).filter(actMatch).sort((a,b)=>Date.parse(b.started)-Date.parse(a.started));
    rows=running.concat(rows);
    acts=new Map(rows.map(e=>[actKey(e),e]));
    thumbs.forEach((_,id)=>{if(!acts.has(id))thumbs.delete(id)});
    // Rows only animate in once the first paint is done and the tab is showing.
    syncRows(rows,!actFirst&&$('activity').classList.contains('active'));
    actFirst=false;
  }
  // A row's key is its node and id: a running row and its finished row share it, so one DOM row turns
  // into the other. Local rows key on the id alone, which is also their capture id.
  const actKey=e=>(e.node?e.node+'/':'')+e.id;
  // A snapshot or poll holds local rows only; peer rows reach the page as done events, so keep them, merged by time.
  const actWithPeers=rows=>rows.concat(actRows.filter(e=>e.node)).sort((a,b)=>Date.parse(b.timestamp)-Date.parse(a.timestamp)).slice(0,ACT_MAX);
  // actProgress takes the latest in-progress list. A running row that leaves it without its finished row
  // (a peer gone stale) is marked lost and fades out ACT_LOST_MS later.
  // A peer that restarted numbers its requests from 1 again, so a running peer entry that started after
  // the finished row with its key is a new request: that old row is dropped and the entry shows as running.
  function actProgress(list){
    const old=new Map(actRows.filter(e=>e.node).map(e=>[actKey(e),e]));
    const reused=new Set(list.filter(e=>e.node&&old.has(actKey(e))&&Date.parse(e.started)>Date.parse(old.get(actKey(e)).timestamp)).map(actKey));
    if(reused.size)actRows=actRows.filter(e=>!reused.has(actKey(e)));
    const now=new Set(list.map(actKey)),fin=new Set(actRows.map(actKey));
    actLive.forEach((e,k)=>{
      if(now.has(k)||e.lost)return;
      if(fin.has(k)){actLive.delete(k);return}
      e.lost=true;
      setTimeout(()=>{if(actLive.get(k)===e){actLive.delete(k);actPaint(actRows)}},ACT_LOST_MS);
    });
    list.forEach(e=>{const k=actKey(e);if(!fin.has(k))actLive.set(k,Object.assign(e,{running:true}))});
  }
  async function activity(){
    try{
      const d=await json('/_router/activity');
      // A poll lists local running requests only: peer running rows need the live stream, so they are
      // left out here rather than marked lost while the peer still runs them.
      actLive.forEach((e,k)=>{if(e.node)actLive.delete(k)});
      actPaint(actWithPeers(d.entries||[]),d.in_progress||[]);
      $('activity-hint').textContent='Polling every 3 s · in-memory on this node · peer requests show live only';
    }catch(e){
      // Keep the rows already on screen; a failed poll should not wipe the table.
      $('activity-hint').textContent=e.message;
      if(!$('activity-rows').querySelector('tr[data-id]'))setNote(e.message);
    }
  }
  let actFirst=true,acts=new Map(),actRows=[],actLive=new Map();
  // The peer filter matches the server's origin field: Local is a client of this node, 'p:'+name a request that peer sent.
  function actMatch(e){const f=$('act-peer').value;return !f||e.origin===(f==='local'?f:f.slice(2))}
  // Peer options are the configured peers, every origin in the rows (a sender need not be configured) and the current pick, so it never drops.
  const actPeers=(rows,cur)=>[...new Set(Object.keys(status.peers||{}).concat(rows.map(e=>e.origin),cur.startsWith('p:')?[cur.slice(2)]:[]))].filter(p=>p&&p!=='local').sort();
  function actPeerFill(){const peers=actPeers(actRows.concat([...actLive.values()]),$('act-peer').value);fillSelect($('act-peer'),['','local'].concat(peers.map(p=>'p:'+p)),'<option value="">All</option><option value="local">Local</option>'+peers.map(p=>'<option value="p:'+esc(p)+'">'+esc(p)+'</option>').join(''))}
  $('act-peer').onchange=()=>actPaint(actRows);

  // The Activity view listens on /_router/activity/stream only while it is showing.
  // A proxy that holds the stream back sends no snapshot, so after ACT_STREAM_WAIT
  // (or on an error before the first snapshot) the view polls every 3 s instead.
  const ACT_STREAM_WAIT=5000,ACT_MAX=200,ACT_LOST_MS=60000; // ACT_MAX is the server's ring size.
  let actES=null,actWait=0,actPollTimer=0;
  function actStop(){
    if(actES)actES.close();
    actES=null;
    clearTimeout(actWait);clearInterval(actPollTimer);
  }
  function actPoll(){
    actStop();
    activity();
    actPollTimer=setInterval(activity,3000);
  }
  function actStart(){
    actStop();
    if(!window.EventSource)return actPoll();
    const es=actES=new EventSource('/_router/activity/stream');
    let live=false;
    actWait=setTimeout(actPoll,ACT_STREAM_WAIT);
    es.addEventListener('snapshot',ev=>{
      live=true;clearTimeout(actWait);
      const d=JSON.parse(ev.data);
      actLive.clear();
      actPaint(actWithPeers(d.entries||[]),d.in_progress||[]);
      $('activity-hint').textContent='Live · in-memory on this node';
    });
    // progress lists what runs here and on peers; done is a finished row that takes over its running row.
    es.addEventListener('progress',ev=>actPaint(actRows,JSON.parse(ev.data).in_progress||[]));
    es.addEventListener('done',ev=>{
      const e=JSON.parse(ev.data),k=actKey(e);
      actLive.delete(k);
      actPaint([e,...actRows.filter(r=>actKey(r)!==k)].slice(0,ACT_MAX));
    });
    // A dropped live stream reconnects by itself and resends the snapshot; poll only
    // when it never went live or the browser has given up.
    es.onerror=()=>{
      if(actES!==es)return;
      if(!live||es.readyState===EventSource.CLOSED)return actPoll();
      $('activity-hint').textContent='Reconnecting to the live stream…';
    };
  }
  // Running rows tick their elapsed time here, between updates from the server. Clamped at 0 for a clock running ahead.
  const elapsed=t=>fmtDur(Math.max(0,Date.now()-t));
  setInterval(()=>$('activity-rows').querySelectorAll('[data-started]').forEach(n=>{n.textContent=elapsed(n.dataset.started)}),1000);

  // Rows are matched by id and updated in place; see syncRows.
  const IMG_PATH=/\/(images\/(generations|edits|variations)|txt2img|img2img)$/;
  const ACT_NOTE='No generation requests yet. Send one from the Playground or any client.';
  function setNote(text){$('activity-rows').querySelectorAll('.act-note').forEach(n=>n.remove());$('activity-rows').insertAdjacentHTML('beforeend','<tr class="act-note"><td colspan="11" class="empty">'+esc(text)+'</td></tr>')}
  function actCells(e){
    const tok=e.tokens||{};
    const thumb=IMG_PATH.test(e.path||'')?'<span class="thumb" aria-hidden="true" data-id="'+esc(e.id)+'" data-state="'+(e.has_capture?'wait':'none')+'"'+(e.thumb_issue?' title="'+esc(e.thumb_issue)+'"':'')+'></span>':'';
    const io=e.media?mediaCells(e.media,e).map(esc):[fmtTok(tok.prompt_tokens)+(tok.cached_tokens?' <span class="subtle">('+fmtTok(tok.cached_tokens)+' cached)</span>':''),fmtTok(tok.completion_tokens)];
    return [
      ['mono',esc(fmtTime(e.timestamp))],
      ['','<div class="model-line">'+thumb+'<div><div class="model-id">'+esc(e.model||'—')+'</div><div class="subtle">'+esc(actWhere(e))+'</div></div></div>'],
      ['mono',esc(e.origin||'—')],
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
  // actWhere is the node line under the model: the peer that ran the row, or where this node sent it from or to.
  const actWhere=e=>e.node?'on '+e.node:e.from_peer?'← '+e.from_peer:e.target&&e.target!==status.node?'→ '+e.target:e.target||'';
  // liveCells fills a running row. The Duration cell's data-started lets the timer below actStart tick it.
  function liveCells(e){
    const t=Date.parse(e.started);
    return [
      ['mono',esc(fmtTime(e.started))],
      ['','<div class="model-line"><div><div class="model-id">'+esc(e.model||'—')+'</div><div class="subtle">'+esc(actWhere(e))+'</div></div></div>'],
      ['mono',esc(e.origin||'—')],
      ['mono',esc(e.method||'')+' '+esc(e.path||'')],
      e.lost?['status-bad','Lost']:['','<span class="spinner" aria-hidden="true"></span>Running'],
      ['mono','—'],
      ['mono',esc(fmtTok(e.tokens))],
      ['mono','—'],
      ['mono','—'],
      ['mono',esc(e.ttft_ms?fmtDur(e.ttft_ms):'—')],
      ['mono',e.lost?'—':'<span data-started="'+t+'">'+esc(elapsed(t))+'</span>']
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
    // A running row has no capture to open yet, so it leaves the tab order until it finishes.
    tr.classList.toggle('running',!!e.running);tr.classList.toggle('lost',!!e.lost);tr.tabIndex=e.running?-1:0;
    (e.running?liveCells(e):actCells(e)).forEach((c,i)=>{
      const td=tr.cells[i];
      if(td.className!==c[0])td.className=c[0];
      if(tr._c[i]!==c[1]){tr._c[i]=c[1];td.innerHTML=c[1]}
    });
    tr.querySelectorAll('.thumb[data-state=wait]').forEach(hydrateThumb);
  }
  const liveRow=n=>{while(n&&n.classList.contains('leaving'))n=n.nextElementSibling;return n};
  function syncRows(entries,animate){
    const tb=$('activity-rows'),have=new Map(),keep=new Set(entries.map(actKey));
    tb.querySelectorAll('.act-note').forEach(n=>n.remove());
    [...tb.children].forEach(tr=>{if(tr.dataset.id&&!tr.classList.contains('leaving'))have.set(tr.dataset.id,tr)});
    have.forEach((tr,id)=>{
      if(keep.has(id))return;
      tr.classList.add('leaving');tr.removeAttribute('data-id');tr.removeAttribute('tabindex');
      setTimeout(()=>tr.remove(),240);
    });
    let ref=liveRow(tb.firstElementChild);
    entries.forEach(e=>{
      const id=actKey(e);
      let tr=have.get(id);
      if(!tr){
        tr=document.createElement('tr');tr.tabIndex=0;tr.dataset.id=id;tr._c=[];
        for(let i=0;i<11;i++)tr.appendChild(document.createElement('td'));
        if(animate)tr.classList.add('row-in');
      }
      paintRow(tr,e);
      if(tr===ref)ref=liveRow(ref.nextElementSibling);else tb.insertBefore(tr,ref);
    });
    if(!entries.length)setNote(actRows.length?'No requests match the selected path groups and peer filter. Select another group, All, or another peer.':ACT_NOTE);
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
    return json('/_router/activity/'+id).then(c=>resultImgs(parseJSON(c.resp_body),id)[0]||'').catch(()=>{thumbs.delete(id);return ''});
  }

  const parseJSON=s=>{try{return JSON.parse(s)}catch(e){return null}};
  const CLIPPED_IMG=/^\[image (\d+): /;
  // imgType sniffs PNG, JPEG or WEBP from the first decoded bytes of bare base64; '' when it is none.
  function imgType(v){
    let h='';
    try{h=atob(v.slice(0,16))}catch(e){}
    return h.startsWith('\x89PNG')?'png':h.startsWith('\xff\xd8\xff')?'jpeg':h.startsWith('RIFF')&&h.slice(8,12)==='WEBP'?'webp':'';
  }
  // capImgs collects the images in a parsed body in document order. A clipped capture holds
  // "[image 0: png 1.3 MB]" in place of the bytes, which the server keeps as a preview; a data URL,
  // a url field or inline base64 that survived clipping is shown as it is.
  function capImgs(v,id,side,key,out=[]){
    if(typeof v==='string'){
      const m=CLIPPED_IMG.exec(v);
      if(m)out.push('/_router/activity/'+id+'/image/'+side+'/'+m[1]);
      else if(/^data:image\//i.test(v)||key==='url'&&/^https?:/i.test(v))out.push(v);
      else if(imgType(v))out.push('data:image/'+imgType(v)+';base64,'+v);
    }else if(v&&typeof v==='object')Object.entries(v).forEach(([k,x])=>capImgs(x,id,side,Array.isArray(v)?key:k,out));
    return out;
  }
  // resultImgs reads an OpenAI-style (data[]) or sd.cpp-style (images[]) response; other fields may echo the inputs.
  const resultImgs=(d,id)=>capImgs(d&&(d.data||d.images),id,'resp');
  const capImg=(s,alt)=>'<a class="cap-open" href="'+esc(s)+'" target="_blank" rel="noopener" title="Open full size in a new tab"><img class="cap-img" alt="'+alt+'" src="'+esc(s)+'"></a>';
  // chatReply reads a chat completion, whole or streamed as SSE. It returns null when the body is neither.
  function chatReply(body){
    const out={text:'',think:'',finish:''};
    let last='text';
    const take=d=>{
      const c=d&&Array.isArray(d.choices)&&d.choices[0];
      if(!c)return false;
      const m=c.message||c.delta||{};
      if(typeof m.content==='string'){out.text+=m.content;if(m.content)last='text'}
      if(typeof m.reasoning_content==='string'){out.think+=m.reasoning_content;if(m.reasoning_content)last='think'}
      if(c.finish_reason)out.finish=c.finish_reason;
      return true;
    };
    const s=String(body||'');
    const whole=parseJSON(s);
    if(whole)return take(whole)?out:null;
    let ok=false;
    // A clipped stream keeps its first and last events; mark the gap where the middle was.
    s.split('\n').forEach(l=>{if(l.startsWith('data:')){const d=parseJSON(l.slice(5).trim());if(d&&take(d))ok=true}else if(l.startsWith(': [clipped '))out[last]+='\n'+l.slice(2)+'\n'});
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
      const resp=parseJSON(cap.resp_body),imgs=resultImgs(resp,e.id),r=req||{};
      ok=!!(req||imgs.length);
      const size=r.size||(r.width&&r.height?r.width+'×'+r.height:'—');
      html+=capBlock('Image request','<dl class="cap-summary">'+capStat('Prompt',r.prompt||'—')+capStat('Size',size)+capStat('Count',r.n||imgs.length||'—')+'</dl>'+capImgs(req,e.id,'req').map(s=>capImg(s,'Input image')).join(''));
      html+=capBlock('Result',imgs.length?imgs.map(s=>capImg(s,'Generated image')).join(''):'<p class="subtle">No image in the response body. It may be cut or not JSON; the raw view has the bytes.</p>');
    }else if(req&&Array.isArray(req.messages)){
      const reply=chatReply(cap.resp_body);
      ok=true;
      const stop=req.stop==null?'—':Array.isArray(req.stop)?req.stop.join(', '):String(req.stop);
      html+=capBlock('Parameters','<dl class="cap-summary">'+capStat('Temperature',req.temperature??'—')+capStat('Max tokens',req.max_tokens??req.max_completion_tokens??'—')+capStat('Stream',req.stream??'—')+capStat('Stop',stop)+'</dl>');
      html+=capBlock('Messages',req.messages.map(m=>'<div class="cap-msg"><div class="cap-role">'+esc((m&&m.role)||'?')+'</div>'+capText(msgText(m&&m.content))+capImgs(m&&m.content,e.id,'req').map(s=>capImg(s,'Message image')).join('')+'</div>').join('')||'<p class="subtle">No messages.</p>');
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
  // Browsers will not open a data URL in a new tab, so an inline image opens as a blob URL.
  $('capture-parsed').addEventListener('click',ev=>{
    const a=ev.target.closest('a.cap-open[href^="data:"]');
    if(a)a.href=URL.createObjectURL(new Blob([Uint8Array.from(atob(a.href.split(',')[1]),c=>c.charCodeAt(0))],{type:a.href.slice(5,a.href.indexOf(';'))}));
  });
  // A preview the ring has since shed for room 404s. Response image 0 falls back once to the row's
  // thumbnail, which outlives previews; otherwise say so instead of showing a broken image.
  $('capture-parsed').addEventListener('error',ev=>{
    const im=ev.target,a=im.closest&&im.closest('a.cap-open');
    if(!a)return;
    const thumb=im.src.replace(/\/image\/resp\/0$/,'/thumb');
    if(thumb!==im.src){a.href=im.src=thumb;return}
    a.replaceWith(Object.assign(document.createElement('p'),{className:'subtle',textContent:'Image preview no longer stored; the raw view has the capture.'}));
  },true);
  async function openCapture(id, row){
    const dlg=$('activity-dialog'),e=acts.get(String(id))||{};
    if(e.running)return;
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
        // A peer row is the summary from the peer's telemetry; its capture stays on the peer.
        const peerUI=e.node&&/^https?:\/\//i.test(e.peer_url||'')?'<p><a href="'+esc(e.peer_url.replace(/\/+$/,'')+'/ui/')+'" target="_blank" rel="noopener">Open the UI on '+esc(e.node)+'</a></p>':'';
        $('capture-meta').textContent=e.node?'Summary from '+e.node+'. The capture stays on that peer.':'No captured body.';
        $('capture-parsed').innerHTML=capParsed(e,{req_body:'',resp_body:''}).html+peerUI;
        setCapView('parsed');
        return;
      }
      const cap=await json('/_router/activity/'+id);
      const p=capParsed(e,cap);
      const notes=(cap.notes||[]).length?'Pipeline: '+cap.notes.join('; ')+'. ':'';
      const clipped=(cap.clipped_fields||[]).length?'Clipped to fit 256 KB: '+cap.clipped_fields.join(', ')+'. ':'';
      $('capture-meta').textContent=notes+clipped+(cap.truncated?'Bodies cut at 256 KB as raw bytes. ':'')+(p.ok?'':'Not a recognised chat or image body, showing the raw view. ');
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

  actFilterPaint();
  // A link carrying an Activity filter opens on the Activity view.
  if(actHashPaths!==null)document.querySelector('.nav [data-view=activity]').click();
  resumeVideo();refresh();setInterval(refresh,3000);
})();
