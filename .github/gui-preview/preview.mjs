// Renders the embedded GUI in CI so reviewers can inspect layout and interaction
// without running the Go binary locally. It serves web/index.html against a stub
// API that mirrors the real response shapes, then captures screenshots and fails
// when the page raises a JavaScript error.

import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const outDir = process.env.OUT_DIR || join(root, 'gui-preview-out');

const BOOTSTRAP = {
  version: 'preview',
  image: 'alpine:latest',
  output: 'alpine_latest.tar',
  output_explicit: false,
  proxy: '',
  has_saved_proxy_credentials: false,
  username: '',
  insecure: false,
  has_saved_password: false
};

const SETTINGS = {
  version: 1,
  default_remote_id: 'rem-build',
  registries: [
    { id: 'reg-hub', name: 'Docker Hub', kind: 'registry', endpoint: 'https://registry-1.docker.io', namespace: 'library' },
    { id: 'reg-harbor', name: 'Harbor 生产', kind: 'harbor', endpoint: 'https://harbor.example.com', namespace: 'platform', default_credential_id: 'cred-robot' },
    { id: 'reg-mirror', name: '内网镜像站', kind: 'registry', endpoint: 'https://mirror.internal', insecure: true }
  ],
  credentials: [
    { id: 'cred-robot', registry_id: 'reg-harbor', name: '发布机器人', username: 'robot$publisher', has_secret: true },
    { id: 'cred-reader', registry_id: 'reg-harbor', name: '只读账号', username: 'reader', has_secret: true },
    { id: 'cred-mirror', registry_id: 'reg-mirror', name: '内网只读', username: 'mirror-ro', has_secret: false }
  ],
  remotes: [
    {
      id: 'rem-build', name: 'build-01', address: '10.0.0.11', user: 'ops', port: 22, auth_method: 'key',
      key_path: '~/.ssh/id_ed25519', host_key_fingerprint: 'SHA256:8sJ1p0Yb2m5Qd7Xk3Rn6Tz9Wc4Vf1Hg0Lu2Ai5Bo3Ce',
      workspace: '~/.local/share/dia', storage_roots: ['/data/archive', '/mnt/bigdisk'], has_secret: true
    },
    {
      id: 'rem-archive', name: 'archive-02', address: '10.0.0.42', user: 'ops', port: 2222, auth_method: 'agent',
      workspace: '~/.local/share/dia', storage_roots: [], has_secret: false
    }
  ],
  image_lists: [
    { id: 'list-release', name: 'v3.5.10 发布集', content: '# service images\nteam/api:v3.5.10\nteam/worker:v3.5.10 -> archive/worker:v3.5.10\nteam/gateway:v3.5.10\n' },
    { id: 'list-base', name: '基础镜像', content: '# base images\nalpine:3.20\ndebian:bookworm-slim\n' }
  ]
};

const RUNNING_TASK = {
  id: 'task-sync-1',
  status: 'running',
  input: { kind: 'sync', image: '', output: '', remote_id: 'rem-build', image_list_id: 'list-release', selected: [] },
  stage: 'copy-blob',
  message: 'team/worker:v3.5.10 → archive/worker:v3.5.10',
  platform: 'linux/amd64',
  current_layer: 4, total_layers: 9,
  current_image: 2, total_images: 3,
  bytes_done: 486_539_264, bytes_total: 1_073_741_824,
  speed_bps: 41_943_040, eta_seconds: 84,
  engine: 'native', remote_id: 'rem-build',
  updated_at: new Date().toISOString()
};

const DONE_TASK = {
  id: 'task-export-1',
  status: 'done',
  input: { kind: 'export', image: 'alpine:latest', output: 'alpine_latest.tar', selected: [] },
  stage: 'finished',
  message: '导出完成',
  bytes_done: 8_732_160, bytes_total: 8_732_160,
  output_files: ['alpine_latest_linux_amd64.tar', 'alpine_latest_linux_arm64.tar', 'alpine_latest_platforms.json'],
  docker_load_commands: ['docker load -i alpine_latest_linux_amd64.tar', 'docker load -i alpine_latest_linux_arm64.tar'],
  updated_at: new Date(Date.now() - 240_000).toISOString()
};

const FAILED_TASK = {
  id: 'task-sync-0',
  status: 'failed',
  input: { kind: 'sync', image: '', output: '', remote_id: 'rem-archive', image_list_id: 'list-base', selected: [] },
  stage: 'resolve',
  error: 'archive-02: dial tcp 10.0.0.42:2222: connect: connection refused',
  sync_result: {
    items: [
      { status: 'done', source: 'alpine:3.20', target: 'archive/alpine:3.20' },
      { status: 'failed', source: 'debian:bookworm-slim', target: '', error: 'connection refused' }
    ]
  },
  updated_at: new Date(Date.now() - 900_000).toISOString()
};

const STORAGE = [
  { path: '/data/archive', total_bytes: 2_199_023_255_552, available_bytes: 1_649_267_441_664, recommended: true },
  { path: '/mnt/bigdisk', total_bytes: 1_099_511_627_776, available_bytes: 219_902_325_555, recommended: false },
  { path: '/home/ops', total_bytes: 107_374_182_400, available_bytes: 32_212_254_720, recommended: false }
];

