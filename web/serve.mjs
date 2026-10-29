import http from 'node:http';
import {readFile} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root = path.dirname(fileURLToPath(import.meta.url));
const arg = process.argv.indexOf('--port');
const port = arg >= 0 ? Number(process.argv[arg + 1]) : 18744;
http.createServer(async (req, res) => {
  const pathname = new URL(req.url, 'http://localhost').pathname;
  if (pathname === '/') {res.writeHead(302, {Location: '/app/'}).end(); return;}
  if (pathname === '/app/config.json') {res.writeHead(200, {'Content-Type': 'application/json'}).end('{"oidc":null}'); return;}
  const file = pathname === '/app/' ? 'index.html' : pathname.slice('/app/'.length);
  if (!pathname.startsWith('/app/') || !/^(index\.html|style\.css|src\/[a-z-]+\.js|vendor\/(keycloak|deprecations)\.js)$/.test(file)) {res.writeHead(404).end(); return;}
  try {
    const bytes = await readFile(path.join(root, file));
    res.writeHead(200, {'Content-Type': file.endsWith('.js') ? 'text/javascript' : file.endsWith('.css') ? 'text/css' : 'text/html', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff'}).end(bytes);
  } catch {res.writeHead(404).end();}
}).listen(port, '127.0.0.1', () => console.log(`WMS preview http://127.0.0.1:${port}/app/?demo=1`));
