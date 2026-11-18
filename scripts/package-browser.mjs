import {existsSync, mkdirSync, rmSync} from 'node:fs';
import path from 'node:path';
import {fixture, docker, delay, root, waitHTTPS, writeBrowserCredentials} from './package-fixture.mjs';
// Docker services are detached; keep this supervisor alive until Playwright stops it.
const keepAlive=setInterval(()=>{},1000);
const controller=new AbortController();
let stop;const stopped=new Promise(resolve=>{stop=()=>{controller.abort();resolve();};});
process.on('SIGTERM',stop);process.on('SIGINT',stop);
let f;
let browserFileWritten=false;
const destination=path.join(root,'.build/testdb/browser.json');
try {
    if(existsSync(destination))throw Error('Browser fixture credentials already exist; stop their owning fixture before starting another run.');
    f=await fixture();
    controller.signal.throwIfAborted();
    const browserFiles=f.volume('browser-files');f.put(browserFiles,'initialized','synthetic fixture output');
    f.run('browser-api',['-d',...f.common,'--mount',`type=volume,src=${f.socket},dst=/run/cosmoformer`,'--mount',`type=volume,src=${browserFiles},dst=/suite/.build/testdb`,'-e','COSMOFORMER_TEST_BROWSER=1','-e','COSMOFORMER_TEST_OWNER_DSN=postgres://postgres@127.0.0.1:5432/cosmoformer_test_control?sslmode=disable','-e','COSMOFORMER_TEST_MARKER_FILE=/run/inputs/marker','-e','COSMOFORMER_BROWSER_SOCKET=/run/cosmoformer/api.sock',f.image('browser-fixture')]);
    mkdirSync(path.dirname(destination),{recursive:true,mode:0o700});
    let copied=false;
    for(let i=0;i<180;i++) {
        controller.signal.throwIfAborted();
        let bytes;
        try {bytes=f.read(browserFiles,'browser.json');JSON.parse(bytes);}catch{await delay(250);continue;}
        // An exclusive write also closes the race after the startup check.
        writeBrowserCredentials(destination,bytes);browserFileWritten=true;copied=true;break;
    }
    if(!copied)throw Error('Packaged browser fixture did not initialize: '+docker(['logs',`${f.id}-browser-api`]));
    f.proxy(5174);
    await waitHTTPS('https://localhost:5174/health/ready',{signal:controller.signal});
    console.log('Packaged Nginx browser fixture ready.');
    await stopped;
    docker(['stop','--time=15',`${f.id}-browser-api`]);
} catch(error) {
    if(!controller.signal.aborted || error.name!=='AbortError') {
        if(f)for(const suffix of ['browser-api','proxy'])try{console.error(docker(['logs',`${f.id}-${suffix}`]));}catch{}
        throw error;
    }
} finally {clearInterval(keepAlive);if(browserFileWritten)rmSync(path.join(root,'.build/testdb/browser.json'),{force:true});if(f)await f.stop();}
