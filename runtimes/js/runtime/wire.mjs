// What crosses the JavaScript runtime's edges, and nothing heavier: its
// configuration from the environment, invoke's request and result marker, and
// what deps runs. call reads it without loading the runtime.
import path from 'node:path'

export const APP_DIR = '/app'
export const RESULT_MARKER = '#hivepaas-result '

const WHOLE_NUMBER = /^\+?[0-9]+$/
const BASE64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/
const MAX_LIMIT = 2147483647

// Where deps keeps what it downloads while it installs; removed after.
export const DEPS_CACHE = '/tmp/hivepaas-deps-cache'

/** engine is what runs the runtime: 'bun' or 'node'. */
export function engine() {
  return process.versions.bun ? 'bun' : 'node'
}

// Each engine's default entrypoint: Bun runs TypeScript whole.
const DEFAULT_ENTRYPOINT = { node: 'index.js', bun: 'index.ts' }

/** Each engine's lock file of package.json's dependencies. */
export const LOCK_FILES = { node: 'package-lock.json', bun: 'bun.lock' }

/**
 * depsCommand installs package.json's dependencies with the engine's own
 * manager: from its lock file when there is one (locked), which it makes when
 * there is none. Bun reads an npm lock file to make its own.
 */
export function depsCommand(runOn, locked) {
  if (runOn === 'bun') {
    return {
      command: 'bun',
      args: ['install', '--production', ...(locked ? ['--frozen-lockfile'] : [])],
      env: { BUN_INSTALL_CACHE_DIR: DEPS_CACHE },
    }
  }
  return {
    command: 'npm',
    args: [locked ? 'ci' : 'install', '--omit=dev', '--no-audit', '--no-fund', '--cache', DEPS_CACHE],
    env: {},
  }
}

/** A configuration the runtime cannot start with. */
export class ConfigError extends Error {}

export function loadConfig(env, runOn = engine()) {
  const entrypoint = env.HP_FN_ENTRYPOINT || DEFAULT_ENTRYPOINT[runOn]
  if (path.isAbsolute(entrypoint) || !path.resolve(APP_DIR, entrypoint).startsWith(APP_DIR + path.sep)) {
    throw new ConfigError(`HP_FN_ENTRYPOINT is "${entrypoint}", which is not a file of the function`)
  }
  return {
    entrypoint,
    handler: env.HP_FN_HANDLER || 'default',
    port: positive(env, 'HP_FN_PORT', 8080),
    timeoutMs: positive(env, 'HP_FN_TIMEOUT_MS', 30000),
    maxConcurrency: positive(env, 'HP_FN_MAX_CONCURRENCY', 16),
    maxBodySize: positive(env, 'HP_FN_MAX_BODY_SIZE', 6291456),
  }
}

function positive(env, name, fallback) {
  const value = env[name]
  if (value === undefined || value === '') return fallback
  const n = Number(value)
  if (!WHOLE_NUMBER.test(value) || n <= 0 || n > MAX_LIMIT) {
    throw new ConfigError(`${name} is "${value}", which is not a positive whole number`)
  }
  return n
}

/** invokeRequest reads invoke's input: JSON, every field optional. */
export function invokeRequest(input) {
  let given
  try {
    given = JSON.parse(input)
  } catch (e) {
    throw new Error(`the request is not JSON: ${e.message}`)
  }
  if (given === null || typeof given !== 'object' || Array.isArray(given)) {
    throw new Error('the request is not a JSON object')
  }
  const encoded = given.body ?? ''
  if (typeof encoded !== 'string' || !BASE64.test(encoded)) throw new Error("the request's body is not base64")
  const urlPath = given.path || '/'
  const query = new URLSearchParams(pairs(given.query, 'query')).toString()
  return {
    method: given.method || 'GET',
    path: urlPath,
    url: query === '' ? urlPath : `${urlPath}?${query}`,
    rawHeaders: pairs(given.headers, 'headers').flat(),
    body: Buffer.from(encoded, 'base64'),
  }
}

function pairs(given, what) {
  if (given === undefined || given === null) return []
  if (typeof given !== 'object' || Array.isArray(given)) throw new Error(`the request's ${what} is not an object`)
  return Object.entries(given).flatMap(([name, values]) => [values].flat().map((value) => [name, String(value)]))
}
