// TypeScript that is more than types: an enum, and an import that leaves out
// its extension. A runtime that transpiles TypeScript runs it; one that only
// removes types cannot load it.
import { greet } from './typed-lib/greet'

enum Greeting {
  Hello = 'hello',
}

interface Request {
  query: Record<string, string | undefined>
}

export default (req: Request) => ({ body: greet(req.query.name ?? Greeting.Hello) })
