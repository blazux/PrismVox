const fs=require('node:fs');
const vm=require('node:vm');
const assert=require('node:assert/strict');
const src=fs.readFileSync(require('node:path').join(__dirname,'../app/static/index.html'),'utf8');
const context={Date,JSON,Array,Object,String,fmtDuration:()=>'',directionBadge:()=>'',reasonBadge:()=>''};
vm.createContext(context);
vm.runInContext(src.split('\n').find(line=>line.startsWith('function esc(s)'))+'\n'+src.slice(src.indexOf('function buildLogRows(row)'),src.indexOf('function toggleLog(id)')),context);
const html=context.buildLogRows({id:1,started_at:'2026-10-26T18:00:00Z',tools_used:[
 {tool:'calendar',args:{action:'add',title:'<img src=x onerror=evil()>'},outcome:'failed',result:'<script>evil()</script>'},
 {tool:'note',outcome:'started',args:{action:'list'}},
 {tool:'email',outcome:'returned',result:'Long result',result_truncated:true},
 {tool:'end_call'},
]});
assert.ok(html.includes('calendar · add · failed'));
assert.ok(html.includes('Result not received'));
assert.ok(html.includes('Result (truncated)'));
assert.ok(html.includes('&lt;img'));
assert.ok(html.includes('&lt;script&gt;'));
assert.ok(!html.includes('<img')&&!html.includes('<script>'));
assert.equal((html.match(/<details /g)||[]).length,3);
console.log('Call tool traces show actions, arguments, results and incomplete/failed status; content is escaped.');
