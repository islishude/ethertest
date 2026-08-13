import assert from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { once } from 'node:events'
import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import { createConnection } from 'node:net'
import { resolve } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import { after, before, test } from 'node:test'

import {
  createPublicClient,
  createWalletClient,
  defineChain,
  http,
  isAddress,
  isAddressEqual,
  isHash,
  recoverMessageAddress,
  recoverTransactionAddress,
  verifyTypedData,
  webSocket,
  zeroAddress,
  zeroHash,
} from 'viem'
import { prepareAuthorization } from 'viem/actions'
import { privateKeyToAccount } from 'viem/accounts'
import { recoverAuthorizationAddress, verifyAuthorization } from 'viem/utils'

const ROOT = resolve(import.meta.dirname, '../..')
const BINARY = process.env.RPC_E2E_BINARY || resolve(ROOT, 'bin/ethertest')
const CAST = process.env.CAST || 'cast'
const RPC_SPEC = resolve(ROOT, 'specs/upstream/execution-rpc-subset.json')

const ACCOUNT_0_KEY =
  '0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80'
const ACCOUNT_1_KEY =
  '0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d'
const account0 = privateKeyToAccount(ACCOUNT_0_KEY)
const account1 = privateKeyToAccount(ACCOUNT_1_KEY)
const account2 = '0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc'
const account3 = '0x90f79bf6eb2c4f870365e785982e1f101e93b906'
const account9 = '0xa0ee7a142d267c1f36714e4a8f75612f20a79720'

const seenMethods = new Set()
const nodeOutput = []
let nodeProcess
let proxyServer
let upstreamPort
let proxyPort
let rpcUrl
let wsUrl
let chain
let publicClient
let rpcWalletClient
let localWalletClient

function failWithNodeOutput(message) {
  const output = nodeOutput.join('').trim()
  return new Error(
    redact(`${message}${output ? `\n\nethertest output:\n${output}` : ''}`),
  )
}

function redact(value) {
  return String(value)
    .replaceAll(ACCOUNT_0_KEY, '<redacted-development-key>')
    .replaceAll(ACCOUNT_1_KEY, '<redacted-development-key>')
}

function rpcTest(name, callback) {
  test(name, async (context) => {
    try {
      await callback(context)
    } catch (error) {
      throw failWithNodeOutput(error?.stack ?? error)
    }
  })
}

function collectMethods(payload) {
  const requests = Array.isArray(payload) ? payload : [payload]
  for (const request of requests) {
    if (request && typeof request.method === 'string') seenMethods.add(request.method)
  }
}

async function listen(server) {
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  const address = server.address()
  assert(address && typeof address === 'object')
  return address.port
}

async function closeServer(server) {
  if (!server?.listening) return
  await new Promise((resolveClose, reject) =>
    server.close((error) => (error ? reject(error) : resolveClose())),
  )
}

function startProxy(targetPort) {
  const server = createServer(async (request, response) => {
    try {
      const chunks = []
      for await (const chunk of request) chunks.push(chunk)
      const body = Buffer.concat(chunks)
      if (body.length > 0) collectMethods(JSON.parse(body.toString('utf8')))

      const headers = { ...request.headers }
      delete headers.host
      delete headers['content-length']
      const upstream = await fetch(`http://127.0.0.1:${targetPort}${request.url}`, {
        body: body.length > 0 ? body : undefined,
        headers,
        method: request.method,
      })
      response.writeHead(upstream.status, Object.fromEntries(upstream.headers.entries()))
      response.end(Buffer.from(await upstream.arrayBuffer()))
    } catch (error) {
      response.writeHead(502, { 'content-type': 'text/plain' })
      response.end(String(error))
    }
  })
  server.on('upgrade', (request, socket, head) => {
    const upstream = createConnection({ host: '127.0.0.1', port: targetPort })
    upstream.once('connect', () => {
      let header = `${request.method} ${request.url} HTTP/${request.httpVersion}\r\n`
      for (let index = 0; index < request.rawHeaders.length; index += 2) {
        header += `${request.rawHeaders[index]}: ${request.rawHeaders[index + 1]}\r\n`
      }
      upstream.write(`${header}\r\n`)
      if (head.length > 0) upstream.write(head)
      socket.pipe(upstream).pipe(socket)
    })
    upstream.on('error', () => socket.destroy())
    socket.on('error', () => upstream.destroy())
  })
  return server
}

