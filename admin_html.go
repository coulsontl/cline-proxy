package main

const adminHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline 代理管理面板</title>
<style>
:root{--bg:#0d1117;--bg2:#161b22;--bg3:#21262d;--border:#30363d;--text:#e6edf3;--text2:#8b949e;--accent:#58a6ff;--green:#3fb950;--red:#f85149;--yellow:#d29922;--blue:#58a6ff;--badge-green-bg:#0e4429;--badge-yellow-bg:#3d2e00;--badge-red-bg:#3d1117;--danger-bg:var(--bg3);--danger-bg-hover:#3d1117}
[data-theme="light"]{--bg:#ffffff;--bg2:#f6f8fa;--bg3:#eaeef2;--border:#d0d7de;--text:#1f2328;--text2:#656d76;--accent:#0969da;--green:#1a7f37;--red:#cf222e;--yellow:#9a6700;--blue:#0969da;--badge-green-bg:#dafbe1;--badge-yellow-bg:#fff8c5;--badge-red-bg:#ffebe9;--danger-bg:#ffebe9;--danger-bg-hover:#ffd4d0}
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);font-size:14px;line-height:1.5}
.layout{display:flex;min-height:100vh}
.sidebar{width:240px;background:var(--bg2);border-right:1px solid var(--border);padding:16px 0;flex-shrink:0;display:flex;flex-direction:column}
.sidebar h1{font-size:16px;padding:0 16px 16px;border-bottom:1px solid var(--border);margin-bottom:8px;display:flex;align-items:center;gap:8px}
.sidebar h1 span{color:var(--accent)}
.nav-item{display:flex;align-items:center;gap:10px;padding:8px 16px;cursor:pointer;color:var(--text2);transition:0.15s;border-left:2px solid transparent}
.nav-item:hover{color:var(--text);background:var(--bg3)}
.nav-item.active{color:var(--text);background:var(--bg3);border-left-color:var(--accent)}
.main{flex:1;padding:24px 32px;overflow-y:auto}
h2{font-size:20px;margin-bottom:16px;font-weight:600}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px;margin-bottom:24px}
.card{background:var(--bg2);border:1px solid var(--border);border-radius:8px;padding:16px}
.card .num{font-size:28px;font-weight:600}
.card .label{font-size:12px;color:var(--text2);margin-top:4px}
.card .num.green{color:var(--green)}
.card .num.red{color:var(--red)}
.card .num.yellow{color:var(--yellow)}
.card .num.blue{color:var(--blue)}
.section{background:var(--bg2);border:1px solid var(--border);border-radius:8px;margin-bottom:24px;overflow:hidden}
.section-title{padding:12px 16px;border-bottom:1px solid var(--border);font-weight:600;display:flex;align-items:center;gap:8px}
.section-body{padding:16px}
.tabs{display:flex;border-bottom:1px solid var(--border)}
.tab{padding:10px 20px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:13px}
.tab:hover{color:var(--text)}
.tab.active{color:var(--text);border-bottom-color:var(--accent)}
.tab-content{display:none;padding:16px}
.tab-content.active{display:block}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid var(--border);font-size:13px}
th{color:var(--text2);font-weight:600;font-size:12px}
.status{display:inline-flex;align-items:center;gap:4px;padding:2px 8px;border-radius:12px;font-size:12px;font-weight:500}
.status.active{background:var(--badge-green-bg);color:var(--green)}
.status.cooldown{background:var(--badge-yellow-bg);color:var(--yellow)}
.status.expired{background:var(--badge-red-bg);color:var(--red)}
.status-dot{width:6px;height:6px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--green)}
.status-dot.cooldown{background:var(--yellow)}
.status-dot.expired{background:var(--red)}
.btn{display:inline-flex;align-items:center;gap:6px;padding:6px 14px;border:1px solid var(--border);border-radius:6px;background:var(--bg3);color:var(--text);cursor:pointer;font-size:13px;transition:0.15s;text-decoration:none}
.btn:hover{background:var(--border)}
.btn-primary{background:#1f6feb;border-color:#1f6feb;color:#fff}
.btn-primary:hover{background:#388bfd}
.btn-success{background:#1a7f37;border-color:#1a7f37;color:#fff}
.btn-success:hover{background:#238636}
.btn-danger{border-color:var(--red);color:var(--red);background:var(--danger-bg)}
.btn-danger:hover{background:var(--danger-bg-hover)}
.btn-sm{padding:3px 10px;font-size:12px}
input,textarea,select{width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:13px;font-family:inherit}
input:focus,textarea:focus{outline:none;border-color:var(--accent)}
textarea{resize:vertical;min-height:80px;font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px}
.form-row{display:flex;gap:12px;align-items:flex-end;margin-bottom:12px}
.form-row .field{flex:1}
.form-row .field label{display:block;font-size:12px;color:var(--text2);margin-bottom:4px}
.form-actions{display:flex;gap:8px;margin-top:12px}
.toast{position:fixed;top:20px;right:20px;padding:12px 20px;border-radius:8px;color:#fff;z-index:9999;opacity:0;transform:translateY(-10px);transition:0.3s;font-size:13px;max-width:400px}
.toast.show{opacity:1;transform:translateY(0)}
.toast.success{background:#1a7f37}
.toast.error{background:var(--red)}
.toast.info{background:#1f6feb}
.loading{display:inline-block;width:14px;height:14px;border:2px solid var(--text2);border-top-color:var(--accent);border-radius:50%;animation:spin 0.8s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}
.empty{padding:32px;text-align:center;color:var(--text2)}
.mono{font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px}
.flex{display:flex;align-items:center;gap:8px}
.gap-4{gap:4px}
.text-right{text-align:right}
.mt-8{margin-top:8px}
.inline-flex{display:inline-flex;align-items:center;gap:6px}
.key-display{background:var(--bg);padding:8px 12px;border-radius:6px;border:1px solid var(--border);font-family:'Cascadia Code','Fira Code','Consolas',monospace;font-size:12px;word-break:break-all;cursor:pointer}
.key-display:hover{background:var(--bg3)}
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px}
.copy-icon:hover{color:var(--text);background:var(--bg3)}
.empty-state{padding:48px;text-align:center;color:var(--text2)}
.empty-state .icon{font-size:48px;margin-bottom:12px;display:block}
.model-tag{display:inline-block;padding:3px 8px;border-radius:4px;font-size:11px;background:var(--bg3);color:var(--text2);margin:2px}
.model-tag.free{border:1px solid var(--green);color:var(--green)}
.model-tag.pass{border:1px solid var(--yellow);color:var(--yellow)}
.justify-between{display:flex;justify-content:space-between;align-items:center}
</style>
</head>
<body>
<div class="layout">
<div class="sidebar">
<h1><span>⚡</span>Cline 代理<button id="themeToggle" onclick="toggleTheme()" title="切换主题" style="margin-left:auto;background:none;border:none;color:var(--text2);cursor:pointer;font-size:16px;padding:0 4px">🌙</button></h1>
<div class="nav-item active" data-tab="dashboard"><span>📊</span> 仪表盘</div>
<div class="nav-item" data-tab="accounts"><span>👤</span> 账号管理</div>
<div class="nav-item" data-tab="import"><span>📥</span> 导入账号</div>
<div class="nav-item" data-tab="stats"><span>📈</span> 请求统计</div>
<div class="nav-item" data-tab="settings"><span>⚙️</span> 设置</div>
<div style="margin-top:auto;padding:16px;font-size:12px;color:var(--text2)">
  <div>管理面板: <a href="/admin/" style="color:var(--accent)">/admin/</a></div>
  <div>API 地址: <span id="footerApiAddr">http://127.0.0.1:3457</span></div>
</div>
</div>

<div class="main">

<div id="tab-dashboard" class="tab-panel">
<h2>📊 仪表盘</h2>
<div class="cards">
  <div class="card"><div class="num blue" id="statTotal">-</div><div class="label">账号总数</div></div>
  <div class="card"><div class="num green" id="statActive">-</div><div class="label">活跃</div></div>
  <div class="card"><div class="num yellow" id="statCooldown">-</div><div class="label">冷却</div></div>
  <div class="card"><div class="num red" id="statExpired">-</div><div class="label">已过期</div></div>
</div>
<div class="section">
  <div class="section-title">📋 快捷操作</div>
  <div class="section-body" style="display:flex;gap:8px;flex-wrap:wrap">
    <button class="btn btn-primary" onclick="switchTab('import')">+ 添加账号</button>
    <button class="btn" onclick="refreshAllTokens()">🔄 刷新全部 Token</button>
    <button class="btn" onclick="document.getElementById('fileInput').click()">📄 从文件导入</button>
    <input type="file" id="fileInput" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    <button class="btn" onclick="switchTab('settings');generateKey()">🔑 生成 API 密钥</button>
  </div>
</div>
<div class="section">
  <div class="section-title">🧠 可用模型与上下文限制</div>
  <div class="section-body" style="padding:0">
    <table>
      <thead><tr><th>模型</th><th style="width:200px">当前限制</th><th style="width:110px"></th></tr></thead>
      <tbody id="modelLimitBody"><tr><td colspan="3" class="empty">加载中...</td></tr></tbody>
    </table>
    <div style="padding:12px;font-size:12px;color:var(--text2)">输入上下文限制：0 或留空表示不限制。请求入参超过该值时将被直接拒绝（HTTP 413），不会转发到上游。</div>
  </div>
</div>
</div>

<div id="tab-accounts" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:16px">
  <h2>👤 账号管理</h2>
  <div style="display:flex;gap:8px">
    <button class="btn btn-primary btn-sm" onclick="switchTab('import')">+ 添加</button>
    <button class="btn btn-sm" onclick="loadAccounts()">🔄 刷新</button>
  </div>
</div>
<div class="section">
  <div class="section-body" style="padding:0">
    <table>
      <thead>
        <tr><th>邮箱</th><th>状态</th><th>使用次数</th><th>最后使用</th><th>创建时间</th><th>操作</th></tr>
      </thead>
      <tbody id="accountTableBody">
        <tr><td colspan="6" class="empty">加载中...</td></tr>
      </tbody>
    </table>
  </div>
</div>
</div>

<div id="tab-import" class="tab-panel" style="display:none">
<h2>📥 导入账号</h2>
<div class="section">
  <div class="tabs" id="importTabs">
    <div class="tab active" data-tab="oauth">🔑 OAuth 浏览器登录</div>
    <div class="tab" data-tab="token">✏️ 手动输入 Token</div>
    <div class="tab" data-tab="batch">📦 批量导入</div>
  </div>

  <div id="import-oauth" class="tab-content active">
    <p style="color:var(--text2);margin-bottom:12px">通过浏览器完成 OAuth 认证，支持 Google/GitHub/邮箱登录，自动获取 refreshToken。</p>
    <button class="btn btn-primary" onclick="startOAuth()" id="oauthBtn">🚀 开始 OAuth 登录</button>
    <div id="oauthProgress" style="display:none;margin-top:12px">
      <div style="display:flex;align-items:center;gap:12px">
        <div class="loading"></div>
        <div>
          <div style="font-weight:500" id="oauthStatus">等待浏览器授权...</div>
          <div style="color:var(--text2);font-size:12px;margin-top:4px">
            打开 <a href="#" id="oauthUrl" target="_blank" style="color:var(--accent)"></a>
            并输入代码: <strong id="oauthUserCode"></strong>
          </div>
        </div>
      </div>
    </div>
    <div id="oauthResult" style="display:none;margin-top:12px"></div>
  </div>

  <div id="import-token" class="tab-content">
    <p style="color:var(--text2);margin-bottom:12px">输入已有的 Cline refreshToken，系统会自动验证并加入池。</p>
    <div class="form-row">
      <div class="field">
        <label>Refresh Token *</label>
        <input type="text" id="tokenInput" placeholder="粘贴 refreshToken">
      </div>
    </div>
    <div class="form-row">
      <div class="field">
        <label>邮箱（可选，留空自动生成）</label>
        <input type="text" id="tokenEmail" placeholder="user@example.com">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="addByToken()">+ 添加账号</button>
    </div>
    <div id="tokenResult" style="margin-top:8px"></div>
  </div>

  <div id="import-batch" class="tab-content">
    <p style="color:var(--text2);margin-bottom:12px">批量导入多个账号。支持 JSON 数组或每行一个 token。</p>
    <div class="form-row">
      <div class="field">
        <label>JSON 数组格式：[{"refreshToken":"...","email":"..."}]</label>
        <textarea id="batchInput" placeholder='[{"refreshToken":"xxx","email":"u1@x.com"},{"refreshToken":"yyy","email":"u2@x.com"}]'></textarea>
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="batchImport()">📦 导入全部</button>
      <button class="btn" onclick="document.getElementById('fileInput2').click()">📄 选择文件</button>
      <input type="file" id="fileInput2" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    </div>
    <div id="batchResult" style="margin-top:8px"></div>
  </div>
</div>
</div>

<div id="tab-stats" class="tab-panel" style="display:none">
<h2>📈 请求统计</h2>
<div class="flex" style="margin-bottom:16px;gap:8px;align-items:center">
  <span style="color:var(--text2);font-size:13px">时间范围:</span>
  <select id="statsDays" onchange="loadStatsPage()" style="width:auto">
    <option value="1" selected>今天</option>
    <option value="7">近 7 天</option>
    <option value="30">近 30 天</option>
    <option value="90">近 90 天</option>
  </select>
  <button class="btn btn-sm" onclick="loadStatsPage()">🔄 刷新</button>
  <button class="btn btn-sm btn-danger" onclick="clearStats()" style="margin-left:auto">🗑️ 清空统计</button>
</div>
<div class="cards" id="statsOverview"><div class="card"><div class="num">-</div><div class="label">加载中...</div></div></div>
<div class="section">
  <div class="section-title">📊 按天趋势</div>
  <div class="section-body" id="statsTrend"><div class="empty">加载中...</div></div>
</div>
<div class="section">
  <div class="section-title">👤 按账号汇总</div>
  <div class="section-body" style="padding:0">
    <table><thead><tr><th>账号</th><th>请求</th><th>成功</th><th>错误</th><th>输入</th><th>输出</th><th>总计</th></tr></thead>
    <tbody id="statsAccountBody"><tr><td colspan="7" class="empty">加载中...</td></tr></tbody></table>
  </div>
</div>
<div class="section">
  <div class="section-title">🧠 按模型汇总</div>
  <div class="section-body" style="padding:0">
    <table><thead><tr><th>模型</th><th>请求</th><th>成功</th><th>错误</th><th>输入</th><th>输出</th><th>总计</th></tr></thead>
    <tbody id="statsModelBody"><tr><td colspan="7" class="empty">加载中...</td></tr></tbody></table>
  </div>
</div>
<div class="section">
  <div class="section-title">⚠️ 错误明细</div>
  <div class="section-body" style="padding:0">
    <table><thead><tr><th>时间</th><th>账号</th><th>模型</th><th>状态码</th><th>错误信息</th></tr></thead>
    <tbody id="statsErrorBody"><tr><td colspan="5" class="empty">加载中...</td></tr></tbody></table>
  </div>
</div>
</div>

<div id="tab-settings" class="tab-panel" style="display:none">
<h2>⚙️ 设置</h2>

<div class="section">
  <div class="section-title">🔑 API 密钥管理</div>
  <div class="section-body">
    <p style="color:var(--text2);margin-bottom:12px">生成的密钥可用于客户端访问代理 API（作为 x-api-key 或 Authorization 头）。</p>
    <div class="form-actions" style="margin-bottom:12px">
      <button class="btn btn-success" onclick="generateKey()">+ 生成新密钥</button>
    </div>
    <div id="keysList"></div>
    <div id="keyGenResult" style="margin-top:8px"></div>
  </div>
</div>


<div class="section">
  <div class="section-title">🔧 代理配置</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>监听地址</label><input type="text" id="settingAddr" disabled></div>
      <div class="field"><label>默认模型</label><input type="text" id="settingDefModel" disabled></div>
    </div>
    <div class="form-row">
      <div class="field">
        <label>轮询策略</label>
        <select id="settingStrategy" onchange="updateConfig()">
          <option value="round_robin">轮询 (round_robin)</option>
          <option value="fill">填满 (fill)</option>
          <option value="random">随机 (random)</option>
        </select>
      </div>
      <div class="field"><label>引擎版本</label><input type="text" id="settingVersion" disabled></div>
    </div>
    <div class="form-row">
      <div class="field"><label>账号文件</label><input type="text" id="settingPoolPath" disabled></div>
    </div>
  </div>
</div>

<div class="section">
  <div class="section-title">📨 请求头配置（模拟 Cline CLI 发出）</div>
  <div class="section-body">
    <table>
      <thead><tr><th style="width:220px">请求头</th><th>值</th><th style="width:40px"></th></tr></thead>
      <tbody id="headersTableBody">
        <tr><td colspan="3" class="empty">加载中...</td></tr>
      </tbody>
    </table>
    <div class="form-actions" style="margin-top:12px">
      <button class="btn btn-sm" onclick="addHeaderRow()">➕ 添加请求头</button>
      <button class="btn btn-sm btn-primary" onclick="saveHeaders()">💾 保存请求头</button>
    </div>
    <div style="font-size:12px;color:var(--text2);margin-top:8px">这些请求头会附加到所有转发给 Cline API 的请求中，以模拟官方客户端行为。</div>
    <div id="headerSaveResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">📝 系统提示词覆盖（override.md）</div>
  <div class="section-body">
    <div style="font-size:12px;color:var(--text2);margin-bottom:8px">非空时会作为 system prompt 注入所有请求（OpenAI 格式注入到 messages；Anthropic 格式注入到 system）。留空则不覆盖。</div>
    <textarea id="overrideContent" placeholder="在此填写覆盖的系统提示词..." style="width:100%;min-height:120px;font-family:monospace;font-size:13px"></textarea>
    <div class="form-actions" style="margin-top:8px">
      <button class="btn btn-sm btn-primary" onclick="saveOverride()">💾 保存</button>
      <button class="btn btn-sm" onclick="loadOverride()">🔄 重新加载</button>
    </div>
    <div id="overrideSaveResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title">🗑️ 危险操作</div>
  <div class="section-body">
    <div style="display:flex;gap:8px;flex-wrap:wrap">
      <button class="btn btn-danger" onclick="deleteAllAccounts()">🗑️ 删除全部账号</button>
      <button class="btn btn-danger" onclick="deleteAllKeys()">🗑️ 删除全部密钥</button>
    </div>
  </div>
</div>
</div>

</div>
</div>

<div id="toast" class="toast"></div>

<script>
const API = '/admin/api';

const _ = id => document.getElementById(id);
const esc = s => { const d=document.createElement('div'); d.textContent=s||''; return d.innerHTML; };

function toast(msg, t) {
  const el = _('toast');
  el.textContent = msg;
  el.className = 'toast ' + t + ' show';
  setTimeout(() => el.classList.remove('show'), 3500);
}

// ========== 主题切换 ==========
function applyTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  const btn = _('themeToggle');
  if (btn) btn.textContent = t === 'light' ? '☀️' : '🌙';
}
applyTheme(localStorage.getItem('theme') || 'light');
function toggleTheme() {
  const cur = document.documentElement.getAttribute('data-theme') || 'dark';
  const next = cur === 'dark' ? 'light' : 'dark';
  applyTheme(next);
  localStorage.setItem('theme', next);
}

// ========== 导航 ==========
document.querySelectorAll('.nav-item').forEach(el => {
  el.addEventListener('click', () => {
    if (el.classList.contains('active')) return;
    document.querySelectorAll('.nav-item').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
    _('tab-' + el.dataset.tab).style.display = 'block';
    if (el.dataset.tab === 'dashboard') { loadStats(); loadAccounts(); loadModelLimits(); }
    if (el.dataset.tab === 'accounts') loadAccounts();
    if (el.dataset.tab === 'stats') loadStatsPage();
    if (el.dataset.tab === 'settings') { loadKeys(); loadConfig(); loadOverride(); }
  });
});

function switchTab(name) {
  document.querySelectorAll('.nav-item').forEach(e => {
    e.classList.toggle('active', e.dataset.tab === name);
  });
  document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
  _('tab-' + name).style.display = 'block';
  if (name === 'dashboard') { loadStats(); loadAccounts(); loadModelLimits(); }
  if (name === 'accounts') loadAccounts();
  if (name === 'stats') loadStatsPage();
  if (name === 'settings') { loadKeys(); loadOverride(); }
}

// 导入子标签
document.querySelectorAll('#importTabs .tab').forEach(el => {
  el.addEventListener('click', () => {
    document.querySelectorAll('#importTabs .tab').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('#import-oauth,#import-token,#import-batch').forEach(e => e.classList.remove('active'));
    _('import-' + el.dataset.tab).classList.add('active');
  });
});

// ========== API 请求 ==========
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const res = await fetch(API + path, opts);
  const data = await res.json();
  if (!data.success && data.error) throw new Error(data.error);
  return data;
}

// ========== 仪表盘 ==========
async function loadStats() {
  try {
    const d = await api('GET', '/stats');
    const s = d.data;
    _('statTotal').textContent = s.total;
    _('statActive').textContent = s.active;
    _('statCooldown').textContent = s.cooldown;
    _('statExpired').textContent = s.expired;
    if (s.version) _('settingVersion').value = s.version;
    if (s.strategy) _('settingStrategy').value = s.strategy;
  } catch (e) { /* ignore */ }
}

// ========== 账号管理 ==========
async function loadAccounts() {
  try {
    const d = await api('GET', '/accounts');
    const list = d.data.accounts;
    const tbody = _('accountTableBody');
    if (!list || list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty">暂无账号，前往 <a href="#" onclick="switchTab(\'import\')" style="color:var(--accent);cursor:pointer">导入账号</a> 页添加</td></tr>';
      return;
    }
    const sn = { active: '活跃', cooldown: '冷却', expired: '已过期' };
    tbody.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('zh-CN') : '-';
      const cr = a.createdAt ? new Date(a.createdAt).toLocaleString('zh-CN') : '-';
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td><span class="status ' + a.status + '"><span class="status-dot ' + a.status + '"></span>' + (sn[a.status] || a.status) + '</span></td>' +
        '<td>' + (a.usageCount || 0) + '</td>' +
        '<td class="mono" style="font-size:11px">' + lu + '</td>' +
        '<td class="mono" style="font-size:11px">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          '<button class="btn btn-sm" onclick="resetAccount(\'' + a.accountId + '\')" title="重置">↻</button> ' +
          '<button class="btn btn-sm btn-danger" onclick="deleteAccount(\'' + a.accountId + '\')" title="删除">✕</button>' +
        '</td></tr>';
    }).join('');
  } catch (e) { toast('加载账号失败: ' + e.message, 'error'); }
}

async function deleteAccount(id) {
  if (!confirm('确定删除此账号？')) return;
  try {
    await api('POST', '/accounts/delete', { accountId: id });
    toast('账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function resetAccount(id) {
  if (!confirm('确定重置此账号？将清除使用计数并刷新 Token。')) return;
  try {
    await api('POST', '/accounts/reset', { accountId: id });
    toast('账号已重置', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('重置失败: ' + e.message, 'error'); }
}

async function deleteAllAccounts() {
  if (!confirm('⚠️ 确定删除所有账号？不可撤销！')) return;
  try {
    await api('POST', '/accounts/delete-all', {});
    toast('全部账号已删除', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function refreshAllTokens() {
  try {
    await api('POST', '/accounts/refresh-all', {});
    toast('全部 Token 已刷新', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('刷新失败: ' + e.message, 'error'); }
}

// ========== OAuth 登录 ==========
async function startOAuth() {
  const btn = _('oauthBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="loading"></span> 启动中...';
  _('oauthProgress').style.display = 'block';
  _('oauthResult').style.display = 'none';
  _('oauthStatus').textContent = '正在连接 WorkOS...';
  try {
    const d = await api('POST', '/oauth/start');
    const s = d.data;
    _('oauthStatus').textContent = '请在浏览器中打开链接并输入代码';
    const u = _('oauthUrl');
    u.textContent = s.verificationUri;
    u.href = s.verificationUri;
    _('oauthUserCode').textContent = s.userCode;
    const poll = setInterval(async () => {
      try {
        const r = await api('GET', '/oauth/status?sessionId=' + s.sessionId);
        if (r.data.done) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = '🚀 开始 OAuth 登录';
          if (r.data.success) {
            _('oauthProgress').style.display = 'none';
            _('oauthResult').innerHTML = '<div style="color:var(--green);font-weight:500">✓ 账号添加成功: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('账号添加成功！', 'success');
          } else {
            _('oauthStatus').textContent = '失败: ' + (r.data.error || '未知错误');
            toast('OAuth 失败', 'error');
          }
        }
      } catch(e) {}
    }, 2000);
  } catch (e) {
    btn.disabled = false;
    btn.innerHTML = '🚀 开始 OAuth 登录';
    _('oauthStatus').textContent = '错误: ' + e.message;
    toast('OAuth 失败: ' + e.message, 'error');
  }
}

// ========== Token 导入 ==========
async function addByToken() {
  const token = _('tokenInput').value.trim();
  if (!token) { toast('请输入 refreshToken', 'error'); return; }
  const email = _('tokenEmail').value.trim();
  try {
    const d = await api('POST', '/accounts/add', { refreshToken: token, email: email || undefined });
    toast('账号添加成功: ' + (d.data.email || ''), 'success');
    _('tokenInput').value = '';
    _('tokenEmail').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('添加失败: ' + e.message, 'error'); }
}

// ========== 批量导入 ==========
async function batchImport() {
  const raw = _('batchInput').value.trim();
  if (!raw) { toast('请输入账号数据', 'error'); return; }
  let tokens;
  try { tokens = JSON.parse(raw); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = raw.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入完成', 'success');
    _('batchInput').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
}

async function handleFileImport(event) {
  const file = event.target.files[0];
  if (!file) return;
  const text = await file.text();
  let tokens;
  try { tokens = JSON.parse(text); if (!Array.isArray(tokens)) tokens = [tokens]; }
  catch { tokens = text.split('\n').filter(t => t.trim()).map(t => ({ refreshToken: t.trim() })); }
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || '导入了 ' + tokens.length + ' 个账号', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('导入失败: ' + e.message, 'error'); }
  event.target.value = '';
}

// ========== API 密钥管理 ==========
async function loadKeys() {
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys;
    const el = _('keysList');
    if (!keys || keys.length === 0) {
      el.innerHTML = '<div class="empty-state"><span class="icon">🔑</span>暂无 API 密钥</div>';
      return;
    }
    el.innerHTML = keys.map(k =>
      '<div class="flex" style="margin-bottom:8px">' +
        '<span class="key-display" style="flex:1" onclick="copyText(\'' + k + '\')" title="点击复制">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" onclick="deleteKey(\'' + k + '\')">✕</button>' +
      '</div>'
    ).join('');
  } catch (e) { _('keysList').innerHTML = '<div class="empty">加载失败</div>'; }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:var(--bg);border:1px solid var(--green);border-radius:6px;padding:12px">' +
        '<div style="color:var(--green);font-weight:500;margin-bottom:8px">✓ 新密钥已生成（点击复制）</div>' +
        '<div class="key-display" onclick="copyText(\'' + key + '\')">' + esc(key) + '</div>' +
      '</div>';
    loadKeys();
    toast('密钥已生成', 'success');
    setTimeout(() => _('keyGenResult').innerHTML = '', 8000);
  } catch (e) { toast('生成失败: ' + e.message, 'error'); }
}

async function deleteKey(key) {
  if (!confirm('确定删除此密钥？')) return;
  try {
    await api('POST', '/keys/delete', { key });
    toast('密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

async function deleteAllKeys() {
  if (!confirm('确定删除所有 API 密钥？')) return;
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys || [];
    for (const k of keys) await api('POST', '/keys/delete', { key: k });
    toast('全部密钥已删除', 'success');
    loadKeys();
  } catch (e) { toast('删除失败: ' + e.message, 'error'); }
}

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('已复制到剪贴板', 'success')).catch(() => {
    const ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta);
    toast('已复制到剪贴板', 'success');
  });
}

// ========== 配置管理 ==========
async function updateConfig() {
  const strategy = _('settingStrategy').value;
  try {
    await api('POST', '/config/update', { strategy });
    toast('策略已更新为: ' + strategy, 'success');
  } catch (e) { toast('更新失败: ' + e.message, 'error'); }
}

function addHeaderRow() {
  const tbody = _('headersTableBody');
  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:12px;font-family:monospace"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:12px;font-family:monospace"></td>' +
    '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>';
  tbody.appendChild(tr);
}

async function saveHeaders() {
  const tbody = _('headersTableBody');
  const rows = tbody.querySelectorAll('tr');
  const headers = {};
  let hasEmpty = false;
  rows.forEach(tr => {
    const keyInput = tr.querySelector('.header-key');
    const valInput = tr.querySelector('.header-val');
    if (keyInput && valInput) {
      const k = keyInput.value.trim();
      const v = valInput.value.trim();
      if (k) { headers[k] = v; }
      else if (v) { hasEmpty = true; }
    }
  });
  if (hasEmpty) { toast('存在有值无键的行，已忽略', 'info'); }
  try {
    const d = await api('POST', '/config/update', { headers });
    toast('请求头已保存', 'success');
    _('headerSaveResult').innerHTML =
      '<div style="color:var(--green);font-size:12px">✓ 已保存 ' + Object.keys(d.data.headers).length + ' 个请求头</div>';
    setTimeout(() => _('headerSaveResult').innerHTML = '', 5000);
    loadConfig();
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 模型列表 ==========
// ========== 配置加载 ==========
async function loadConfig() {
  try {
    const d = await api('GET', '/config');
    const c = d.data;
    if (c.address) _('settingAddr').value = c.address;
    if (c.strategy) _('settingStrategy').value = c.strategy;
    if (c.version) _('settingVersion').value = c.version;
    if (c.poolPath) _('settingPoolPath').value = c.poolPath;
    if (c.defaultModel) _('settingDefModel').value = c.defaultModel;
    if (c.headers) {
      const tbody = _('headersTableBody');
      tbody.innerHTML = Object.entries(c.headers).map(([k, v]) =>
        '<tr>' +
          '<td><input type="text" class="header-key" value="' + esc(k) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + esc(v) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">✕</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { /* ignore */ }
}

// ========== override.md 编辑 ==========
async function loadOverride() {
  try {
    const d = await api('GET', '/override');
    _('overrideContent').value = d.data.content || '';
  } catch (e) { toast('加载 override 失败: ' + e.message, 'error'); }
}

async function saveOverride() {
  const content = _('overrideContent').value;
  try {
    await api('POST', '/override', { content });
    _('overrideSaveResult').innerHTML = '<div style="color:var(--green);font-size:12px">✓ 已保存（' + content.length + ' 字节）</div>';
    toast('override.md 已保存', 'success');
    setTimeout(() => { _('overrideSaveResult').innerHTML = ''; }, 5000);
  } catch (e) {
    _('overrideSaveResult').innerHTML = '<div style="color:var(--red);font-size:12px">✕ ' + esc(e.message) + '</div>';
    toast('保存失败: ' + e.message, 'error');
  }
}

// ========== 请求统计 ==========
async function loadStatsPage() {
  const days = _('statsDays') ? _('statsDays').value : 7;
  const results = await Promise.all([
    api('GET', '/stats/usage?days=' + days),
    api('GET', '/stats/by-account?days=' + days),
    api('GET', '/stats/by-model?days=' + days),
    api('GET', '/stats/errors?days=' + days + '&limit=50')
  ]).catch(err => { toast('加载统计失败: ' + err.message, 'error'); return null; });
  if (!results) return;
  const [u, a, m, e] = results;
  renderStatsOverview(u.data.overview);
  renderStatsTrend(u.data.trend);
  renderStatsAccounts(a.data.accounts);
  renderStatsModels(m.data.models);
  renderStatsErrors(e.data.errors);
}

function fmtNum(n) {
  const v = Number(n);
  if (!isFinite(v)) return (n === undefined || n === null) ? '0' : String(n);
  const abs = Math.abs(v);
  if (abs >= 1e9) return (v / 1e9).toFixed(2) + 'B';
  if (abs >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (abs >= 1e3) return (v / 1e3).toFixed(2) + 'K';
  return String(v);
}

function statCard(num, label, color) {
  return '<div class="card"><div class="num ' + (color||'') + '">' + fmtNum(num) + '</div><div class="label">' + label + '</div></div>';
}

function renderStatsOverview(o) {
  if (!o) { _('statsOverview').innerHTML = statCard('-', '无数据', ''); return; }
  _('statsOverview').innerHTML =
    statCard(o.total_requests, '总请求', 'blue') +
    statCard(o.success, '成功', 'green') +
    statCard(o.errors, '失败', 'red') +
    statCard(o.prompt_tokens, '输入 Tokens', 'blue') +
    statCard(o.completion_tokens, '输出 Tokens', 'green') +
    statCard(o.total_tokens, '总 Tokens', 'yellow');
}

function renderStatsTrend(trend) {
  if (!trend || trend.length === 0) { _('statsTrend').innerHTML = '<div class="empty">无数据</div>'; return; }
  const max = Math.max.apply(null, trend.map(t => t.requests || 0)) || 1;
  _('statsTrend').innerHTML = trend.map(t => {
    const pct = Math.round((t.requests / max) * 100);
    return '<div style="display:flex;align-items:center;gap:8px;margin-bottom:6px">' +
      '<span style="width:80px;font-family:monospace;font-size:11px;color:var(--text2)">' + esc(t.date) + '</span>' +
      '<div style="flex:1;background:var(--bg3);border-radius:4px;height:18px;overflow:hidden">' +
        '<div style="width:' + pct + '%;height:100%;background:var(--accent);opacity:0.6"></div></div>' +
      '<span style="width:60px;text-align:right;font-size:11px">' + (t.requests||0) + ' 次</span>' +
      '<span style="width:90px;text-align:right;font-size:11px;color:var(--text2)">' + fmtNum(t.total_tokens) + ' tok</span>' +
    '</div>';
  }).join('');
}

function statRow(first, r) {
  return '<tr>' +
    '<td>' + esc(first) + '</td>' +
    '<td>' + (r.requests||0) + '</td>' +
    '<td style="color:var(--green)">' + (r.success||0) + '</td>' +
    '<td style="color:var(--red)">' + (r.errors||0) + '</td>' +
    '<td>' + fmtNum(r.prompt_tokens) + '</td>' +
    '<td>' + fmtNum(r.completion_tokens) + '</td>' +
    '<td>' + fmtNum(r.total_tokens) + '</td>' +
  '</tr>';
}

function renderStatsAccounts(list) {
  if (!list || list.length === 0) { _('statsAccountBody').innerHTML = '<tr><td colspan="7" class="empty">无数据</td></tr>'; return; }
  _('statsAccountBody').innerHTML = list.map(a => statRow(a.email, a)).join('');
}

function renderStatsModels(list) {
  if (!list || list.length === 0) { _('statsModelBody').innerHTML = '<tr><td colspan="7" class="empty">无数据</td></tr>'; return; }
  _('statsModelBody').innerHTML = list.map(m => statRow(m.model, m)).join('');
}

function renderStatsErrors(list) {
  if (!list || list.length === 0) { _('statsErrorBody').innerHTML = '<tr><td colspan="5" class="empty">无错误记录</td></tr>'; return; }
  _('statsErrorBody').innerHTML = list.map(e => {
    const msg = e.error_message || '';
    const preview = esc(msg.slice(0, 80)) + (msg.length > 80 ? '...' : '');
    return '<tr>' +
      '<td class="mono" style="font-size:11px">' + esc(e.created_at) + '</td>' +
      '<td>' + esc(e.account_email) + '</td>' +
      '<td style="font-size:12px">' + esc(e.model) + '</td>' +
      '<td>' + (e.status_code || '-') + '</td>' +
      '<td style="max-width:420px"><details><summary style="cursor:pointer;color:var(--text2);font-size:12px">' + preview +
        '</summary><div style="margin-top:6px;white-space:pre-wrap;word-break:break-all;font-family:monospace;font-size:11px;color:var(--red)">' +
        esc(msg) + '</div></details></td>' +
    '</tr>';
  }).join('');
}

async function clearStats() {
  if (!confirm('确定清空全部统计记录？不可撤销！')) return;
  try {
    const d = await api('POST', '/stats/clear', { beforeDays: 0 });
    toast(d.message || '已清空', 'success');
    loadStatsPage();
  } catch (err) { toast('清空失败: ' + err.message, 'error'); }
}

// ========== 模型上下文限制 ==========
async function loadModelLimits() {
  try {
    const d = await api('GET', '/model-limits');
    const models = d.data.models || {};
    const tbody = _('modelLimitBody');
    const entries = Object.entries(models);
    if (entries.length === 0) { tbody.innerHTML = '<tr><td colspan="3" class="empty">无可用模型</td></tr>'; return; }
    tbody.innerHTML = entries.map(([id, limit], i) => {
      const el = 'ml-' + i;
      const disp = (limit > 0 ? fmtNum(limit) + ' tokens' : '不限制');
      return '<tr>' +
        '<td style="font-size:12px">' + esc(id) + '</td>' +
        '<td style="font-size:12px;white-space:nowrap">' +
          '<span id="' + el + '-label">' + disp + '</span>' +
          '<span id="' + el + '-edit" style="display:none;gap:6px;align-items:center;white-space:nowrap">' +
            '<input type="number" min="0" step="1000" id="' + el + '-input" value="' + (limit || 0) + '" placeholder="0=不限制" style="width:160px;background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:4px 8px;font-size:12px">' +
          '</span>' +
        '</td>' +
        '<td style="white-space:nowrap">' +
          '<button id="' + el + '-btn" onclick="editModelLimit(' + i + ')" title="编辑限制" style="background:none;border:none;cursor:pointer;padding:2px;color:var(--text2);line-height:0">' +
            '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5L17 3z"/></svg>' +
          '</button>' +
          '<span id="' + el + '-actions" style="display:none;gap:6px;white-space:nowrap">' +
            '<button class="btn btn-sm btn-primary" onclick="saveModelLimit(' + i + ',\'' + esc(id) + '\')">保存</button>' +
            '<button class="btn btn-sm" onclick="cancelEditModelLimit(' + i + ')">取消</button>' +
          '</span>' +
        '</td>' +
      '</tr>';
    }).join('');
  } catch (e) { _('modelLimitBody').innerHTML = '<tr><td colspan="3" class="empty">加载失败</td></tr>'; }
}

function editModelLimit(i) {
  _('modelLimitBody').querySelectorAll('[id$="-label"]').forEach(el => el.style.display = 'none');
  _('modelLimitBody').querySelectorAll('[id$="-edit"]').forEach(el => el.style.display = 'none');
  _('modelLimitBody').querySelectorAll('[id$="-actions"]').forEach(el => el.style.display = 'none');
  _('modelLimitBody').querySelectorAll('[id$="-btn"]').forEach(el => el.style.display = '');
  _('ml-' + i + '-label').style.display = 'none';
  _('ml-' + i + '-edit').style.display = 'inline-flex';
  _('ml-' + i + '-actions').style.display = 'inline-flex';
  _('ml-' + i + '-btn').style.display = 'none';
}

function cancelEditModelLimit(i) {
  _('ml-' + i + '-label').style.display = '';
  _('ml-' + i + '-edit').style.display = 'none';
  _('ml-' + i + '-actions').style.display = 'none';
  _('ml-' + i + '-btn').style.display = '';
}

async function saveModelLimit(i, modelId) {
  const input = _('ml-' + i + '-input');
  const limit = parseInt(input && input.value, 10) || 0;
  try {
    await api('POST', '/model-limits/update', { modelId, limit });
    _('ml-' + i + '-label').textContent = (limit > 0 ? fmtNum(limit) + ' tokens' : '不限制');
    _('ml-' + i + '-label').style.display = '';
    _('ml-' + i + '-edit').style.display = 'none';
    _('ml-' + i + '-actions').style.display = 'none';
    _('ml-' + i + '-btn').style.display = '';
    toast('已保存 ' + modelId + ' 限制: ' + (limit || '不限制'), 'success');
  } catch (e) { toast('保存失败: ' + e.message, 'error'); }
}

// ========== 初始化 ==========
if (_('footerApiAddr')) _('footerApiAddr').innerText = window.location.origin;
loadStats();
loadAccounts();
loadKeys();
loadConfig();
loadOverride();
loadModelLimits();
setInterval(() => { loadStats(); }, 10000);
</script>
</body>
</html>`
