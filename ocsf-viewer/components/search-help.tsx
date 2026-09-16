'use client';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
} from '@/components/ui/dialog';
export function SearchHelp({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (value: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="help-modal">
        <DialogTitle>Search the event, not just the message</DialogTitle>
        <DialogDescription>
          All terms are combined with AND. Text matching is case-insensitive.
          Search includes nested JSON and resolved OCSF names.
        </DialogDescription>
        <dl className="search-examples">
          <dt>
            <code>credential</code>
          </dt>
          <dd>Text anywhere in the event</dd>
          <dt>
            <code>{'"authentication failure"'}</code>
          </dt>
          <dd>An exact phrase</dd>
          <dt>
            <code>severity:high</code>
          </dt>
          <dd>A field contains a value</dd>
          <dt>
            <code>{'actor="svc-deploy"'}</code>
          </dt>
          <dd>A field equals a value</dd>
          <dt>
            <code>src_endpoint.ip:10.24.</code>
          </dt>
          <dd>Nested fields using dot notation</dd>
          <dt>
            <code>severity_id&gt;=4 -severity_id=99</code>
          </dt>
          <dd>Numeric comparisons and exclusions</dd>
          <dt>
            <code>-status:success</code>
          </dt>
          <dd>Exclude matching events</dd>
        </dl>
        <p className="help-note">
          Aliases: class, category, severity, source, actor, target, activity,
          status. Use numeric OCSF fields for numeric comparisons. Dates and
          times are UTC.
        </p>
        <p className="help-note">
          <kbd>/</kbd> Focus search · <kbd>Esc</kbd> Close the inspector
        </p>
      </DialogContent>
    </Dialog>
  );
}
