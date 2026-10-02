// The JavaScript function runtime, on Node.js or on Bun: how a handler is
// loaded and called, and what its calls answer. See CONTRACT.md at the
// repository's root.
import { randomBytes } from 'node:crypto'
import http from 'node:http'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { format } from 'node:util'

import { CALL_HEADER } from './call.mjs'
import { APP_DIR, invokeRequest, RESULT_MARKER } from './wire.mjs'

export { CALL_HEADER, callServe } from './call.mjs'
export {
  APP_DIR, ConfigError, DEPS_CACHE, depsCommand, engine, LOCK_FILES, loadConfig, RESULT_MARKER,
} from './wire.mjs'


const REQUEST_ID = /^[A-Za-z0-9._-]{1,128}$/
const BODYLESS = new Set([204, 304])
const EMPTY = Buffer.alloc(0)
const TIMEOUT = Symbol('timeout')
const TOO_LARGE = Symbol('too large')

/** A call that failed; outcome says how. */
class CallError extends Error {
  constructor(message, outcome = 'error') {
    super(message)
    this.outcome = outcome
  }
}

/** The handler: the module's export cfg.handler names, ES module or CommonJS. */
export async function loadHandler(appDir, cfg) {
  const ns = await import(pathToFileURL(path.resolve(appDir, cfg.entrypoint)).href)
  let fn = ns[cfg.handler]
  if (fn === undefined && ns.default !== null && typeof ns.default === 'object') fn = ns.default[cfg.handler]
  if (typeof fn !== 'function' && typeof fn?.default === 'function') fn = fn.default // exports.default
  if (typeof fn !== 'function') throw new Error(`${cfg.entrypoint} has no function "${cfg.handler}"`)
  return fn
}

/** The request a handler is given. */
export function requestOf(method, rawUrl, rawHeaders, body) {
  const q = rawUrl.indexOf('?')
  const query = Object.create(null)
  const queryAll = Object.create(null)
  for (const [name, value] of new URLSearchParams(q === -1 ? '' : rawUrl.slice(q + 1))) {
    if (!(name in queryAll)) {
      queryAll[name] = []
      query[name] = value
    }
    queryAll[name].push(value)
  }
  const headers = Object.create(null)
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    const name = rawHeaders[i].toLowerCase()
    headers[name] = name in headers ? `${headers[name]}, ${rawHeaders[i + 1]}` : rawHeaders[i + 1]
  }
  return {
    method: method.toUpperCase(),
    path: q === -1 ? rawUrl : rawUrl.slice(0, q),
    query,
    queryAll,
    headers,
    body,
    text: () => body.toString('utf8'),
    json: () => JSON.parse(body.toString('utf8')),
  }
}

/** The answer to what a handler returned; a CallError when it is not a response. */
export function toAnswer(value, maxBodySize) {
  if (value === undefined || value === null) return { status: 204, headers: {}, body: EMPTY }
  if (typeof value !== 'object' || Array.isArray(value) || ArrayBuffer.isView(value)) {
    throw new CallError('the handler returned what is not a response: an object with status, headers and body')
  }
  const status = value.status ?? 200
  if (!Number.isInteger(status) || status < 200 || status > 599) {
    throw new CallError(`the response's status ${format('%o', status)} is not a whole number from 200 to 599`)
  }
  const headers = headersOf(value.headers)
  const [body, type] = bodyOf(value.body)
  if (body.length > maxBodySize) {
    throw new CallError(`the response's body of ${body.length} bytes is larger than HP_FN_MAX_BODY_SIZE, ` +
      `${maxBodySize}`, 'response_too_large')
  }
  if (BODYLESS.has(status)) return { status, headers, body: EMPTY }
  if (body.length > 0 && type && !('content-type' in headers)) headers['content-type'] = type
  return { status, headers, body }
}

function headersOf(given) {
  const headers = {}
  if (given === undefined || given === null) return headers
  let entries
  if (given instanceof Headers) entries = [...given]
  else if (typeof given === 'object' && !Array.isArray(given)) entries = Object.entries(given)
  else throw new CallError("the response's headers are not an object")
  for (const [name, value] of entries) {
    const values = Array.isArray(value) ? value.map(String) : [String(value)]
    try {
      http.validateHeaderName(name)
      for (const one of values) http.validateHeaderValue(name, one)
    } catch (e) {
      throw new CallError(`the response's header ${JSON.stringify(name)} cannot be sent: ${e.message}`)
    }
    headers[name.toLowerCase()] = Array.isArray(value) ? values : values[0]
  }
  return headers
}

