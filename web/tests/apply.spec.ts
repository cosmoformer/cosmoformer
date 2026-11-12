import {test,expect,type Page} from '@playwright/test';
import {approvalSummary,reviewSession,projects,runResponse,org,project,env,run,reviewID} from './fixtures/review';
const path=`/o/${org}/p/${project}/runs/${run}?env=${env}`;
async function fixture(page:Page){
 const m={summary:{...structuredClone(approvalSummary),can_apply:true,counted_approvals:2,apply_blocked_reasons:[] as string[]},mutations:[] as any[],status:202,revoked:false};
 await page.route('**/api/v1/**',async route=>{
  const r=route.request(),p=new URL(r.url()).pathname;const send=(json:unknown,status=200)=>route.fulfill({json,status});
  if(p.endsWith('/session'))return send(reviewSession);if(p.endsWith('/projects'))return send(projects);
  if(p.endsWith('/activity'))return send({ok:true});
  if(r.method()!=='GET'){
   m.mutations.push(r.postDataJSON());if(m.status!==202)return send({code:'EXPLICIT_ACTION_REQUIRED',message:'Check operation history before confirming again.'},m.status);
   m.summary.can_apply=false;m.summary.apply_blocked_reasons=['APPLY_ALREADY_PENDING'];
   return send({request_id:reviewID(31),subject_id:run,operation_id:reviewID(30),revision:1,status:'queued'},202);
  }
  if(m.revoked)return send({code:'FORBIDDEN',message:'Access changed.'},403);
  if(p.endsWith('/approvals'))return send(m.summary);return send(runResponse);
 });return m;
}
test('saved-plan confirmation fits both themes at wide and narrow sizes',async({page},info)=>{
 const m=await fixture(page);await page.goto(path);await page.getByRole('button',{name:'Apply',exact:true}).click();const dialog=page.getByRole('dialog');
 for(const theme of ['light','dark']){
  if(theme==='dark'){await page.keyboard.press('Escape');await page.getByRole('button',{name:'Dark theme'}).click();await page.getByRole('button',{name:'Apply',exact:true}).click();}
  for(const [name,width,height] of [['wide',1440,1000],['narrow',390,844]] as const){
   await page.setViewportSize({width,height});await expect(dialog.getByText(runResponse.run.commit,{exact:true})).toBeVisible();
   expect(await dialog.evaluate(e=>e.scrollWidth<=e.clientWidth)).toBe(true);
   await page.screenshot({path:info.outputPath(`apply-${name}-${theme}.png`),fullPage:true});
  }
 }
 await dialog.getByLabel('Apply priority').selectOption('high');await dialog.getByRole('button',{name:'Confirm Apply'}).click();
 await expect(page.getByText('Apply accepted for this exact saved plan.')).toBeVisible();
 expect(m.mutations).toHaveLength(1);expect(m.mutations[0]).toEqual({expected_plan_revision:2,manifest_digest:runResponse.run.plan!.manifest_digest,priority:'high',confirmed_context:{stack_id:runResponse.run.stack_id,environment_id:env,source_commit:runResponse.run.commit}});
 await page.reload();await expect(page.getByRole('button',{name:'Apply',exact:true})).toBeDisabled();expect(m.mutations).toHaveLength(1);
});
test('failed Apply is never replayed after reconnect and authorization loss clears context',async({page,context})=>{
 const m=await fixture(page);m.status=503;await page.goto(path);await page.getByRole('button',{name:'Apply',exact:true}).click();await page.getByRole('button',{name:'Confirm Apply'}).click();
 await expect(page.getByRole('dialog')).toHaveCount(0);await context.setOffline(true);await page.waitForFunction(()=>!navigator.onLine);await context.setOffline(false);await page.reload();await expect(page.getByRole('button',{name:'Apply',exact:true})).toBeVisible();expect(m.mutations).toHaveLength(1);
 m.revoked=true;await expect(page.getByText('Protected content has been cleared. Reopen this run to check current access.')).toBeVisible({timeout:10000});await expect(page.getByRole('button',{name:'Apply',exact:true})).toHaveCount(0);
});
