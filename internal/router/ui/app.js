(function(){
  let status={models:[],telemetry:{},peers:{}};
  const $=id=>document.getElementById(id);
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon=s=>{const paths={model:'<path d="M6 4.5 12 2l6 2.5v6L12 13l-6-2.5v-6Z"/><path d="m6 10.5 6 2.5 6-2.5M12 13v5"/>',peer:'<circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="6" r="2.5"/><circle cx="18" cy="18" r="2.5"/><path d="m8.2 10.8 7.5-3.6M8.2 13.2l7.5 3.6"/>'};return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">'+(paths[s]||paths.model)+'</svg>'};
  const pageCopy={dashboard:['Fleet node','Overview','Live capacity and model activity for this router.'],chat:['Operator tools','Chat lab','Test a streaming completion against any available route.'],image:['Operator tools','Image lab','Send an image request through the configured diffusion backend.'],logs:['Observability','Logs & metrics','Router gauges, backend /metrics, and event rings.']};
  document.querySelectorAll('.nav button').forEach(b=>b.onclick=()=>{const view=b.dataset.view;document.querySelectorAll('.nav button').forEach(x=>{x.classList.remove('active');x.removeAttribute('aria-current')});document.querySelectorAll('.view').forEach(x=>x.classList.remove('active'));b.classList.add('active');b.setAttribute('aria-current','page');$(view).classList.add('active');$('eyebrow').textContent=pageCopy[view][0];$('page-title').textContent=pageCopy[view][1];$('page-subtitle').textContent=pageCopy[view][2]});
  async function json(url,opts){const r=await fetch(url,opts);const text=await r.text();let data;try{data=JSON.parse(text)}catch(e){data={error:text}}if(!r.ok)throw new Error(data.error||text||('HTTP '+r.status));return data}
  function setHealth(ok,message){$('health').textContent=message;$('side-health').textContent=ok?'Status updates every 3 seconds':'Unable to reach status endpoint';$('health-dot').className='status-dot '+(ok?'':'off');$('side-dot').className='status-dot '+(ok?'':'off')}
  window.refresh=async function(){try{status=await json('/_router/status');const node=status.node||'unknown';const now=new Date();$('side-node').textContent=node;$('updated').textContent='Updated '+now.toLocaleTimeString();setHealth(true,'Router healthy');renderSummary();renderGPUs();renderModels();renderPeers();fillSelectors()}catch(e){setHealth(false,e.message);$('updated').textContent='Snapshot unavailable'}};
  function renderSummary(){const gs=status.telemetry&&status.telemetry.gpus||[];const models=status.models||[];const running=models.filter(m=>m.state==='running').length;const peers=status.peers||{};$('stat-gpus').textContent=gs.length;$('stat-running').textContent=running;$('stat-models').textContent=models.length;$('stat-peers').textContent=Object.keys(peers).length;$('model-summary').textContent=models.length+' entries · '+running+' online'}
  function renderGPUs(){const gs=status.telemetry&&status.telemetry.gpus||[];$('gpu-cards').innerHTML=gs.map(g=>{const total=Number(g.total_mb)||0,free=Number(g.free_mb)||0,stale=Number(g.free_if_stale_evicted_mb)||free,idle=Number(g.free_if_idle_evicted_mb)||stale;const used=total?Math.max(0,Math.min(100,((total-free)/total)*100)):0;return '<article class="panel gpu-card"><div class="gpu-title"><span class="gpu-name">'+esc(g.name||'GPU '+g.index)+'</span><span class="gpu-index">GPU '+esc(g.index)+'</span></div><div class="gpu-memory"><span class="gpu-free">'+esc(free)+'</span><span class="gpu-unit">MB free</span></div><div class="meter" role="progressbar" aria-label="GPU memory used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="'+Math.round(used)+'"><div class="meter-fill" style="width:'+used+'%"></div></div><div class="gpu-footer"><span>'+esc(total)+' MB total</span><span>'+esc(idle)+' MB if idle evicted</span></div></article>'}).join('')||'<div class="panel empty">No GPU telemetry available yet.</div>'}
  function renderModels(){const rows=(status.models||[]).map(m=>{const origin=m.origin||'local';const freshness=m.freshness||'';const state=freshness||m.state||(origin==='remote'?'available':'stopped');const can=origin==='local';return '<tr><td><div class="model-cell"><span class="model-glyph">'+icon(origin==='remote'?'peer':'model')+'</span><div><span class="model-id">'+esc(m.id)+'</span>'+(m.name?'<span class="model-name">'+esc(m.name)+'</span>':'')+'</div></div></td><td><span class="state '+esc(origin)+'">'+esc(origin==='remote'?'remote':'local')+'</span></td><td><span class="state '+esc(state)+'">'+esc(state)+'</span></td><td class="mono">'+(m.vram_mb?esc(m.vram_mb)+' MB':'—')+'</td><td>'+(can?'<div class="actions"><button data-act="load" data-id="'+esc(m.id)+'">Load</button><button class="danger" data-act="unload" data-id="'+esc(m.id)+'">Unload</button></div>':'<span class="subtle">Mesh routed</span>')+'</td></tr>'}).join('');$('models').innerHTML=rows||'<tr><td colspan="5" class="empty">No models in the catalog.</td></tr>'}
  function renderPeers(){const ps=status.peers||{};const cards=Object.keys(ps).sort().map(n=>{const p=ps[n]||{};const gs=p.gpus||[];const loaded=(p.loaded_models||[]).map(m=>typeof m==='string'?m:((m.id||'')+(m.freshness?' '+m.freshness:'')+(m.slots?(' '+m.in_flight+'/'+m.slots):'')));return '<article class="panel peer-card"><div class="peer-head"><div><div class="peer-name">'+esc(n)+'</div><div class="subtle mono">'+(p.timestamp?'Last seen '+new Date(p.timestamp*1000).toLocaleTimeString():'No timestamp')+'</div></div><span class="peer-state">'+(gs.length?'Fresh telemetry':'No telemetry')+'</span></div><div class="peer-detail"><div><div class="peer-label">GPU memory</div><div class="peer-value">'+esc(gs.map(g=>g.free_mb+' MB free'+(g.free_if_stale_evicted_mb&&g.free_if_stale_evicted_mb!==g.free_mb?' / '+g.free_if_stale_evicted_mb+' if stale evicted':'')).join(' · ')||'—')+'</div></div><div><div class="peer-label">Loaded models</div><div class="peer-value">'+esc(loaded.join(', ')||'None reported')+'</div></div></div></article>'}).join('');$('peers').innerHTML=cards||'<div class="panel empty">No fresh peer telemetry.</div>'}
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
  async function logs(){try{const d=await json('/_router/logs');$('logs-output').textContent=(d.entries||[]).join('\n')||'No router events yet.';$('upstream-logs').textContent=(d.upstream||[]).join('\n')||'No upstream output yet.';$('mesh-logs').textContent=(d.mesh||[]).join('\n')||'No mesh events yet.';const bm=d.backend_metrics||[];$('upstream-metrics').textContent=bm.length?bm.map(x=>'# '+x.id+'\n'+(x.body||'')+'\n').join('\n'):'No running backends to scrape.'}catch(e){$('logs-output').textContent=e.message}try{$('metrics').textContent=await (await fetch('/metrics')).text()}catch(e){$('metrics').textContent=e.message}}
  refresh();logs();setInterval(refresh,3000);setInterval(logs,3000);
})();
