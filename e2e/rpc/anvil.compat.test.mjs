import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { once } from 'node:events'
import { createServer } from 'node:net'
import { resolve } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import { test } from 'node:test'
import { createTestClient, http } from 'viem'
import { privateKeyToAccount } from 'viem/accounts'

const BINARY = process.env.RPC_E2E_BINARY || resolve(import.meta.dirname, '../../bin/ethertest')
const ANVIL = process.env.ANVIL || 'anvil'
const CAST = process.env.CAST || 'cast'
const account = privateKeyToAccount('0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80')
const address = '0x0000000000000000000000000000000000001234'
const zeroHash = `0x${'00'.repeat(32)}`
const slotValue = `0x${'00'.repeat(31)}01`

async function freePort() {
  const server = createServer().listen(0, '127.0.0.1')
  await once(server, 'listening')
  const port = server.address().port
  await new Promise(resolve => server.close(resolve))
  return port
}
async function start(name, t) {
  const port = await freePort()
  const common = ['--host', '127.0.0.1', '--port', String(port), '--chain-id', '1337', '--accounts', '10', '--balance', '10000', '--no-mining', '--quiet']
  const args = name === 'anvil' ? [...common, '--hardfork', 'osaka', '--timestamp', '1800000000'] : [...common, '--genesis-time', '1800000000', '--amsterdam-epoch', '-1']
  const child = spawn(name === 'anvil' ? ANVIL : BINARY, args, { stdio: ['ignore', 'pipe', 'pipe'] })
  let output = ''
  child.stdout.on('data', data => { output += data })
  child.stderr.on('data', data => { output += data })
  const exited = new Promise(resolve => { child.once('exit', resolve); child.once('error', resolve) })
  t.after(async () => {
    child.kill('SIGTERM')
    await Promise.race([exited, delay(5000, undefined, { ref: false }).then(() => child.kill('SIGKILL'))])
  })
  const url = `http://127.0.0.1:${port}`
  async function response(method, params = []) {
    const response = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }), signal: AbortSignal.timeout(5000) })
    return response.json()
  }
  async function rpc(method, params = []) {
    const result = await response(method, params)
    assert(!result.error, `${name} ${method}: ${JSON.stringify(result.error)}`)
    return result.result
  }
  for (let i = 0; i < 100; i++) {
    try { await rpc('eth_chainId'); return { name, url, rpc, response } } catch {
      if (child.exitCode !== null) throw new Error(`${name} exited: ${output}`)
      await delay(50)
    }
  }
  throw new Error(`${name} startup timeout: ${output}`)
}

async function stream(node) {
  const socket = new WebSocket(node.url.replace('http:', 'ws:'))
  await once(socket, 'open')
  let next = 0
  const calls = new Map(), notifications = []
  socket.addEventListener('message', event => {
    const value = JSON.parse(event.data)
    if (value.id) { calls.get(value.id)?.(value); calls.delete(value.id) }
    else notifications.push(value.params)
  })
  return {
    socket,
    async subscribe(...params) {
      const id = ++next
      const reply = new Promise(resolve => calls.set(id, resolve))
      socket.send(JSON.stringify({ jsonrpc: '2.0', id, method: 'eth_subscribe', params }))
      const response = await reply
      assert(!response.error, JSON.stringify(response.error))
      return response.result
    },
    async quiet(subscription) {
      await delay(150)
      assert.equal(notifications.filter(item => item.subscription === subscription).length, 0)
    },
    async take(subscription) {
      for (let i = 0; i < 200; i++) {
        const index = notifications.findIndex(item => item.subscription === subscription)
        if (index >= 0) return notifications.splice(index, 1)[0].result
        await delay(10)
      }
      throw new Error(`${node.name} subscription timeout`)
    },
  }
}

