// Optional browser verification: use an already-installed Playwright package.
// The application itself requires no JavaScript packages or build step.
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs');

(async () => {
 const base = process.argv[2];
 if (!base || !/^http:\/\/(127\.0\.0\.1|localhost):\d+$/.test(base)) throw new Error('Use the isolated web-preview localhost URL.');
 const out = process.argv[3] || 'docs/screenshots';
 fs.mkdirSync(out, { recursive:true });
 const browser = await chromium.launch({headless:true, executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || chromium.executablePath()});
 try {
  const context = await browser.newContext({viewport:{width:1200,height:1000},colorScheme:'dark',reducedMotion:'reduce'});
  context.setDefaultTimeout(10000);
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', err => errors.push(err.message));
  await page.goto(base);
  await page.locator('#connection.online').waitFor();
  await page.locator('#plan-meta').filter({hasText:'acme'}).waitFor();
  await page.locator('#attention').getByRole('heading',{name:'Choose the review scope'}).waitFor();
  assert.equal(await page.locator('#need-count').textContent(),'3');
  await page.screenshot({path:path.join(out,'web-desktop-dark.png')});
  await page.setViewportSize({width:390,height:844});
  await page.getByRole('button',{name:'Change color theme'}).click();
  assert.equal(await page.evaluate(() => document.documentElement.dataset.theme),'light');
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),true,'phone overflow');
  await page.screenshot({path:path.join(out,'web-mobile-light.png')});
  await page.locator('nav a[href="#plan"]').click();
  await page.evaluate(() => document.getElementById('plan').scrollIntoView());
  await page.screenshot({path:path.join(out,'web-plan-mobile-light.png')});
  await page.locator('#add-panel summary').click();
  await page.locator('#add-item input[name="item"]').fill('ACME-99');
  const title = '<img src=x onerror="window.injected=true">';
  await page.locator('#add-item input[name="title"]').fill(title);
  await page.locator('#add-item select[name="lane"]').selectOption('B');
  await page.getByRole('button',{name:'Add to plan',exact:true}).click();
  const row = page.locator('#plan-items [data-item="ACME-99"]');
  await row.waitFor();
  assert.equal(await row.locator('.item-title').textContent(),title);
  assert.equal(await row.locator('img').count(),0,'plan text must be escaped');
  assert.equal(await page.evaluate(() => window.injected),undefined);
  page.once('dialog', dialog => dialog.accept('Review pending'));
  await row.getByRole('button',{name:'Hold',exact:true}).click();
  await row.getByRole('button',{name:'Release',exact:true}).waitFor();
  await row.getByRole('button',{name:'Release',exact:true}).click();
  await row.getByRole('button',{name:'Hold',exact:true}).waitFor();
  const before = Number(await row.getAttribute('data-position'));
  await row.getByRole('button',{name:'Move ACME-99 up',exact:true}).click();
  await page.waitForFunction(expected => Number(document.querySelector('#plan-items [data-item="ACME-99"]').dataset.position) === expected,before - 1);
  // Native desktop drag shares the same server-backed move path.
  await page.setViewportSize({width:1200,height:1000});
  await row.evaluate(el => el.scrollIntoView({block:'center'}));
  await row.locator('.grip').dragTo(page.locator(`#plan-items [data-position="${before - 2}"]`));
  await page.waitForFunction(expected => Number(document.querySelector('#plan-items [data-item="ACME-99"]').dataset.position) === expected,before - 2).catch(async err => { throw new Error(`Drag expected ${before - 2}; saw ${await row.getAttribute('data-position')}; ${await page.locator('#notice').textContent()}`); });
  page.once('dialog', dialog => dialog.accept());
  await row.getByRole('button',{name:'Remove',exact:true}).click();
  await row.waitFor({state:'detached'});
  // Force a competing daemon edit after the browser has captured its versions.
  await page.route('**/api/plan/items/hold',async route => {
   const args = route.request().postDataJSON();
   const competing = await context.request.post(base+'/api/plan/items/hold',{data:{...args,reason:'Competing edit'}});
   assert.equal(competing.status(),200);
   await route.continue();
  });
  page.once('dialog', dialog => dialog.accept('Stale edit'));
  await page.locator('#plan-items [data-item="B1"]').getByRole('button',{name:'Hold',exact:true}).click();
  await page.locator('#notice').filter({hasText:'Your edit was refused'}).waitFor();
  await page.unroute('**/api/plan/items/hold');
  const snapshot = await (await context.request.get(base+'/api/plan')).json();
  const held = snapshot.view.items.find(i => i.id === 'B1');
  assert.equal(held.held_reason,'Competing edit');
  assert.deepEqual(errors,[],'browser errors');
  await context.close();
  // A reachable non-owner can read, but edit controls and writes are refused.
  const reader = await browser.newContext({extraHTTPHeaders:{'X-Remote-User':'guest'}});
  const readPage = await reader.newPage();
  await readPage.goto(base);
  await readPage.locator('#access').filter({hasText:'Read only'}).waitFor();
  assert.equal(await readPage.locator('#plan-items [data-edit]:enabled').count(),0);
  const refused = await reader.request.post(base+'/api/plan/items/release',{data:{project:'acme',day:'2026-03-14',item:'B1',if_version:held.version,if_plan_version:snapshot.plan.version}});
  assert.equal(refused.status(),403);
  await reader.close();
  console.log('PASS: phone layout, themes, embedded UI, add/hold/release/move/drag/remove, conflict refresh, escaped text and owner access.');
 } finally { await browser.close(); }
})().catch(err => { console.error(err); process.exitCode=1; });
