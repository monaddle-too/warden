// Pure helpers behind the math rendering in RichText: the detector that
// decides whether a message pays for the KaTeX chunk, and the options KaTeX
// runs with. The chunk itself is `katex.ts`.
import type { KatexOptions } from "katex";

/* KaTeX runs on agent text. `trust: false` refuses \href, \url,
   \includegraphics and the \html* commands (nothing KaTeX emits can then
   carry a URL, a class or a style the agent chose); colours are validated by
   KaTeX to hex or a name, so \color cannot smuggle CSS either. `maxSize`
   caps \rule and friends so a formula cannot paint the whole page,
   `maxExpand` bounds macro recursion, and `strict: "ignore"` keeps agent
   text from writing warnings to the owner's console. Errors render the
   source in the danger colour rather than throwing. */
export const KATEX_OPTIONS: KatexOptions = {
  trust: false,
  throwOnError: false,
  strict: "ignore",
  maxSize: 10,
  maxExpand: 1000,
  errorColor: "var(--danger)",
};

/* Whether remark-math would find math in the markdown. Display math is any
   `$$` or a ```math fence; inline math follows Pandoc's rule (an opening
   `$` right before a non-space, a closing `$` right after one and not
   followed by a digit, on one line), which is stricter than remark-math's
   own. The gate matters beyond the chunk size: remark-math would also read
   "$5 and $10" as math, so a message without real math keeps its dollars. */
export function hasMath(markdown: string): boolean {
  return (
    markdown.includes("$$") ||
    /^ {0,3}(`{3,}|~{3,}) *math\b/im.test(markdown) ||
    /(^|[^\\$])\$(?=\S)[^$\n]*?\S\$(?!\d)/m.test(markdown)
  );
}
