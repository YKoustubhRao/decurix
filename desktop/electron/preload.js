'use strict';

const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('decurixHost', {
  pickFolders: () => ipcRenderer.invoke('pick-host-folders'),
});

contextBridge.exposeInMainWorld('decurixStartup', {
  retry: () => ipcRenderer.send('retry-startup'),
  onStatus: (callback) => ipcRenderer.on('startup-status', (_event, status) => callback(status)),
});