const FILES = [
  { name: 'releases', path: '/data/archive/releases', directory: true, size: 0, modified_at: '2026-08-11T09:12:00Z' },
  { name: 'nightly', path: '/data/archive/nightly', directory: true, size: 0, modified_at: '2026-08-12T22:03:00Z' },
  { name: 'team_api_v3.5.10_linux_amd64.tar', path: '/data/archive/team_api_v3.5.10_linux_amd64.tar', directory: false, size: 412_384_256, modified_at: '2026-08-12T10:41:00Z' },
  { name: 'team_worker_v3.5.10_linux_arm64.tar', path: '/data/archive/team_worker_v3.5.10_linux_arm64.tar', directory: false, size: 288_358_400, modified_at: '2026-08-12T10:47:00Z' },
  { name: 'platform_index.json', path: '/data/archive/platform_index.json', directory: false, size: 2_048, modified_at: '2026-08-12T10:47:00Z' }
];

const PROJECTS = ['platform', 'library', 'team-api', 'team-worker', 'sandbox', 'archive', 'infra', 'observability']
  .map((name, index) => ({ name, repo_count: 3 + index, project_id: index + 1 }));

const REPOSITORIES = ['api', 'worker', 'gateway', 'scheduler']
  .map((name, index) => ({ name: `platform/${name}`, resource_name: `platform/${name}`, artifact_count: 6 + index }));

const ARTIFACTS = [
  {
    digest: 'sha256:2f0d1a3c9b6e5d4c8a7b1e0f9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c',
    type: 'IMAGE', size: 412_384_256, push_time: '2026-08-12T10:41:00Z',
    tags: [{ name: 'v3.5.10' }, { name: 'stable' }]
  },
  {
    digest: 'sha256:8b7a6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b',
    type: 'IMAGE', size: 409_112_064, push_time: '2026-08-05T14:20:00Z',
    tags: [{ name: 'v3.5.9' }]
  },
  {
    digest: 'sha256:1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d',
    type: 'IMAGE', size: 398_458_880, push_time: '2026-07-28T08:02:00Z',
    tags: []
  }
];

const PLATFORMS = [
  { index: 0, display_index: 1, manifest_ref: 'sha256:aaa', os: 'linux', architecture: 'amd64', label: 'linux/amd64' },
  { index: 1, display_index: 2, manifest_ref: 'sha256:bbb', os: 'linux', architecture: 'arm64', variant: 'v8', label: 'linux/arm64/v8' },
  { index: 2, display_index: 3, manifest_ref: 'sha256:ccc', os: 'linux', architecture: 'arm', variant: 'v7', label: 'linux/arm/v7' },
  { index: 3, display_index: 4, manifest_ref: 'sha256:ddd', os: 'linux', architecture: 's390x', label: 'linux/s390x' }
];

function readBody(request) {
  return new Promise((resolve) => {
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      try { resolve(raw ? JSON.parse(raw) : {}); } catch { resolve({}); }
    });
  });
}

function sendJSON(response, payload, status = 200) {
  const body = JSON.stringify(payload);
  response.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
  response.end(body);
}

function harborResponse(action) {
  switch (action.action) {
    case 'health':
      return { status: 'healthy', components: [{ name: 'core', status: 'healthy' }, { name: 'registry', status: 'healthy' }, { name: 'database', status: 'healthy' }] };
    case 'list_projects':
      return { items: PROJECTS, total: PROJECTS.length, next_page: 0 };
    case 'list_repositories':
      return { items: REPOSITORIES, total: REPOSITORIES.length, next_page: 0 };
    case 'list_artifacts':
      return { items: ARTIFACTS, total: ARTIFACTS.length, next_page: 0 };
    default:
      return { ok: true };
  }
}

async function startStubServer(html) {
  const server = createServer(async (request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    const path = url.pathname;

    if (path === '/') {
      response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      response.end(html);
      return;
    }
    if (path === '/api/settings') return sendJSON(response, SETTINGS);
    if (path === '/api/tasks') return sendJSON(response, [RUNNING_TASK, DONE_TASK, FAILED_TASK]);

    if (path.startsWith('/api/tasks/')) {
      const [id, action] = path.replace('/api/tasks/', '').split('/');
      const task = [RUNNING_TASK, DONE_TASK, FAILED_TASK].find((item) => item.id === id) || RUNNING_TASK;
      if (action === 'events') {
        response.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' });
        let done = task.bytes_done || 0;
        const push = () => {
          response.write(`data: ${JSON.stringify({ ...task, bytes_done: done, updated_at: new Date().toISOString() })}\n\n`);
          done = Math.min(task.bytes_total || 0, done + 33_554_432);
        };
        push();
        const timer = setInterval(push, 800);
        request.on('close', () => clearInterval(timer));
        return undefined;
      }
      if (action === 'cancel') return sendJSON(response, { ...task, status: 'canceling' }, 202);
      return sendJSON(response, task);
    }

    if (path.startsWith('/api/remotes/')) {
      if (path.endsWith('/storage')) return sendJSON(response, STORAGE);
      if (path.endsWith('/files')) return sendJSON(response, FILES);
      if (path.endsWith('/test')) return sendJSON(response, { ok: true, fingerprint: 'SHA256:preview', info: { os: 'linux', architecture: 'amd64', version: 'preview' } });
    }

    if (path === '/api/harbor') return sendJSON(response, harborResponse((await readBody(request)).action || {}));
    if (path === '/api/inspect') return sendJSON(response, { image: 'alpine:latest', default_output: 'alpine_latest.tar', platforms: PLATFORMS });
    if (path === '/api/export' || path === '/api/sync') return sendJSON(response, { task_id: RUNNING_TASK.id }, 202);

    sendJSON(response, { ok: true });
  });

  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  return { server, origin: `http://127.0.0.1:${server.address().port}` };
}

