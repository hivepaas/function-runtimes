// call: a request handed to the serve running beside it, its answer written as
// invoke writes a result.
import assert from 'node:assert/strict'
import http from 'node:http'
import { test } from 'node:test'

import { callServe } from '../runtime/call.mjs'
import { loadConfig, RESULT_MARKER } from '../runtime/wire.mjs'

/** A server answering each request with the next of answers; it keeps what it was sent. */
async function server(answers) {
  const seen = []
  const s = http.createServer((req, res) => {
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      seen.push({ method: req.method, url: req.url, headers: req.headers, body: Buffer.concat(chunks).toString() })
      const a = answers[Math.min(seen.length, answers.length) - 1]
      res.writeHead(a.status, a.headers)
      res.end(a.body ?? '')
    })
  })
  await new Promise((resolve) => s.listen(0, '127.0.0.1', resolve))
  return { port: s.address().port, seen, close: () => s.close() }
}

async function call(port, input, env = {}) {
  const out = []
  const err = []
  const code = await callServe(input, loadConfig({ HP_FN_PORT: String(port), ...env }),
    { out: (line) => out.push(line), err: (line) => err.push(line) })
  return { code, out, err }
}

test('a request is sent to serve, and its answer written as a result', async (t) => {
  const s = await server([{
    status: 201,
    headers: {
      'content-type': 'application/json', 'x-request-id': 'r-1', 'set-cookie': ['a=1', 'b=2'],
      'x-hivepaas-outcome': 'ok', 'x-hivepaas-duration-ms': '2.5',
    },
    body: '{"ok":true}',
  }])
  t.after(s.close)
  const body = Buffer.from('payload').toString('base64')

  const { code, out } = await call(s.port, JSON.stringify({
    method: 'post', path: '/echo', query: { a: ['1', '2'] }, headers: { 'X-Custom': ['v'] }, body,
  }))

  assert.equal(code, 0)
  assert.equal(out.length, 1)
  assert.ok(out[0].startsWith(RESULT_MARKER))
  const result = JSON.parse(out[0].slice(RESULT_MARKER.length))
  assert.deepEqual(result, {
    status: 201,
    headers: { 'content-type': ['application/json'], 'x-request-id': ['r-1'], 'set-cookie': ['a=1', 'b=2'] },
    body: Buffer.from('{"ok":true}').toString('base64'),
    requestId: 'r-1', durationMs: 2.5, outcome: 'ok',
  })
  const sent = s.seen[0]
  assert.equal(sent.method, 'POST')
  assert.equal(sent.url, '/echo?a=1&a=2')
  assert.equal(sent.headers['x-hivepaas-call'], '1')
  assert.equal(sent.headers['x-custom'], 'v')
  assert.equal(sent.body, 'payload')
})

test('a call turned away is sent again while its timeout allows', async (t) => {
  const busy = { status: 429, headers: { 'retry-after': '0', 'x-hivepaas-outcome': 'throttled' } }
  const s = await server([busy, busy, { status: 200, headers: { 'x-hivepaas-outcome': 'ok' }, body: 'done' }])
  t.after(s.close)

  const { code, out } = await call(s.port, '{}')

  assert.equal(code, 0)
  assert.equal(s.seen.length, 3)
  assert.equal(JSON.parse(out[0].slice(RESULT_MARKER.length)).status, 200)
})

test('a call still turned away at its timeout is a throttled result', async (t) => {
  const s = await server([{ status: 429, headers: { 'retry-after': '1', 'x-hivepaas-outcome': 'throttled' } }])
  t.after(s.close)

  const { code, out } = await call(s.port, '{}', { HP_FN_TIMEOUT_MS: '200' })

  assert.equal(code, 0)
  const result = JSON.parse(out[0].slice(RESULT_MARKER.length))
  assert.deepEqual([result.status, result.outcome], [429, 'throttled'])
  assert.equal(s.seen.length, 1, 'no time was left to wait for a place')
})

test('a request that cannot be read exits 2; no server exits 3', async () => {
  const unreadable = await call(1, 'not json')
  assert.equal(unreadable.code, 2)
  assert.deepEqual(unreadable.out, [])

  const s = await server([])
  const { port } = s
  s.close()
  const nobody = await call(port, '{}')
  assert.equal(nobody.code, 3)
  assert.deepEqual(nobody.out, [])
  assert.match(nobody.err.join('\n'), /serve does not answer/)
})

test('a request that cannot go on an HTTP request line or header exits 2', async () => {
  for (const request of [
    { path: 'no-slash' }, { path: '/a b' }, { method: 'G ET' }, { headers: { 'x-a': ['v\r\nx-b: w'] } },
    { headers: { 'bad name': ['v'] } },
  ]) {
    const { code, out } = await call(1, JSON.stringify(request))
    assert.equal(code, 2, JSON.stringify(request))
    assert.deepEqual(out, [])
  }
})
