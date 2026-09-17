import { describe, it, expect } from "vitest";
import { TITLE_LIMIT, agentLink, isPunycode, labelHosts } from "./links";

describe("agent links", () => {
  it("shows the host of a plain link without a hint", () => {
    const link = agentLink("https://Example.com/docs?q=1#top", "the docs");
    expect(link).toEqual({
      href: "https://example.com/docs?q=1#top",
      host: "example.com",
      title: "https://example.com/docs?q=1#top",
      hint: false,
    });
    expect(agentLink("http://localhost:8080/", "local")?.host).toBe(
      "localhost:8080",
    );
  });
  it("needs no hint when the label names the same host", () => {
    for (const label of [
      "https://example.com/other",
      "example.com",
      "www.example.com is down",
      "EXAMPLE.COM/path",
      "see https://example.com now",
    ])
      expect(
        agentLink(
          label.startsWith("www")
            ? "https://www.example.com"
            : "https://example.com/x",
          label,
        )?.hint,
        label,
      ).toBe(false);
  });
  it("hints when the label names another host", () => {
    for (const label of [
      "https://apple.com",
      "apple.com/support",
      "see https://apple.com for details",
      "user@apple.com",
      "node.js",
    ])
      expect(agentLink("https://evil.example/x", label)?.hint, label).toBe(
        true,
      );
  });
  it("does not read prose, versions or paths as hosts", () => {
    for (const label of ["click here", "v1.2.3", "a/b.txt", "1.2.3.4", ""])
      expect(labelHosts(label), label).toEqual([]);
    expect(agentLink("https://evil.example/x", "v1.2.3")?.hint).toBe(false);
  });
  it("renders a lookalike host as punycode and hints", () => {
    const link = agentLink("https://аpple.com/", "https://аpple.com/");
    expect(link?.host).toBe("xn--pple-43d.com");
    expect(link?.href).toBe("https://xn--pple-43d.com/");
    expect(link?.hint).toBe(true);
    expect(agentLink("https://xn--pple-43d.com/", "Apple")?.hint).toBe(true);
    expect(isPunycode("xn--pple-43d.com")).toBe(true);
    expect(isPunycode("sub.xn--pple-43d.com")).toBe(true);
    expect(isPunycode("apple.com")).toBe(false);
    expect(isPunycode("xn.example.com")).toBe(false);
  });
  it("drops credentials that read like a host and hints", () => {
    const link = agentLink("https://apple.com:secret@evil.example/", "Apple");
    expect(link?.href).toBe("https://evil.example/");
    expect(link?.title).toBe("https://evil.example/");
    expect(link?.host).toBe("evil.example");
    expect(link?.hint).toBe(true);
  });
  it("leaves anything that is not an http(s) URL as text", () => {
    for (const href of [
      undefined,
      "",
      "mailto:a@b.c",
      "javascript:alert(1)",
      "file:///etc/passwd",
      "ftp://example.com/",
      "docs/readme.md",
      "#top",
      "https://",
      "http://exa mple.com/",
    ])
      expect(agentLink(href, "x"), String(href)).toBeUndefined();
  });
  it("cuts a very long title", () => {
    const link = agentLink("https://example.com/" + "a".repeat(400), "x");
    expect(link?.href.length).toBeGreaterThan(TITLE_LIMIT);
    expect(link?.title.length).toBe(TITLE_LIMIT);
    expect(link?.title.endsWith("…")).toBe(true);
  });
});
