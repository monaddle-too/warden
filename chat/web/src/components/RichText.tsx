import { useEffect, useMemo, useState } from "react";
import Markdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import { needsHighlighter } from "../code";
import { workspaceImagePath } from "../images";
import { hasMath } from "../math";
import type { MathPlugins } from "../katex";
import { CodeBlock } from "./CodeBlock";
import { InlineImage } from "./InlineImage";

type RehypePlugins = NonNullable<Options["rehypePlugins"]>;

// The highlighter (rehype-highlight + the common highlight.js grammars) and
// the math stack (remark-math + rehype-katex + KaTeX and its CSS) are lazy
// chunks loaded once, the first time a message needs them; the transcript's
// first paint never pays for either.
let highlighter: RehypePlugins | undefined;
let loadingHighlighter: Promise<RehypePlugins> | undefined;
function loadHighlighter() {
  return (loadingHighlighter ??= import("rehype-highlight").then(
    (m) => (highlighter = [[m.default, { detect: false }]]),
  ));
}

let math: MathPlugins | undefined;
let loadingMath: Promise<MathPlugins> | undefined;
function loadMath() {
  return (loadingMath ??= import("../katex").then((m) => (math = m.plugins)));
}

/* The chunk's value once it has loaded, if `wanted`; a failed load leaves
   the message rendered without it. */
function useChunk<T>(
  cached: T | undefined,
  wanted: boolean,
  load: () => Promise<T>,
): T | undefined {
  const [value, setValue] = useState(cached);
  useEffect(() => {
    if (!wanted || value) return;
    let stopped = false;
    void load().then(
      (loaded) => {
        if (!stopped) setValue(loaded);
      },
      () => {},
    );
    return () => {
      stopped = true;
    };
  }, [wanted, value, load]);
  return value;
}

export function RichText({
  text,
  streaming,
  chatID,
  entryID,
  onFile,
}: {
  text: string;
  /* Set while the entry is still being written; renderers that need the
     whole fence (Mermaid) wait for it to clear. */
  streaming?: boolean;
  /* Where `![alt](path)` images are read from; without a chat they stay
     as alt text. */
  chatID?: string;
  entryID?: string;
  onFile?: (href: string) => void;
}) {
  const highlight = useChunk(
    highlighter,
    needsHighlighter(text),
    loadHighlighter,
  );
  const mathPlugins = useChunk(math, hasMath(text), loadMath);
  const remarkPlugins = useMemo(
    () => [remarkGfm, ...(mathPlugins?.remark ?? [])],
    [mathPlugins],
  );
  // KaTeX first, so a ```math fence becomes display math before the
  // highlighter could see a `language-math` block it has no grammar for.
  const rehypePlugins = useMemo(
    () => [...(mathPlugins?.rehype ?? []), ...(highlight ?? [])],
    [mathPlugins, highlight],
  );
  return (
    <div className="rich-text">
      <Markdown
        remarkPlugins={remarkPlugins}
        rehypePlugins={rehypePlugins}
        components={{
          a: ({ href, children }) =>
            href?.startsWith("https://") || href?.startsWith("http://") ? (
              <a href={href} target="_blank" rel="noopener noreferrer">
                {children}
              </a>
            ) : href && !href.startsWith("#") && onFile ? (
              <a
                href={href}
                onClick={(event) => {
                  event.preventDefault();
                  onFile(href);
                }}
              >
                {children}
              </a>
            ) : (
              <span>{children}</span>
            ),
          // Only a relative workspace path becomes an image, read through
          // the chat service; a URL or data: source stays as its alt text.
          img: ({ src, alt }) => {
            const path = chatID && workspaceImagePath(src);
            return path ? (
              <InlineImage
                chatID={chatID}
                entryID={entryID || ""}
                path={path}
                alt={alt}
              />
            ) : (
              <span>{alt || "Image"}</span>
            );
          },
          // Every fence and indented block goes through CodeBlock, which is
          // also where later renderers (Mermaid, diffs) dispatch by language.
          pre: ({ node, children }) => (
            <CodeBlock node={node} streaming={streaming}>
              {children}
            </CodeBlock>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
