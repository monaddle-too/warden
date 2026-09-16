export function JsonView({ value }: { value: unknown }) {
  const json = JSON.stringify(value, null, 2);
  const parts = json.split(
    /("(?:\\.|[^"\\])*"\s*:|"(?:\\.|[^"\\])*"|\btrue\b|\bfalse\b|\bnull\b|-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b)/g,
  );
  return (
    <pre className="json-view">
      {parts.map((part, index) => (
        <span
          key={index}
          className={
            part.startsWith('"')
              ? part.endsWith(':')
                ? 'json-key'
                : 'json-string'
              : /^(true|false|null)$/.test(part)
                ? 'json-literal'
                : /^-?\d/.test(part)
                  ? 'json-number'
                  : undefined
          }
        >
          {part}
        </span>
      ))}
    </pre>
  );
}
