'use client';
// The lint compiler currently throws an internal invariant on the file-drop handler.
/* oxlint-disable react/react-compiler */
import { useEffect, useRef, useState } from 'react';
import {
  AlertTriangle,
  Check,
  FileJson,
  LoaderCircle,
  Upload,
} from 'lucide-react';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from '@/components/ui/dialog';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { MAX_BYTES, MAX_EVENTS, type ImportResult } from '@/lib/events';
export function ImportDialog({
  open,
  onOpenChange,
  onImport,
  currentCount,
  isSample,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onImport: (result: ImportResult, name: string, append: boolean) => void;
  currentCount: number;
  isSample: boolean;
}) {
  const [text, setText] = useState('');
  const [preview, setPreview] = useState<ImportResult | null>(null);
  const [name, setName] = useState('Pasted events');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [append, setAppend] = useState(false);
  const [drag, setDrag] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const worker = useRef<Worker | null>(null);
  const generation = useRef(0);
  useEffect(() => {
    if (!open) {
      generation.current++;
      worker.current?.terminate();
    }
  }, [open]);
  useEffect(() => () => worker.current?.terminate(), []);
  function analyze(content: string, label: string) {
    worker.current?.terminate();
    const job = ++generation.current;
    setPreview(null);
    setError('');
    setName(label);
    setBusy(true);
    try {
      const instance = new Worker(
        new URL('../lib/import.worker.ts', import.meta.url),
        { type: 'module' },
      );
      worker.current = instance;
      instance.onmessage = (
        message: MessageEvent<{ result?: ImportResult; error?: string }>,
      ) => {
        if (generation.current !== job) return;
        setBusy(false);
        if (message.data.error) setError(message.data.error);
        else if (message.data.result) setPreview(message.data.result);
        instance.terminate();
      };
      instance.onerror = () => {
        if (generation.current !== job) return;
        setBusy(false);
        setError(
          'Could not start the import worker. Reload the page and try again.',
        );
        instance.terminate();
      };
      instance.postMessage({
        text: content,
        source: `import-${Date.now()}-${job}`,
      });
    } catch {
      setBusy(false);
      setError(
        'Your browser could not start the import worker. Try a current desktop browser.',
      );
    }
  }
  async function file(file: File | undefined) {
    if (!file) return;
    generation.current++;
    const current = generation.current;
    setPreview(null);
    setError('');
    setBusy(true);
    if (file.size > MAX_BYTES) {
      setBusy(false);
      setError('File exceeds 25 MB. Split it into smaller files.');
      return;
    }
    try {
      const content = await file.text();
      if (generation.current === current) analyze(content, file.name);
    } catch {
      if (generation.current === current) {
        setBusy(false);
        setError(
          'Could not read the file. Check that it is accessible and try again.',
        );
      }
    }
  }
  const tooMany =
    preview &&
    append &&
    !isSample &&
    preview.events.length + currentCount > MAX_EVENTS;
  return (
    <Dialog
      open={open}
      onOpenChange={(value) => {
        if (!value) {
          generation.current++;
          worker.current?.terminate();
          setBusy(false);
        }
        onOpenChange(value);
      }}
    >
      <DialogContent className="import-modal">
        <DialogTitle>Import OCSF events</DialogTitle>
        <DialogDescription>
          Open JSON, an event array, an {'{ events: […] }'} envelope, or
          newline-delimited JSON. Files stay on this device.
        </DialogDescription>
        <Tabs
          defaultValue="file"
          onValueChange={() => {
            generation.current++;
            worker.current?.terminate();
            setBusy(false);
            setPreview(null);
            setError('');
          }}
        >
          <TabsList>
            <TabsTrigger value="file">Open file</TabsTrigger>
            <TabsTrigger value="paste">Paste JSON</TabsTrigger>
          </TabsList>
          <TabsContent value="file">
            <div
              className={`drop-zone ${drag ? 'dragging' : ''}`}
              onDragOver={(e) => {
                e.preventDefault();
                setDrag(true);
              }}
              onDragLeave={() => setDrag(false)}
              onDrop={(e) => {
                e.preventDefault();
                setDrag(false);
                void file(e.dataTransfer.files[0]);
              }}
            >
              <Upload size={27} />
              <strong>Drop an event file here</strong>
              <span>JSON, JSONL, or NDJSON · up to 25 MB / 50,000 records</span>
              <Button
                variant="outline"
                disabled={busy}
                onClick={() => fileInput.current?.click()}
              >
                Choose file
              </Button>
              <input
                type="file"
                accept=".json,.jsonl,.ndjson,application/json"
                className="sr-only"
                ref={fileInput}
                aria-label="Choose event file"
                onChange={(e) => {
                  void file(e.target.files?.[0]);
                  e.target.value = '';
                }}
              />
            </div>
          </TabsContent>
          <TabsContent value="paste">
            <textarea
              className="paste-input"
              aria-label="OCSF JSON to import"
              placeholder={'{"class_uid":3002,"activity_id":1,…}'}
              value={text}
              onChange={(e) => {
                setText(e.target.value);
                setPreview(null);
              }}
              spellCheck={false}
            />
            <Button
              variant="outline"
              disabled={busy || !text.trim()}
              onClick={() => analyze(text, 'Pasted events')}
            >
              <FileJson />
              Check pasted events
            </Button>
          </TabsContent>
        </Tabs>
        {busy && (
          <output className="import-progress">
            <LoaderCircle className="animate-spin" size={17} />
            Reading and checking events…
          </output>
        )}
        {error && (
          <div className="import-error" role="alert">
            <AlertTriangle size={17} />
            {error}
          </div>
        )}
        {preview && !busy && (
          <div className="import-preview">
            <div className="import-preview-heading">
              <Check size={18} />
              <strong>
                {preview.events.length.toLocaleString()} readable events
              </strong>
              <span>{name}</span>
            </div>
            <p>
              {preview.events
                .filter((e) => e.issues.length)
                .length.toLocaleString()}{' '}
              events have basic field issues. These remain available for
              inspection.
            </p>
            {preview.rejected > 0 && (
              <div className="import-error">
                <AlertTriangle size={17} />
                <span>
                  {preview.rejected.toLocaleString()} malformed records will be
                  skipped if you continue.
                </span>
              </div>
            )}
            {preview.problems.length > 0 && (
              <details>
                <summary>
                  Review skipped records
                  {preview.rejected > 100 ? ' (first 100)' : ''}
                </summary>
                <ul>
                  {preview.problems.map((problem, i) => (
                    <li key={i}>
                      <strong>{problem.location}:</strong> {problem.message}
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </div>
        )}
        {!isSample && currentCount > 0 && (
          <label className="append-choice">
            <input
              type="checkbox"
              checked={append}
              onChange={(e) => setAppend(e.target.checked)}
            />
            Append to the current {currentCount.toLocaleString()} events
          </label>
        )}
        {tooMany && (
          <p className="error-text">
            Combined data exceeds 50,000 events. Replace the current dataset
            instead.
          </p>
        )}
        <div className="modal-footer">
          <span>
            {append && !isSample
              ? 'Adds to current dataset.'
              : 'Replaces the current dataset.'}{' '}
            Data is held for this tab session.
          </span>
          <Button
            disabled={busy || !preview?.events.length || !!tooMany}
            onClick={() => {
              if (preview) {
                onImport(preview, name, append && !isSample);
                onOpenChange(false);
              }
            }}
          >
            Import{' '}
            {preview?.events.length
              ? preview.events.length.toLocaleString()
              : ''}{' '}
            events
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
