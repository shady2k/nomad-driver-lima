import pathlib,unittest
ROOT=pathlib.Path(__file__).parents[1]
class PhaseOneTemplateGuidanceTests(unittest.TestCase):
 def skill(self):return (ROOT/'skills/nomad-workers/SKILL.md').read_text()
 def test_initial_plan_must_include_reuse_assessment(self):
  plan=self.skill().split('At the end of successful Phase 1 checks',1)[0]
  self.assertIn('initial Phase 1 plan MUST include a reusable-environment assessment',plan)
  self.assertIn('`example-project-dev`',plan)
 def test_success_report_offers_draft_before_project_stop(self):
  report=self.skill().split('At the end of successful Phase 1 checks',1)[1].split('This is a SHARED node',1)[0]
  self.assertLess(report.index('MUST include'),report.index('Then STOP'))
  for required in ('concrete template draft proposal','template ID/version','`prepare-template`','`template-draft`','already requested','show its SHA','exact version'):
   self.assertIn(required,report)
 def test_blocked_checks_and_irreversible_boundary_preserved(self):
  text=self.skill()
  for required in ('incomplete template evidence','never present an unverified environment as reusable','Actual source-VM stop','require explicit confirmation','Never snapshot live credentials','never auto-stop,\npurge, capture or publish'):
   self.assertIn(required,text)
  self.assertIn('do not ask for a dollar amount',text)
  self.assertIn('Coordinator selection and native Prime RLM worker selections are independent',text)
