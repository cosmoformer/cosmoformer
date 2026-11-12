import type { Schema } from '../../app/lib/api';
import { project, reviewID } from './review';
export const moduleFixture: Schema['ModulePublic'] = {
 id:reviewID(60),project_id:project,repository_id:reviewID(8),commit:'a'.repeat(40),subdirectory:'modules/instance',name:'Synthetic instance',
 instructions:'<img src=x onerror=alert(1)> is plain documentation.',example_paths:['examples/basic'],archived:false,revision:1,
 repository_url:'https://bitbucket.org/synthetic/infrastructure.git',
 source_url:'https://bitbucket.org/synthetic/infrastructure/src/'+'a'.repeat(40)+'/modules/instance',
 source:'git::https://bitbucket.org/synthetic/infrastructure.git//modules/instance?ref='+'a'.repeat(40),
 snippet:'module "catalog_module" { source = "git::https://bitbucket.org/synthetic/infrastructure.git//modules/instance?ref='+'a'.repeat(40)+'" }',
 examples:[{path:'examples/basic',url:'https://bitbucket.org/synthetic/infrastructure/src/'+'a'.repeat(40)+'/examples/basic'}],available:true,can_manage:false,
};