async function main() {
  const template = await readFile(join(root, 'web', 'index.html'), 'utf8');
  const escaped = JSON.stringify(BOOTSTRAP).replaceAll('\\', '\\\\').replaceAll("'", "\\'");
  const html = template.replaceAll('__DIA_BOOTSTRAP_JSON__', escaped);
  await mkdir(outDir, { recursive: true });

  const { server, origin } = await startStubServer(html);
  const browser = await chromium.launch();
  const problems = [];

  const openPage = async (viewport) => {
    const context = await browser.newContext({ viewport, deviceScaleFactor: 2, locale: 'zh-CN' });
    const page = await context.newPage();
    page.on('pageerror', (error) => problems.push(`pageerror: ${error.message}`));
    page.on('console', (message) => { if (message.type() === 'error') problems.push(`console: ${message.text()}`); });
    return { context, page };
  };

  const pick = async (page, comboID, optionText) => {
    await page.click(`#${comboID}-btn`);
    await page.click(`#${comboID} .combo-list li:has-text("${optionText}")`);
  };
  const shoot = async (page, name, { keepToasts = false } = {}) => {
    await page.waitForTimeout(600);
    if (!keepToasts) await page.evaluate(() => document.getElementById('toasts').replaceChildren());
    await page.screenshot({ path: join(outDir, `${name}.png`), fullPage: true });
    console.log(`captured ${name}.png`);
  };

  const { context, page } = await openPage({ width: 1440, height: 900 });
  await page.goto(`${origin}/#sync`, { waitUntil: 'networkidle' });

  await pick(page, 'cmb-syncRemote', 'build-01');
  await pick(page, 'cmb-syncSourceRegistry', 'Docker Hub');
  await pick(page, 'cmb-syncTargetRegistry', 'Harbor 生产');
  await pick(page, 'cmb-syncImageList', 'v3.5.10 发布集');
  await shoot(page, '01-sync');

  await page.click('#cmb-syncSourceRegistry-btn');
  await shoot(page, '02-sync-combobox');
  await page.keyboard.press('Escape');
  await page.click('#cmb-syncSourceRegistry-btn');

  await page.click('#startSync');
  await page.waitForSelector('#page-tasks.active');
  await shoot(page, '03-tasks');

  await page.goto(`${origin}/#sync`, { waitUntil: 'networkidle' });
  await pick(page, 'cmb-syncRemote', 'build-01');
  await page.click('#probeSyncStorage');
  await shoot(page, '03b-sync-storage');

  await page.goto(`${origin}/#harbor`, { waitUntil: 'networkidle' });
  await pick(page, 'cmb-harborRegistry', 'Harbor 生产');
  await page.click('#harborHealth');
  await page.waitForTimeout(400);
  await pick(page, 'cmb-harborProject', 'platform');
  await page.waitForTimeout(400);
  await pick(page, 'cmb-harborRepository', 'platform/api');
  await shoot(page, '04-harbor');

  await page.goto(`${origin}/#files`, { waitUntil: 'networkidle' });
  await pick(page, 'cmb-fileRemote', 'build-01');
  await page.click('#loadStorage');
  await shoot(page, '05-files');

  await page.goto(`${origin}/#export`, { waitUntil: 'networkidle' });
  await page.click('#inspectExport');
  await page.waitForTimeout(500);
  await shoot(page, '06-export');

  await page.goto(`${origin}/#settings`, { waitUntil: 'networkidle' });
  await shoot(page, '07-settings');

  await page.click('#registryForm [type=submit]');
  await page.waitForTimeout(300);
  await shoot(page, '08-settings-validation');
  await context.close();

  const mobile = await openPage({ width: 414, height: 896 });
  await mobile.page.goto(`${origin}/#sync`, { waitUntil: 'networkidle' });
  await shoot(mobile.page, '09-sync-mobile');
  await mobile.page.goto(`${origin}/#tasks`, { waitUntil: 'networkidle' });
  await shoot(mobile.page, '10-tasks-mobile');
  await mobile.context.close();

  await browser.close();
  server.close();

  if (problems.length) {
    console.error('\nGUI raised runtime problems:');
    problems.forEach((problem) => console.error(`  - ${problem}`));
    process.exitCode = 1;
    return;
  }
  console.log('\nno page errors detected');
}

await main();