function bodyOf(body) {
  if (body === undefined || body === null) return [EMPTY, undefined]
  if (typeof body === 'string') return [Buffer.from(body, 'utf8'), 'text/plain; charset=utf-8']
  if (ArrayBuffer.isView(body)) {
    return [Buffer.from(body.buffer, body.byteOffset, body.byteLength), 'application/octet-stream']
  }
  if (body instanceof ArrayBuffer) return [Buffer.from(body), 'application/octet-stream']
  let json
  try {
    json = JSON.stringify(body)
  } catch (e) {
    throw new CallError(`the response's body cannot be JSON: ${e.message}`)
  }
  if (json === undefined) throw new CallError("the response's body cannot be JSON")
  return [Buffer.from(json, 'utf8'), 'application/json']
}

function failure(status, message, id, outcome) {
  const body = Buffer.from(JSON.stringify({ error: message, requestId: id }))
  return { status, headers: { 'content-type': 'application/json' }, body, outcome, durationMs: 0 }
}

function own(status, value) {
  return { status, headers: { 'content-type': 'application/json' }, body: Buffer.from(JSON.stringify(value)) }
}

function requestId(sent) {
  return REQUEST_ID.test(sent ?? '') ? sent : randomBytes(16).toString('hex')
}

function withoutHeader(rawHeaders, name) {
  const kept = []
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    if (rawHeaders[i].toLowerCase() !== name) kept.push(rawHeaders[i], rawHeaders[i + 1])
  }
  return kept
}

function firstHeader(rawHeaders, name) {
  for (let i = 0; i + 1 < rawHeaders.length; i += 2) {
    if (rawHeaders[i].toLowerCase() === name) return rawHeaders[i + 1]
  }
  return undefined
}

function describe(error) {
  if (error instanceof CallError) return error.message
  return error instanceof Error ? error.stack : format('%o', error)
}

function milliseconds(since) {
  return Math.round((performance.now() - since) * 1000) / 1000
}

/** Runtime runs one handler under the configured limits. */
export class Runtime {
  constructor(handler, cfg, io = {}) {
    this.handler = handler // null when it could not be loaded
    this.cfg = cfg
    this.running = 0
    this.stopping = false
    this.closed = false
    const out = io.out ?? ((line) => process.stdout.write(`${line}\n`))
    this.out = (line) => this.closed || out(line)
    this.err = io.err ?? ((text) => process.stderr.write(`${text}\n`))
  }

  /** load is a runtime for the function in /app; a handler that cannot be loaded is logged. */
  static async load(cfg, io) {
    const runtime = new Runtime(null, cfg, io)
    try {
      runtime.handler = await loadHandler(APP_DIR, cfg)
    } catch (e) {
      runtime.err(`hivepaas: the handler could not be loaded: ${describe(e)}`)
    }
    return runtime
  }

  /** call runs the handler on request within the timeout. */
  async call(request, id) {
    const start = performance.now()
    const controller = new AbortController()
    const ctx = {
      requestId: id,
      deadline: Date.now() + this.cfg.timeoutMs,
      signal: controller.signal,
      log: (...args) => this.out(JSON.stringify({ hp: 'log', requestId: id, msg: format(...args) })),
    }
    // A call that times out keeps its place until the handler returns.
    this.running++
    const handled = (async () => this.handler(request, ctx))()
      .then((value) => ({ value }), (error) => ({ error }))
      .finally(() => this.running--)
    let timer
    const timedOut = new Promise((resolve) => {
      timer = setTimeout(resolve, this.cfg.timeoutMs, TIMEOUT)
    })
    const settled = await Promise.race([handled, timedOut])
    clearTimeout(timer)
    if (settled === TIMEOUT) {
      controller.abort(new Error('the call outlived its timeout'))
      return { ...failure(504, 'timeout', id, 'timeout'), durationMs: milliseconds(start) }
    }
    try {
      if ('error' in settled) throw settled.error
      return { ...toAnswer(settled.value, this.cfg.maxBodySize), outcome: 'ok', durationMs: milliseconds(start) }
    } catch (e) {
      this.err(`hivepaas: call ${id} failed: ${describe(e)}`)
      return { ...failure(500, 'internal error', id, e?.outcome ?? 'error'), durationMs: milliseconds(start) }
    }
  }

  logInvocation(id, method, urlPath, answer) {
    this.out(JSON.stringify({
      hp: 'invocation', requestId: id, method, path: urlPath,
      status: answer.status, durationMs: answer.durationMs, outcome: answer.outcome,
    }))
  }

  /** listener answers HTTP requests. */
  listener() {
    return (req, res) => {
      this.serve(req, res).catch((e) => {
        this.err(`hivepaas: ${describe(e)}`)
        res.destroy()
      })
    }
  }