test('Anvil v1.7.1 common workflow contract and explicit differences', { timeout: 120000 }, async t => {
  const version = spawnSync(ANVIL, ['--version'], { encoding: 'utf8' })
  assert.equal(version.status, 0, 'Anvil is required; this suite must not skip')
  assert.match(version.stdout, /anvil Version: 1\.7\.1\b/)
  assert.match(version.stdout, /4072e48705af9d93e3c0f6e29e93b5e9a40caed8/)
  const nodes = [await start('anvil', t), await start('ethertest', t)]
  async function equal(method, params = [], expected) {
    const results = await Promise.all(nodes.map(node => node.rpc(method, params)))
    assert.deepEqual(results[1], results[0], method)
    if (expected !== undefined) assert.deepEqual(results[0], expected, method)
    return results[0]
  }
  await equal('eth_chainId', [], '0x539')
  await equal('eth_accounts')
  await equal('anvil_getGenesisTime', [], 1800000000)
  await equal('anvil_getAutomine', [], false)
  await equal('anvil_getIntervalMining', [], null)
  for (const [method, params, result] of [
    ['anvil_setIntervalMining', [3600], null], ['anvil_getIntervalMining', [], 3600],
    ['anvil_setAutomine', [false], null], ['anvil_getIntervalMining', [], 3600],
    ['anvil_setAutomine', [true], null], ['anvil_getAutomine', [], true], ['anvil_getIntervalMining', [], null],
    ['evm_setAutomine', [false], null], ['evm_setIntervalMining', [0], null],
    ['anvil_setCoinbase', [address], null], ['eth_coinbase', [], address],
  ]) await equal(method, params, result)

  for (const [method, params, result] of [
    ['anvil_setBalance', [address, 42], null], ['anvil_setCode', [address, '0x00'], null],
    ['anvil_setNonce', [address, '0x3'], null], ['anvil_setStorageAt', [address, '0x0', slotValue], true],
  ]) {
    const before = await Promise.all(nodes.map(n => n.rpc('eth_blockNumber')))
    await equal(method, params, result)
    const after = await Promise.all(nodes.map(n => n.rpc('eth_blockNumber')))
    assert.equal(BigInt(after[0]) - BigInt(before[0]), 0n, 'Anvil setter must not mine')
    assert.equal(BigInt(after[1]) - BigInt(before[1]), 1n, 'ethertest setter must mine a control block')
  }
  for (const [value, expected] of [['42','0x2a'],['0X2a','0x2a'],['0b10','0x2'],['0o10','0x8'],['1_0','0xa'],['','0x0'],['0x','0x0']]) {
    await equal('anvil_setBalance', [address, value], null)
    await equal('eth_getBalance', [address, 'latest'], expected)
  }
  await equal('anvil_setBalance', [address, 42], null)
  await equal('eth_getBalance', [address, 'latest'], '0x2a')
  await equal('eth_getCode', [address, 'latest'], '0x00')
  await equal('eth_getTransactionCount', [address, 'latest'], '0x3')
  await equal('eth_getStorageAt', [address, '0x0', 'latest'], slotValue)
  for (const node of nodes) {
    const before = BigInt(await node.rpc('eth_blockNumber'))
    assert.equal(await node.rpc('anvil_mine', ['0x2', '0x6']), null)
    assert.equal(BigInt(await node.rpc('eth_blockNumber')), before + 2n)
    assert.equal(await node.rpc('evm_mine'), '0x0')
    const snapshot = await node.rpc('evm_snapshot')
    await node.rpc('anvil_setBalance', [address, '0x63'])
    assert.equal(await node.rpc('evm_revert', [snapshot]), true)
    assert.equal(await node.rpc('evm_revert', [snapshot]), false)
    assert.equal(await node.rpc('eth_getBalance', [address, 'latest']), '0x2a')
    const testClient = createTestClient({ mode: 'anvil', transport: http(node.url) })
    await testClient.setBalance({ address, value: 42n })
    await testClient.setAutomine(false)
    const cast = spawnSync(CAST, ['rpc', '--rpc-url', node.url, 'anvil_getAutomine'], { encoding: 'utf8', env: { ...process.env, NO_PROXY: '127.0.0.1,localhost', no_proxy: '127.0.0.1,localhost' } })
    assert.equal(cast.status, 0, cast.stderr)
    assert.equal(JSON.parse(cast.stdout), false)
  }
  // An explicit impossible timestamp is rejected without writes by ethertest.
  const local = nodes[1]
  const before = await local.rpc('eth_blockNumber')
  assert.equal((await local.response('evm_mine', [1])).error.code, -32602)
  assert.equal((await local.response('anvil_mine', [2, 0])).error.code, -32602)
  assert.equal(await local.rpc('eth_blockNumber'), before)
  for (const [method, params] of [
    ['anvil_setBalance', [address, -1]], ['anvil_setBalance', [address, `0x1${'0'.repeat(64)}`]],
    ['anvil_setStorageAt', [address, '0x0', '0x1']],
  ]) {
    const responses = await Promise.all(nodes.map(node => node.response(method, params)))
    assert.equal(responses[0].error.code, -32602)
    assert.equal(responses[1].error.code, -32602)
  }
  for (const [field, opcode, value] of [['number', '43', '0x123'], ['time', '42', '0x70000000'], ['gasLimit', '45', '0x100000'], ['feeRecipient', '41', address], ['prevRandao', '44', `0x${'0'.repeat(61)}123`], ['baseFeePerGas', '48', '0x123'], ['blobBaseFee', '4a', '0x123']]) {
    const args = [{ to: address }, 'latest', { [address]: { code: `0x${opcode}60005260206000f3` } }, { [field]: value }]
    const expected = `0x${BigInt(value).toString(16).padStart(64, '0')}`
    if (field === 'blobBaseFee') {
      // v1.7.1 ignores this field; ethertest deliberately implements it.
      const oracle = await nodes[0].rpc('eth_call', args)
      assert.equal(oracle, await nodes[0].rpc('eth_call', args.slice(0, 3)))
      assert.notEqual(oracle, expected)
      assert.equal(await nodes[1].rpc('eth_call', args), expected)
    } else await equal('eth_call', args, expected)
    await equal('eth_estimateGas', args)
  }

  const raw = await account.signTransaction({ chainId: 1337, type: 'eip1559', nonce: 0, to: address, value: 1n, gas: 21000n, maxFeePerGas: 3000000000n, maxPriorityFeePerGas: 1000000000n })
  const hash = await equal('eth_sendRawTransaction', [raw])
  await equal('txpool_status')
  await equal('txpool_inspect')
  await equal('anvil_dropTransaction', [hash], hash)
  await equal('anvil_dropTransaction', [hash], null)
  await equal('eth_sendRawTransaction', [raw], hash)
  await equal('anvil_removePoolTransactions', [account.address], null)
  await equal('eth_sendRawTransaction', [raw], hash)
  await equal('anvil_dropAllTransactions', [], null)
  await equal('eth_sendRawTransaction', [raw], hash)
  await equal('anvil_setAutomine', [true], null)
  for (const node of nodes) {
    for (let i = 0; i < 100 && await node.rpc('eth_getTransactionReceipt', [hash]) === null; i++) await delay(20)
    assert((await node.rpc('eth_getTransactionReceipt', [hash])) !== null, 'enabling automine drains queued transactions')
  }
  await equal('anvil_setAutomine', [false], null)

  for (const node of nodes) {
    await node.rpc('anvil_setCode', [address, '0x60006000a000'])
    const snapshot = await node.rpc('evm_snapshot')
    const events = await stream(node)
    t.after(() => events.socket.close())
    const logs = await events.subscribe('logs', { address })
    const pending = await events.subscribe('newPendingTransactions', true)
    const raw = await account.signTransaction({ chainId: 1337, type: 'eip1559', nonce: 1, to: address, value: 0n, gas: 100000n, maxFeePerGas: 3000000000n, maxPriorityFeePerGas: 1000000000n })
    const hash = await node.rpc('eth_sendRawTransaction', [raw])
    await node.rpc('anvil_dropAllTransactions')
    const transaction = await events.take(pending)
    assert.equal(transaction.hash, hash)
    assert.equal(transaction.blockHash, null)
    await node.rpc('eth_sendRawTransaction', [raw])
    await node.rpc('evm_mine')
    const canonical = await events.take(logs)
    assert.equal(canonical.removed, false)
    assert.equal(await node.rpc('evm_revert', [snapshot]), true)
    // Anvil v1.7.1 evm_revert does not emit removed logs. ethertest must
    // preserve its stronger canonical revision contract for all rewinds.
    if (node.name === 'ethertest') {
      const removed = await events.take(logs)
      assert.equal(removed.removed, true)
      assert.equal(removed.blockHash, canonical.blockHash)
    } else await events.quiet(logs)
    await node.rpc('eth_sendRawTransaction', [raw])
    await node.rpc('evm_mine')
    const replacement = await events.take(logs)
    assert.equal(replacement.removed, false)
    events.socket.close()
  }
})
