'use strict';
const $ = id => document.getElementById(id);
const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
const chip = state => `<span class="chip ${esc(['blocked','held','bounced','working','dispatched','done','merged','settled','ratified'].includes(state) ? state : '')}">${esc(state || 'unknown')}</span>`;
const empty = text => `<p class="empty">${esc(text)}</p>`;
const age = (start, end) => {
 const mins = Math.max(0, Math.floor(((end || Date.now()) - start) / 60000));
 return mins < 60 ? `${mins}m` : `${Math.floor(mins / 60)}h ${mins % 60}m`;
};
const date = ts => new Date(ts).toLocaleString();
let plan = null, activity = null, canWrite = false, online = false, busy = false, principal = '', planRequest = 0, dragging = null;
let scope = new URLSearchParams(location.search);
$('scope').elements.project.value = scope.get('project') || '';
$('scope').elements.day.value = scope.get('day') || '';
function notice(text) { $('notice').textContent = text; $('notice').hidden = !text; }
function connection(live) {
 online = live;
 $('connection').textContent = live ? '● Live' : 'Reconnecting…';
 $('connection').classList.toggle('online', live);
 access();
}
function access() {
 const editable = canWrite && online && !busy && plan && plan.plan.status !== 'final';
 document.querySelectorAll('[data-edit], #add-item input, #add-item select').forEach(el => el.disabled = !editable);
 document.querySelectorAll('.grip').forEach(el => el.draggable = Boolean(editable));
 $('access').textContent = !online ? 'Edits paused while reconnecting.' : !canWrite ? 'Read only · owner identity required for edits.' : plan?.plan.status === 'final' ? 'Final plan · edits are closed.' : `Editing as ${principal} · every edit checks the current version.`;
}
async function json(path, options) {
 const res = await fetch(path, options);
 const body = await res.json();
 if (!res.ok) { const err = new Error(body.message || 'Request failed'); err.status = res.status; throw err; }
 return body;
}
async function identity() {
 try { const value = await json('/api/identity'); canWrite = value.can_write; principal = value.principal; }
 catch { canWrite = false; }
 access();
}
async function refreshPlan() {
 const request = ++planRequest;
 const query = scope.toString();
 try {
  const value = await json('/api/plan?' + query);
  if (request !== planRequest) return;
  if (plan?.plan.id === value.plan.id && plan.plan.version === value.plan.version) return;
  plan = value;
  renderPlan();
  if (activity) renderWorkers();
 } catch (err) {
  if (request !== planRequest) return;
  plan = null;
  $('plan-meta').textContent = '';
  $('plan-items').innerHTML = empty(err.status === 404 ? 'No plan yet. Import a plan with horch plan import.' : err.message);
  ['lane-summary','held-items','decisions','plan-notes','timeline'].forEach(id => $(id).replaceChildren());
  ['held-count','event-count'].forEach(id => $(id).textContent = '');
  access();
 }
}
function workerState(w) {
 if (!['live','release_failed'].includes(w.state)) return 'done';
 return w.live_status || 'unknown';
}
function modelFor(w) {
 const d = w.dispatch;
 const item = d && plan?.view?.items.find(i => i.dispatch_ref.includes(`/${d.id}@${w.pane_id}`));
 return item?.model || 'Not reported';
}
function workerCard(w) {
 return `<article class="card"><div class="row-head"><h3>${esc(w.name || w.pane_id)}</h3>${chip(workerState(w))}</div>
  <p>${esc(w.task?.title || 'No active task')}</p><p class="mono">${esc(w.worktree || 'Worktree not reported')}</p>
  <small>${esc(w.agent || 'Agent unknown')} · model: ${esc(modelFor(w))} · ${age(w.started_at, w.released_at)} ${w.released_at ? 'total' : 'elapsed'}</small>
  ${w.note ? `<p class="body">${esc(w.note)}</p>` : ''}</article>`;
}
function messageCard(m, attention = false) {
 return `<article class="card ${attention ? 'attention' : ''}"><div class="row-head"><h3>${esc(m.subject || m.kind)}</h3>${chip(m.kind)}</div>
  <small>${esc(m.from)} → ${esc(m.to)} · ${date(m.created_at)}</small><p class="body">${esc(m.body)}</p><small class="mono">${esc(m.id)}</small></article>`;
}
function renderWorkers() {
 const live = activity.workers.filter(w => ['live','release_failed'].includes(w.state));
 const history = activity.workers.filter(w => !['live','release_failed'].includes(w.state));
 $('worker-count').textContent = `${live.length} live`;
 $('live-workers').innerHTML = live.map(workerCard).join('') || empty('No live workers.');
 $('history-count').textContent = `(${history.length})`;
 $('worker-history').innerHTML = history.slice().reverse().map(workerCard).join('') || empty('No worker history.');
}
function renderActivity(value) {
 activity = value;
 const blocked = value.workers.filter(w => ['live','release_failed'].includes(w.state) && w.live_status === 'blocked');
 const attention = blocked.map(w => `<article class="card attention"><div class="row-head"><h3>${esc(w.name || w.pane_id)} is blocked</h3>${chip('blocked')}</div><p>${esc(w.task?.title || 'Worker waiting at a prompt')}</p><small>Inspect prompt: <span class="mono">horch worker read --worker ${esc(w.pane_id)}</span></small></article>`);
 attention.push(...value.needs.map(m => messageCard(m, true)));
 attention.push(...value.gates.map(g => `<article class="card attention"><div class="row-head"><h3>${esc(g.question)}</h3>${chip(g.status)}</div><p>${esc((g.options || []).join(' · '))}</p><small class="mono">Gate ${esc(g.id)} · task ${esc(g.task_id || 'none')}</small><p class="muted">Resolve with horch gate resolve.</p></article>`));
 $('need-count').textContent = String(attention.length);
 $('attention').innerHTML = attention.join('') || empty('All clear. No open prompts, asks, escalations or gates.');
 renderWorkers();
 $('messages').innerHTML = value.messages.slice().reverse().map(m => messageCard(m)).join('') || empty('No messages yet.');
}
function itemCard(item, held = false) {
 const p = plan.plan;
 return `<article class="card plan-row" data-item="${esc(item.id)}" data-version="${item.version}" data-plan-version="${p.version}" data-position="${item.position}">
  <button class="grip" data-edit aria-label="Drag ${esc(item.id)} to reorder" title="Drag to reorder; use Up and Down on a phone">⠿</button>
  <div><div class="row-head"><span class="mono">${item.position}. ${esc(item.id)}</span>${chip(item.state)}</div>
  <div class="item-title">${esc(item.title || item.what?.output || item.id)}</div>
  <small>${esc(item.lane || 'Unassigned')} · ${esc(item.kind || 'item')} · ${esc(item.model || 'Default model')}</small>
  ${item.issues?.length ? `<p class="mono">${esc(item.issues.join(', '))}</p>` : ''}
  ${item.why ? `<p class="body">${esc(item.why)}</p>` : ''}
  ${item.held_reason ? `<p class="body">Held: ${esc(item.held_reason)}</p>` : ''}
  <div class="actions"><button data-edit data-action="up" aria-label="Move ${esc(item.id)} up">↑ Up</button><button data-edit data-action="down" aria-label="Move ${esc(item.id)} down">↓ Down</button>
  <button data-edit data-action="${held ? 'release' : 'hold'}">${held ? 'Release' : 'Hold'}</button><button data-edit data-action="remove" class="danger">Remove</button></div></div></article>`;
}
function renderPlan() {
 const view = plan.view;
 $('plan-meta').textContent = `${plan.plan.project} · ${plan.plan.day} · v${plan.plan.version} · ${plan.plan.status}`;
 const counts = new Map([['local',0],['A',0],['B',0]]);
 view.items.forEach(i => counts.set(i.lane || 'Unassigned', (counts.get(i.lane || 'Unassigned') || 0) + 1));
 $('lane-summary').innerHTML = [...counts].map(([lane,count]) => `<div class="lane">${esc(['A','B'].includes(lane) ? `Lane ${lane}` : lane)} <strong>${count}</strong></div>`).join('');
 // Ordered list includes held rows, so a drag target always means its actual position.
 $('plan-items').innerHTML = view.items.filter(i => i.listed).map(i => itemCard(i,i.held)).join('') || empty('No ordered items. Add the first item below.');
 const held = view.items.filter(i => i.held);
 $('held-count').textContent = `(${held.length})`;
 $('held-items').innerHTML = held.map(i => i.listed ? `<article class="card"><h3>${esc(i.id)} · ${esc(i.title)}</h3><p class="body">${esc(i.held_reason)}</p><small>Release using the ordered row above.</small></article>` : itemCard(i,true)).join('') || empty('Nothing held.');
 $('decisions').innerHTML = view.decisions.map(d => `<article class="card"><h3>${esc(d.title)}</h3><p class="body">${esc(d.body)}</p></article>`).join('') || empty('No decisions recorded.');
 $('plan-notes').innerHTML = plan.plan.sections.filter(s => s.kind === 'text').map(s => `<article class="card"><h3>${esc(s.heading)}</h3><p class="body">${esc(s.body)}</p></article>`).join('') || empty('No additional notes.');
 $('event-count').textContent = `(${plan.events.length})`;
 $('timeline').innerHTML = plan.events.slice().reverse().map(e => `<li><strong>${esc(e.op)} ${esc(e.item)}</strong> · v${e.plan_version}<br><small>${date(e.ts)} · ${esc(e.principal || e.actor)}${e.approval ? ' · owner approval' : ''}</small>${e.note ? `<p class="body">${esc(e.note)}</p>` : ''}</li>`).join('');
 access();
}
function captured(row) {
 return { project:plan.plan.project, day:plan.plan.day, item:row.dataset.item, if_version:Number(row.dataset.version), if_plan_version:Number(row.dataset.planVersion) };
}
async function edit(action, args) {
 if (!canWrite || !online || busy || !plan) return;
 busy = true;
 access();
 try {
  await json(`/api/plan/items/${action}`, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(args)});
  notice('Plan updated.');
  await refreshPlan();
 } catch (err) {
  if (err.status === 409) { notice('The plan changed. Your edit was refused; the latest plan is loaded. Review and try again.'); await refreshPlan(); }
  else { notice(err.message); if (err.status === 403) await identity(); }
 } finally { busy = false; access(); }
}
$('plan').addEventListener('click', event => {
 const button = event.target.closest('button[data-action]');
 if (!button || button.disabled) return;
 const row = button.closest('[data-item]');
 const args = captured(row);
 let action = button.dataset.action;
 if (action === 'up' || action === 'down') { args.position = Math.max(1, Number(row.dataset.position) + (action === 'up' ? -1 : 1)); action = 'move'; }
 if (action === 'hold') { args.reason = prompt('Why hold this item?'); if (args.reason === null) return; if (!args.reason.trim()) { notice('Enter a hold reason.'); return; } }
 if (action === 'remove' && !confirm(`Remove ${args.item} from the plan? Its event history is kept.`)) return;
 edit(action,args);
});
$('add-item').addEventListener('submit', async event => {
 event.preventDefault();
 if (!plan) return;
 const fields = new FormData(event.target);
 const args = {project:plan.plan.project,day:plan.plan.day,if_plan_version:plan.plan.version,item:fields.get('item').trim(),patch:{title:fields.get('title').trim(),kind:fields.get('kind'),lane:fields.get('lane'),model:fields.get('model').trim(),issues:fields.get('issues').split(',').map(s => s.trim()).filter(Boolean)}};
 await edit('add',args);
});
$('scope').addEventListener('submit', event => {
 event.preventDefault();
 scope = new URLSearchParams();
 const fields = new FormData(event.target);
 for (const key of ['project','day']) if (fields.get(key).trim()) scope.set(key,fields.get(key).trim());
 history.replaceState(null,'',location.pathname + (scope.size ? '?' + scope : ''));
 plan = null;
 access();
 refreshPlan();
});
// Native desktop drag and touch grip gestures share the same version-bound edit.
$('plan').addEventListener('dragstart', event => {
 const grip = event.target.closest('.grip');
 if (!grip || !canWrite || !online || busy) { event.preventDefault(); return; }
 dragging = captured(grip.closest('[data-item]'));
 event.dataTransfer.setData('text/plain',dragging.item);
 event.dataTransfer.effectAllowed = 'move';
});
$('plan').addEventListener('dragover', event => { if (dragging && event.target.closest('[data-item]')) event.preventDefault(); });
$('plan').addEventListener('drop', event => {
 event.preventDefault();
 const row = event.target.closest('[data-item]');
 if (dragging && row && row.dataset.item !== dragging.item) edit('move',{...dragging,position:Number(row.dataset.position)});
 dragging = null;
});
$('plan').addEventListener('dragend', () => { dragging = null; });
$('plan').addEventListener('pointerdown', event => {
 const grip = event.target.closest('.grip');
 if (event.pointerType !== 'touch' || !grip || grip.disabled) return;
 dragging = captured(grip.closest('[data-item]'));
 grip.setPointerCapture(event.pointerId);
});
$('plan').addEventListener('pointerup', event => {
 if (event.pointerType !== 'touch' || !dragging) return;
 const row = document.elementFromPoint(event.clientX,event.clientY)?.closest('[data-item]');
 if (row && row.dataset.item !== dragging.item) edit('move',{...dragging,position:Number(row.dataset.position)});
 dragging = null;
});
$('plan').addEventListener('pointercancel', event => { if (event.pointerType === 'touch') dragging = null; });
try { const theme = localStorage.getItem('horch-theme'); if (theme) document.documentElement.dataset.theme = theme; } catch {}
$('theme').addEventListener('click', () => {
 const dark = document.documentElement.dataset.theme === 'dark' || (!document.documentElement.dataset.theme && matchMedia('(prefers-color-scheme: dark)').matches);
 const theme = dark ? 'light' : 'dark';
 document.documentElement.dataset.theme = theme;
 try { localStorage.setItem('horch-theme',theme); } catch {}
});
async function refreshAll() {
 await identity();
 await Promise.all([refreshPlan(),json('/api/activity').then(renderActivity).catch(err => { notice(err.message); connection(false); })]);
}
const events = new EventSource('/api/events');
events.addEventListener('ready', () => { connection(true); refreshAll(); });
events.addEventListener('plan', () => { refreshPlan(); });
events.addEventListener('activity', event => { renderActivity(JSON.parse(event.data)); });
events.addEventListener('unavailable', () => { connection(false); });
events.onerror = () => { connection(false); };
refreshAll();
