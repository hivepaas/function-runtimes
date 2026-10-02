#!/usr/bin/env node
// hivepaas-runtime for Node.js and Bun: serve, invoke, call, health and deps, as
// CONTRACT.md at the repository's root says.
import { spawnSync } from 'node:child_process'
import { existsSync, rmSync } from 'node:fs'
import http from 'node:http'
import { text } from 'node:stream/consumers'

// The runtime is loaded only by the commands that run the handler: call hands
// its request to serve, and starts as fast as a process can.
import { APP_DIR, ConfigError, DEPS_CACHE, depsCommand, engine, LOCK_FILES, loadConfig } from './wire.mjs'

const USAGE = 'usage: hivepaas-runtime serve|invoke|call|health|deps'
// Longer than a proxy in front keeps an idle connection (Traefik: 90s).
const IDLE_TIMEOUT_MS = 5 * 60 * 1000
const HEADERS_TIMEOUT_MS = 10 * 1000
const HEALTH_TIMEOUT_MS = 3 * 1000

async function main(command = 'serve') {
  if (command === 'deps') return deps()
  if (!['serve', 'invoke', 'call', 'health'].includes(command)) {
    console.error(USAGE)
    return 2
  }
  let cfg
  try {
    cfg = loadConfig(process.env)
  } catch (e) {
    if (!(e instanceof ConfigError)) throw e
    console.error(`hivepaas: ${e.message}`)
    return 2
  }
  if (command === 'health') return health(cfg)
  // call hands its request to serve: the handler is not loaded here.
  if (command === 'call') {
    const { callServe } = await import('./call.mjs')
    return callServe(await text(process.stdin), cfg)
  }
  const { Runtime } = await import('./runtime.mjs')
  const runtime = await Runtime.load(cfg)
  if (command === 'invoke') return runtime.invoke(await text(process.stdin))
  return serve(runtime, cfg)
}

/** serve answers until SIGTERM, then lets the running calls finish, for at most one timeout. */
function serve(runtime, cfg) {
  const server = http.createServer({
    requestTimeout: 0, // the runtime bounds a body's arrival itself
    headersTimeout: HEADERS_TIMEOUT_MS,
    keepAliveTimeout: IDLE_TIMEOUT_MS,
  }, runtime.listener())
  return new Promise((resolve) => {
    server.on('error', (e) => {
      console.error(`hivepaas: ${e.message}`)
      resolve(1)
    })
    const stop = () => {
      runtime.stopping = true
      server.close(() => resolve(0))
      server.closeIdleConnections()
      setTimeout(resolve, cfg.timeoutMs, 0).unref()
    }
    process.once('SIGTERM', stop)
    process.once('SIGINT', stop)
    server.listen(cfg.port)
  })
}

/** health asks the server on this container: 0 when it answers 200. */
function health(cfg) {
  return new Promise((resolve) => {
    const req = http.get({ host: '127.0.0.1', port: cfg.port, path: '/_hivepaas/health' }, (res) => {
      res.resume()
      if (res.statusCode !== 200) console.error(`hivepaas: health is ${res.statusCode}`)
      resolve(res.statusCode === 200 ? 0 : 1)
    })
    req.setTimeout(HEALTH_TIMEOUT_MS, () => req.destroy(new Error('no answer')))
    req.on('error', (e) => {
      console.error(`hivepaas: ${e.message}`)
      resolve(1)
    })
  })
}

/** deps installs package.json's dependencies with the engine's manager, which makes the lock file when there is none. */
function deps() {
  if (!existsSync(`${APP_DIR}/package.json`)) return 0
  const runOn = engine()
  const { command, args, env } = depsCommand(runOn, existsSync(`${APP_DIR}/${LOCK_FILES[runOn]}`))
  const install = spawnSync(command, args, { cwd: APP_DIR, stdio: 'inherit', env: { ...process.env, ...env } })
  rmSync(DEPS_CACHE, { recursive: true, force: true })
  if (install.error) console.error(`hivepaas: ${install.error.message}`)
  return install.status ?? 1
}

// Exits at once: an invoke whose handler still runs past its timeout is done.
process.exit(await main(process.argv[2]))
