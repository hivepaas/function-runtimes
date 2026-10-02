// What the conformance suite does not reach: how a module is loaded, how
// whatever a handler returns becomes a response, and how a request is read.
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test } from 'node:test'

import { depsCommand, loadConfig, loadHandler, requestOf, Runtime, toAnswer } from '../runtime/runtime.mjs'

const defaults = loadConfig({})

test('the defaults are the contract’s', () => {
  assert.deepEqual(defaults, {
    entrypoint: 'index.js', handler: 'default', port: 8080,
    timeoutMs: 30000, maxConcurrency: 16, maxBodySize: 6291456,
  })
})

test('Bun’s default entrypoint is TypeScript', () => {
  assert.equal(loadConfig({}, 'bun').entrypoint, 'index.ts')
  assert.equal(loadConfig({}, 'node').entrypoint, 'index.js')
  assert.equal(loadConfig({ HP_FN_ENTRYPOINT: 'main.mjs' }, 'bun').entrypoint, 'main.mjs')
})

test('deps installs with the engine’s own manager, from its lock file when there is one', () => {
  const npm = ['--omit=dev', '--no-audit', '--no-fund', '--cache', '/tmp/hivepaas-deps-cache']
  assert.deepEqual(depsCommand('node', true), { command: 'npm', args: ['ci', ...npm], env: {} })
  assert.deepEqual(depsCommand('node', false), { command: 'npm', args: ['install', ...npm], env: {} })
  const bunEnv = { BUN_INSTALL_CACHE_DIR: '/tmp/hivepaas-deps-cache' }
  assert.deepEqual(depsCommand('bun', true),
    { command: 'bun', args: ['install', '--production', '--frozen-lockfile'], env: bunEnv })
  assert.deepEqual(depsCommand('bun', false), { command: 'bun', args: ['install', '--production'], env: bunEnv })
})

test('the environment sets the limits', () => {
  const cfg = loadConfig({
    HP_FN_ENTRYPOINT: 'src/main.mjs', HP_FN_HANDLER: 'hello', HP_FN_PORT: '9000',
    HP_FN_TIMEOUT_MS: '1500', HP_FN_MAX_CONCURRENCY: '2', HP_FN_MAX_BODY_SIZE: '1024',
  })
  assert.deepEqual(cfg, {
    entrypoint: 'src/main.mjs', handler: 'hello', port: 9000,
    timeoutMs: 1500, maxConcurrency: 2, maxBodySize: 1024,
  })
})

test('a limit that is not a positive whole number is refused', () => {
  for (const value of ['0', '-1', 'abc', '1.5', '1e3']) {
    assert.throws(() => loadConfig({ HP_FN_TIMEOUT_MS: value }), /HP_FN_TIMEOUT_MS/, value)
  }
})

test('an entrypoint outside the function is refused', () => {
  assert.throws(() => loadConfig({ HP_FN_ENTRYPOINT: '../etc/passwd' }), /HP_FN_ENTRYPOINT/)
  assert.throws(() => loadConfig({ HP_FN_ENTRYPOINT: '/etc/passwd' }), /HP_FN_ENTRYPOINT/)
})

function app(files) {
  const dir = mkdtempSync(path.join(tmpdir(), 'hp-node-'))
  for (const [name, text] of Object.entries(files)) writeFileSync(path.join(dir, name), text)
  return dir
}

async function loaded(files, entrypoint, handler = 'default') {
  const fn = await loadHandler(app(files), { ...defaults, entrypoint, handler })
  return fn()
}

test('a handler is an ES module’s default export, or the export it is named', async () => {
  assert.equal(await loaded({ 'a.mjs': 'export default () => "esm default"' }, 'a.mjs'), 'esm default')
  assert.equal(await loaded({ 'a.mjs': 'export const hello = () => "esm named"' }, 'a.mjs', 'hello'), 'esm named')
})

test('a handler may be CommonJS, transpiled or not', async () => {
  assert.equal(await loaded({ 'a.cjs': 'module.exports = () => "cjs"' }, 'a.cjs'), 'cjs')
  assert.equal(await loaded({ 'a.cjs': 'module.exports = { hello: () => "cjs named" }' }, 'a.cjs', 'hello'),
    'cjs named')
  const transpiled = 'Object.defineProperty(exports, "__esModule", { value: true });' +
    'exports.default = () => "transpiled"'
  assert.equal(await loaded({ 'a.cjs': transpiled }, 'a.cjs'), 'transpiled')
})

test('a handler that is not there, or a module that fails, cannot be loaded', async () => {
  await assert.rejects(loadHandler(app({ 'a.mjs': 'export const x = 1' }), { ...defaults, entrypoint: 'a.mjs' }),
    /default/)
  await assert.rejects(loadHandler(app({ 'a.mjs': 'throw new Error("at load")' }), { ...defaults, entrypoint: 'a.mjs' }),
    /at load/)
  await assert.rejects(loadHandler(app({}), { ...defaults, entrypoint: 'none.mjs' }), /none\.mjs/)
})

