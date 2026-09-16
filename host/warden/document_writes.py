"""Allowlisted document edits. Creation is host-executed only after owner approval."""
import json
import re
from .google_docs import operation as read_operation


def operation(method, path, query, body):
    if method == 'GET':
        _, doc = read_operation(method, path, query, body)
        return 'read', doc
    match = re.fullmatch(r'/v1/documents/([A-Za-z0-9_-]{1,256}):batchUpdate', path)
    if method != 'POST' or not match or query or not body or len(body) > 256*1024:
        raise ValueError('unsupported document write')
    data=json.loads(body)
    if not isinstance(data,dict) or set(data)-{'requests','writeControl'}:raise ValueError('invalid document edit')
    edits=data.get('requests')
    if not isinstance(edits,list) or not 1<=len(edits)<=100:raise ValueError('expected 1–100 edits')
    control=data.get('writeControl',{})
    if not isinstance(control,dict) or set(control)-{'requiredRevisionId'}:raise ValueError('unsupported revision control')
    if control and (not isinstance(control.get('requiredRevisionId'),str) or not 1<=len(control['requiredRevisionId'])<=256):raise ValueError('invalid revision')
    for edit in edits:
        if not isinstance(edit,dict) or len(edit)!=1:raise ValueError('invalid edit')
        key=next(iter(edit));value=edit[key]
        if key not in ('insertText','deleteContentRange','updateTextStyle','updateParagraphStyle') or not isinstance(value,dict):
            raise ValueError('only text, text styling and paragraph styling edits are allowed')
        # Google validates field-level schemas; these methods cannot fetch URLs,
        # change sharing, create other files, delete the document or edit another ID.
    return 'write',match[1]
