// Disposable package histories must never execute a developer's signing tools
// or hooks. Keep this environment local to fixture Git commands.
import {execFileSync} from 'node:child_process';
export function fixtureGit(repo, inherited = process.env) {
    const env=Object.fromEntries(Object.entries(inherited).filter(([key])=>!key.startsWith('GIT_')));
    Object.assign(env,{GIT_CONFIG_GLOBAL:'/dev/null',GIT_CONFIG_SYSTEM:'/dev/null',GIT_CONFIG_NOSYSTEM:'1'});
    return (args, dates = {}) => execFileSync('git',[
        '-c','core.hooksPath=/dev/null','-c','init.templateDir=',
        '-c','commit.gpgsign=false','-c','tag.gpgsign=false',
        '-c','user.name=Package cache fixture','-c','user.email=synthetic@example.invalid',
        ...args,
    ],{cwd:repo,encoding:'utf8',stdio:'pipe',env:{...env,...dates}});
}
