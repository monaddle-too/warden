import { useEffect, useState } from "react";
import Markdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import { needsHighlighter } from "../code";
import { CodeBlock } from "./CodeBlock";

type RehypePlugins = NonNullable<Options["rehypePlugins"]>;

// The highlighter (rehype-highlight + the common highlight.js grammars) is a
// lazy chunk loaded once, the first time a fence with a language shows up;
// the transcript's first paint never pays for it.
let highlighter: RehypePlugins | undefined;
let loading: Promise<RehypePlugins> | undefined;
function loadHighlighter() {
  return (loading ??= import("rehype-highlight").then(
    (m) => (highlighter = [[m.default, { detect: false }]]),
  ));
}

export function RichText({
  text,
  onFile,
}: {
  text: string;
  onFile?: (href: string) => void;
}) {
  const [rehypePlugins, setRehypePlugins] = useState(highlighter);
  const wanted = !rehypePlugins && needsHighlighter(text);
  useEffect(() => {
    if (!wanted) return;
    let stopped = false;
    void loadHighlighter().then(
      (plugins) => {
        if (!stopped) setRehypePlugins(plugins);
      },
      () => {},
    );
    return () => {
      stopped = true;
    };
  }, [wanted]);
  return (
    <div className="rich-text">
      <Markdown
        remarkPlugins={[remarkGfm]}
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
          img: ({ alt }) => <span>{alt || "Image"}</span>,
          // Every fence and indented block goes through CodeBlock, which is
          // also where later renderers (Mermaid, diffs) dispatch by language.
          pre: ({ node, children }) => (
            <CodeBlock node={node}>{children}</CodeBlock>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
