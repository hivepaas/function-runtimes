// call: a request handed to the serve running on this container, its answer
// written as invoke writes a result. It loads nothing of the runtime.
import http from 'node:http'

import { invokeRequest, RESULT_MARKER } from './wire.mjs'

// A request with this header is call's: serve's answer to it says the call's
// outcome and duration, and the handler does not see it.
export const CALL_HEADER = 'x-hivepaas-call'
// How much longer than a call's timeout call waits for serve, which answers at it.
const CALL_WAIT_MARGIN_MS = 10 * 1000
// The headers of serve's answer that are the connection's or the runtime's.
const NOT_THE_CALLS = new Set(['content-length', 'transfer-encoding', 'connection', 'keep-alive', 'date',
  'x-hivepaas-outcome', 'x-hivepaas-duration-ms'])

/**
 * callServe hands the request in input - invoke's - to the serve running on
 * this container, and writes its answer as invoke writes a result. A call turned
 * away (429) is sent again after Retry-After while the timeout allows. It
 * returns the exit code: 0 with a result, 2 when the request cannot be read,
 * 3 when serve does not answer.
 */
export async function callServe(input, cfg, io = {}) {
  const out = io.out ?? ((line) => process.stdout.write(`${line}\n`))
  const err = io.err ?? ((text) => process.stderr.write(`${text}\n`))
  let request
  try {
    request = invokeRequest(input)
    checkSendable(request)
  } catch (e) {
    err(`hivepaas: ${e.message}`)
    return 2
  }
  const deadline = Date.now() + cfg.timeoutMs
  for (;;) {
    let answer
    try {
      answer = await sendToServe(cfg.port, request, cfg.timeoutMs + CALL_WAIT_MARGIN_MS)
    } catch (e) {
      err(`hivepaas: serve does not answer: ${e.message}`)
      return 3
    }
    const wait = Number(answer.headers['retry-after']?.[0] ?? 1) * 1000
    if (answer.status === 429 && Date.now() + wait < deadline) {
      await new Promise((resolve) => setTimeout(resolve, wait))
      continue
    }
    out(RESULT_MARKER + JSON.stringify(callResult(answer)))
    return 0
  }
}

function sendToServe(port, request, timeoutMs) {
  return new Promise((resolve, reject) => {
    const headers = { [CALL_HEADER]: '1' }
    for (let i = 0; i + 1 < request.rawHeaders.length; i += 2) {
      const name = request.rawHeaders[i].toLowerCase()
      if (name !== CALL_HEADER) headers[name] = [...(headers[name] ?? []), request.rawHeaders[i + 1]]
    }
    const req = http.request({
      host: '127.0.0.1', port, method: request.method.toUpperCase(), path: request.url, headers, agent: false,
    }, (res) => {
      const chunks = []
      res.on('data', (chunk) => chunks.push(chunk))
      res.on('end', () => resolve({ status: res.statusCode, headers: rawHeadersByName(res.rawHeaders), body: Buffer.concat(chunks) }))
      res.on('error', reject)
    })
    req.setTimeout(timeoutMs, () => req.destroy(new Error('no answer within the timeout')))
    req.on('error', reject)
    req.end(request.body)
  })
}

function rawHeadersByName(rawHeaders) {
  const headers = {}
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    const name = rawHeaders[i].toLowerCase()
    headers[name] = [...(headers[name] ?? []), rawHeaders[i + 1]]
  }
  return headers
}

/** callResult is serve's answer as invoke's result line says one. */
function callResult(answer) {
  const headers = {}
  for (const [name, values] of Object.entries(answer.headers)) {
    if (!NOT_THE_CALLS.has(name)) headers[name] = values
  }
  return {
    status: answer.status,
    headers,
    body: answer.body.toString('base64'),
    requestId: headers['x-request-id']?.[0] ?? '',
    durationMs: Number(answer.headers['x-hivepaas-duration-ms']?.[0] ?? 0),
    outcome: answer.headers['x-hivepaas-outcome']?.[0] ?? 'ok',
  }
}

const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/
// eslint-disable-next-line no-control-regex
const PATH = /^\/[^\s\x00-\x1f\x7f]*$/
// eslint-disable-next-line no-control-regex
const BAD_VALUE = /[\r\n\x00]/

/** checkSendable refuses what cannot go on an HTTP/1.1 request line or header. */
function checkSendable(request) {
  if (!TOKEN.test(request.method)) throw new Error(`the request's method "${request.method}" cannot be sent`)
  if (!PATH.test(request.url)) throw new Error(`the request's path "${request.url}" cannot be sent`)
  for (let i = 0; i + 1 < request.rawHeaders.length; i += 2) {
    if (!TOKEN.test(request.rawHeaders[i]) || BAD_VALUE.test(request.rawHeaders[i + 1])) {
      throw new Error(`the request's header "${request.rawHeaders[i]}" cannot be sent`)
    }
  }
}
