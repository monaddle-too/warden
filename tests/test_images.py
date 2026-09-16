import base64,hashlib,tempfile,unittest
from warden.sharing import Sharing
PNG=base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScLttAAAAABJRU5ErkJggg==')
class ImageTests(unittest.TestCase):
 def setUp(self):self.tmp=tempfile.TemporaryDirectory();self.s=Sharing(self.tmp.name)
 def tearDown(self):self.s.db.close();self.tmp.cleanup()
 def add(self,**kw):return self.s.dispatch('image_add',{'chatID':'chat','sandboxID':'sandbox','caption':'A screenshot','png':base64.b64encode(PNG).decode(),**kw})
 def test_immutable_scoped_and_persistent(self):
  r=self.add();self.assertEqual(r,self.add());id=r['image_id'];self.s.db.close();self.s=Sharing(self.tmp.name)
  self.assertEqual(self.s.images.get(id,'chat','sandbox')['png'],PNG)
  for chat,sbx in [('other','sandbox'),('chat','other')]:
   with self.assertRaises(ValueError):self.s.images.get(id,chat,sbx)
  with self.s.db:self.s.db.execute('UPDATE images SET png=? WHERE id=?',(b'changed',id))
  with self.assertRaises(ValueError):self.s.images.get(id,'chat','sandbox')
 def test_limits(self):
  for kw in ({'png':base64.b64encode(b'<svg/>').decode()},{'caption':'x'*501},{'png':base64.b64encode(b'\x89PNG\r\n\x1a\n'+b'x'*(4*1024*1024)).decode()}):
   with self.assertRaises(ValueError):self.add(**kw)
