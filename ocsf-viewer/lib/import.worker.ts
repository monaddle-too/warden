import { parseEvents } from './events';
self.onmessage = (message: MessageEvent<{ text: string; source: string }>) => {
  try {
    self.postMessage({
      result: parseEvents(message.data.text, message.data.source),
    });
  } catch (error) {
    self.postMessage({
      error:
        error instanceof Error ? error.message : 'Could not parse this file.',
    });
  }
};
