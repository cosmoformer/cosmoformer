import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import path from 'node:path';
import openapiTS, { astToString } from 'openapi-typescript';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import standalone from 'ajv/dist/standalone/index.js';
import { _ } from 'ajv/dist/compile/codegen/index.js';
import { build } from 'vite';
import ts from 'typescript';
const root = path.resolve(import.meta.dirname, '../..');
mkdirSync(path.join(root, '.build'), { recursive: true });
const local = path.join(root, '.build/tools/go/bin/go');
const raw = execFileSync(process.env.COSMOFORMER_GO || (existsSync(local) ? local : 'go'), ['run', './cmd/contract'], { cwd: root, env: { ...process.env, GOCACHE: path.join(root, '.build/go-cache'), GOPATH: path.join(root, '.build/gopath') }, maxBuffer: 16 * 1024 * 1024 });
writeFileSync(path.join(root, '.build/openapi.json'), raw);
const api = JSON.parse(raw), dir = path.join(root, 'web/app/.generated');
mkdirSync(dir, { recursive: true });
writeFileSync(path.join(dir, 'api.ts'), astToString(await openapiTS(api)));
function refs(value) { if (Array.isArray(value))
    return value.map(refs); if (value && typeof value === 'object')
    return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, k === '$ref' && typeof v === 'string' && v.startsWith('#/components/schemas/') ? 'urn:cosmoformer:schema:' + v.slice(21) : refs(v)])); return value; }
const ajv = new Ajv2020({ strict: true, allErrors: false, code: { source: true, esm: false, formats: _`{ ...require("ajv-formats/dist/formats").fullFormats, int64: {validate: Number.isSafeInteger} }` } });
addFormats(ajv);
ajv.addFormat('int64', { type: 'number', validate: Number.isSafeInteger });
const schemas = api.components.schemas;
const ids = {};
for (const [name, schema] of Object.entries(schemas)) {
    const id = 'urn:cosmoformer:schema:' + name;
    ajv.addSchema({ ...refs(schema), $id: id });
    ids[name] = id;
}
const cjs = path.join(dir, 'validators.cjs'), entry = path.join(dir, 'validators-entry.mjs');
writeFileSync(cjs, standalone(ajv, ids));
writeFileSync(entry, `import validators from './validators.cjs';\n` + Object.keys(ids).map(name => `export const ${name} = validators.${name};`).join('\n'));
// Bundle Ajv's CommonJS runtime helpers into browser-native ESM. No runtime
// schema compilation, dynamic require or unsafe-eval is shipped to the browser.
await build({ configFile: false, logLevel: 'warn', build: { lib: { entry, formats: ['es'], fileName: () => 'validators.js' }, outDir: dir, emptyOutDir: false, minify: false, commonjsOptions: { include: [/node_modules/, /validators\.cjs$/] } } });
rmSync(entry);
rmSync(cjs);
const emitted = ts.createSourceFile('validators.js', readFileSync(path.join(dir, 'validators.js'), 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);
function check(node) { if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) && ['require', 'eval', 'Function'].includes(node.expression.text))
    throw new Error('Unbundled or dynamically compiled validator dependency'); if (ts.isNewExpression(node) && ts.isIdentifier(node.expression) && node.expression.text === 'Function')
    throw new Error('Dynamic validator compilation'); ts.forEachChild(node, check); }
check(emitted);
writeFileSync(path.join(dir, 'validators.d.ts'), Object.keys(ids).map(n => `export declare const ${n}: (value: unknown) => boolean;`).join('\n') + '\n');
console.log(`Generated types and ${Object.keys(ids).length} strict response validators from Go.`);
