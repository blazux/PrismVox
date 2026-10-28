const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname,'../app/static/index.html'),'utf8');
const source = html.slice(html.indexOf('let embeddingBusy = false;'),html.indexOf('async function loadCollections()'));
function client(api) {
  const nodes = new Map();
  const element = id => {
    if (!nodes.has(id)) nodes.set(id,{value:'',textContent:'',checked:false,disabled:false});
    return nodes.get(id);
  };
  const context={api,document:{getElementById:element,querySelectorAll:()=>['embedding-url','embedding-model','embedding-key','embedding-clear-key'].map(element)}};
  vm.createContext(context);vm.runInContext(source,context);
  element('embedding-url').value='http://provider/v1';
  element('embedding-model').value='new-model';
  element('embedding-key').value='fixture-key';
  return {context,element};
}

test('failed save retains the entered provider and releases the form',async()=>{
  const {context,element}=client(async()=>{throw Error('Provider unavailable');});
  await context.saveEmbeddingProvider({preventDefault(){}});
  assert.equal(element('embedding-url').value,'http://provider/v1');
  assert.equal(element('embedding-model').value,'new-model');
  assert.equal(element('embedding-key').value,'fixture-key');
  assert.equal(element('embedding-provider-status').textContent,'Provider unavailable');
  assert.equal(element('embedding-save').disabled,false);
  assert.equal(element('embedding-url').disabled,false);
});

test('saving indexes every page and then reloads the persisted public settings',async()=>{
  const calls=[];
  const {context,element}=client(async(method,url,body)=>{
    calls.push([method,url,body]);
    if(method==='PUT') return {dimensions:4096,indexed:0,total:35,pending:35};
    if(method==='POST') return body.after_id===0
      ? {index_key:'same-model',indexed:32,total:35,pending:3,next_after:32}
      : {index_key:'same-model',indexed:35,total:35,pending:0,next_after:null};
    return {embedding_url:'http://provider/v1',embedding_model:'new-model',embedding_dimensions:4096,api_key_configured:true,configured:true,indexed:35,total:35,pending:0};
  });
  await context.saveEmbeddingProvider({preventDefault(){}});
  assert.deepEqual(calls.map(c=>c.slice(0,2)),[['PUT','/api/embeddings'],['POST','/api/rag/embed-all'],['POST','/api/rag/embed-all'],['GET','/api/embeddings']]);
  assert.equal(calls[2][2].after_id,32);
  assert.equal(element('embedding-provider-status').textContent,'✓ 35/35 passages indexed.');
  assert.equal(element('embedding-key').value,'');
  assert.match(element('embedding-key').placeholder,/Saved key/);
  assert.equal(element('embedding-save').disabled,false);
});

test('a different index identity aborts pagination and reports the change',async()=>{
  let page=0;
  const {context,element}=client(async(method)=>{
    if(method==='PUT') return {dimensions:3,indexed:0,total:35,pending:35};
    if(method==='POST') return ++page===1
      ? {index_key:'first',indexed:32,total:35,pending:3,next_after:32}
      : {index_key:'second',indexed:3,total:35,pending:32,next_after:null};
    return {embedding_url:'http://provider/v1',embedding_model:'changed',api_key_configured:false};
  });
  await context.saveEmbeddingProvider({preventDefault(){}});
  assert.match(element('embedding-provider-status').textContent,/settings changed/);
  assert.equal(element('embedding-save').disabled,false);
});
