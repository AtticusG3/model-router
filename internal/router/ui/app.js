(function(){
  let status={models:[],telemetry:{},peers:{}};
  const $=id=>document.getElementById(id);
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon=s=>{const paths={model:'<path d="M6 4.5 12 2l6 2.5v6L12 13l-6-2.5v-6Z"/><path d="m6 10.5 6 2.5 6-2.5M12 13v5"/>',peer:'<circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="m8.2 10.8 7.5-3.6M8.2 13.2l7.5 3.6"/>'};return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">'+(paths[s]||paths.model)+'</svg>'};
  const pageCopy={dashboard:['Fleet node','Overview','Live capacity and model activity for this router.'],chat:['Operator tools','Chat lab','Test a streaming completion against any available route.'],image:['Operator tools','Image lab','Send an image request through the configured diffusion backend.'],activity:['Observability','Activity','Generation requests on this node, with token stats and captured bodies.'],logs:['Observability','Logs & metrics','Router gauges, backend /metrics, and event rings.']};
  document.querySelectorAll('.nav button').forEach(b=>b.onclick=()=>{const view=b.dataset.view;document.querySelectorAll('.nav button').forEach(x=>{x.classList.remove('active');x.removeAttribute('aria-current')});document.querySelectorAll('.view').forEach(x=>x.classList.remove('active'));b.classList.add('active');b.setAttribute('aria-current','page');$(view).classList.add('active');$('eyebrow').textContent=pageCopy[view][0];$('page-title').textContent=pageCopy[view][1];$('page-subtitle').textContent=pageCopy[view][2]});
  async function json(url,opts){const r=await fetch(url,opts);const text=await r.text();let data;try{data=JSON.parse(text)}catch(e){data={error:text}}if(!r.ok)throw new Error(data.error||text||('HTTP '+r.status));return data}
  function setHealth(ok,message){$('health').textContent=message;$('side-health').textContent=ok?'Status updates every 3 seconds':'Unable to reach status endpoint';$('health-dot').className='status-dot '+(ok?'':'off');$('side-dot').className='status-dot '+(ok?'':'off')}
  window.refresh=async function(){try{status=await json('/_router/status');const node=status.node||'unknown';const now=new Date();$('side-node').textContent=node;$('updated').textContent='Updated '+now.toLocaleTimeString();setHealth(true,'Router healthy');renderSummary();renderGPUs();renderModels();renderPeers();fillSelectors()}catch(e){setHealth(false,e.message);$('updated').textContent='Snapshot unavailable'}};
  function renderSummary(){const gs=status.telemetry&&status.telemetry.gpus||[];const models=status.models||[];const running=models.filter(m=>m.state==='running').length;const peers=status.peers||{};$('stat-gpus').textContent=gs.length;$('stat-running').textContent=running;$('stat-models').textContent=models.length;$('stat-peers').textContent=Object.keys(peers).length;$('model-summary').textContent=models.length+' entries · '+running+' online'}
  function renderGPUs(){const gs=status.telemetry&&status.telemetry.gpus||[];const lm=status.telemetry&&status.telemetry.loaded_models||[];$('gpu-cards').innerHTML=gs.map(g=>{const on=lm.filter(m=>m.gpu===g.index).map(m=>(m.instance||m.id)+(m.slots?' '+m.in_flight+'/'+m.slots:''));const total=Number(g.total_mb)||0,free=Number(g.free_mb)||0,stale=Number(g.free_if_stale_evicted_mb)||free,idle=Number(g.free_if_idle_evicted_mb)||stale;const used=total?Math.max(0,Math.min(100,((total-free)/total)*100)):0;return '<article class="panel gpu-card"><div class="gpu-title"><span class="gpu-name">'+esc(g.name||'GPU '+g.index)+'</span><span class="gpu-index">GPU '+esc(g.index)+'</span></div><div class="gpu-memory"><span class="gpu-free">'+esc(free)+'</span><span class="gpu-unit">MB free</span></div><div class="meter" role="progressbar" aria-label="GPU memory used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="'+Math.round(used)+'"><div class="meter-fill" style="width:'+used+'%"></div></div><div class="gpu-footer"><span>'+esc(total)+' MB total</span><span>'+esc(idle)+' MB if idle evicted</span></div><div class="gpu-footer"><span class="mono">'+esc(on.join(', ')||'No models')+'</span></div></article>'}).join('')||'<div class="panel empty">No GPU telemetry available yet.</div>'}
  function renderModels(){const rows=(status.models||[]).map(m=>{const origin=m.origin||'local';const freshness=m.freshness||'';const state=freshness||m.state||(origin==='remote'?'available':'stopped');const can=origin==='local';return '<tr><td><div class="model-cell"><span class="model-glyph">'+icon(origin==='remote'?'peer':'model')+'</span><div><span class="model-id">'+esc(m.id)+'</span>'+(m.name?'<span class="model-name">'+esc(m.name)+'</span>':'')+'</div></div></td><td><span class="state '+esc(origin)+'">'+esc(origin==='remote'?'remote':'local')+'</span></td><td><span class="state '+esc(state)+'">'+esc(state)+'</span></td><td class="mono">'+(m.vram_mb?esc(m.vram_mb)+' MB':'—')+'</td><td>'+(can?'<div class="actions"><a class="ghost" href="/upstream/'+encodeURIComponent(m.id)+'/" target="_blank" rel="noopener">Open UI</a><button data-act="load" data-id="'+esc(m.id)+'">Load</button><button class="danger" data-act="unload" data-id="'+esc(m.id)+'">Unload</button></div>':'<span class="subtle">Mesh routed</span>')+'</td></tr>'}).join('');$('models').innerHTML=rows||'<tr><td colspan="5" class="empty">No models in the catalog.</td></tr>'}
  function renderPeers(){const ps=status.peers||{};const cards=Object.keys(ps).sort().map(n=>{const p=ps[n]||{};const gs=p.gpus||[];const loaded=(p.loaded_models||[]).map(m=>typeof m==='string'?m:((m.instance||m.id||'')+(m.gpu>=0?' @gpu'+m.gpu:'')+(m.freshness?' '+m.freshness:'')+(m.slots?(' '+m.in_flight+'/'+m.slots):'')));return '<article class="panel peer-card"><div class="peer-head"><div><div class="peer-name">'+esc(n)+'</div><div class="subtle mono">'+(p.timestamp?'Last seen '+new Date(p.timestamp*1000).toLocaleTimeString():'No timestamp')+'</div></div><span class="peer-state">'+(gs.length?'Fresh telemetry':'No telemetry')+'</span></div><div class="peer-detail"><div><div class="peer-label">GPU memory</div><div class="peer-value">'+esc(gs.map(g=>g.free_mb+' MB free'+(g.free_if_stale_evicted_mb&&g.free_if_stale_evicted_mb!==g.free_mb?' / '+g.free_if_stale_evicted_mb+' if stale evicted':'')).join(' · ')||'—')+'</div></div><div><div class="peer-label">Loaded models</div><div class="peer-value">'+esc(loaded.join(', ')||'None reported')+'</div></div></div></article>'}).join('');$('peers').innerHTML=cards||'<div class="panel empty">No fresh peer telemetry.</div>'}
  function sameOptions(sel,ids){return sel.options.length===ids.length&&ids.every((id,i)=>sel.options[i].value===id)}
  function fillSelect(sel,ids,html){const prev=sel.value;if(sameOptions(sel,ids))return;sel.innerHTML=html;if([...sel.options].some(o=>o.value===prev))sel.value=prev}
  function fillSelectors(){const models=(status.models||[]).filter(m=>m.api_type!=='image');const ids=models.map(m=>m.id);fillSelect($('chat-model'),ids.length?ids:[''],ids.length?models.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+(m.origin==='remote'?' (remote)':'')+'</option>').join(''):'<option value="">No models available</option>');const imgs=(status.models||[]).filter(m=>m.api_type==='image');fillSelect($('image-model'),[''].concat(imgs.map(m=>m.id)),'<option value="">Path default</option>'+imgs.map(m=>'<option value="'+esc(m.id)+'">'+esc(m.id)+'</option>').join(''))}
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
      renderLogStats(rows);
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
      }).join('')||'<tr><td colspan="10" class="empty">No generation requests yet. Send one from Chat lab or any client.</td></tr>';
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

  // ---- Logs & metrics ----
  const LEVEL_CLASS={error:'lvl-error',warn:'lvl-warn',debug:'lvl-debug',mesh:'lvl-mesh',upstream:'lvl-upstream',info:''};
  let logData={entries:[],upstream:[],mesh:[],backend_metrics:[]};
  let logLevel='all';
  let logQuery='';
  let logPause=false;
  const setText=(id,v)=>{const el=$(id);if(el)el.textContent=v};

  // renderLogStats fills the four cards above the log panels from the same
  // activity ring the Activity page uses. Cheap, and gives an at-a-glance
  // answer before reading a single line.
  function renderLogStats(rows){
    const reqs=rows.length;
    const errs=rows.filter(e=>Number(e.status)>=400).length;
    const durs=rows.map(e=>Number(e.duration_ms)||0).filter(v=>v>0).sort((a,b)=>a-b);
    const gens=rows.map(e=>Number(e.tokens&&e.tokens.tokens_per_second)||0).filter(v=>v>0);
    const p95=durs.length?durs[Math.min(durs.length-1,Math.ceil(durs.length*0.95)-1)]:0;
    setText('log-req-count',reqs||'0');
    setText('log-err-rate',reqs?((errs/reqs)*100).toFixed(errs&&(errs/reqs)<0.01?2:1)+'%':'—');
    setText('log-p95',p95?fmtDur(p95):'—');
    const tps=gens.length?gens.reduce((a,b)=>a+b,0)/gens.length:0;
    const tpsEl=$('log-tps');if(tpsEl)tpsEl.innerHTML=(tps?tps.toFixed(1):'—')+'<small> tok/s</small>';
    const errEl=$('log-err-rate');if(errEl)errEl.className='stat-value'+(reqs&&errs/reqs>=0.05?' error':'');
  }

  // splitTs peels the HH:MM:SS.mmm prefix the logger writes into the ring.
  function splitTs(line){
    const m=/^(\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)\s+([\s\S]*)$/.exec(String(line||''));
    return m?{ts:m[1],msg:m[2]}:{ts:'',msg:String(line||'')};
  }

  // classify maps a ring line to a severity for colouring. The logger encodes
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

  function matchLevel(lvl){
    if(logLevel==='all')return true;
    if(logLevel==='error')return lvl==='error';
    if(logLevel==='warn')return lvl==='warn';
    if(logLevel==='debug')return lvl==='debug';
    return true;
  }

  // highlight escapes first, then wraps search hits so the markup cannot be
  // injected through a crafted log line.
  function highlight(s){
    const safe=esc(s);
    if(!logQuery)return safe;
    try{
      const q=logQuery.replace(/[.*+?^${}()|[\]\\]/g,'\\$&');
      return safe.replace(new RegExp(q,'gi'),m=>'<mark>'+m+'</mark>');
    }catch(e){return safe}
  }

  function renderRing(id,lines,emptyText){
    const el=$(id);if(!el)return 0;
    const out=[];
    for(const line of lines){
      const {ts,msg}=splitTs(line);
      const lvl=classify(msg);
      if(!matchLevel(lvl))continue;
      if(logQuery&&msg.toLowerCase().indexOf(logQuery)<0)continue;
      out.push('<div class="log-line '+LEVEL_CLASS[lvl]+'"><span class="log-ts">'+esc(ts)+'</span><span class="log-msg">'+highlight(msg)+'</span></div>');
    }
    const nearBottom=el.scrollTop+el.clientHeight>=el.scrollHeight-8;
    el.innerHTML=out.length?out.join(''):'<div class="log-empty">'+esc(emptyText)+'</div>';
    if(nearBottom)el.scrollTop=el.scrollHeight;
    return out.length;
  }

  function setBadge(id,shown,total){
    const el=$(id);if(!el)return;
    el.textContent=shown===total?total+' lines':shown+'/'+total+' lines';
    el.classList.toggle('hot',total>0&&shown===0);
  }

  // renderMetricsText formats Prometheus exposition for reading: HELP/TYPE
  // dimmed, metric name cyan, labels muted, value emphasised. A zero on a
  // "healthy means 1" series is flagged so a down peer or backend stands out.
  function renderMetricsText(text){
    const out=[];
    for(const raw of String(text||'').split('\n')){
      const line=raw.replace(/\s+$/,'');
      if(!line)continue;
      if(line.startsWith('# TYPE ')){out.push('<span class="m-type">'+esc(line)+'</span>');continue}
      if(line.startsWith('#')){out.push('<span class="m-help">'+esc(line)+'</span>');continue}
      const sp=line.lastIndexOf(' ');
      let name=line,val='';
      if(sp>0){name=line.slice(0,sp);val=line.slice(sp+1)}
      let base=name,labels='';
      const br=name.indexOf('{');
      if(br>0){base=name.slice(0,br);labels=name.slice(br)}
      // Only genuine faults go red. A stopped model is normal — the router
      // spawns on demand — so flagging every idle stanza would cry wolf.
      const hot=val==='0'&&/^(model_router_peer_fresh|model_router_up)$/.test(base);
      out.push('<span class="m-line'+(hot?' hot':'')+'"><span class="m-name">'+esc(base)+'</span><span class="m-lbl">'+esc(labels)+'</span> <span class="m-val">'+esc(val)+'</span></span>');
    }
    return out.join('');
  }

  function summarizeMetrics(text){
    const t=String(text||'');
    const count=re=>(t.match(re)||[]).length;
    const running=count(/^model_router_model_running\{[^}]*\} 1$/gm);
    const declared=count(/^model_router_model_running\{[^}]*\} \d+$/gm);
    const fresh=count(/^model_router_peer_fresh\{[^}]*\} 1$/gm);
    const peerTotal=count(/^model_router_peer_fresh\{[^}]*\} \d+$/gm);
    let free=0,total=0;
    t.replace(/^model_router_gpu_free_mb\{[^}]*\} (\d+)$/gm,(_,v)=>{free+=Number(v);return''});
    t.replace(/^model_router_gpu_total_mb\{[^}]*\} (\d+)$/gm,(_,v)=>{total+=Number(v);return''});
    let reqTotal=0;
    t.replace(/^model_router_requests_total\{[^}]*\} (\d+)$/gm,(_,v)=>{reqTotal+=Number(v);return''});
    const inflight=/^model_router_inflight_requests\{[^}]*\} (\d+)$/m.exec(t);
    const pills=[];
    if(declared)pills.push('<span class="metric-pill'+(running>0?'':' bad')+'"><b>'+running+'</b>/'+declared+' models running</span>');
    if(total){const usedPct=Math.round((1-free/total)*100);pills.push('<span class="metric-pill'+(usedPct>=90?' warn':'')+'"><b>'+usedPct+'%</b> GPU used · '+Math.round(free/1024)+' GB free</span>')}
    if(peerTotal)pills.push('<span class="metric-pill'+(fresh<peerTotal?' warn':'')+'"><b>'+fresh+'</b>/'+peerTotal+' peers fresh</span>');
    if(reqTotal)pills.push('<span class="metric-pill"><b>'+reqTotal+'</b> requests since start</span>');
    if(inflight&&Number(inflight[1])>0)pills.push('<span class="metric-pill good"><b>'+inflight[1]+'</b> in flight</span>');
    return pills.join('')||'<span class="metric-pill">No series parsed yet</span>';
  }

  function renderLogs(){
    const e=renderRing('logs-output',logData.entries,'No router events yet.');
    const u=renderRing('upstream-logs',logData.upstream,'No upstream output yet.');
    const m=renderRing('mesh-logs',logData.mesh,'No mesh events yet.');
    setBadge('count-logs-output',e,logData.entries.length);
    setBadge('count-upstream-logs',u,logData.upstream.length);
    setBadge('count-mesh-logs',m,logData.mesh.length);
    const counts={all:0,error:0,warn:0,debug:0};
    for(const ring of [logData.entries,logData.upstream,logData.mesh]){
      for(const line of ring){
        const lvl=classify(splitTs(line).msg);
        counts.all++;
        if(lvl==='error')counts.error++;
        else if(lvl==='warn')counts.warn++;
        else if(lvl==='debug')counts.debug++;
      }
    }
    for(const k in counts)setText('n-'+k,counts[k]);
    const chip=$('log-levels')&&$('log-levels').querySelector('button[data-level="error"]');
    if(chip)chip.classList.toggle('hot',counts.error>0);
  }

  async function logs(){
    if(logPause)return;
    try{
      const d=await json('/_router/logs');
      logData={entries:d.entries||[],upstream:d.upstream||[],mesh:d.mesh||[],backend_metrics:d.backend_metrics||[]};
      const total=logData.entries.length+logData.upstream.length+logData.mesh.length;
      setText('log-updated',total+' lines · '+new Date().toLocaleTimeString());
      renderLogs();
    }catch(e){
      const el=$('logs-output');if(el)el.innerHTML='<div class="log-empty">'+esc(e.message)+'</div>';
    }
    try{
      const text=await (await fetch('/metrics')).text();
      const mEl=$('metrics');if(mEl)mEl.innerHTML=renderMetricsText(text);
      const sEl=$('metrics-summary');if(sEl)sEl.innerHTML=summarizeMetrics(text);
      setText('count-metrics',text.split('\n').filter(l=>l&&!l.startsWith('#')).length+' series');
      const bm=logData.backend_metrics||[];
      const uEl=$('upstream-metrics');
      if(uEl)uEl.innerHTML=bm.length?bm.map(x=>'<span class="m-help"># '+esc(x.id)+'</span>'+renderMetricsText(x.body||'')).join(''):'<div class="log-empty">No running backends to scrape.</div>';
      setText('count-upstream-metrics',bm.length?bm.length+' backends':'none');
    }catch(e){
      const el=$('metrics');if(el)el.innerHTML='<div class="log-empty">'+esc(e.message)+'</div>';
    }
  }

  const levelsEl=$('log-levels');
  if(levelsEl)levelsEl.addEventListener('click',ev=>{
    const b=ev.target.closest('button[data-level]');if(!b)return;
    logLevel=b.dataset.level;
    levelsEl.querySelectorAll('button').forEach(x=>{
      const on=x===b;
      x.classList.toggle('active',on);
      x.setAttribute('aria-pressed',on?'true':'false');
    });
    renderLogs();
  });
  const searchEl=$('log-search');
  if(searchEl){
    let timer=null;
    searchEl.addEventListener('input',()=>{
      clearTimeout(timer);
      timer=setTimeout(()=>{logQuery=searchEl.value.trim().toLowerCase();renderLogs()},140);
    });
  }
  const pauseEl=$('log-pause');
  if(pauseEl){
    pauseEl.addEventListener('change',()=>{
      logPause=pauseEl.checked;
      if(logPause)setText('log-updated','paused');
      else logs();
    });
  }
  document.querySelectorAll('[data-copy]').forEach(b=>{
    b.addEventListener('click',async()=>{
      const target=$(b.dataset.copy);
      if(!target)return;
      const label=b.textContent;
      try{
        await navigator.clipboard.writeText(target.innerText);
        b.textContent='Copied';
      }catch(e){
        b.textContent='Copy failed';
      }
      setTimeout(()=>{b.textContent=label},1200);
    });
  });

  refresh();logs();activity();setInterval(refresh,3000);setInterval(logs,3000);setInterval(activity,3000);
})();
