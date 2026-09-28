import { afterEach, expect, it, vi } from "vitest";
import {
  configureReferences,
  invalidateDocumentReferences,
  searchReferences,
  referenceTrigger,
  referenceMarkdown,
  referenceFromURL,
  localReferences,
} from "./references";
afterEach(() => {
  configureReferences("");
  vi.unstubAllGlobals();
});
it("completes multiword titles but preserves paths, resource syntax, emails and links", () => {
  expect(referenceTrigger("See @Annual planning", 20)?.query).toBe(
    "Annual planning",
  );
  for (const text of [
    "a@b.com",
    "@./file",
    "@~/file",
    "@doc:Plan",
    "@repo:main",
    "@preview:web",
    "@src/main",
    "[Link](https://a/@x)",
    "@name\nnext",
  ])
    expect(referenceTrigger(text, text.length), text).toBeUndefined();
  expect(referenceTrigger("See @one @two", 13)?.query).toBe("two");
});
it("escapes Markdown labels and only recognizes canonical same-origin targets", () => {
  vi.stubGlobal("location", { origin: "https://app.example" });
  expect(
    referenceMarkdown({
      kind: "chat",
      id: "a",
      title: "[test]*",
      url: "/?chat=a",
    }),
  ).toBe("[\\[test\\]\\*](https://app.example/?chat=a)");
  expect(referenceFromURL("https://other.example/documents/x")).toBeUndefined();
  expect(referenceFromURL("/?chat=a")).toEqual({ kind: "chat", id: "a" });
});
it("deduplicates requests, retains active subscribers, caches results and isolates organization changes", async () => {
  let complete!: (value: unknown) => void;
  const fetcher = vi.fn(
    () =>
      new Promise((resolve) => {
        complete = resolve;
      }),
  );
  vi.stubGlobal("fetch", fetcher);
  configureReferences("user:org1");
  const first = new AbortController(),
    second = new AbortController();
  const p = searchReferences("Budget", first.signal),
    q = searchReferences("budget", second.signal);
  const rejected = expect(p).rejects.toMatchObject({ name: "AbortError" });
  first.abort();
  await rejected;
  expect(fetcher).toHaveBeenCalledTimes(1);
  const items = [
    { kind: "document", id: "a", title: "Budget", url: "/documents/a" },
  ];
  complete({ ok: true, json: async () => ({ items }) });
  expect(await q).toEqual(items);
  expect(
    await searchReferences("budget", new AbortController().signal),
  ).toEqual(items);
  expect(fetcher).toHaveBeenCalledTimes(1);
  expect(localReferences("budget")).toEqual(items);
  configureReferences("user:org2");
  expect(localReferences("budget")).toEqual([]);
});
it("a late cancelled response cannot repopulate another organization's cache", async () => {
  let complete!: (value: unknown) => void;
  vi.stubGlobal(
    "fetch",
    vi.fn(
      () =>
        new Promise((resolve) => {
          complete = resolve;
        }),
    ),
  );
  configureReferences("org1");
  const controller = new AbortController();
  const p = searchReferences("x", controller.signal);
  const rejected = expect(p).rejects.toMatchObject({ name: "AbortError" });
  configureReferences("org2");
  complete({
    ok: true,
    json: async () => ({
      items: [{ kind: "chat", id: "secret", title: "x", url: "/?chat=secret" }],
    }),
  });
  await rejected;
  expect(localReferences("x")).toEqual([]);
});

it("invalidates deleted document suggestions immediately", async () => {
  configureReferences("org");
  const items = [
    { kind: "document", id: "a", title: "Budget", url: "/documents/a" },
  ];
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });
  vi.stubGlobal("fetch", fetcher);
  await searchReferences("budget", new AbortController().signal);
  expect(localReferences("budget")).toHaveLength(1);
  invalidateDocumentReferences();
  expect(localReferences("budget")).toEqual([]);
  expect(
    await searchReferences("budget", new AbortController().signal),
  ).toEqual([]);
  expect(fetcher).toHaveBeenCalledTimes(2);
});
