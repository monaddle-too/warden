// The Node calls the tests make (chat.css.test.ts reads the stylesheets
// from disk, since a "?raw" import is empty under vitest; shortcuts.test.ts
// walks the components' sources). The app never imports Node modules and
// the build has no @types/node.
declare module "node:fs" {
  export function readFileSync(path: string | URL, encoding: "utf8"): string;
  export function readdirSync(path: string | URL): string[];
  export function statSync(path: string | URL): { isDirectory(): boolean };
}
declare module "node:path" {
  export function join(...parts: string[]): string;
}
