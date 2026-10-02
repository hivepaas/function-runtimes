// An enum is TypeScript that cannot be removed: Node.js cannot load this file.
enum Greeting {
  Hello = 'hello',
}

export default () => ({ body: Greeting.Hello })
