(function(){
  let status={models:[],telemetry:{},peers:{}};
  const $=id=>document.getElementById(id);
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon=s=>{const paths={model:'<path d="M6 4.5 12 2l6 2.5v6L12 13l-6-2.5v-6Z"/><path d="m6 10.5 6 2.5 6-2.5M12 13v5"/>',peer:'<circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="m8.2 10.8 7.5-3.6M8.2 13.2l7.5 3.6"/>'};return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">'+(paths[s]||paths.model)+'</svg>'};
  const pageCopy={dashboard:['Fleet node','Overview','Live capacity and model activity for this router.'],playground:['Operator tools','Playground','Try a streaming chat, an image, an embeddings, a rerank, a speech, a transcription request or a load test against this router.'],activity:['Observability','Activity','Generation requests on this node, with token stats and captured bodies.'],logs:['Observability','Logs','Live router, upstream and mesh output.']};
  document.querySelectorAll('.nav button').forEach(b=>b.onclick=()=>{const view=b.dataset.view;document.querySelectorAll('.nav button').forEach(x=>{x.classList.remove('active');x.removeAttribute('aria-current')});const wasLogs=$('logs').classList.contains('active');document.querySelectorAll('.view').forEach(x=>x.classList.remove('active'));b.classList.add('active');b.setAttribute('aria-current','page');$(view).classList.add('active');$('eyebrow').textContent=pageCopy[view][0];$('page-title').textContent=pageCopy[view][1];$('page-subtitle').textContent=pageCopy[view][2];if(view!=='logs')logStop();else if(!wasLogs)logStart()});
  async function json(url,opts){const r=await fetch(url,opts);const text=await r.text();let data;try{data=JSON.parse(text)}catch(e){data={error:text}}if(!r.ok)throw new Error(data.error||text||('HTTP '+r.status));return data}
  function setHealth(ok,message){$('health').textContent=message;$('side-health').textContent=ok?'Status updates every 3 seconds':'Unable to reach status endpoint';$('health-dot').className='status-dot '+(ok?'':'off');$('side-dot').className='status-dot '+(ok?'':'off')}
  window.refresh=async function(){try{status=await json('/_router/status');const node=status.node||'unknown';const now=new Date();$('side-node').textContent=node;$('updated').textContent='Updated '+now.toLocaleTimeString();setHealth(true,'Router healthy');renderSummary();renderGPUs();renderModels();renderPeers();fillSelectors()}catch(e){setHealth(false,e.message);$('updated').textContent='Snapshot unavailable'}};
  function renderSummary(){const gs=status.telemetry&&status.telemetry.gpus||[];const models=status.models||[];const running=models.filter(m=>m.state==='running').length;const peers=status.peers||{};$('stat-gpus').textContent=gs.length;$('stat-running').textContent=running;$('stat-models').textContent=models.length;$('stat-peers').textContent=Object.keys(peers).length;$('model-summary').textContent=models.length+' entries · '+running+' online'}
  function renderGPUs(){const gs=status.telemetry&&status.telemetry.gpus||[];const lm=status.telemetry&&status.telemetry.loaded_models||[];$('gpu-cards').innerHTML=gs.map(g=>{const on=lm.filter(m=>m.gpu===g.index).map(m=>(m.instance||m.id)+(m.slots?' '+m.in_flight+'/'+m.slots:''));const total=Number(g.total_mb)||0,free=Number(g.free_mb)||0,stale=Number(g.free_if_stale_evicted_mb)||free,idle=Number(g.free_if_idle_evicted_mb)||stale;const used=total?Math.max(0,Math.min(100,((total-free)/total)*100)):0;return '<article class="panel gpu-card"><div class="gpu-title"><span class="gpu-name">'+esc(g.name||'GPU '+g.index)+'</span><span class="gpu-index">GPU '+esc(g.index)+'</span></div><div class="gpu-memory"><span class="gpu-free">'+esc(free)+'</span><span class="gpu-unit">MB free</span></div><div class="meter" role="progressbar" aria-label="GPU memory used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="'+Math.round(used)+'"><div class="meter-fill" style="width:'+used+'%"></div></div><div class="gpu-footer"><span>'+esc(total)+' MB total</span><span>'+esc(idle)+' MB if idle evicted</span></div><div class="gpu-footer"><span class="mono">'+esc(on.join(', ')||'No models')+'</span></div></article>'}).join('')||'<div class="panel empty">No GPU telemetry available yet.</div>'}
  function renderModels(){const rows=(status.models||[]).map(m=>{const origin=m.origin||'local';const freshness=m.freshness||'';const state=freshness||m.state||(origin==='remote'?'available':'stopped');const can=origin==='local';return '<tr><td><div class="model-cell"><span class="model-glyph">'+icon(origin==='remote'?'peer':'model')+'</span><div><span class="model-id">'+esc(m.id)+'</span>'+(m.name?'<span class="model-name">'+esc(m.name)+'</span>':'')+'</div></div></td><td><span class="state '+esc(origin)+'">'+esc(origin==='remote'?'remote':'local')+'</span></td><td><span class="state '+esc(state)+'">'+esc(state)+'</span></td><td class="mono">'+(m.vram_mb?esc(m.vram_mb)+' MB':'—')+'</td><td>'+(can?'<div class="actions"><a class="ghost" href="/upstream/'+encodeURIComponent(m.id)+'/" target="_blank" rel="noopener">Open UI</a><button data-act="load" data-id="'+esc(m.id)+'">Load</button><button class="danger" data-act="unload" data-id="'+esc(m.id)+'">Unload</button></div>':'<span class="subtle">Mesh routed</span>')+'</td></tr>'}).join('');$('models').innerHTML=rows||'<tr><td colspan="5" class="empty">No models in the catalog.</td></tr>'}
  function renderPeers(){const ps=status.peers||{};const cards=Object.keys(ps).sort().map(n=>{const p=ps[n]||{};const gs=p.gpus||[];const loaded=(p.loaded_models||[]).map(m=>typeof m==='string'?m:((m.instance||m.id||'')+(m.gpu>=0?' @gpu'+m.gpu:'')+(m.freshness?' '+m.freshness:'')+(m.slots?(' '+m.in_flight+'/'+m.slots):'')));return '<article class="panel peer-card"><div class="peer-head"><div><div class="peer-name">'+esc(n)+'</div><div class="subtle mono">'+(p.timestamp?'Last seen '+new Date(p.timestamp*1000).toLocaleTimeString():'No timestamp')+'</div></div><span class="peer-state">'+(gs.length?'Fresh telemetry':'No telemetry')+'</span></div><div class="peer-detail"><div><div class="peer-label">GPU memory</div><div class="peer-value">'+esc(gs.map(g=>g.free_mb+' MB free'+(g.free_if_stale_evicted_mb&&g.free_if_stale_evicted_mb!==g.free_mb?' / '+g.free_if_stale_evicted_mb+' if stale evicted':'')).join(' · ')||'—')+'</div></div><div><div class="peer-label">Loaded models</div><div class="peer-value">'+esc(loaded.join(', ')||'None reported')+'</div></div></div></article>'}).join('');$('peers').innerHTML=cards||'<div class="panel empty">No fresh peer telemetry.</div>'}
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
  window.generateImage=async function(){const out=$('image-output');out.className='image-result';out.textContent='Generating…';const body={prompt:$('image-prompt').value,negative_prompt:$('image-negative').value,width:Number($('image-width').value),height:Number($('image-height').value),steps:Number($('image-steps').value),seed:Number($('image-seed').value)};if($('image-model').value)body.model=$('image-model').value;try{const d=await json('/sdapi/v1/txt2img',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});const imgs=d.images||[];out.innerHTML=imgs.length?imgs.map(x=>'<img alt="Generated image" src="data:image/png;base64,'+x+'">').join(''):'<span class="muted">No images returned.</span>'}catch(e){out.className='image-result error';out.textContent=e.message}};

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
  function spark(el, values, color){
    const w=320,h=96,pad=8;
    if(!values.length){
      el.innerHTML='<text x="160" y="52" text-anchor="middle" fill="#5d6c7f" font-size="11">No samples yet</text>';
      return;
    }
    const min=Math.min(...values), max=Math.max(...values);
    const span=max-min||1;
    const pts=values.map((v,i)=>{
      const x=pad+(i/(Math.max(values.length-1,1)))*(w-pad*2);
      const y=h-pad-((v-min)/span)*(h-pad*2);
      return x.toFixed(1)+','+y.toFixed(1);
    }).join(' ');
    const last=values[values.length-1];
    el.innerHTML='<polyline fill="none" stroke="'+color+'" stroke-width="2" points="'+pts+'"/><circle cx="'+pts.split(' ').pop().split(',')[0]+'" cy="'+pts.split(' ').pop().split(',')[1]+'" r="3" fill="'+color+'"/><text x="'+(w-pad)+'" y="14" text-anchor="end" fill="'+color+'" font-size="11">'+last.toFixed(last>=100?0:1)+'</text>';
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
      $('activity-rows').innerHTML=rows.map(e=>{
        const tok=e.tokens||{};
        const bad=e.status>=400;
        return '<tr tabindex="0" data-id="'+e.id+'"'+(e.has_capture?'':' data-empty="1"')+'><td class="mono">'+esc(fmtTime(e.timestamp))+'</td><td><div class="model-id">'+esc(e.model||'—')+'</div><div class="subtle">'+esc(e.target||'')+'</div></td><td class="mono">'+esc(e.method||'')+' '+esc(e.path||'')+'</td><td class="'+(bad?'status-bad':'status-ok')+'">'+esc(e.status)+'</td><td class="mono">'+fmtTok(tok.prompt_tokens)+(tok.cached_tokens?' <span class="subtle">('+fmtTok(tok.cached_tokens)+' cached)</span>':'')+'</td><td class="mono">'+fmtTok(tok.completion_tokens)+'</td><td class="mono">'+esc(fmtSpeed(tok.prompt_per_second))+'</td><td class="mono">'+esc(fmtSpeed(tok.tokens_per_second))+'</td><td class="mono">'+esc(e.ttft_ms?fmtDur(e.ttft_ms):'—')+'</td><td class="mono">'+esc(fmtDur(e.duration_ms))+'</td></tr>';
      }).join('')||'<tr><td colspan="10" class="empty">No generation requests yet. Send one from the Playground or any client.</td></tr>';
    }catch(e){
      $('activity-rows').innerHTML='<tr><td colspan="10" class="empty">'+esc(e.message)+'</td></tr>';
    }
  }
  async function openCapture(id, row){
    const dlg=$('activity-dialog');
    $('capture-title').textContent='Request '+id;
    $('capture-meta').textContent='Loading capture…';
    $('capture-req').textContent='';
    $('capture-resp').textContent='';
    if(row)row.setAttribute('aria-busy','true');
    dlg.showModal();
    try{
      const cap=await json('/_router/activity/'+id);
      $('capture-meta').textContent=(cap.truncated?'Bodies truncated to 256 KB. ':'')+'Click Close or press Escape.';
      $('capture-req').textContent=prettyHeaders(cap.req_headers)+'\n\n'+prettyBody(cap.req_body);
      $('capture-resp').textContent=prettyHeaders(cap.resp_headers)+'\n\n'+prettyBody(cap.resp_body);
    }catch(e){
      $('capture-meta').textContent='Capture unavailable';
      $('capture-req').textContent=e.message;
      $('capture-resp').textContent='';
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
  async function logStart(){
    logStop();
    logLines=[];logPending=[];logOut.innerHTML='';
    const ac=logAbort=new AbortController();
    logStatus('Connecting…');
    try{
      const url='/logs/stream'+(logSrc?'/'+logSrc.split('/').map(encodeURIComponent).join('/'):'');
      const r=await fetch(url,{signal:ac.signal});
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
