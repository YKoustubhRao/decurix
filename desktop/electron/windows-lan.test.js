'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { checkScript, forwardedIn, forwardScript, ensureLanForward } = require('./windows-lan');

test('an unspecified or loopback address is never forwarded (the rule would forward to itself)', async () => {
  for (const address of ['0.0.0.0', '127.0.0.1', '127.1.2.3']) {
    const lines = [];
    assert.equal(await ensureLanForward('.', address, 8090, (l) => lines.push(l)), false);
    assert.match(lines.join('\n'), /no usable address/);
  }
});

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

test('the firewall rule is scoped to the local subnet on the address\'s own interface, Private profile only', () => {
  const script = forwardScript('192.168.1.40', 8090);
  assert.match(script, /\$alias = \(Get-NetIPAddress -IPAddress '192\.168\.1\.40'/);
  assert.match(script, /New-NetFirewallRule [^;]*-Profile Private -RemoteAddress LocalSubnet -InterfaceAlias \$alias/);
});

test('a forward that does not reach the access-code page is taken out again in the same elevated run', () => {
  const script = forwardScript('192.168.1.40', 8090);
  const added = script.indexOf('portproxy add');
  const checked = script.indexOf("Contains('/__decurix_access/login')");
  const rollback = script.indexOf('if (-not $served) { netsh interface portproxy delete');
  assert.ok(added >= 0 && checked > added && rollback > checked);
  assert.match(script.slice(rollback), /Remove-NetFirewallRule -DisplayName 'Decurix LAN access \(8090\)'[^;]*; throw /);
});

test('nothing is forwarded, and Windows is not asked, when the port is not the gated listener', async () => {
  const lines = [];
  const ok = await ensureLanForward('.', '192.168.1.40', 8080, (l) => lines.push(l), {
    checkPort: async () => { throw new Error('port 8080 is the primary listener'); },
  });
  assert.equal(ok, false);
  assert.match(lines.join('\n'), /not forwarded - port 8080 is the primary listener/);
  assert.doesNotMatch(lines.join('\n'), /asking Windows/);
});

test('the check that a forward is in place asks for the scoped rule, not just any rule by name', () => {
  const script = checkScript('192.168.1.40', 8090);
  assert.match(script, /\$_.Profile -eq 'Private'/);
  assert.match(script, /Get-NetFirewallInterfaceFilter\)\.InterfaceAlias -contains \$alias/);
  assert.match(script, /RemoteAddress -contains 'LocalSubnet'/);
  assert.match(script, /DECURIX-PUBLIC-NETWORK/);
});
