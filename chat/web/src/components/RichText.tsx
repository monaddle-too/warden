import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { ExternalLink } from "lucide-react";
import Markdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import { hastText, needsHighlighter, type HastNode } from "../code";
import { workspaceImagePath } from "../images";
import { agentLink } from "../links";
import { hasMath } from "../math";
import { displayText, holdOpenMath, openFrom, touchesEnd } from "../streaming";
import type { MathPlugins } from "../katex";
import { CodeBlock } from "./CodeBlock";
import { InlineImage } from "./InlineImage";

type RehypePlugins = NonNullable<Options["rehypePlugins"]>;
type Components = NonNullable<Options["components"]>;

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

// What a block needs to know about the message it is in while it streams:
// a context rather than props, so the component map below stays the same
// object across chunks. A new map would be a new component type to React,
// which would remount every block on every chunk and lose its state (wrap,
// expanded, a rendered diagram).
const Stream = createContext({ streaming: false, length: 0 });

/* Every fence and indented block goes through CodeBlock, which is also
   where Mermaid and diffs dispatch by language. */
function Pre({ node, children }: { node?: HastNode; children?: ReactNode }) {
  const { streaming, length } = useContext(Stream);
  return (
    <CodeBlock node={node} open={streaming && touchesEnd(node, length)}>
      {children}
    </CodeBlock>
  );
}

/* An http(s) link an agent wrote: opens in a new tab, its real host (ASCII,
   credentials dropped) in the hover title, an external-link glyph after the
   label, and the host beside the label when the label would mislead (see
   `links.ts`). Anything else the agent linked stays as text. */
function AgentLink({
  href,
  node,
  children,
}: {
  href?: string;
  node?: HastNode;
  children?: ReactNode;
}) {
  const link = agentLink(href, hastText(node));
  if (!link) return <span>{children}</span>;
  return (
    <>
      <a
        className="agent-link"
        href={link.href}
        title={link.title}
        target="_blank"
        rel="noopener noreferrer"
      >
        {children}
        <ExternalLink size={12} aria-hidden="true" />
      </a>
      {link.hint && <span className="link-host">{link.host}</span>}
    </>
  );
}

export function RichText({
  text: source,
  streaming,
  chatID,
  entryID,
  onFile,
  agent,
}: {
  text: string;
  /* Set while the entry is still being written: tail lines whose reading is
     not settled are held back, and a fence or formula that reaches the end
     of the text is shown as source until it closes (see `streaming.ts`). */
  streaming?: boolean;
  /* Where `![alt](path)` images are read from; without a chat they stay
     as alt text. */
  chatID?: string;
  entryID?: string;
  onFile?: (href: string) => void;
  /* Set for text an agent wrote: its links show where they really go
     (`AgentLink`); the owner's own links are shown as written. */
  agent?: boolean;
}) {
  const text = useMemo(
    () => (streaming ? displayText(source) : source),
    [source, streaming],
  );
  const highlight = useChunk(
    highlighter,
    needsHighlighter(text),
    loadHighlighter,
  );
  const mathPlugins = useChunk(math, hasMath(text), loadMath);
  const remarkPlugins = useMemo(
    () => [
      remarkGfm,
      ...(mathPlugins?.remark ?? []),
      ...(mathPlugins && streaming ? [holdOpenMath] : []),
    ],
    [mathPlugins, streaming],
  );
  // KaTeX first, so a ```math fence becomes display math before the
  // highlighter could see a `language-math` block it has no grammar for.
  const rehypePlugins = useMemo(
    () => [...(mathPlugins?.rehype ?? []), ...(highlight ?? [])],
    [mathPlugins, highlight],
  );
  const stream = useMemo(
    () => ({ streaming: !!streaming, length: streaming ? openFrom(text) : 0 }),
    [streaming, text],
  );
  const components = useMemo<Components>(
    () => ({
      a: ({ href, node, children }) =>
        // Any scheme in an agent's link goes through AgentLink; a workspace
        // path or fragment is handled below as for the owner's own text.
        agent && /^[a-z][a-z0-9+.-]*:/i.test(href ?? "") ? (
          <AgentLink href={href} node={node}>
            {children}
          </AgentLink>
        ) : href?.startsWith("https://") || href?.startsWith("http://") ? (
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
      // Only a relative workspace path becomes an image, read through the
      // chat service; a URL or data: source stays as its alt text.
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
      pre: Pre,
    }),
    [chatID, entryID, onFile, agent],
  );
  return (
    <div className="rich-text">
      <Stream.Provider value={stream}>
        <Markdown
          remarkPlugins={remarkPlugins}
          rehypePlugins={rehypePlugins}
          components={components}
        >
          {text}
        </Markdown>
      </Stream.Provider>
    </div>
  );
}
