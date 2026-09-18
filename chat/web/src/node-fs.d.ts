// The one Node call the tests make (chat.css.test.ts reads the stylesheets
// from disk, since a "?raw" import is empty under vitest). The app never
// imports Node modules and the build has no @types/node.
declare module "node:fs" {
  export function readFileSync(path: string | URL, encoding: "utf8"): string;
}
