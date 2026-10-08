'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('decurixHost', {
  pickFolders: () => ipcRenderer.invoke('pick-host-folders'),
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
