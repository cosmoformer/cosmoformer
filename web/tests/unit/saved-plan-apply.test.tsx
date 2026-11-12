import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, expect, it, vi } from 'vitest';
import { SavedPlanApply } from '../../app/components/saved-plan-apply';
import { renderPage, reply } from './helpers';
import { approvalSummary, runFixture, plan, reviewID } from '../fixtures/review';
import type { Schema } from '../../app/lib/api';
let summary: Schema['ApprovalSummary'], mutations: RequestInit[], status: number;
const denied=vi.fn(),reload=vi.fn();
beforeEach(()=>{
 summary={...approvalSummary,can_apply:true,counted_approvals:2,apply_blocked_reasons:[]};mutations=[];status=202;denied.mockReset();reload.mockReset();
 vi.mocked(fetch).mockImplementation(async(_path,options)=>{
  if(options?.method==='GET')return reply(summary);
  mutations.push(options!);
  if(status!==202)return reply({code:'RECONFIRM_REQUIRED',message:'Check history and explicitly confirm again.'},status);
  summary={...summary,can_apply:false,apply_blocked_reasons:['APPLY_ALREADY_PENDING']};
  return reply({request_id:reviewID(31),subject_id:runFixture.id,operation_id:reviewID(30),revision:1,status:'queued'},202);
 });
});
function mount(){return renderPage(<SavedPlanApply org="synthetic-org" run={runFixture} onDenied={denied} reload={reload}/>);}
it('requires exact-plan confirmation and starts each operation at Medium',async()=>{
 mount();const user=userEvent.setup();await user.click(await screen.findByRole('button',{name:/^Apply$/}));
 const dialog=within(screen.getByRole('dialog'));
 for(const text of [runFixture.stack_name,runFixture.environment_name,runFixture.stack_id,runFixture.environment_id,runFixture.commit,plan.id,plan.manifest_digest])expect(dialog.getByText(text,{exact:true})).toBeVisible();
 expect(dialog.getByLabelText('Apply priority')).toHaveValue('medium');
 await user.selectOptions(dialog.getByLabelText('Apply priority'),'high');
 await user.click(dialog.getByRole('button',{name:'Confirm Apply'}));
 await screen.findByText('Apply accepted for this exact saved plan.');
 expect(JSON.parse(String(mutations[0].body))).toEqual({expected_plan_revision:2,manifest_digest:plan.manifest_digest,priority:'high',confirmed_context:{stack_id:runFixture.stack_id,environment_id:runFixture.environment_id,source_commit:runFixture.commit}});
 expect(reload).toHaveBeenCalled();expect(screen.getByRole('button',{name:/^Apply$/})).toBeDisabled();
});
it('does not create an intent while approvals or permission are missing',async()=>{
 summary={...summary,can_apply:false,counted_approvals:1,apply_blocked_reasons:['APPROVALS_REQUIRED']};
 mount();expect(await screen.findByRole('button',{name:/^Apply$/})).toBeDisabled();expect(mutations).toHaveLength(0);
});
it.each([401,403,409,503])('closes confirmation on %s and never replays on reconnect',async(code)=>{
 status=code;mount();const user=userEvent.setup();await user.click(await screen.findByRole('button',{name:/^Apply$/}));await user.click(screen.getByRole('button',{name:'Confirm Apply'}));
 await waitFor(()=>expect(screen.queryByRole('button',{name:'Confirm Apply'})).not.toBeInTheDocument());
 fireEvent(window,new Event('offline'));fireEvent(window,new Event('online'));
 await waitFor(()=>expect(mutations).toHaveLength(1));
 if(code===403)expect(denied).toHaveBeenCalled();
});
it('hides Apply from users without current scoped deployment permission',async()=>{
 summary={...summary,can_apply:false,apply_blocked_reasons:['APPLY_AUTHORITY_REQUIRED']};mount();await waitFor(()=>expect(fetch).toHaveBeenCalled());expect(screen.queryByRole('button',{name:/^Apply$/})).not.toBeInTheDocument();
});
