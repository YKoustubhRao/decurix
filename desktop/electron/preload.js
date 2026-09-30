'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('decurixStartup', {
  retry: () => ipcRenderer.send('retry-startup'),
  onStatus: (callback) => ipcRenderer.on('startup-status', (_event, status) => callback(status)),
});
