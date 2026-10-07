import json, shutil
from pathlib import Path
from decurix_agent.computer import skills
p=Path('/home/headless/.decurix/skills/enter-vendor-bill-in-erpnext.json')
s=json.loads(p.read_text())
last=s['steps'][-1]
if last['id']=='s29' and last['kind']=='manual' and 'note' in last.get('intent',''):
 shutil.copy2(p,p.with_suffix('.json.before-note-cleanup'))
 s['steps'].pop()
 for note in s.get('notes',[]):
  if note.get('before')=='s29': note['before']=None
 s.get('understanding',{}).get('steps',{}).pop('29',None)
 s['done_when']=s['understanding']['done_when']
 s['version']=s.get('version',1)+1
 skills.renumber(s)
 skills.save(s)
 print('Removed the note-only manual step; original kept alongside skill')
else: print('No note-only manual step to remove')
