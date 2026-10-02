// The fixture's TypeScript handler: Node.js removes its types as it loads it.
// It imports a file of its own and a type, each by its file's name.
import { greet } from './typed-lib/greet.ts'
import type { Greeting } from './typed-lib/types.ts'

interface Request {
  query: Record<string, string | undefined>
}

export default async function (req: Request): Promise<{ body: Greeting }> {
  return { body: greet(req.query.name ?? 'world') }
}
