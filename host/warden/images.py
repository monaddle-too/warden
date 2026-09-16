"""Private immutable PNGs, accepted only from the host's image normalizer."""
import base64
import hashlib
import re
class Images:
    def __init__(self,s):
        self.s=s
        with s.lock,s.db:s.db.execute('CREATE TABLE IF NOT EXISTS images (id TEXT PRIMARY KEY, chat TEXT, sandbox TEXT, caption TEXT, digest TEXT, png BLOB)')
    def get(self,id,chat,sandbox):
        if not isinstance(id,str) or not re.fullmatch('[a-f0-9]{64}',id):raise ValueError('invalid image')
        with self.s.lock:
            r=self.s.db.execute('SELECT * FROM images WHERE id=? AND chat=? AND sandbox=?',(id,chat,sandbox)).fetchone()
            if not r or hashlib.sha256(r['png']).hexdigest()!=r['digest']:raise ValueError('image unavailable for this conversation')
            return dict(r)
    def dispatch(self,op,d):
        chat,sandbox=d['chatID'],d['sandboxID']
        if not all(isinstance(v,str) and 0<len(v)<=128 for v in (chat,sandbox)):raise ValueError('invalid image context')
        if op=='image_get':
            r=self.get(d['id'],chat,sandbox);return {'png':base64.b64encode(r['png']).decode()}
        if op!='image_add':raise ValueError('invalid image action')
        raw=base64.b64decode(d['png'],validate=True);caption=d['caption']
        if len(raw)>4*1024*1024 or not raw.startswith(b'\x89PNG\r\n\x1a\n') or not isinstance(caption,str) or len(caption)>500:raise ValueError('invalid normalized image')
        digest=hashlib.sha256(raw).hexdigest();id=hashlib.sha256((chat+'\0'+sandbox+'\0'+digest).encode()).hexdigest()
        with self.s.lock,self.s.db:
            if not self.s.db.execute('SELECT 1 FROM images WHERE id=?',(id,)).fetchone():
                count,size=self.s.db.execute('SELECT COUNT(*),COALESCE(SUM(length(png)),0) FROM images WHERE chat=?',(chat,)).fetchone()
                total=self.s.db.execute('SELECT COALESCE(SUM(length(png)),0) FROM images').fetchone()[0]
                if count>=100 or size+len(raw)>64*1024*1024 or total+len(raw)>512*1024*1024:raise ValueError('image storage quota reached')
                self.s.db.execute('INSERT INTO images VALUES (?,?,?,?,?,?)',(id,chat,sandbox,caption,digest,raw))
        return {'image_id':id,'caption':caption,'sha256':digest,'mime_type':'image/png'}
