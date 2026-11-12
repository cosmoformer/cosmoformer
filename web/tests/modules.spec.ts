import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { moduleFixture } from './fixtures/modules';
import { org, project, env, reviewSession, projects } from './fixtures/review';

test('real catalog registration retains an exact revision across refresh and edit',async({page},info)=>{
 const c=JSON.parse(readFileSync('../.build/testdb/browser.json','utf8')),r=c.review.run;
 await page.goto('/o/'+c.org+'/p/'+r.project_id+'?view=modules&env='+r.environment_id);
 await page.getByLabel('Username',{exact:true}).fill(c.review_username);
 await page.getByLabel('Password',{exact:true}).fill(c.review_password);
 await page.getByRole('button',{name:'Sign in',exact:true}).click();
 await page.getByText('Register a module',{exact:true}).click();
 await page.getByLabel('Module name',{exact:true}).fill('Synthetic instance '+info.project.name);
 await page.getByLabel('Registered repository').selectOption({index:1});
 await page.getByLabel('Full commit',{exact:true}).fill(r.commit);
 await page.getByLabel('Module directory').fill('modules/instance');
 await page.getByLabel('Usage instructions',{exact:true}).fill('Synthetic module instructions for this exact commit.');
 await page.getByLabel('Example paths (one per line, up to 16)').fill('examples/basic');
 await page.getByRole('button',{name:'Register module',exact:true}).click();
 await expect(page.getByRole('heading',{name:'Usage at this revision'})).toBeVisible();
 const snippet=page.getByRole('textbox',{name:'Commit-pinned module snippet'});
 await expect(snippet).toHaveValue(new RegExp('ref='+r.commit));
 await expect(page.getByRole('link',{name:'examples/basic'})).toHaveAttribute('href',new RegExp('/src/'+r.commit+'/examples/basic'));
 await page.reload();await expect(snippet).toHaveValue(new RegExp('ref='+r.commit));
 await page.getByText('Edit catalog entry',{exact:true}).click();
 await page.getByRole('textbox',{name:'Usage instructions',exact:true}).fill('Updated usage for the same pinned source.');
 await page.getByRole('button',{name:'Save catalog revision'}).click();
 await expect(page.locator('.evidence-statement')).toHaveText('Updated usage for the same pinned source.');
 await expect(snippet).toHaveValue(new RegExp('ref='+r.commit));
 expect(page.url()).toContain('/modules/');
 await page.screenshot({path:info.outputPath('real-module-catalog.png'),fullPage:true});
});
test('catalog remains scoped and readable at wide/narrow sizes in both themes',async({page},info)=>{
 let revoked=false;
 await page.route('**/api/v1/**',async route=>{
  const path=new URL(route.request().url()).pathname;
  const send=(json:unknown,status=200)=>route.fulfill({json,status});
  if(path.endsWith('/session'))return send(reviewSession);
  if(path.endsWith('/projects'))return send(projects);
  if(path.endsWith('/activity'))return send({ok:true});
  if(revoked)return send({code:'FORBIDDEN',message:'Catalog access changed.'},403);
  if(path.includes('/modules/'))return send(moduleFixture);
  return send({modules:[moduleFixture],repositories:[],can_register:false,can_register_shared:false});
 });
 await page.goto('/o/'+org+'/p/'+project+'?view=modules&env='+env);
 await page.getByRole('link',{name:moduleFixture.name,exact:true}).click();
 await expect(page.getByRole('heading',{name:'Usage at this revision'})).toBeVisible();
 for(const theme of ['light','dark']){
  if(theme==='dark')await page.getByRole('button',{name:'Dark theme'}).click();
  for(const width of [1440,390]){
   await page.setViewportSize({width,height:900});
   await expect(page.getByRole('textbox',{name:'Commit-pinned module snippet'})).toBeVisible();
   expect(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth)).toBe(true);
   await page.screenshot({path:info.outputPath('module-'+width+'-'+theme+'.png'),fullPage:true});
  }
 }
 await expect(page.getByRole('link',{name:'examples/basic'})).toHaveAttribute('href',moduleFixture.examples[0].url);
 await expect(page.getByText(moduleFixture.instructions)).toBeVisible();
 expect(await page.locator('.evidence-statement img').count()).toBe(0);
 revoked=true;
 await expect(page.getByText('Catalog access changed.',{exact:true}).first()).toBeVisible({timeout:15000});
 await expect(page.getByRole('heading',{name:moduleFixture.name,exact:true})).toHaveCount(0);
});
