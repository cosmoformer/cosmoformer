import { Link } from 'react-router';
import { useRead } from '../lib/polling';
import { projectPath, ReadState } from './workspace';
export function StackDependencies({org,stack}:{org:string;stack:string}){
 const read=useRead('/organizations/'+org+'/stacks/'+stack+'/dependencies','DependenciesOutputBody',10000);
 if(!read.data)return <ReadState error={read.error} retry={read.reload}/>;
 return <section className="panel"><h2>Dependencies</h2><p>Defined in tracked repository source for manual coordination. These links do not start or order runs. Only stacks in your current access appear.</p>
 {read.data.dependencies.length ? <ul>{read.data.dependencies.map(d=><li key={d.id}><Link to={projectPath(org,d.project_id)+'/stacks/'+d.id+'?'+new URLSearchParams({env:d.environment_id})}>{d.name}</Link>{d.archived&&' · Archived'}</li>)}</ul> : <p>No visible dependencies.</p>}</section>;
}
