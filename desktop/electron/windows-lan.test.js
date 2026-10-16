'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { forwardedIn, forwardScript } = require('./windows-lan');

const table = [
  '',
  'Listen on ipv4:             Connect to ipv4:',
  '',
  'Address         Port        Address         Port',
  '--------------- ----------  --------------- ----------',
  '192.168.1.40    8090        172.26.37.209   8090',
  '10.0.0.5        8090        127.0.0.1       8090',
  '',
].join('\r\n');

test('a forward counts only when it goes to this computer\'s 127.0.0.1 on the same port', () => {
  assert.equal(forwardedIn(table, '10.0.0.5', 8090), true);
  // An older rule to WSL's own address breaks when WSL is given another.
  assert.equal(forwardedIn(table, '192.168.1.40', 8090), false);
  assert.equal(forwardedIn(table, '10.0.0.5', 8091), false);
  assert.equal(forwardedIn('', '10.0.0.5', 8090), false);
});

test('the elevated script replaces the rule and opens only the gated port', () => {
  const script = forwardScript('192.168.1.40', 8090);
  assert.match(script, /portproxy delete v4tov4 listenaddress=192\.168\.1\.40 listenport=8090/);
  assert.match(script, /portproxy add v4tov4 listenaddress=192\.168\.1\.40 listenport=8090 connectaddress=127\.0\.0\.1 connectport=8090/);
  assert.match(script, /New-NetFirewallRule -DisplayName 'Decurix LAN access \(8090\)'.*-LocalPort 8090 /);
  assert.doesNotMatch(script, /8080/);
});
