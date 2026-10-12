'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('decurixHost', {
  pickFolders: () => ipcRenderer.invoke('pick-host-folders'),
  localModelStatus: () => ipcRenderer.invoke('local-model-status'),
  setLocalHosting: enabled => ipcRenderer.invoke('local-model-hosting', enabled),
  prepareLocalModels: () => ipcRenderer.invoke('local-model-prepare'),
});

contextBridge.exposeInMainWorld('decurixStartup', {
  retry: () => ipcRenderer.send('retry-startup'),
  onStatus: (callback) => ipcRenderer.on('startup-status', (_event, status) => callback(status)),
});

// The web dashboard stays ordinary browser content; only the Windows shell
// opts into the integrated caption controls and draggable header.
if (process.platform === 'win32') {
  window.addEventListener('DOMContentLoaded', () => {
    document.documentElement.classList.add('desktop-windows');
    const header = document.querySelector('.topbar');
    if (!header) return;
    const hostingStyle = document.createElement('style');
    hostingStyle.textContent = "\n.lh-shell{--lh-rail:48px;--lh-panel:300px;padding-left:var(--lh-rail)}\n.lh-shell>.topbar{margin-left:calc(-1 * var(--lh-rail))}\n.lh-shell.lh-sidebar-open{padding-left:calc(var(--lh-rail) + var(--lh-panel))}\n.lh-shell.lh-sidebar-open>.topbar{margin-left:calc(-1 * (var(--lh-rail) + var(--lh-panel)))}\n.lh-rail[hidden]{display:none}.lh-rail{position:fixed;left:0;top:53px;bottom:0;width:48px;z-index:38;display:flex;flex-direction:column;align-items:center;background:var(--surface-2,#f3f3f7);border-right:1px solid var(--line,#ddd)}\n.lh-rail-button{position:relative;width:47px;height:52px;display:grid;place-items:center;padding:0;border:0;border-left:2px solid transparent;background:transparent;color:var(--text-3,#888);cursor:pointer}\n.lh-rail-button[aria-expanded=true]{color:var(--text,#252532);border-left-color:var(--accent,#136c4b);background:var(--surface,#fff)}\n.lh-rail-button:hover{color:var(--text,#252532)}.lh-rail-button:focus-visible,.lh-close:focus-visible{outline:2px solid var(--accent,#136c4b);outline-offset:-4px}\n.lh-rail-button svg{width:22px;height:22px}.lh-dot{position:absolute;right:9px;bottom:12px;width:6px;height:6px;border-radius:50%;background:#92909e;box-shadow:0 0 0 2px var(--surface-2,#f3f3f7)}.lh-dot.online{background:var(--live,#12724d)}\n.lh-sr-only{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap}\n.lh-sidebar{position:fixed;left:48px;top:53px;bottom:0;width:300px;z-index:37;display:flex;flex-direction:column;background:var(--surface,#fff);color:var(--text,#252532);border-right:1px solid var(--line,#ddd)}\n.lh-sidebar[hidden]{display:none}.lh-sidebar-head{height:44px;min-height:44px;padding:0 12px 0 20px;display:flex;align-items:center;justify-content:space-between;border-bottom:1px solid var(--line,#ddd)}\n.lh-sidebar-title{margin:0;font:600 10px system-ui;letter-spacing:1.4px;text-transform:uppercase;color:var(--text-2,#666)}\n.lh-close{width:28px;height:28px;border:0;border-radius:5px;background:transparent;color:var(--text-3,#888);font:22px system-ui;cursor:pointer}.lh-close:hover{background:var(--surface-2,#f3f3f7);color:var(--text,#252532)}\n.lh-sidebar-content{overflow-y:auto;overscroll-behavior:contain;flex:1;min-height:0}\n.lh-card{position:relative;isolation:isolate;padding:24px 20px;font-family:inherit;overflow:hidden}.lh-card:before{content:\"\";position:absolute;z-index:-1;width:360px;height:280px;right:-200px;top:-220px;background:radial-gradient(circle,var(--accent-soft,#136c4b1a),transparent 70%);pointer-events:none}\n.lh-kicker{font:600 9px system-ui;letter-spacing:1.5px;text-transform:uppercase;color:var(--accent,#136c4b);margin-bottom:10px}.lh-card h2{font-size:22px;font-weight:600;letter-spacing:-.7px;line-height:1.2;margin:0 0 10px}.lh-copy{font-size:12px;line-height:1.65;margin:0 0 24px;color:var(--text-3,#838390)}\n.lh-control{display:flex;align-items:center;justify-content:space-between;gap:14px;margin-bottom:22px}.lh-control strong{display:block;font-size:13px;font-weight:600}.lh-control small{display:block;font-size:10px;color:var(--text-3,#838390);margin-top:4px}\n.lh-switch{position:relative;display:inline-flex;cursor:pointer;flex-shrink:0}.lh-switch input{position:absolute;opacity:0;width:42px;height:24px;margin:0;cursor:pointer}.lh-track{display:block;width:42px;height:24px;border-radius:99px;background:#9494a338;transition:background .2s}.lh-track:after{content:\"\";position:absolute;left:3px;top:3px;width:18px;height:18px;border-radius:50%;background:#fff;box-shadow:0 1px 4px #0003;transition:transform .2s}.lh-switch input:checked+.lh-track{background:var(--accent,#136c4b)}.lh-switch input:checked+.lh-track:after{transform:translateX(18px)}.lh-switch input:focus-visible+.lh-track{outline:3px solid var(--ring,#136c4b4d);outline-offset:3px}.lh-switch input:disabled+.lh-track{opacity:.5}\n.lh-metrics{display:flex;flex-direction:column;border-top:1px solid var(--line,#ddd);border-bottom:1px solid var(--line,#ddd);padding:8px 0}.lh-metric{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:8px 0}.lh-metric span{font-size:11px;color:var(--text-3,#838390)}.lh-metric strong{font-size:11px;font-weight:500;text-align:right;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.lh-model{font:500 11px ui-monospace,monospace;color:var(--text-2,#666);margin:14px 0 22px;overflow-wrap:anywhere}\n.lh-footer{padding-top:20px;border-top:1px solid var(--line,#ddd)}.lh-steps{display:flex;flex-direction:column;gap:16px;font-size:11px;color:var(--text-3,#838390);margin-bottom:22px}.lh-step{display:flex;align-items:center;gap:10px}.lh-step i{width:6px;height:6px;border-radius:50%;background:#a0a0ae55}.lh-step.active{color:var(--accent,#136c4b)}.lh-step.active i{background:var(--accent,#136c4b);animation:lh-pulse 1.3s infinite}.lh-step.done i{background:var(--live,#12724d)}\n.lh-status{font-size:11px;line-height:1.65;margin:0;overflow-wrap:anywhere;color:var(--text-2,#666);max-height:180px;overflow-y:auto}.lh-policy{font-size:10px;line-height:1.6;color:var(--text-3,#838390);margin:14px 0 0}.lh-retry{margin-top:10px;border:1px solid var(--line,#ddd);border-radius:6px;background:var(--surface-2,#f7f7fb);color:var(--text,#20202b);font:500 11px system-ui;padding:7px 10px;cursor:pointer}.lh-retry[hidden]{display:none}\n@keyframes lh-pulse{50%{opacity:.3}}@media(max-width:1000px){.lh-shell.lh-sidebar-open{padding-left:var(--lh-rail)}.lh-shell.lh-sidebar-open>.topbar{margin-left:calc(-1 * var(--lh-rail))}.lh-sidebar{box-shadow:12px 0 32px #0002}}\n@media(prefers-reduced-motion:reduce){.lh-step.active i{animation:none}.lh-track,.lh-track:after{transition:none}}\n";
    document.head.append(hostingStyle);
    const controls = [];
    const element = (tag, className, text) => {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text) node.textContent = text;
      return node;
    };
    const makeCard = () => {
      const card = element('section', 'lh-card');
      card.setAttribute('aria-label', 'Local intelligence');
      const intro = element('div');
      intro.append(element('div', 'lh-kicker', 'Local intelligence'), element('h2', '', 'Local intelligence'),
        element('p', 'lh-copy', 'A model chosen for this computer. Bring it online when you need it.'));
      const right = element('div');
      const control = element('div', 'lh-control');
      const description = element('div');
      description.append(element('strong', '', 'Host locally'), element('small', '', 'Your hardware. Your model.'));
      const label = element('label', 'lh-switch');
      const toggle = element('input');
      toggle.type = 'checkbox';
      toggle.setAttribute('role', 'switch');
      toggle.setAttribute('aria-label', 'Host local model');
      label.append(toggle, element('span', 'lh-track'));
      control.append(description, label);
      const metrics = element('div', 'lh-metrics');
      const values = ['Memory available', 'GPU available', 'CPU cores'].map(name => {
        const metric = element('div', 'lh-metric');
        const value = element('strong', '', '—');
        metric.append(element('span', '', name), value);
        metrics.append(metric);
        return value;
      });
      const model = element('p', 'lh-model', 'Model selected when hosting starts');
      right.append(control, metrics, model);
      const footer = element('div', 'lh-footer');
      const steps = element('div', 'lh-steps');
      const stepNodes = ['Runtime installed', 'Model prepared', 'Hosting live'].map(name => {
        const node = element('span', 'lh-step');
        node.append(element('i'), element('span', '', name));
        steps.append(node);
        return node;
      });
      const live = element('div');
      const status = element('p', 'lh-status');
      status.setAttribute('role', 'status');
      const policy = element('p', 'lh-policy', 'Turning off unloads the model and deletes its downloads. Ollama stays installed.');
      const retry = element('button', 'lh-retry', 'Retry setup');
      retry.type = 'button';
      retry.hidden = true;
      live.append(status, policy, retry);
      footer.append(steps, live);
      card.append(intro, right, footer);
      toggle.addEventListener('change', () => perform(toggle.checked));
      retry.addEventListener('click', () => perform(toggle.checked));
      controls.push({ card, toggle, values, model, status, stepNodes, retry });
      return card;
    };
    const rail = element('nav', 'lh-rail');
    rail.setAttribute('aria-label', 'Workspace tools');
    const trigger = element('button', 'lh-rail-button');
    trigger.type = 'button';
    trigger.title = 'Local model setup (Ctrl+Shift+L)';
    trigger.setAttribute('aria-controls', 'localModelSidebar');
    const icon = element('span');
    icon.setAttribute('aria-hidden', 'true');
    icon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v4m6-4v4M9 18v4m6-4v4M2 9h4m-4 6h4m12-6h4m-4 6h4"/><path d="M10 10h4v4h-4z"/></svg>';
    const dot = element('span', 'lh-dot');
    dot.setAttribute('aria-hidden', 'true');
    const chipText = element('span', 'lh-sr-only', 'Local model setup · Off');
    trigger.append(icon, dot, chipText);
    rail.append(trigger);
    const sidebar = element('aside', 'lh-sidebar');
    sidebar.id = 'localModelSidebar';
    sidebar.setAttribute('aria-labelledby', 'localModelSidebarTitle');
    const sidebarHead = element('div', 'lh-sidebar-head');
    const sidebarTitle = element('h2', 'lh-sidebar-title', 'Local model setup');
    sidebarTitle.id = 'localModelSidebarTitle';
    const close = element('button', 'lh-close', '×');
    close.type = 'button';
    close.title = 'Close sidebar';
    close.setAttribute('aria-label', 'Close local model setup');
    sidebarHead.append(sidebarTitle, close);
    const content = element('div', 'lh-sidebar-content');
    content.append(makeCard());
    sidebar.append(sidebarHead, content);
    document.body.append(rail, sidebar);
    document.body.classList.add('lh-shell');
    let isOpen = true;
    try { isOpen = window.localStorage.getItem('decurix.localModelSidebar') !== 'closed'; } catch { /* optional preference */ }
    const setOpen = (value, focus = false) => {
      isOpen = value;
      sidebar.hidden = !value;
      trigger.setAttribute('aria-expanded', String(value));
      document.body.classList.toggle('lh-sidebar-open', value);
      try { window.localStorage.setItem('decurix.localModelSidebar', value ? 'open' : 'closed'); } catch { /* optional preference */ }
      if (focus) (value ? close : trigger).focus();
    };
    setOpen(isOpen);
    trigger.addEventListener('click', () => setOpen(!isOpen, true));
    close.addEventListener('click', () => setOpen(false, true));
    document.addEventListener('keydown', event => {
      if ((event.ctrlKey || event.metaKey) && event.shiftKey && event.key.toLowerCase() === 'l') {
        event.preventDefault();
        setOpen(!isOpen, true);
      } else if (event.key === 'Escape' && isOpen && sidebar.contains(event.target)) {
        setOpen(false, true);
      }
    });
    const render = state => {
      const ready = state.phase === 'ready';
      chipText.textContent = 'Local model setup · ' + (ready ? 'Live' : state.phase === 'error' ? 'Needs attention' : state.busy ? 'Preparing' : 'Off');
      dot.classList.toggle('online', ready);
      const r = state.resources;
      for (const c of controls) {
        c.toggle.checked = state.enabled;
        c.toggle.disabled = state.phase === 'stop';
        c.status.textContent = state.detail || 'Installing Ollama in the background…';
        c.model.textContent = state.model || 'Model selected when hosting starts';
        c.values[0].textContent = r ? r.ram_free_gb.toFixed(1) + ' GB' : '—';
        c.values[1].textContent = r ? r.gpu_name ? (r.vram_free_gb || 0).toFixed(1) + ' GB VRAM' : 'CPU inference' : '—';
        c.values[1].title = r && r.gpu_name || '';
        c.values[2].textContent = r ? String(r.cores) : '—';
        c.stepNodes.forEach((node, i) => {
          node.classList.toggle('active', (i === 0 && state.phase === 'install') || (i === 1 && ['ensure', 'prepare'].includes(state.phase)));
          node.classList.toggle('done', i === 0 ? state.runtimeInstalled : ready);
        });
        c.retry.hidden = state.phase !== 'error';
        c.retry.disabled = state.busy;
      }
    };
    const refresh = async () => {
      try { render(await ipcRenderer.invoke('local-model-status')); }
      catch { rail.hidden = true; sidebar.hidden = true; document.body.classList.toggle('lh-shell', false); document.body.classList.toggle('lh-sidebar-open', false); }
    };
    const perform = async enabled => {
      for (const c of controls) c.status.textContent = enabled ? 'Preparing local intelligence…' : 'Unloading and deleting local models…';
      try { render(await ipcRenderer.invoke('local-model-hosting', enabled)); }
      catch (error) { await refresh(); for (const c of controls) c.status.textContent = error.message; }
    };
    refresh();
    setInterval(refresh, 1500);
    const syncTheme = () => {
      const style = getComputedStyle(header);
      ipcRenderer.send('window-titlebar-theme', {
        color: style.getPropertyValue('--surface').trim(),
        symbolColor: style.getPropertyValue('--text').trim(),
      });
    };
    syncTheme();
    new MutationObserver(syncTheme).observe(document.documentElement, {
      attributes: true, attributeFilter: ['data-theme'],
    });
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', syncTheme);
  });
}
