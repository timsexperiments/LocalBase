import {resolve,sep,extname} from 'node:path';
const root=resolve(import.meta.dir);
const port=Number(process.env.LOCALBASE_DESIGN_PORT||24082);
if(port<24000||port>29999||[24173,25611,25612,25613].includes(port))throw new Error('Use an allowed scratch port');
const types={'.html':'text/html; charset=utf-8','.png':'image/png','.md':'text/plain; charset=utf-8','.json':'application/json','.js':'text/javascript','.css':'text/css'};
const server=Bun.serve({hostname:'127.0.0.1',port,async fetch(req){const url=new URL(req.url);const pathname=decodeURIComponent(url.pathname);const path=resolve(root,'.'+(pathname.endsWith('/')?pathname+'index.html':pathname));if(!path.startsWith(root+sep))return new Response('Not found',{status:404});const file=Bun.file(path);return await file.exists()?new Response(file,{headers:{'Content-Type':types[extname(path)]||'text/plain'}}):new Response('Not found',{status:404});}});
console.log(`Design preview PID ${process.pid}: ${server.url}`);
