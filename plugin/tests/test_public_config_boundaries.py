import json,pathlib,re,unittest
ROOT=pathlib.Path(__file__).parents[2]
class PublicConfigTests(unittest.TestCase):
 def test_public_sources_have_no_installation_literal_patterns(self):
  patterns=[r'https?://[^\s\"\x27]*\.inside\.',r'(?<![0-9])192\.168\.[0-9]+\.[0-9]+',r'/(?:Users|home)/(?!OPERATOR|operator|user(?:/|$)|<)[a-z][A-Za-z0-9_-]*/']
  manifest=json.loads((ROOT/'tools/publication/manifest.json').read_text())
  bad=[]
  for name in manifest['include']:
   if name==str(pathlib.Path(__file__).relative_to(ROOT)):continue
   try:raw=(ROOT/name).read_text()
   except UnicodeError:continue
   if any(re.search(pattern,raw)for pattern in patterns):bad.append(name)
  self.assertEqual(bad,[],'Installation values belong in external private config')
if __name__=='__main__':unittest.main()
