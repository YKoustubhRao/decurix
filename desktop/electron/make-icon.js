'use strict';

// Regenerates icon.ico and icon.png from web/favicon.svg (the brand mark), so the installer,
// the shortcuts and the window show Decurix's icon instead of Electron's.
// Run after the mark changes:  npx electron make-icon.js
// Both are committed; the build does not run this.

const { app, BrowserWindow, nativeTheme } = require('electron');
const fs = require('node:fs');
const path = require('node:path');

const sizes = [16, 24, 32, 48, 64, 128, 256];
// The Mac and Linux builds take one large PNG (electron-builder makes the
// .icns and the menu icons from it).
const large = 1024;

// ico packs PNGs behind a 6-byte header and one 16-byte entry per size.
function ico(images) {
  const header = Buffer.alloc(6 + 16 * images.length);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(images.length, 4);
  let offset = header.length;
  images.forEach(({ size, png }, i) => {
    const entry = 6 + 16 * i;
    header.writeUInt8(size === 256 ? 0 : size, entry);
    header.writeUInt8(size === 256 ? 0 : size, entry + 1);
    header.writeUInt16LE(1, entry + 4);
    header.writeUInt16LE(32, entry + 6);
    header.writeUInt32LE(png.length, entry + 8);
    header.writeUInt32LE(offset, entry + 12);
    offset += png.length;
  });
  return Buffer.concat([header, ...images.map((image) => image.png)]);
}

app.disableHardwareAcceleration();
app.whenReady().then(async () => {
  // The mark has a dark-scheme palette; an icon is always the light one.
  nativeTheme.themeSource = 'light';
  const svg = fs.readFileSync(path.join(__dirname, '..', '..', 'web', 'favicon.svg'));
  const window = new BrowserWindow({ show: false, webPreferences: { sandbox: true } });
  await window.loadURL('about:blank');
  // Each size is drawn from the vector, not scaled down from the largest.
  const drawn = await window.webContents.executeJavaScript(`(async () => {
    const image = new Image();
    image.src = 'data:image/svg+xml;base64,${svg.toString('base64')}';
    await image.decode();
    return ${JSON.stringify([...sizes, large])}.map((size) => {
      const canvas = document.createElement('canvas');
      canvas.width = canvas.height = size;
      canvas.getContext('2d').drawImage(image, 0, 0, size, size);
      return canvas.toDataURL('image/png').split(',')[1];
    });
  })()`);
  const images = sizes.map((size, i) => ({ size, png: Buffer.from(drawn[i], 'base64') }));
  fs.writeFileSync(path.join(__dirname, 'icon.ico'), ico(images));
  console.log(`icon.ico: ${sizes.join(', ')} px`);
  fs.writeFileSync(path.join(__dirname, 'icon.png'), Buffer.from(drawn[sizes.length], 'base64'));
  console.log(`icon.png: ${large} px`);
  app.quit();
}).catch((error) => { console.error(error); app.exit(1); });
