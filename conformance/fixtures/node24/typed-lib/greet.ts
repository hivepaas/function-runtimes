import type { Greeting } from './types.ts'

export function greet(name: string): Greeting {
  return { hello: name }
}
