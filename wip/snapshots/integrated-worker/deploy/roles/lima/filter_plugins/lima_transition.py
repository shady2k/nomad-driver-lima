"""Pure config projection shared with the private review planner."""
import importlib.util
from pathlib import Path
s=importlib.util.spec_from_file_location('lima_review_transition',Path(__file__).resolve().parents[3]/'prepare-lima-only.py')
m=importlib.util.module_from_spec(s)
s.loader.exec_module(m)
def transition(source,config,disabled):
    value,plan=m.candidate(source,config,disabled)
    return {'config':value,'plan':plan}
class FilterModule:
    def filters(self):return {'lima_only_transition':transition}
