// Playground mic and camera capture (#194). Loaded before app.js.
//
// Captures stay in page memory only: a take is a Blob plus a blob URL, it goes
// out in the request that uses it, and it is freed on discard or reload. The
// only server-side write is the explicit "Save to voice library" button.
//
// The pure helpers at the top have no DOM and run under node in
// testdata/capture_test.js. The widgets below them only run in a browser.
(function(root){
  'use strict';

  // ---------- pure helpers ----------

  // downmix averages the channels of a take into one mono channel.
  function downmix(channels){
    if(!channels||!channels.length)return new Float32Array(0);
    if(channels.length===1)return Float32Array.from(channels[0]);
    const n=Math.min(...channels.map(c=>c.length)),out=new Float32Array(n);
    for(let i=0;i<n;i++){let s=0;for(const c of channels)s+=c[i];out[i]=s/channels.length}
    return out;
  }

  // resampleLinear is the fallback when OfflineAudioContext is missing. It is
  // plain linear interpolation, good enough for speech at 16 or 24 kHz.
  function resampleLinear(samples,from,to){
    if(from===to||!samples.length)return Float32Array.from(samples);
    const n=Math.max(1,Math.round(samples.length*to/from)),out=new Float32Array(n),step=from/to;
    for(let i=0;i<n;i++){
      const x=i*step,j=Math.floor(x),f=x-j,a=samples[Math.min(j,samples.length-1)],b=samples[Math.min(j+1,samples.length-1)];
      out[i]=a+(b-a)*f;
    }
    return out;
  }

  // encodeWAV writes mono 16-bit PCM with a 44-byte RIFF header, the shape
  // whisper.cpp reads without --convert and the voice library accepts.
  function encodeWAV(samples,rate){
    const n=samples.length,buf=new ArrayBuffer(44+n*2),v=new DataView(buf);
    const str=(o,s)=>{for(let i=0;i<s.length;i++)v.setUint8(o+i,s.charCodeAt(i))};
    str(0,'RIFF');v.setUint32(4,36+n*2,true);str(8,'WAVE');
    str(12,'fmt ');v.setUint32(16,16,true);v.setUint16(20,1,true);v.setUint16(22,1,true);
    v.setUint32(24,rate,true);v.setUint32(28,rate*2,true);v.setUint16(32,2,true);v.setUint16(34,16,true);
    str(36,'data');v.setUint32(40,n*2,true);
    for(let i=0;i<n;i++){const s=Math.max(-1,Math.min(1,samples[i]||0));v.setInt16(44+i*2,s<0?Math.round(s*0x8000):Math.round(s*0x7fff),true)}
    return new Uint8Array(buf);
  }

  // pickEven returns n indices spread evenly over total items, first and last
  // included, so a clip's opening and closing frames both reach the model.
  function pickEven(total,n){
    total=Math.max(0,total|0);n=Math.max(0,n|0);
    if(!total||!n)return [];
    if(n>=total)return Array.from({length:total},(_,i)=>i);
    if(n===1)return [Math.floor((total-1)/2)];
    const out=[];
    for(let i=0;i<n;i++)out.push(Math.round(i*(total-1)/(n-1)));
    return out;
  }

  // fitSize scales w x h down so the longest side is at most max. It never
  // scales up.
  function fitSize(w,h,max){
    if(!w||!h)return {w:0,h:0};
    const k=Math.min(1,max/Math.max(w,h));
    return {w:Math.max(1,Math.round(w*k)),h:Math.max(1,Math.round(h*k))};
  }

  function bytesToBase64(u8){
    let s='';
    for(let i=0;i<u8.length;i+=0x8000)s+=String.fromCharCode.apply(null,u8.subarray(i,i+0x8000));
    return btoa(s);
  }

  // chatPlan says which capture controls a chat model gets from its
  // capabilities: stills and frame-sampled clips need vision, a native clip
  // needs video_in, the mic needs audio_in.
  function chatPlan(caps){
    caps=caps||[];
    const vision=caps.includes('vision'),videoIn=caps.includes('video_in');
    return {camera:vision||videoIn,still:vision,clip:videoIn?'native':vision?'frames':null,audio:caps.includes('audio_in')};
  }

  // chatContent builds the user message content. With no attachments it is
  // the plain prompt string, so text-only models see the same body as before.
  // Attachments: {kind:'image',url}, {kind:'clip',url,frames:[url...]},
  // {kind:'audio',b64,format}. A clip goes as one video_url part to a video_in
  // model, else as up to maxFrames evenly picked image_url parts.
  function chatContent(text,atts,caps,maxFrames){
    const plan=chatPlan(caps),parts=[];
    for(const a of atts||[]){
      if(a.kind==='image'&&plan.still)parts.push({type:'image_url',image_url:{url:a.url}});
      else if(a.kind==='clip'&&plan.clip==='native'&&a.url)parts.push({type:'video_url',video_url:{url:a.url}});
      else if(a.kind==='clip'&&plan.still){const f=a.frames||[];for(const i of pickEven(f.length,maxFrames||6))parts.push({type:'image_url',image_url:{url:f[i]}})}
      else if(a.kind==='audio'&&plan.audio)parts.push({type:'input_audio',input_audio:{data:a.b64,format:a.format||'wav'}});
    }
    if(!parts.length)return text;
    return [{type:'text',text}].concat(parts);
  }

  // cloneFields is a one-off voice reference in the stanza's clone_mode, the
  // same two shapes the router injects for a library voice.
  function cloneFields(mode,wavB64,refText,lang){
    if(mode==='speaker_ref_b64'){const o={speaker_ref_b64:wavB64};if(lang)o.lang=lang;return o}
    const o={ref_audio:'data:audio/wav;base64,'+wavB64};
    if(refText)o.ref_text=refText;
    if(lang)o.language=lang;
    return o;
  }

  // mediaError turns a getUserMedia or MediaRecorder failure into a sentence
  // an operator can act on.
  function mediaError(e,what){
    what=what||'device';
    switch(e&&e.name){
      case 'NotAllowedError':case 'PermissionDeniedError':return 'Permission to use the '+what+' was refused. Allow it in the browser\'s site settings, then try again.';
      case 'NotFoundError':case 'DevicesNotFoundError':return 'No '+what+' was found. Plug one in or pick another device.';
      case 'NotReadableError':case 'TrackStartError':return 'The '+what+' is busy or blocked. Close other apps using it, then try again.';
      case 'OverconstrainedError':return 'The chosen '+what+' is no longer available. Pick another device.';
      case 'SecurityError':return insecureMessage();
      case 'AbortError':return 'The '+what+' stopped before it started. Try again.';
      case 'EncodingError':return 'The browser could not decode the recording. Try a shorter take or another browser.';
      default:return (e&&e.message)||('The '+what+' failed.');
    }
  }
  function insecureMessage(){return 'The browser only allows the mic and camera on HTTPS or localhost. Open this UI over HTTPS, or through an SSH tunnel to localhost.'}

  // canCapture is true when getUserMedia exists, which browsers only allow in
  // a secure context (HTTPS or localhost).
  function canCapture(nav,secure){return !!(secure!==false&&nav&&nav.mediaDevices&&nav.mediaDevices.getUserMedia)}

  function fmtClock(s){s=Math.max(0,Math.floor(s||0));return Math.floor(s/60)+':'+String(s%60).padStart(2,'0')}

  const helpers={downmix,resampleLinear,encodeWAV,pickEven,fitSize,bytesToBase64,chatPlan,chatContent,cloneFields,mediaError,canCapture,fmtClock};
  if(typeof module!=='undefined'&&module.exports){module.exports=helpers;return}

  // ---------- browser widgets ----------

  const doc=root.document,$=id=>doc.getElementById(id);
  if(!doc||!$('playground'))return;
  const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const nav=root.navigator,secure=root.isSecureContext!==false;
  const supported=canCapture(nav,secure);
  const models=()=>typeof root.routerModels==='function'?root.routerModels():[];
  const modelInfo=id=>models().find(m=>m.id===id)||{};
  const el=(tag,attrs,html)=>{const e=doc.createElement(tag);for(const k in attrs||{})e.setAttribute(k,attrs[k]);if(html!==undefined)e.innerHTML=html;return e};

  // Device lists fill once permission is granted; before that browsers hide
  // the labels, so the picker shows "Default" plus numbered devices.
  const pickers={audioinput:new Set(),videoinput:new Set()};
  async function fillDevices(){
    if(!supported||!nav.mediaDevices.enumerateDevices)return;
    let list=[];try{list=await nav.mediaDevices.enumerateDevices()}catch(e){return}
    for(const kind of ['audioinput','videoinput']){
      const devs=list.filter(d=>d.kind===kind&&d.deviceId);
      for(const sel of pickers[kind]){
        const prev=sel.value;
        sel.innerHTML='<option value="">Default</option>'+devs.map((d,i)=>'<option value="'+esc(d.deviceId)+'">'+esc(d.label||(kind==='audioinput'?'Microphone ':'Camera ')+(i+1))+'</option>').join('');
        if([...sel.options].some(o=>o.value===prev))sel.value=prev;
      }
    }
  }
  if(supported&&nav.mediaDevices.addEventListener)nav.mediaDevices.addEventListener('devicechange',fillDevices);
  function picker(kind,label){const s=el('select',{'aria-label':label,class:'media-dev'});pickers[kind].add(s);return s}

  // Every live stream is tracked so a tab switch or page hide turns the
  // camera light and mic off.
  const live=new Set();
  function stopAll(){for(const w of [...live])w.stop()}
  doc.addEventListener('visibilitychange',()=>{if(doc.hidden)stopAll()});
  const tabs=$('playground').querySelector('[role=tablist]');
  if(tabs)tabs.addEventListener('click',ev=>{if(ev.target.closest('[data-tab]'))stopAll()});

  function recorderType(kind){
    if(!root.MediaRecorder||!MediaRecorder.isTypeSupported)return '';
    const want=kind==='audio'?['audio/webm;codecs=opus','audio/ogg;codecs=opus','audio/mp4','audio/webm']:['video/mp4;codecs=avc1','video/mp4','video/webm;codecs=vp9','video/webm;codecs=vp8','video/webm'];
    return want.find(t=>MediaRecorder.isTypeSupported(t))||'';
  }

  // toWAV decodes any recording or upload the browser can play and writes it
  // as mono 16-bit WAV at rate.
  async function toWAV(blob,rate){
    const AC=root.AudioContext||root.webkitAudioContext;
    if(!AC)throw new Error('This browser has no Web Audio, so it cannot convert the recording.');
    const ctx=new AC();
    let buf;
    try{buf=await ctx.decodeAudioData(await blob.arrayBuffer())}finally{if(ctx.close)ctx.close()}
    let mono;
    if(root.OfflineAudioContext&&buf.sampleRate!==rate){
      const off=new OfflineAudioContext(1,Math.max(1,Math.ceil(buf.duration*rate)),rate),src=off.createBufferSource();
      src.buffer=buf;src.connect(off.destination);src.start();
      mono=(await off.startRendering()).getChannelData(0);
    }else{
      const ch=[];for(let i=0;i<buf.numberOfChannels;i++)ch.push(buf.getChannelData(i));
      mono=resampleLinear(downmix(ch),buf.sampleRate,rate);
    }
    const bytes=encodeWAV(mono,rate);
    return {blob:new Blob([bytes],{type:'audio/wav'}),b64:bytesToBase64(bytes),seconds:mono.length/rate};
  }

  // Mic renders a record button (click to start and stop, or hold to talk),
  // a level meter, a timer and the playable take. opts: rate, maxSec, onTake,
  // onClear.
  function Mic(box,opts){
    const w={take:null,stream:null,rec:null,stop:()=>w.halt(false)};
    const sel=picker('audioinput','Microphone');
    const btn=el('button',{type:'button',class:'media-rec','aria-pressed':'false'},'<span aria-hidden="true">●</span> Record');
    const meter=el('meter',{min:'0',max:'1',value:'0',class:'media-meter','aria-label':'Microphone level'});
    const clock=el('span',{class:'mono media-time',role:'timer','aria-label':'Recording length'},'0:00');
    const note=el('p',{class:'subtle media-note',role:'status','aria-live':'polite'});
    const takeBox=el('div',{class:'media-take',hidden:''});
    const row=el('div',{class:'media-row'});
    row.append(sel,btn,meter,clock);box.append(row,note,takeBox);
    note.textContent=supported?'Click Record to start and again to stop, or hold the button and let go to stop. Limit '+fmtClock(opts.maxSec)+'.':insecureMessage();
    if(!supported){btn.disabled=true;sel.disabled=true}
    let chunks=[],t0=0,timer=0,raf=0,ac=null,pressAt=0,startedByPress=false;
    function clear(){if(w.take){URL.revokeObjectURL(w.take.url);w.take=null}takeBox.hidden=true;takeBox.innerHTML='';if(opts.onClear)opts.onClear()}
    w.clear=clear;
    async function start(){
      clear();
      try{
        w.stream=await nav.mediaDevices.getUserMedia({audio:sel.value?{deviceId:{exact:sel.value}}:true});
      }catch(e){note.textContent=mediaError(e,'microphone');return false}
      fillDevices();
      const type=recorderType('audio');
      try{w.rec=new MediaRecorder(w.stream,type?{mimeType:type}:undefined)}catch(e){w.halt(true);note.textContent=mediaError(e,'microphone');return false}
      chunks=[];w.rec.ondataavailable=ev=>{if(ev.data&&ev.data.size)chunks.push(ev.data)};
      w.rec.onstop=finish;
      w.rec.start(250);live.add(w);t0=performance.now();
      btn.setAttribute('aria-pressed','true');btn.classList.add('recording');btn.innerHTML='<span aria-hidden="true">■</span> Stop';
      note.textContent='Recording…';
      timer=setInterval(()=>{const s=(performance.now()-t0)/1000;clock.textContent=fmtClock(s);if(s>=opts.maxSec)w.halt(false)},200);
      const AC=root.AudioContext||root.webkitAudioContext;
      if(AC){try{ac=new AC();const an=ac.createAnalyser();an.fftSize=1024;ac.createMediaStreamSource(w.stream).connect(an);const d=new Float32Array(an.fftSize);const tick=()=>{an.getFloatTimeDomainData(d);let s=0;for(const x of d)s+=x*x;meter.value=Math.min(1,Math.sqrt(s/d.length)*4);raf=requestAnimationFrame(tick)};tick()}catch(e){}}
      return true;
    }
    // halt stops the tracks; drop=true throws the take away.
    w.halt=function(drop){
      clearInterval(timer);cancelAnimationFrame(raf);meter.value=0;
      if(ac){ac.close();ac=null}
      if(w.rec&&w.rec.state!=='inactive'){if(drop)w.rec.onstop=null;w.rec.stop()}
      if(w.stream)w.stream.getTracks().forEach(t=>t.stop());
      w.stream=null;live.delete(w);
      btn.setAttribute('aria-pressed','false');btn.classList.remove('recording');btn.innerHTML='<span aria-hidden="true">●</span> Record';
    };
    async function finish(){
      const raw=new Blob(chunks,{type:(w.rec&&w.rec.mimeType)||'audio/webm'});w.rec=null;chunks=[];
      if(!raw.size){note.textContent='Nothing was recorded. Check the level meter moves when you speak.';return}
      note.textContent='Converting to WAV…';
      try{
        const t=await toWAV(raw,opts.rate);
        // handoff: the caller keeps the take (chat attachments), so no player here.
        if(opts.handoff){note.textContent='Recorded '+fmtClock(t.seconds)+'. Added to the attachments.';opts.onTake(t);return}
        t.url=URL.createObjectURL(t.blob);t.file=new File([t.blob],opts.fileName||'recording.wav',{type:'audio/wav'});
        w.take=t;
        takeBox.innerHTML='<audio controls src="'+esc(t.url)+'" aria-label="Recorded audio"></audio><span class="subtle">'+esc(fmtClock(t.seconds))+' · WAV '+(opts.rate/1000)+' kHz mono</span> <button type="button" class="ghost" data-discard>Discard</button>';
        takeBox.hidden=false;takeBox.querySelector('[data-discard]').onclick=()=>{clear();note.textContent='Take discarded.'};
        note.textContent='Recorded '+fmtClock(t.seconds)+'. '+(opts.doneNote||'It is used on the next send.');
        if(opts.onTake)opts.onTake(t);
      }catch(e){note.textContent=mediaError(e,'microphone')}
    }
    btn.addEventListener('pointerdown',async ev=>{
      if(ev.button)return;
      if(w.rec){w.halt(false);return}
      pressAt=performance.now();startedByPress=await start();
    });
    btn.addEventListener('pointerup',()=>{if(startedByPress&&w.rec&&performance.now()-pressAt>=500)w.halt(false);startedByPress=false});
    // Keyboard (Enter or Space) clicks have detail 0: toggle.
    btn.addEventListener('click',ev=>{if(ev.detail!==0)return;if(w.rec)w.halt(false);else start()});
    sel.addEventListener('change',()=>{if(w.rec){w.halt(true);start()}});
    return w;
  }

  // Camera renders a live preview with Take photo and Record clip. A clip
  // also keeps a JPEG frame every frameMs while it records, so a vision model
  // without video_in can get evenly spaced frames. opts: still, clip, maxSec,
  // stillMax, frameMax, onStill, onClip.
  function Camera(box,opts){
    const w={stream:null,rec:null};
    const sel=picker('videoinput','Camera');
    const toggle=el('button',{type:'button'},'Start camera');
    const shot=el('button',{type:'button',disabled:''},'Take photo');
    const clipBtn=el('button',{type:'button',disabled:'','aria-pressed':'false'},'Record clip');
    const clock=el('span',{class:'mono media-time',role:'timer','aria-label':'Clip length'},'');
    const video=el('video',{class:'media-preview',muted:'',playsinline:'',autoplay:'',hidden:'','aria-label':'Camera preview'});
    video.muted=true;
    const note=el('p',{class:'subtle media-note',role:'status','aria-live':'polite'});
    const row=el('div',{class:'media-row'});
    row.append(sel,toggle,shot,clipBtn,clock);box.append(row,video,note);
    w.setModes=function(still,clip){opts.still=still;opts.clip=clip;shot.hidden=!still;clipBtn.hidden=!clip;render()};
    function render(){const on=!!w.stream;toggle.textContent=on?'Stop camera':'Start camera';shot.disabled=!on||!!w.rec;clipBtn.disabled=!on;video.hidden=!on}
    if(!supported){toggle.disabled=true;sel.disabled=true;note.textContent=insecureMessage()}
    let chunks=[],frames=[],t0=0,timer=0,frameTimer=0;
    async function start(){
      try{w.stream=await nav.mediaDevices.getUserMedia({video:sel.value?{deviceId:{exact:sel.value}}:{width:{ideal:1280},height:{ideal:720}},audio:false})}
      catch(e){note.textContent=mediaError(e,'camera');return}
      fillDevices();live.add(w);video.srcObject=w.stream;note.textContent='';render();
    }
    w.stop=function(){
      if(w.rec)stopClip();
      if(w.stream)w.stream.getTracks().forEach(t=>t.stop());
      w.stream=null;video.srcObject=null;live.delete(w);render();
    };
    function grab(max,type,q){
      const vw=video.videoWidth,vh=video.videoHeight;
      if(!vw||!vh)return null;
      const s=fitSize(vw,vh,max),c=doc.createElement('canvas');c.width=s.w;c.height=s.h;
      c.getContext('2d').drawImage(video,0,0,s.w,s.h);
      return {canvas:c,url:c.toDataURL(type,q),w:s.w,h:s.h};
    }
    shot.onclick=()=>{
      const g=grab(opts.stillMax||1280,'image/jpeg',0.9);
      if(!g){note.textContent='The camera has no picture yet. Wait a moment and try again.';return}
      g.canvas.toBlob(b=>{const file=new File([b],'camera.jpg',{type:'image/jpeg'});note.textContent='Photo taken ('+g.w+' × '+g.h+').';opts.onStill({url:g.url,file,blobUrl:URL.createObjectURL(b)})},'image/jpeg',0.9);
    };
    function startClip(){
      const type=recorderType('video');
      try{w.rec=new MediaRecorder(w.stream,type?{mimeType:type}:undefined)}catch(e){note.textContent=mediaError(e,'camera');return}
      chunks=[];frames=[];
      w.rec.ondataavailable=ev=>{if(ev.data&&ev.data.size)chunks.push(ev.data)};
      w.rec.onstop=finishClip;
      w.rec.start(250);t0=performance.now();
      const f=()=>{const g=grab(opts.frameMax||640,'image/jpeg',0.8);if(g)frames.push(g.url)};f();
      frameTimer=setInterval(f,opts.frameMs||500);
      timer=setInterval(()=>{const s=(performance.now()-t0)/1000;clock.textContent=fmtClock(s)+' / '+fmtClock(opts.maxSec);if(s>=opts.maxSec)stopClip()},200);
      clipBtn.textContent='Stop clip';clipBtn.setAttribute('aria-pressed','true');clipBtn.classList.add('recording');note.textContent='Recording a clip…';render();
    }
    function stopClip(){
      clearInterval(timer);clearInterval(frameTimer);
      const g=grab(opts.frameMax||640,'image/jpeg',0.8);if(g)frames.push(g.url);
      if(w.rec&&w.rec.state!=='inactive')w.rec.stop();
      clipBtn.textContent='Record clip';clipBtn.setAttribute('aria-pressed','false');clipBtn.classList.remove('recording');clock.textContent='';
    }
    function finishClip(){
      const type=(w.rec&&w.rec.mimeType||'video/webm').split(';')[0];w.rec=null;render();
      const blob=new Blob(chunks,{type});chunks=[];
      if(!blob.size){note.textContent='Nothing was recorded.';return}
      const ext=type.includes('mp4')?'mp4':'webm',seconds=(performance.now()-t0)/1000,got=frames;frames=[];
      const fr=new FileReader();
      fr.onload=()=>{note.textContent='Clip recorded ('+fmtClock(seconds)+', '+got.length+' frames kept).';opts.onClip({url:fr.result,frames:got,file:new File([blob],'camera.'+ext,{type}),blobUrl:URL.createObjectURL(blob),seconds,type})};
      fr.readAsDataURL(blob);
    }
    toggle.onclick=()=>{if(w.stream)w.stop();else start()};
    clipBtn.onclick=()=>{if(w.rec)stopClip();else startClip()};
    sel.addEventListener('change',()=>{if(w.stream){w.stop();start()}});
    render();
    return w;
  }

  // ---------- Chat: attachments for vision, video_in and audio_in models ----------

  const chat={atts:[],plan:chatPlan([])};
  const attList=$('chat-attach');
  function renderAtts(){
    attList.innerHTML=chat.atts.map((a,i)=>{
      const label=a.kind==='image'?'Image':a.kind==='clip'?(chat.plan.clip==='native'?'Video clip':'Clip, '+Math.min(a.frames.length,frameCount())+' of '+a.frames.length+' frames'):'Audio '+fmtClock(a.seconds);
      const media=a.kind==='image'?'<img alt="" src="'+esc(a.preview)+'">':a.kind==='clip'?'<video muted src="'+esc(a.preview)+'"></video>':'<audio controls src="'+esc(a.preview)+'"></audio>';
      return '<li>'+media+'<span>'+esc(label)+'</span><button type="button" class="ghost" data-drop="'+i+'" aria-label="Remove '+esc(label)+'">Remove</button></li>';
    }).join('');
    attList.hidden=!chat.atts.length;
  }
  function addAtt(a){chat.atts.push(a);renderAtts()}
  function dropAtt(i){const [a]=chat.atts.splice(i,1);if(a&&a.revoke)URL.revokeObjectURL(a.preview);renderAtts()}
  attList.onclick=ev=>{const b=ev.target.closest('[data-drop]');if(b)dropAtt(Number(b.dataset.drop))};
  const frameCount=()=>Math.max(1,Math.min(16,Number($('chat-frames').value)||6));
  const chatCam=Camera($('chat-cam'),{maxSec:10,stillMax:1280,frameMax:640,frameMs:500,
    onStill:s=>addAtt({kind:'image',url:s.url,preview:s.blobUrl,revoke:true}),
    onClip:c=>addAtt({kind:'clip',url:c.url,frames:c.frames,preview:c.blobUrl,revoke:true})});
  const chatMic=Mic($('chat-mic'),{rate:16000,maxSec:30,handoff:true,onTake:t=>addAtt({kind:'audio',b64:t.b64,format:'wav',seconds:t.seconds,preview:URL.createObjectURL(t.blob),revoke:true})});
  $('chat-image-file').addEventListener('change',ev=>{
    for(const f of ev.target.files){const r=new FileReader();r.onload=()=>addAtt({kind:'image',url:r.result,preview:r.result});r.readAsDataURL(f)}
    ev.target.value='';
  });
  $('chat-frames').addEventListener('input',renderAtts);
  function gateChat(){
    const m=modelInfo($('chat-model').value),plan=chatPlan(m.capabilities);
    chat.plan=plan;
    $('chat-media').hidden=!(plan.camera||plan.audio);
    $('chat-cam-box').hidden=!plan.camera;$('chat-mic-box').hidden=!plan.audio;
    $('chat-frames-box').hidden=plan.clip!=='frames';$('chat-image-box').hidden=!plan.still;
    chatCam.setModes(plan.still,!!plan.clip);
    if(!plan.camera)chatCam.stop();
    if(!plan.audio)chatMic.halt(true);
    // Attachments the new model cannot take are dropped, not sent silently.
    for(let i=chat.atts.length-1;i>=0;i--){const k=chat.atts[i].kind;if((k==='image'&&!plan.still)||(k==='clip'&&!plan.clip)||(k==='audio'&&!plan.audio))dropAtt(i)}
    const n=$('chat-media-hint');
    n.hidden=plan.camera||plan.audio||!m.id;
    n.innerHTML='Text only. Add <code>vision</code>, <code>video_in</code> or <code>audio_in</code> to this model\'s <code>capabilities</code> to attach camera or mic input.';
    renderAtts();
  }

  // ---------- Transcription: mic take instead of a file ----------

  let sttTake=null;
  const sttMic=Mic($('transcribe-mic'),{rate:16000,maxSec:120,doneNote:'Transcribe sends it.',onTake:t=>{sttTake=t;$('transcribe-file').value=''},onClear:()=>{sttTake=null}});
  $('transcribe-file').addEventListener('change',()=>{if($('transcribe-file').files.length)sttMic.clear()});

  // ---------- Speech: voice clone from the mic or a file ----------

  let cloneTake=null;
  const cloneStatus=$('clone-status');
  const cloneMic=Mic($('clone-mic'),{rate:24000,maxSec:30,fileName:'voice.wav',doneNote:'Tick "Use this clip" or save it to the voice library.',onTake:t=>{cloneTake=t;$('clone-file').value='';cloneReady()},onClear:()=>{cloneTake=null;cloneReady()}});
  $('clone-file').addEventListener('change',async ev=>{
    const f=ev.target.files[0];if(!f)return;
    cloneMic.clear();cloneStatus.textContent='Converting to WAV…';
    try{cloneTake=await toWAV(f,24000);cloneTake.file=new File([cloneTake.blob],'voice.wav',{type:'audio/wav'});cloneStatus.textContent='Loaded '+fmtClock(cloneTake.seconds)+' from '+f.name+'.'}
    catch(e){cloneTake=null;cloneStatus.textContent=mediaError(e,'file')}
    cloneReady();
  });
  function cloneReady(){$('clone-save').disabled=!cloneTake;$('clone-use').disabled=!cloneTake;if(!cloneTake)$('clone-use').checked=false}
  cloneReady();
  $('clone-save').onclick=async()=>{
    const name=$('clone-name').value.trim(),text=$('clone-text').value.trim(),lang=$('clone-language').value.trim();
    if(!cloneTake){cloneStatus.textContent='Record or choose a clip first.';return}
    if(!/^[a-z0-9-]{1,64}$/.test(name)){cloneStatus.textContent='The name must be lower-case letters, digits and dashes, up to 64.';return}
    if(!text||!lang){cloneStatus.textContent='Fill in what the clip says and its language.';return}
    if(!$('clone-consent').checked){cloneStatus.textContent='Tick the consent box. The library refuses a clip without it.';return}
    const fd=new FormData();fd.append('name',name);fd.append('ref_text',text);fd.append('language',lang);fd.append('consent','playground: speaker consent confirmed');fd.append('file',cloneTake.file);
    cloneStatus.textContent='Saving…';
    try{
      const r=await fetch('/v1/audio/voices',{method:'POST',body:fd});
      if(!r.ok)throw new Error('HTTP '+r.status+': '+((await r.text()).trim()||r.statusText));
      cloneStatus.textContent='Saved as "'+name+'". Pick it in the Voice list.';
      if(root.loadVoices)await root.loadVoices(true,name);
    }catch(e){cloneStatus.textContent=e.message}
  };

  // ---------- Video: camera still or clip as the reference ----------

  let vidRef=null;
  const vidOut=$('video-cam-take');
  // keepFile leaves a just-chosen reference file in place.
  function setVidRef(r,keepFile){
    if(vidRef&&vidRef.blobUrl)URL.revokeObjectURL(vidRef.blobUrl);
    vidRef=r;if(!keepFile)$('video-reference').value='';
    vidOut.innerHTML=r?(r.kind==='image'?'<img alt="Camera reference" src="'+esc(r.blobUrl)+'">':'<video controls muted src="'+esc(r.blobUrl)+'"></video>')+'<button type="button" class="ghost" data-clear>Remove</button>':'';
    vidOut.hidden=!r;
    if(r)vidOut.querySelector('[data-clear]').onclick=()=>setVidRef(null);
  }
  Camera($('video-cam'),{still:true,clip:true,maxSec:10,stillMax:1280,frameMax:640,
    onStill:s=>setVidRef({kind:'image',file:s.file,blobUrl:s.blobUrl}),
    onClip:c=>setVidRef({kind:'video',file:c.file,blobUrl:c.blobUrl})}).setModes(true,true);
  $('video-reference').addEventListener('change',()=>{if($('video-reference').files.length&&vidRef)setVidRef(null,true)});

  // ---------- hooks for app.js ----------

  $('chat-model').addEventListener('change',gateChat);
  // fillSelect rewrites the options when status arrives, which may change the
  // selected model without a change event.
  if(root.MutationObserver)new MutationObserver(gateChat).observe($('chat-model'),{childList:true});
  gateChat();

  root.MediaCapture=Object.assign({},helpers,{
    // chatContent is the user content for the selected chat model.
    chatContent:text=>chatContent(text,chat.atts,modelInfo($('chat-model').value).capabilities,frameCount()),
    chatAttachmentCount:()=>chat.atts.length,
    transcriptionFile:()=>sttTake?sttTake.file:null,
    videoReference:()=>vidRef?vidRef.file:null,
    // speechClone is the one-off reference for model when "Use once" is ticked.
    speechClone:model=>{
      if(!cloneTake||!$('clone-use').checked)return null;
      return cloneFields(modelInfo(model).clone_mode,cloneTake.b64,$('clone-text').value.trim(),$('clone-language').value.trim());
    },
    stopAll
  });
})(typeof window!=='undefined'?window:globalThis);
