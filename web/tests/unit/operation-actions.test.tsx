import {screen,waitFor} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import {beforeEach,expect,it,vi} from 'vitest';
import {OperationActions} from '../../app/components/operation-actions';
import {renderPage,reply,deferred} from './helpers';
import {reviewID} from '../fixtures/review';
import type {Schema} from '../../app/lib/api';
const operation:Schema['OperationPublic']={id:reviewID(40),kind:'apply',status:'failed',reason:'PREPARATION_FAILED',priority:'medium',revision:8,accepted_at:'2026-11-11T10:00:00Z',queue_reason:'',can_retry:true};
const reload=vi.fn(),denied=vi.fn();
beforeEach(()=>{reload.mockReset();denied.mockReset();vi.mocked(fetch).mockResolvedValue(reply({ok:true}));});
function mount(o=operation){return renderPage(<OperationActions org="org" operation={o} reload={reload} onDenied={denied}/>);}
it('explicitly retries the same preparation with its expected operation revision',async()=>{
 mount();await userEvent.click(screen.getByRole('button',{name:'Retry apply preparation'}));
 await waitFor(()=>expect(reload).toHaveBeenCalled());
 expect(fetch).toHaveBeenCalledWith(expect.stringContaining(`/operations/${operation.id}/retry`),expect.objectContaining({method:'POST',body:JSON.stringify({expected_operation_revision:8})}));
});
it('changes only waiting priority through an explicit action',async()=>{
 mount({...operation,status:'queued',kind:'plan',can_retry:false,can_prioritize:true});
 await userEvent.selectOptions(screen.getByLabelText('Waiting plan priority'),'high');
 await waitFor(()=>expect(reload).toHaveBeenCalled());
 expect(fetch).toHaveBeenCalledWith(expect.stringContaining('/priority'),expect.objectContaining({method:'POST',body:JSON.stringify({expected_operation_revision:8,priority:'high'})}));
});
it.each([403,404,409,503])('handles %s without replaying an operation action',async status=>{
 vi.mocked(fetch).mockResolvedValue(reply({code:'EXPLICIT_ACTION_REQUIRED',message:'Refresh and explicitly retry.'},status));
 mount();await userEvent.click(screen.getByRole('button',{name:'Retry apply preparation'}));
 if(status===403||status===404)await waitFor(()=>expect(denied).toHaveBeenCalled());else expect(await screen.findByRole('alert')).toHaveTextContent('The request could not be completed.');
 expect(fetch).toHaveBeenCalledTimes(1);
});
it('has no action for consumed or unauthorized work',()=>{mount({...operation,can_retry:false});expect(screen.queryByRole('button')).not.toBeInTheDocument();expect(fetch).not.toHaveBeenCalled();});
it('disables duplicate actions and aborts a pending request on unmount',async()=>{
 const pending=deferred<Response>();vi.mocked(fetch).mockReturnValue(pending.promise);const view=mount();const button=screen.getByRole('button',{name:'Retry apply preparation'});await userEvent.click(button);expect(button).toBeDisabled();view.unmount();pending.resolve(reply({ok:true}));await Promise.resolve();expect(reload).not.toHaveBeenCalled();
});