test('a body is sent with the type it has', () => {
  const text = toAnswer({ body: 'hi' }, 100)
  assert.equal(text.status, 200)
  assert.equal(text.headers['content-type'], 'text/plain; charset=utf-8')
  assert.equal(text.body.toString(), 'hi')

  assert.equal(toAnswer({ body: Buffer.from([1, 2]) }, 100).headers['content-type'], 'application/octet-stream')
  assert.deepEqual([...toAnswer({ body: new Uint8Array([3]) }, 100).body], [3])

  for (const [value, json] of [[{ a: 1 }, '{"a":1}'], [[1, 2], '[1,2]'], [0, '0'], [false, 'false']]) {
    const answer = toAnswer({ body: value }, 100)
    assert.equal(answer.headers['content-type'], 'application/json')
    assert.equal(answer.body.toString(), json)
  }
})

test('nothing is no content, and no body is an empty one', () => {
  for (const value of [undefined, null]) {
    const answer = toAnswer(value, 100)
    assert.equal(answer.status, 204)
    assert.equal(answer.body.length, 0)
  }
  const empty = toAnswer({ status: 201 }, 100)
  assert.equal(empty.status, 201)
  assert.equal(empty.body.length, 0)
  assert.equal(empty.headers['content-type'], undefined)
})

test('the handler’s headers are sent, its content type first', () => {
  const answer = toAnswer({ headers: { 'Content-Type': 'text/csv', 'Set-Cookie': ['a=1', 'b=2'] }, body: 'a,b' }, 100)
  assert.equal(answer.headers['content-type'], 'text/csv')
  assert.deepEqual(answer.headers['set-cookie'], ['a=1', 'b=2'])
})

test('a status that carries no body sends none', () => {
  assert.equal(toAnswer({ status: 204, body: 'x' }, 100).body.length, 0)
  assert.equal(toAnswer({ status: 304, body: 'x' }, 100).body.length, 0)
})

test('what is not a response is an error', () => {
  for (const value of ['hello', 42, [], { status: 42 }, { status: 600 }, { status: '200' }, { status: 200.5 },
    { headers: { 'bad name': 'x' } }, { headers: { ok: 'line\nbreak' } }, { body: () => 1 }]) {
    assert.throws(() => toAnswer(value, 100), (e) => e.outcome === 'error', JSON.stringify(value))
  }
})

test('a body over the limit is its own error', () => {
  assert.throws(() => toAnswer({ body: 'x'.repeat(101) }, 100), (e) => e.outcome === 'response_too_large')
  assert.equal(toAnswer({ body: 'x'.repeat(100) }, 100).body.length, 100)
})

test('a request is read as it was sent', () => {
  const req = requestOf('post', '/echo/a%20b?a=1&a=2&b=x+y&__proto__=p',
    ['X-Custom', 'v', 'Accept', 'a', 'accept', 'b', 'Host', 'example.com'], Buffer.from('{"k":1}'))

  assert.equal(req.method, 'POST')
  assert.equal(req.path, '/echo/a%20b')
  assert.equal(req.query.a, '1')
  assert.deepEqual(req.queryAll.a, ['1', '2'])
  assert.equal(req.query.b, 'x y')
  assert.equal(req.query.__proto__, 'p')
  assert.equal({}.p, undefined)
  assert.equal(req.headers['x-custom'], 'v')
  assert.equal(req.headers.accept, 'a, b')
  assert.equal(req.headers.host, 'example.com')
  assert.equal(req.text(), '{"k":1}')
  assert.deepEqual(req.json(), { k: 1 })
})

test('a call that outlives its timeout is answered, and its signal aborted', async () => {
  let signal
  const runtime = new Runtime(async (_req, ctx) => {
    signal = ctx.signal
    await new Promise((resolve) => setTimeout(resolve, 200))
  }, { ...defaults, timeoutMs: 20 }, { out: () => {}, err: () => {} })

  const answer = await runtime.call(requestOf('GET', '/', [], Buffer.alloc(0)), 'id-1')

  assert.equal(answer.status, 504)
  assert.equal(answer.outcome, 'timeout')
  assert.equal(signal.aborted, true)
})

test('a call that times out keeps its place until the handler returns', async () => {
  let release
  const runtime = new Runtime(() => new Promise((resolve) => { release = resolve }),
    { ...defaults, timeoutMs: 20 }, { out: () => {}, err: () => {} })

  const answer = await runtime.call(requestOf('GET', '/', [], Buffer.alloc(0)), 'id-1')

  assert.equal(answer.status, 504)
  assert.equal(runtime.running, 1)
  release()
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(runtime.running, 0)
})

test('invoke refuses a body that is not base64', async () => {
  const errors = []
  const runtime = new Runtime(() => undefined, defaults, { out: () => {}, err: (text) => errors.push(text) })

  assert.equal(await runtime.invoke('{"body":"%%%"}'), 2)
  assert.match(errors.join('\n'), /base64/)
})
