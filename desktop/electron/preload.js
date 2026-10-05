'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('decurixStartup', {
  retry: () => ipcRenderer.send('retry-startup'),
  onStatus: (callback) => ipcRenderer.on('startup-status', (_event, status) => callback(status)),
});

contextBridge.exposeInMainWorld('decurixNative', {
  openExplorer: (targetPath) => ipcRenderer.invoke('open-explorer', targetPath),
  pickPaths: (options) => ipcRenderer.invoke('pick-paths', options),
});
