import { useEffect, useRef } from 'react';
import { flushSync } from 'react-dom';
import {
  filterEvents,
  object,
  parseQuery,
  type EventRecord,
  type Filters,
} from './events';
type Tool = {
  name: string;
  description: string;
  inputSchema: object;
  annotations: { readOnlyHint: boolean; untrustedContentHint: boolean };
  execute: (input: unknown) => unknown;
};
export function useExplorerTools(
  events: EventRecord[],
  filters: Filters,
  bookmarks: string[],
  update: (patch: Partial<Filters>) => void,
  select: (id: string) => void,
) {
  const state = useRef({ events, filters, bookmarks, update, select });
  useEffect(() => {
    state.current = { events, filters, bookmarks, update, select };
  });
  useEffect(() => {
    const context = (
      document as unknown as {
        modelContext?: {
          registerTool: (
            tool: Tool,
            options: { signal: AbortSignal },
          ) => unknown;
        };
      }
    ).modelContext;
    if (!context?.registerTool) return;
    const lifecycle = new AbortController();
    const register = (tool: Tool) => {
      try {
        void Promise.resolve(
          context.registerTool(tool, { signal: lifecycle.signal }),
        ).catch(() => {});
      } catch {
        /* Enhancement only; the visible UI remains available. */
      }
    };
    register({
      name: 'search_ocsf_events',
      description:
        'Set the visible event search, keeping other filters. Returns up to 20 matching event summaries.',
      inputSchema: {
        type: 'object',
        properties: { query: { type: 'string', maxLength: 2000 } },
        required: ['query'],
        additionalProperties: false,
      },
      annotations: { readOnlyHint: false, untrustedContentHint: true },
      execute(input) {
        if (
          !object(input) ||
          typeof input.query !== 'string' ||
          input.query.length > 2000
        )
          throw new Error('query must be a string of up to 2000 characters.');
        const query = parseQuery(input.query);
        if (query.error) throw new Error(query.error);
        const next = { ...state.current.filters, query: input.query };
        flushSync(() => state.current.update({ query: input.query as string }));
        const matches = filterEvents(
          state.current.events,
          next,
          state.current.bookmarks,
        );
        return {
          total: matches.length,
          events: matches
            .slice(0, 20)
            .map((e) => ({
              id: e.id,
              time: e.time,
              class: e.className,
              severity: e.severityName,
              message: e.message,
            })),
        };
      },
    });
    register({
      name: 'inspect_ocsf_event',
      description:
        'Open an existing event in the visible inspector. Returns its original JSON and basic field diagnostics.',
      inputSchema: {
        type: 'object',
        properties: { id: { type: 'string' } },
        required: ['id'],
        additionalProperties: false,
      },
      annotations: { readOnlyHint: false, untrustedContentHint: true },
      execute(input) {
        if (!object(input) || typeof input.id !== 'string')
          throw new Error('id must be a string.');
        const event = state.current.events.find((e) => e.id === input.id);
        if (!event) throw new Error('Event not found.');
        flushSync(() => state.current.select(event.id));
        return { event: event.raw, issues: event.issues };
      },
    });
    return () => lifecycle.abort();
  }, []);
}
