import {request} from 'node:http';
import {statSync} from 'node:fs';
import path from 'node:path';
import {dev} from './workflow-lib.mjs';

export const apiSocket = path.join(dev, 'api.sock');
export function validateSocket(socketPath) {
    if (!path.isAbsolute(socketPath)) throw Error('The local API requires an absolute private socket path.');
    const parent = statSync(path.dirname(socketPath)), socket = statSync(socketPath);
    if (!parent.isDirectory() || parent.uid !== process.getuid() || (parent.mode & 0o077) || !socket.isSocket() || socket.uid !== process.getuid() || (socket.mode & 0o077)) throw Error('The local API socket and its private directory must belong to this user.');
}
export function localRequest(socketPath, method, route, body, headers = {}) {
    validateSocket(socketPath);
    return new Promise((resolve, reject) => {
        const req = request({socketPath, method, path: route, headers: {...headers, ...(body === undefined ? {} : {'Content-Length': Buffer.byteLength(body)})}}, response => {
            const chunks = []; let size = 0;
            response.on('data', chunk => { size += chunk.length; if (size > 1024 * 1024) response.destroy(Error('Local API response limit exceeded.')); else chunks.push(chunk); });
            response.on('error', reject);
            response.on('end', () => resolve({status: response.statusCode, headers: response.headers, text: Buffer.concat(chunks).toString('utf8')}));
        });
        const deadline = setTimeout(() => req.destroy(Error('Local API request timed out.')), 15000);
        req.on('close', () => clearTimeout(deadline)); req.on('error', reject);
        req.end(body);
    });
}
