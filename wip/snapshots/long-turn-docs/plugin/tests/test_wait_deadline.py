import copy,pathlib,sys,unittest
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations
class WaitDeadlineTests(unittest.TestCase):
 def row(self,phase='running'):return {'phase':phase,'policy':{'max_timeout_seconds':7200},'operation_id':'existing','state':{'phase':phase}}
 def test_one_30min_wait_polls_internally_and_can_complete_after_60_seconds(self):
  clock=[0];calls=[]
  def status(*args):calls.append(clock[0]);return self.row('completed'if clock[0]>=180 else'running')
  with mock.patch.object(operations,'status',side_effect=status),mock.patch.object(operations.time,'monotonic',side_effect=lambda:clock[0]),mock.patch.object(operations.time,'sleep',side_effect=lambda n:clock.__setitem__(0,clock[0]+n)),mock.patch.object(operations,'cancel')as cancel,mock.patch.object(operations,'start')as start:
   frames=list(operations.wait(None,'session','existing',1800));self.assertEqual(frames[-1]['phase'],'completed');self.assertEqual(frames[-1]['wait_status'],'terminal');self.assertGreater(frames[-1]['wait_elapsed_seconds'],60);cancel.assert_not_called();start.assert_not_called()
 def test_deadline_only_ends_wait_and_does_not_extend_guest_timeout(self):
  clock=[0];row=self.row();original=copy.deepcopy(row)
  with mock.patch.object(operations,'status',return_value=row),mock.patch.object(operations.time,'monotonic',side_effect=lambda:clock[0]),mock.patch.object(operations.time,'sleep',side_effect=lambda n:clock.__setitem__(0,clock[0]+n)):
   last=list(operations.wait(None,'s','op',65))[-1];self.assertEqual(last['wait_status'],'deadline_reached');self.assertTrue(last['operation_continues']);self.assertEqual(row,original)
  with mock.patch.object(operations,'status',return_value=row):
   with self.assertRaisesRegex(ValueError,'POLICY'):list(operations.wait(None,'s','op',7201))
 def test_interrupt_or_unknown_never_cancels_or_replays(self):
  with mock.patch.object(operations,'status',side_effect=KeyboardInterrupt),mock.patch.object(operations,'cancel')as cancel,mock.patch.object(operations,'start')as start:
   with self.assertRaises(KeyboardInterrupt):list(operations.wait(None,'s','op',1800))
   cancel.assert_not_called();start.assert_not_called()
  with mock.patch.object(operations,'status',return_value=self.row('unknown')):self.assertEqual(list(operations.wait(None,'s','op',1800))[-1]['wait_status'],'unknown')
