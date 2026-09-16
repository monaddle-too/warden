import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
export function RichText({
  text,
  onFile,
}: {
  text: string;
  onFile?: (href: string) => void;
}) {
  return (
    <div className="rich-text">
      <Markdown
        remarkPlugins={[remarkGfm]}
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
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
