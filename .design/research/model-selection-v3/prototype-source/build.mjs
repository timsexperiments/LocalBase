import {readFile,writeFile,mkdir} from 'node:fs/promises';
const root=new URL('../',import.meta.url);
const catalog=JSON.parse(await readFile(new URL('data/catalog-snapshot.json',root),'utf8'));
const hf=JSON.parse(await readFile(new URL('data/qwen3.8-pinned-hf.json',root),'utf8'));
const files=hf.siblings.filter(f=>f.rfilename.endsWith('.gguf')).map(f=>({name:f.rfilename,bytes:f.size}));
const css=await readFile(new URL('prototype-source/prototype.css',root),'utf8');
const js=await readFile(new URL('prototype-source/prototype.js',root),'utf8');
for(const [key,title] of [['shelf','Shelf'],['compass','Compass'],['control','Control room']]){
 await mkdir(new URL('concepts/'+key+'/',root),{recursive:true});
 const html=`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark"><title>LocalBase · ${title} · Round 3</title><style>${css}</style></head><body><script>(()=>{const CONCEPT=${JSON.stringify(key)};const CATALOG=${JSON.stringify(catalog)};const FILES=${JSON.stringify(files)};${js}})();</script></body></html>`;
 await writeFile(new URL('concepts/'+key+'/index.html',root),html);
}
console.log('Built 3 standalone HTML prototypes with inline CSS, JS and catalog data.');