async function waitForNodeReady() {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    if (nodeProcess.exitCode !== null) {
      throw failWithNodeOutput(`ethertest exited early with code ${nodeProcess.exitCode}`)
    }
    for (const line of nodeOutput.join('').split('\n')) {
      try {
        const entry = JSON.parse(line)
        if (entry.event === 'node_started') return entry
      } catch {
        // The final stream chunk may contain an incomplete JSON log line.
      }
    }
    await delay(50)
  }
  throw failWithNodeOutput('timed out waiting for the node_started JSON log')
}

async function stopNode() {
  if (!nodeProcess || nodeProcess.exitCode !== null) return
  const exited = once(nodeProcess, 'exit')
  nodeProcess.kill('SIGTERM')
  let timeoutHandle
  const timeout = new Promise((resolveTimeout) => {
    timeoutHandle = setTimeout(() => resolveTimeout('timeout'), 5_000)
  })
  const result = await Promise.race([exited, timeout])
  clearTimeout(timeoutHandle)
  if (result === 'timeout') {
    nodeProcess.kill('SIGKILL')
    await exited
  }
}

async function cast(...args) {
  const child = spawn(CAST, args, {
    cwd: ROOT,
    env: { ...process.env, NO_COLOR: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const stdout = []
  const stderr = []
  child.stdout.on('data', (chunk) => stdout.push(chunk))
  child.stderr.on('data', (chunk) => stderr.push(chunk))

  const exited = once(child, 'exit')
  const timer = setTimeout(() => child.kill('SIGKILL'), 45_000)
  const [status, signal] = await exited
  clearTimeout(timer)
  if (status !== 0) {
    throw failWithNodeOutput(
      `cast failed (${status ?? signal ?? 'spawn error'}):\n${Buffer.concat(stderr).toString()}`,
    )
  }
  return Buffer.concat(stdout).toString().trim()
}

async function castRpc(method, params = []) {
  const raw = await cast(
    'rpc',
    '--rpc-url',
    rpcUrl,
    '--raw',
    method,
    JSON.stringify(params),
  )
  return JSON.parse(raw)
}

async function rpc(method, params = []) {
  const response = await fetch(rpcUrl, {
    body: JSON.stringify({ id: 1, jsonrpc: '2.0', method, params }),
    headers: { 'content-type': 'application/json' },
    method: 'POST',
  })
  const payload = await response.json()
  if (payload.error) {
    const error = new Error(payload.error.message)
    error.code = payload.error.code
    error.data = payload.error.data
    throw error
  }
  return payload.result
}

async function rpcError(method, params, code) {
  await assert.rejects(rpc(method, params), (error) => {
    assert.equal(error.code, code)
    return true
  })
}

before(async () => {
  try {
    assert(Number.parseInt(process.versions.node, 10) >= 24, 'Node.js 24 or newer is required')
    const castVersion = spawnSync(CAST, ['--version'], { encoding: 'utf8' })
    assert.equal(castVersion.status, 0, castVersion.stderr)
    assert.match(castVersion.stdout, /cast Version: 1\.7\.1\b/)

    nodeProcess = spawn(
      BINARY,
      ['--http', '127.0.0.1:0', '--no-beacon', '--log-json', '--log-level', 'info'],
      {
        cwd: ROOT,
        env: { ...process.env, ETHERTEST_MINING_MODE: 'manual' },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    )
    nodeProcess.stdout.on('data', (chunk) => nodeOutput.push(chunk.toString()))
    nodeProcess.stderr.on('data', (chunk) => nodeOutput.push(chunk.toString()))
    const started = await waitForNodeReady()
    upstreamPort = Number(new URL(started.execution_endpoint).port)
    assert(Number.isInteger(upstreamPort) && upstreamPort > 0)

    proxyServer = startProxy(upstreamPort)
    proxyPort = await listen(proxyServer)
    rpcUrl = `http://127.0.0.1:${proxyPort}`
    wsUrl = `ws://127.0.0.1:${proxyPort}`

    chain = defineChain({
      id: 1337,
      name: 'ethertest',
      nativeCurrency: { decimals: 18, name: 'Ether', symbol: 'ETH' },
      rpcUrls: { default: { http: [rpcUrl], webSocket: [wsUrl] } },
    })
    publicClient = createPublicClient({
      batch: { multicall: false },
      chain,
      transport: http(rpcUrl, { retryCount: 0, timeout: 10_000 }),
    })
    rpcWalletClient = createWalletClient({
      account: account0.address,
      chain,
      transport: http(rpcUrl, { retryCount: 0, timeout: 10_000 }),
    })
    localWalletClient = createWalletClient({
      account: account0,
      chain,
      transport: http(rpcUrl, { retryCount: 0, timeout: 10_000 }),
    })
  } catch (error) {
    throw failWithNodeOutput(error?.stack ?? error)
  }
})

after(async () => {
  const cleanupErrors = []
  try {
    await closeServer(proxyServer)
  } catch (error) {
    cleanupErrors.push(error)
  }
  try {
    await stopNode()
  } catch (error) {
    cleanupErrors.push(error)
  }
  if (cleanupErrors.length > 0) throw new AggregateError(cleanupErrors, 'E2E cleanup failed')
})

rpcTest('viem typed public and wallet actions exercise the canonical RPC path', async () => {
  assert.equal(await publicClient.getChainId(), 1337)
  assert.equal(await publicClient.getBlockNumber(), 0n)
  const accounts = await rpcWalletClient.getAddresses()
  assert.equal(accounts.length, 10)
  assert(isAddressEqual(accounts[0], account0.address))
  assert.equal(await publicClient.getBalance({ address: account0.address }), 10_000n * 10n ** 18n)
  assert.equal(await publicClient.getTransactionCount({ address: account0.address }), 0)
  assert.equal(await publicClient.getCode({ address: account9 }), undefined)
  assert.equal(await publicClient.getStorageAt({ address: account9, slot: zeroHash }), zeroHash)
  const proof = await publicClient.getProof({ address: account9, storageKeys: [zeroHash] })
  assert.equal(proof.storageProof.length, 1)

  assert((await publicClient.getBlobBaseFee()) > 0n)
  assert((await publicClient.getGasPrice()) > 0n)
  assert.equal(await publicClient.estimateMaxPriorityFeePerGas(), 1_000_000_000n)
  const feeHistory = await publicClient.getFeeHistory({
    blockCount: 1,
    blockTag: 'latest',
    rewardPercentiles: [0, 50, 100],
  })
  assert.equal(feeHistory.baseFeePerGas.length, 2)

  assert.equal(
    await publicClient.estimateGas({ account: account0.address, to: account2, value: 1n }),
    21_000n,
  )
  const callResult = await publicClient.call({ account: account0.address, to: account2 })
  assert.equal(callResult.data, undefined)
  const accessList = await publicClient.createAccessList({ account: account0.address, to: account2 })
  assert.equal(accessList.gasUsed, 21_000n)
  assert.deepEqual(accessList.accessList, [])
  const simulations = await publicClient.simulateBlocks({
    blocks: [{ calls: [{ account: account0.address, to: account2, value: 1n }] }],
  })
  assert.equal(simulations.length, 1)
  assert.equal(simulations[0].calls[0].status, 'success')

  const eventFilter = await publicClient.createEventFilter()
  assert.deepEqual(await publicClient.getFilterChanges({ filter: eventFilter }), [])
  assert.deepEqual(await publicClient.getFilterLogs({ filter: eventFilter }), [])
  assert.deepEqual(await publicClient.getLogs({ fromBlock: 0n, toBlock: 'latest' }), [])
  assert.equal(await publicClient.uninstallFilter({ filter: eventFilter }), true)

  const blockFilter = await publicClient.createBlockFilter()
  const pendingFilter = await publicClient.createPendingTransactionFilter()
  const pendingHash = await rpcWalletClient.sendTransaction({
    account: account0.address,
    gas: 21_000n,
    maxFeePerGas: 3_000_000_000n,
    maxPriorityFeePerGas: 1_000_000_000n,
    nonce: 0,
    to: account2,
    value: 1n,
  })
  assert(isHash(pendingHash))
  assert.deepEqual(await publicClient.getFilterChanges({ filter: pendingFilter }), [pendingHash])
  assert.equal(await publicClient.getBlockTransactionCount({ blockTag: 'pending' }), 1)
  const pending = await publicClient.getTransaction({ blockTag: 'pending', index: 0 })
  assert.equal(pending.hash, pendingHash)
  assert.equal(pending.blockHash, null)

  const signedMessage = await rpcWalletClient.request({
    method: 'eth_sign',
    params: [account0.address, '0x010203'],
  })
  assert(isAddressEqual(await recoverMessageAddress({ message: { raw: '0x010203' }, signature: signedMessage }), account0.address))
  const nonce = await publicClient.getTransactionCount({ address: account0.address, blockTag: 'pending' })
  const signedTransaction = await rpcWalletClient.signTransaction({
    gas: 21_000n,
    maxFeePerGas: 3_000_000_000n,
    maxPriorityFeePerGas: 1_000_000_000n,
    nonce,
    to: account2,
    type: 'eip1559',
    value: 2n,
  })
  assert(isAddressEqual(await recoverTransactionAddress({ serializedTransaction: signedTransaction }), account0.address))

  const typedData = {
    domain: { chainId: 1337, name: 'Ether Mail', verifyingContract: account2, version: '1' },
    message: { contents: 'viem authoritative RPC E2E' },
    primaryType: 'Mail',
    types: { Mail: [{ name: 'contents', type: 'string' }] },
  }
  const typedSignature = await rpcWalletClient.signTypedData(typedData)
  assert(
    await verifyTypedData({
      ...typedData,
      address: account0.address,
      signature: typedSignature,
    }),
  )

  const rawHash = await publicClient.sendRawTransaction({ serializedTransaction: signedTransaction })
  assert(isHash(rawHash))
  const mined = await rpc('evm_mine', ['0x1'])
  assert.equal(mined.length, 1)
  const latest = await publicClient.getBlock({ blockTag: 'latest', includeTransactions: true })
  assert.equal(latest.number, 1n)
  assert.equal(latest.transactions.length, 2)
  assert.equal(await publicClient.getBlockTransactionCount({ blockHash: latest.hash }), 2)
  assert.equal((await publicClient.getBlock({ blockHash: latest.hash })).hash, latest.hash)
  assert.equal((await publicClient.getTransaction({ hash: pendingHash })).blockHash, latest.hash)
  assert.equal((await publicClient.getTransaction({ blockHash: latest.hash, index: 0 })).hash, pendingHash)
  assert.equal((await publicClient.getTransaction({ blockNumber: latest.number, index: 1 })).hash, rawHash)
  assert.equal((await publicClient.getTransactionReceipt({ hash: pendingHash })).status, 'success')
  assert.equal((await publicClient.getBlockReceipts({ blockHash: latest.hash })).length, 2)
  assert.deepEqual(await publicClient.getFilterChanges({ filter: blockFilter }), [latest.hash])
  assert.equal(await publicClient.uninstallFilter({ filter: blockFilter }), true)
  assert.equal(await publicClient.uninstallFilter({ filter: pendingFilter }), true)

  const preparedAuthorization = await prepareAuthorization(publicClient, {
    account: account1.address,
    contractAddress: account2,
  })
  const authorizationRPCArgs = {
    address: preparedAuthorization.address,
    chainId: `0x${preparedAuthorization.chainId.toString(16)}`,
    nonce: `0x${preparedAuthorization.nonce.toString(16)}`,
  }
  const rawAuthorization = await rpc('ethertest_signAuthorization', [
    account1.address,
    authorizationRPCArgs,
  ])
  const authorization = {
    ...rawAuthorization,
    chainId: Number(BigInt(rawAuthorization.chainId)),
    nonce: BigInt(rawAuthorization.nonce),
    yParity: Number(BigInt(rawAuthorization.yParity)),
  }
  assert.equal(
    await recoverAuthorizationAddress({ authorization }),
    account1.address,
  )
  assert.equal(
    await verifyAuthorization({ address: account1.address, authorization }),
    true,
  )
  const authorizationHash = await localWalletClient.sendTransaction({
    authorizationList: [authorization],
    to: account0.address,
  })
  await rpc('evm_mine', ['0x1'])
  const authorizationReceipt = await publicClient.getTransactionReceipt({ hash: authorizationHash })
  assert.equal(authorizationReceipt.status, 'success')
  assert.equal(authorizationReceipt.type, 'eip7702')
  assert.equal((await publicClient.getTransaction({ hash: authorizationHash })).type, 'eip7702')
  assert.equal(await publicClient.getCode({ address: account1.address }), `0xef0100${account2.slice(2)}`)

  const selfPrepared = await prepareAuthorization(publicClient, {
    account: account2,
    contractAddress: account3,
    executor: 'self',
  })
  assert.equal(BigInt(selfPrepared.nonce), 1n)
  const rawSelfAuthorization = await rpc('ethertest_signAuthorization', [
    account2,
    {
      address: selfPrepared.address,
      chainId: `0x${selfPrepared.chainId.toString(16)}`,
      nonce: `0x${selfPrepared.nonce.toString(16)}`,
    },
  ])
  const selfAuthorization = {
    ...rawSelfAuthorization,
    chainId: Number(BigInt(rawSelfAuthorization.chainId)),
    nonce: BigInt(rawSelfAuthorization.nonce),
    yParity: Number(BigInt(rawSelfAuthorization.yParity)),
  }
  assert.equal(
    await verifyAuthorization({
      address: account2,
      authorization: selfAuthorization,
    }),
    true,
  )
  const selfWalletClient = createWalletClient({
    account: account2,
    chain,
    transport: http(rpcUrl, { retryCount: 0, timeout: 10_000 }),
  })
  const selfAuthorizationHash = await selfWalletClient.sendTransaction({
    authorizationList: [selfAuthorization],
    to: account0.address,
  })
  await rpc('evm_mine', ['0x1'])
  assert.equal(
    (await publicClient.getTransactionReceipt({ hash: selfAuthorizationHash })).status,
    'success',
  )
  assert.equal(await publicClient.getCode({ address: account2 }), `0xef0100${account3.slice(2)}`)

  const authorizationCapabilities = await rpc('ethertest_capabilities')
  assert.equal(authorizationCapabilities.authorizationSigning, true)
  await rpcError(
    'ethertest_signAuthorization',
    [account1.address, { address: account2, chainId: '0x539' }],
    -32602,
  )
  for (const method of [
    'eth_signAuthorization',
    'anvil_signAuthorization',
    'evm_signAuthorization',
  ]) {
    await rpcError(method, [account1.address, authorizationRPCArgs], -32601)
  }
})

rpcTest('cast typed commands and raw RPC calls exercise the CLI compatibility path', async () => {
  assert.equal(await cast('chain-id', '--rpc-url', rpcUrl), '1337')
  assert(BigInt(await cast('balance', account0.address, '--rpc-url', rpcUrl)) > 0n)
  assert.equal(
    await cast(
      'estimate',
      account2,
      '--from',
      account0.address,
      '--value',
      '1wei',
      '--rpc-url',
      rpcUrl,
    ),
    '21000',
  )
  assert.equal(await cast('call', account2, '--rpc-url', rpcUrl), '0x')

  const sent = await cast(
    'send',
    account2,
    '--value',
    '1wei',
    '--private-key',
    ACCOUNT_0_KEY,
    '--rpc-url',
    rpcUrl,
    '--async',
    '--json',
  )
  const castHash = sent.startsWith('{') ? JSON.parse(sent).transactionHash : sent.replaceAll('"', '')
  assert(isHash(castHash))
  await castRpc('evm_mine', ['0x1'])
  const receipt = JSON.parse(await cast('receipt', castHash, '--rpc-url', rpcUrl, '--json'))
  assert.equal(receipt.status, '0x1')
  assert.equal(
    await cast('block-number', '--rpc-url', rpcUrl),
    String(BigInt(receipt.blockNumber)),
  )
  assert.match(await cast('block', 'latest', '--rpc-url', rpcUrl, '--json'), /"hash"/)

  const latest = await castRpc('eth_getBlockByNumber', ['latest', false])
  assert(isHash(latest.hash))
  assert.equal(await castRpc('eth_syncing'), false)
  assert.equal(await castRpc('net_version'), '1337')
  assert(isAddress(await castRpc('eth_coinbase')))
  assert.match(await castRpc('debug_getRawHeader', ['latest']), /^0x[0-9a-f]+$/)
  assert.match(await castRpc('debug_getRawBlock', ['latest']), /^0x[0-9a-f]+$/)
  assert(Array.isArray(await castRpc('debug_getRawReceipts', ['latest'])))
  assert.match(await castRpc('debug_getRawTransaction', [castHash]), /^0x[0-9a-f]+$/)
  assert((await castRpc('eth_capabilities')).blocks)
  assert((await castRpc('eth_config')).current)
  const storage = await castRpc('eth_getStorageValues', [
    { [account9]: [zeroHash] },
    'latest',
  ])
  assert.deepEqual(Object.values(storage), [[zeroHash]])
  assert((await castRpc('txpool_content')).pending)
  assert((await castRpc('txpool_contentFrom', [account0.address])).pending)
  assert.equal((await castRpc('txpool_status')).pending, '0x0')
})

rpcTest('HTTP batching, WebSocket newHeads, extensions, and canonical errors are visible', async () => {
  const beforeBlock = await publicClient.getBlockNumber({ cacheTime: 0 })
  const batch = await fetch(rpcUrl, {
    body: JSON.stringify([
      { id: 1, jsonrpc: '2.0', method: 'eth_chainId', params: [] },
      { id: 2, jsonrpc: '2.0', method: 'eth_blockNumber', params: [] },
    ]),
    headers: { 'content-type': 'application/json' },
    method: 'POST',
  }).then((response) => response.json())
  assert.deepEqual(
    batch.map(({ id }) => id),
    [1, 2],
  )
  assert.equal(batch[0].result, '0x539')
  assert.equal(BigInt(batch[1].result), beforeBlock)

  const wsClient = createPublicClient({
    chain,
    transport: webSocket(wsUrl, {
      keepAlive: false,
      reconnect: false,
      retryCount: 0,
      timeout: 10_000,
    }),
  })
  let resolveHead
  let rejectHead
  const headPromise = new Promise((resolvePromise, rejectPromise) => {
    resolveHead = resolvePromise
    rejectHead = rejectPromise
  })
  const headTimeout = setTimeout(
    () => rejectHead(new Error('timed out waiting for newHeads')),
    10_000,
  )
  const subscription = await wsClient.transport.subscribe({
    params: ['newHeads'],
    onData({ result }) {
      clearTimeout(headTimeout)
      resolveHead(result)
    },
    onError(error) {
      clearTimeout(headTimeout)
      rejectHead(error)
    },
  })
  await rpc('evm_mine', ['0x1'])
  const head = await headPromise
  assert.equal(BigInt(head.number), beforeBlock + 1n)
  await subscription.unsubscribe()
  ;(await wsClient.transport.getRpcClient()).close()

  const snapshot = await rpc('evm_snapshot')
  await rpc('evm_mine', ['0x1'])
  assert.equal(await rpc('evm_revert', [snapshot]), true)
  const setBalanceBlock = await rpc('anvil_setBalance', [account3, '0x2a'])
  assert(isHash(setBalanceBlock))
  assert.equal(await publicClient.getBalance({ address: account3 }), 42n)

  const network = await rpc('ethertest_networkConfig')
  assert.equal(network.chainId, 1337)
  assert.equal(network.consensusMode, 'synthetic')
  const safety = await rpc('ethertest_safetyStatus')
  assert.equal(safety.session_tainted, true)
  assert.equal(safety.consensus_mode, 'synthetic')

  await rpcError('ethertest_methodThatDoesNotExist', [], -32601)
  await rpcError('eth_getBalance', [zeroAddress, 'not-a-block'], -32602)
})

rpcTest('synthetic finality pause and resume stays consistent through the real CLI', async () => {
  const capabilities = await rpc('ethertest_capabilities')
  assert.equal(capabilities.finalityControls, true)

  const before = await rpc('ethertest_finalityStatus')
  assert.equal(before.paused, false)
  assert.equal(before.current_slot, before.finality_slot)
  assert.equal(before.consensus_mode, 'synthetic')
  const safeBefore = await rpc('eth_getBlockByNumber', ['safe', false])
  const finalizedBefore = await rpc('eth_getBlockByNumber', ['finalized', false])
  assert.equal(safeBefore.hash, before.safe_block_hash)
  assert.equal(finalizedBefore.hash, before.finalized_block_hash)

  assert.equal(await rpc('ethertest_pauseFinality'), true)
  assert.equal(await rpc('ethertest_pauseFinality'), true)
  const paused = await rpc('ethertest_finalityStatus')
  assert.equal(paused.paused, true)
  assert.equal(paused.finality_slot, before.current_slot)

  const missed = await rpc('ethertest_missSlots', ['0x11'])
  assert.equal(missed.length, 17)
  await rpc('evm_mine', ['0x1'])
  const advanced = await rpc('ethertest_finalityStatus')
  assert.equal(advanced.paused, true)
  assert(advanced.current_slot > advanced.finality_slot)
  assert.equal(advanced.safe_block_hash, before.safe_block_hash)
  assert.equal(advanced.finalized_block_hash, before.finalized_block_hash)
  assert.equal((await rpc('eth_getBlockByNumber', ['safe', false])).hash, before.safe_block_hash)
  assert.equal(
    (await rpc('eth_getBlockByNumber', ['finalized', false])).hash,
    before.finalized_block_hash,
  )

  assert.equal(await rpc('ethertest_resumeFinality'), true)
  assert.equal(await rpc('ethertest_resumeFinality'), true)
  const resumed = await rpc('ethertest_finalityStatus')
  assert.equal(resumed.paused, false)
  assert.equal(resumed.current_slot, resumed.finality_slot)
  assert.equal((await rpc('eth_getBlockByNumber', ['safe', false])).hash, resumed.safe_block_hash)
  assert.equal(
    (await rpc('eth_getBlockByNumber', ['finalized', false])).hash,
    resumed.finalized_block_hash,
  )

  await rpcError('ethertest_pauseFinality', [true], -32602)
  await rpcError('anvil_pauseFinality', [], -32601)
  await rpcError('evm_finalityStatus', [], -32601)
})

rpcTest('every locked beta.7 implemented method was called through the proxy', async () => {
  const specification = JSON.parse(await readFile(RPC_SPEC, 'utf8'))
  const expected = specification.methods
    .filter(({ status }) => status === 'implemented')
    .map(({ name }) => name)
    .sort()
  assert.equal(expected.length, specification.implementedMethods)

  const lockedNames = new Set(specification.methods.map(({ name }) => name))
  const observedLocked = [...seenMethods].filter((method) => lockedNames.has(method)).sort()
  assert.deepEqual(observedLocked, expected)
})