  async serve(req, res) {
    const id = requestId(firstHeader(req.rawHeaders, 'x-request-id'))
    const isCall = firstHeader(req.rawHeaders, CALL_HEADER) === '1'
    const q = req.url.indexOf('?')
    const urlPath = q === -1 ? req.url : req.url.slice(0, q)
    if (urlPath.startsWith('/_hivepaas/')) {
      this.send(res, id, this.ownPath(urlPath), isCall)
      return
    }
    const method = req.method.toUpperCase()
    const answer = (a) => {
      this.logInvocation(id, method, urlPath, a)
      this.send(res, id, a, isCall)
    }

    // A body refused unread closes the connection: what is left of it is not read.
    const tooLarge = () => {
      const refused = failure(413, 'request too large', id, 'request_too_large')
      refused.headers.connection = 'close'
      return refused
    }
    if (Number(req.headers['content-length']) > this.cfg.maxBodySize) {
      answer(tooLarge())
      return
    }
    let body
    try {
      body = await readBody(req, this.cfg)
    } catch {
      req.socket.destroy() // the body did not arrive: not a call
      return
    }
    if (body === TOO_LARGE) {
      answer(tooLarge())
      return
    }
    if (this.handler === null) {
      answer(failure(500, 'internal error', id, 'not_loaded'))
      return
    }
    if (this.running >= this.cfg.maxConcurrency) {
      const throttled = failure(429, 'too many requests', id, 'throttled')
      throttled.headers['retry-after'] = '1'
      answer(throttled)
      return
    }
    answer(await this.call(requestOf(req.method, req.url, withoutHeader(req.rawHeaders, CALL_HEADER), body), id))
  }

  ownPath(urlPath) {
    if (urlPath === '/_hivepaas/health') {
      return this.handler === null ? own(503, { status: 'unavailable' }) : own(200, { status: 'ok' })
    }
    return own(404, { error: 'not found' })
  }

  /** send writes answer; to a call, what the runtime made of it too. */
  send(res, id, answer, isCall = false) {
    for (const [name, value] of Object.entries(answer.headers)) res.setHeader(name, value)
    res.removeHeader('transfer-encoding')
    res.setHeader('x-request-id', id)
    if (BODYLESS.has(answer.status)) res.removeHeader('content-length')
    else res.setHeader('content-length', answer.body.length)
    if (this.stopping) res.setHeader('connection', 'close')
    if (isCall && answer.outcome !== undefined) {
      res.setHeader('x-hivepaas-outcome', answer.outcome)
      res.setHeader('x-hivepaas-duration-ms', String(answer.durationMs ?? 0))
    }
    res.writeHead(answer.status)
    res.end(answer.body)
  }

  /** invoke calls the handler once with the request in input; it returns the exit code. */
  async invoke(input) {
    if (this.handler === null) return 3
    let request
    try {
      request = invokeRequest(input)
    } catch (e) {
      this.err(`hivepaas: ${e.message}`)
      return 2
    }
    const id = requestId(firstHeader(request.rawHeaders, 'x-request-id'))
    const method = request.method.toUpperCase()
    const answer = request.body.length > this.cfg.maxBodySize
      ? failure(413, 'request too large', id, 'request_too_large')
      : await this.call(requestOf(method, request.url, request.rawHeaders, request.body), id)
    this.logInvocation(id, method, request.path, answer)
    const headers = {}
    for (const [name, value] of Object.entries(answer.headers)) headers[name] = [value].flat()
    headers['x-request-id'] = [id]
    this.out(RESULT_MARKER + JSON.stringify({
      status: answer.status, headers, body: answer.body.toString('base64'),
      requestId: id, durationMs: answer.durationMs, outcome: answer.outcome,
    }))
    this.closed = true // nothing after the result, even from a handler still running
    return 0
  }
}

function readBody(req, cfg) {
  return new Promise((resolve, reject) => {
    const chunks = []
    let size = 0
    const timer = setTimeout(() => reject(new Error('the body did not arrive in time')), cfg.timeoutMs)
    req.on('data', (chunk) => {
      size += chunk.length
      if (size > cfg.maxBodySize) {
        clearTimeout(timer)
        resolve(TOO_LARGE)
        return
      }
      chunks.push(chunk)
    })
    req.on('end', () => {
      clearTimeout(timer)
      resolve(Buffer.concat(chunks))
    })
    req.on('error', (e) => {
      clearTimeout(timer)
      reject(e)
    })
    req.on('close', () => {
      clearTimeout(timer)
      if (!req.complete) reject(new Error('the client went away'))
    })
  })
}
