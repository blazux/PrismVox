const assert=require('node:assert/strict'),{spawnSync}=require('node:child_process'),path=require('node:path');
const script=path.resolve(__dirname,'../../scripts/is-stable-release.sh');
for(const [ref,want] of [['refs/tags/v1.0.0','true'],['refs/tags/v12.30.4','true'],['refs/tags/v0.0.0','true'],['refs/tags/v1.0.0-rc.1','false'],['refs/tags/v1.0.0-beta','false'],['refs/tags/v1.0.0+build','false'],['refs/tags/v1','false'],['refs/tags/v01.0.0','false'],['refs/tags/v1.00.0','false'],['refs/tags/vnext','false'],['refs/heads/main','false'],['','false']]){
 const r=spawnSync('bash',[script,ref],{encoding:'utf8'});assert.equal(r.status,0,r.stderr);assert.equal(r.stdout.trim(),want,ref);
}
const r=spawnSync('bash',[script],{encoding:'utf8'});assert.equal(r.status,2);assert.equal(r.stdout,'');
console.log('Stable release promotion: versions, prereleases, branches and malformed refs passed');
