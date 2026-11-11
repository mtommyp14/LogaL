// Uses an isolated Chrome DevTools session and Node's built-in WebSocket.
import fs from 'node:fs/promises';
import assert from 'node:assert/strict';
import path from 'node:path';
const credentials = JSON.parse(await fs.readFile('.sandbox/credentials.json', 'utf8'));
const base = 'http://127.0.0.1:19222';
const target = await (await fetch(base + '/json/new?about:blank', {method:'PUT'})).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolve, reject) => {ws.onopen=resolve;ws.onerror=reject;});
let id=0;
const pending=new Map();
const errors=[];
const results=[];
const downloadEvents=[];
function send(method, params={}) {
 return new Promise((resolve,reject)=>{
  const requestId=++id;
  const timer=setTimeout(()=>{pending.delete(requestId);reject(new Error('CDP timeout '+method));},30000);
  pending.set(requestId,{resolve,reject,timer});ws.send(JSON.stringify({id:requestId,method,params}));
 });
}
ws.onmessage=async event=>{
 const msg=JSON.parse(event.data);
 if (msg.id) {
  const p=pending.get(msg.id);if(!p)return;clearTimeout(p.timer);pending.delete(msg.id);
  if(msg.error)p.reject(new Error(JSON.stringify(msg.error)));else p.resolve(msg.result);return;
 }
 if(msg.method==='Fetch.requestPaused')send('Fetch.continueRequest',{requestId:msg.params.requestId}).catch(()=>{});
 if(msg.method==='Fetch.authRequired')send('Fetch.continueWithAuth',{requestId:msg.params.requestId,authChallengeResponse:{response:'ProvideCredentials',username:credentials.username,password:credentials.password}}).catch(()=>{});
 if(msg.method?.toLowerCase().includes('download'))downloadEvents.push(msg);
 if(msg.method==='Runtime.exceptionThrown')errors.push(msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
};
async function evaluate(expression) {
 const value=await send('Runtime.evaluate',{expression,awaitPromise:true,returnByValue:true,userGesture:true});
 if(value.exceptionDetails)throw new Error(value.exceptionDetails.exception?.description || value.exceptionDetails.text);
 return value.result.value;
}
async function wait(expression, timeout=25000) {
 const start=Date.now();
 while(Date.now()-start<timeout){if(await evaluate(expression))return;await new Promise(r=>setTimeout(r,200));}
 throw new Error('UI timeout: '+expression);
}
async function check(name,fn){
 try{const detail=await fn();results.push({name,status:'PASS',detail});console.log('PASS',name,detail??'');}
 catch(e){results.push({name,status:'FAIL',error:e.message});console.log('FAIL',name,e.message);}
}
await send('Page.enable');await send('Runtime.enable');
await send('Fetch.enable',{handleAuthRequests:true});
await send('Emulation.setDeviceMetricsOverride',{width:1440,height:1000,deviceScaleFactor:1,mobile:false});
await send('Browser.grantPermissions',{origin:'http://127.0.0.1:18081',permissions:['clipboardReadWrite','clipboardSanitizedWrite']});
await send('Page.setDownloadBehavior',{behavior:'allow',downloadPath:path.resolve('.sandbox/downloads')});
await send('Browser.setDownloadBehavior',{behavior:'allow',downloadPath:path.resolve('.sandbox/downloads'),eventsEnabled:true});
try {
await check('browser Basic Auth and initial cluster list',async()=>{
 await send('Page.navigate',{url:'http://127.0.0.1:18081'});
 await wait(`document.readyState==='complete' && [...(document.querySelector('#nsSelect')?.options || [])].some(o=>o.value==='logal-sandbox')`);
 assert.ok(await evaluate(`document.querySelector('#ctxSelect').value.length>0`));
});
await check('browser workload autocomplete and three-pod selection',async()=>{
 await evaluate(`(async()=>{document.querySelector('#nsSelect').value='logal-sandbox';document.querySelector('#kindSelect').value='Deployment';await onNamespaceChange();document.querySelector('#workloadInput').value='demo-api';await loadWorkloadInfo('demo-api');await loadPodsForStream(document.querySelector('#ctxSelect').value,'logal-sandbox','demo-api');})()`);
 assert.equal(await evaluate(`document.querySelectorAll('#podChips .selector-chip.checked').length`),3);
 assert.equal(await evaluate(`document.querySelectorAll('#containerChips .selector-chip.checked').length`),1);
});
await check('browser live SSE and quoted text rendered safely',async()=>{
 await evaluate(`(async()=>{document.querySelector('#sinceSelect').value='realtime';document.querySelector('#filterInput').value='SANDBOX_QUOTED';await startStream();})()`);
 await wait(`lineCount>=6`);
 assert.equal(await evaluate(`document.querySelectorAll('#logContent tag').length`),0);
 assert.ok(await evaluate(`allLogs.every(l=>l.includes('"value":"hello"') && l.includes('<tag>'))`));
 await evaluate('stopStream(true)');
 const shot=await send('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});
 await fs.writeFile('.sandbox/browser-live.png',Buffer.from(shot.data,'base64'));
 return await evaluate('lineCount');
});
await check('browser stop freezes incoming log count',async()=>{
 const count=await evaluate('lineCount');await new Promise(r=>setTimeout(r,1000));assert.equal(await evaluate('lineCount'),count);
});
await check('browser copy preserves displayed logs',async()=>{
 await send('Page.bringToFront');await evaluate('copyLogs()');
 await wait(`document.querySelector('#toast').textContent==='Copied to clipboard!'`);
 assert.ok(await evaluate(`(async()=>(await navigator.clipboard.readText()).split(String.fromCharCode(13,10)).join(String.fromCharCode(10))===allLogs.join(String.fromCharCode(10)))()`));
});
await check('browser downloads real log file',async()=>{
 const before=new Set(await fs.readdir('.sandbox/downloads').catch(()=>[]));
 await evaluate('downloadLogs()');
 let files=[];for(let i=0;i<30;i++){try{files=(await fs.readdir('.sandbox/downloads')).filter(f=>f.endsWith('.log') && !before.has(f));}catch{}if(files.length)break;await new Promise(r=>setTimeout(r,200));}
 assert.ok(files.length,'no .log download; events='+JSON.stringify(downloadEvents));
 const text=await fs.readFile('.sandbox/downloads/'+files.at(-1),'utf8');assert.ok(text.includes('SANDBOX_QUOTED'));return files.at(-1);
});
await check('browser custom history completes and filter highlights WARN',async()=>{
 await evaluate(`(async()=>{document.querySelector('#sinceSelect').value='custom';onSinceChange();document.querySelector('#dtFrom').value=toLocalDatetimeInput(new Date(Date.now()-1800000));document.querySelector('#dtTo').value=toLocalDatetimeInput(new Date());await startStream();})()`);
 await wait(`lineCount>0 && eventSource===null`);
 assert.ok(await evaluate(`document.querySelectorAll('.log-line.level-warn').length>0`));
 return await evaluate('lineCount');
});
await check('browser StatefulSet selection streams worker logs',async()=>{
 await evaluate(`(async()=>{stopStream(true);document.querySelector('#kindSelect').value='StatefulSet';await onNamespaceChange();document.querySelector('#workloadInput').value='demo-worker';document.querySelector('#filterInput').value='';document.querySelector('#sinceSelect').value='realtime';onSinceChange();await startStream();})()`);
 await wait(`lineCount>=2`);
 assert.ok(await evaluate(`allLogs.every(l=>l.includes('SANDBOX_WORKER'))`));
});
await check('browser All types resolves StatefulSet without a manual type selection',async()=>{
 await evaluate(`(async()=>{stopStream(true);document.querySelector('#kindSelect').value='';await onNamespaceChange();document.querySelector('#workloadInput').value='demo-worker';await startStream();})()`);
 await wait(`lineCount>=2`);
 assert.ok(await evaluate(`allLogs.every(l=>l.includes('SANDBOX_WORKER'))`));
 assert.equal(await evaluate(`document.querySelector('#kindSelect').value`),'');
});
await check('browser restores saved session on reload',async()=>{
 await send('Page.reload');await wait(`document.querySelector('#workloadInput')?.value==='demo-worker' && typeof lineCount!=='undefined' && lineCount>=2`);
 await evaluate('stopStream(true)');
});
await check('browser has no uncaught JavaScript errors',async()=>assert.deepEqual(errors,[]));
} finally {
 await fs.writeFile('.sandbox/browser-results.json',JSON.stringify(results,null,2));
 await evaluate('stopStream(true)').catch(()=>{});ws.close();
}
const failed=results.filter(r=>r.status==='FAIL').length;
console.log(`${results.length-failed}/${results.length} browser checks passed`);process.exitCode=failed?1:0;
