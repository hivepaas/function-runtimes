// The function the conformance suite calls: each path shows one thing the
// contract promises.
import { setTimeout as sleep } from 'node:timers/promises'

import lib from 'fixture-lib'

export default async function (req, ctx) {
  switch (req.path) {
    case '/':
      return undefined
    case '/json':
      return { body: { hello: req.query.name } }
    case '/text':
      return { body: 'hello' }
    case '/bytes':
      return { body: Buffer.from([0, 1, 2, 255]) }
    case '/echo':
      return {
        body: {
          method: req.method,
          path: req.path,
          query: req.query.a,
          queryAll: req.queryAll.a,
          header: req.headers['x-custom'],
          body: req.text(),
          requestId: ctx.requestId,
        },
      }
    case '/status':
      return { status: Number(req.query.code) }
    case '/error':
      throw new Error('fixture failure')
    case '/slow':
      await sleep(Number(req.query.ms)) // past the timeout on purpose
      return { body: 'slow done' }
    case '/big':
      return { body: 'x'.repeat(Number(req.query.bytes)) }
    case '/invalid':
      return { status: 42 }
    case '/env':
      return { body: process.env.GREETING ?? '' }
    case '/deadline':
      return { body: { remainingMs: ctx.deadline - Date.now() } }
    case '/log':
      ctx.log('fixture log line')
      return { body: 'logged' }
    case '/whoami':
      return { body: { uid: process.getuid(), gid: process.getgid() } }
    case '/lib':
      return { body: lib.name() }
  }
  return { status: 404, body: { error: 'no route' } }
}
