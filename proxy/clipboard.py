"""Fixed, bounded read of user-supplied text. No guest-selected RPC or fields."""
import json
import socket
import uuid

def exchange(message):
    with socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM) as connection:
        connection.settimeout(3)
        connection.connect((socket.VMADDR_CID_HOST, 7000))
        connection.sendall((json.dumps(message)+'\n').encode())
        with connection.makefile('rb') as stream:
            response = stream.readline(800000)
        if not response.endswith(b'\n') or len(response) >= 800000:
            raise ValueError('clipboard response limit')
        return json.loads(response)

def take():
    value = exchange({'action':'clipboard.take'})
    if set(value) != {'text','id'}:
        raise ValueError('clipboard response rejected')
    text = value['text']
    if text is None:
        return None
    if not isinstance(text, str):
        raise ValueError('clipboard response rejected')
    data = text.encode('utf-8')
    if len(data) > 65536:
        raise ValueError('clipboard response limit')
    return data, str(uuid.UUID(value['id']))

def acknowledge(transfer_id):
    transfer_id = str(uuid.UUID(transfer_id))
    return exchange({'action':'clipboard.ack','id':transfer_id}) == {'accepted':True}
