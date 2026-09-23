// Disposable S3 request accounting proxy. Never logs credentials or bodies.
import http from 'node:http';
let counts = {};
http.createServer((req, res) => {
  const url = new URL(req.url, 'http://local');
  const parts = url.pathname.split('/').filter(Boolean);
  const category = url.searchParams.has('list-type') ? 'LIST' : (parts[2] || 'bucket');
  // Do not reuse sockets across MinIO's idle-close boundary: a reset on a
  // conditional PUT is an ambiguous commit, not a safely retryable read.
  const upstream = http.request({ agent: false, hostname: 'minio', port: 9000, path: req.url, method: req.method, headers: req.headers }, reply => {
    const key = `${req.method} ${category} ${reply.statusCode}`;
    counts[key] = (counts[key] || 0) + 1;
    res.writeHead(reply.statusCode, reply.headers);
    reply.pipe(res);
  });
  upstream.on('error', error => { const key = `ERROR ${error.code || 'unknown'}`; counts[key] = (counts[key] || 0) + 1; res.writeHead(502); res.end(error.message); });
  req.pipe(upstream);
}).listen(9000, '0.0.0.0');
http.createServer((req, res) => { res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(counts)); }).listen(9001, '0.0.0.0');
