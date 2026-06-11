// 临时盘前端逻辑 (极简明亮风格 + 文件夹支持)
// 全部使用 DOM API 操作，防止 XSS（禁止使用 innerHTML）

// ===== 全局状态 =====
let currentDir = '/';
let player = null;

// ===== 工具函数 =====

/** 将字节数格式化为可读字符串 */
function formatSize(bytes) {
  if (bytes === undefined || bytes === null || bytes === '') return '-';
  const v = Number(bytes);
  if (isNaN(v) || v < 0) return '-';
  if (v === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let val = v;
  while (val >= 1024 && i < units.length - 1) { val /= 1024; i++; }
  return val.toFixed(i === 0 ? 0 : 1) + ' ' + units[i];
}

/** 将 Unix 时间戳格式化为日期字符串 */
function formatTime(ts) {
  if (!ts) return '-';
  const d = new Date(Number(ts) * 1000);
  return d.toLocaleString('zh-CN', { hour12: false });
}

/** 根据文件扩展名返回 Emoji 图标 */
function fileIcon(name, isDir) {
  if (isDir) return '📁';
  const ext = (name || '').split('.').pop().toLowerCase();
  const map = {
    'jpg': '🖼️', 'jpeg': '🖼️', 'png': '🖼️', 'gif': '🖼️', 'webp': '🖼️', 'svg': '🖼️',
    'mp4': '🎬', 'mov': '🎬', 'avi': '🎬', 'mkv': '🎬', 'flv': '🎬',
    'mp3': '🎵', 'flac': '🎵', 'wav': '🎵', 'aac': '🎵',
    'pdf': '📄', 'doc': '📝', 'docx': '📝', 'xls': '📊', 'xlsx': '📊', 'ppt': '📋', 'pptx': '📋',
    'zip': '📦', 'rar': '📦', '7z': '📦', 'tar': '📦', 'gz': '📦',
    'exe': '⚙️', 'msi': '⚙️', 'dmg': '⚙️', 'apk': '📱',
    'txt': '📃', 'md': '📃', 'json': '🔧', 'xml': '🔧', 'yaml': '🔧',
  };
  return map[ext] || '📄';
}

/** 判断是否为音频文件 */
function isAudioFile(name) {
  const ext = (name || '').split('.').pop().toLowerCase();
  return ['mp3', 'flac', 'wav', 'aac', 'm4a', 'ogg'].includes(ext);
}

/** 初始化播放器 */
function initPlayer() {
  if (!window.APlayer) return;
  const container = document.getElementById('player-container');
  player = new APlayer({
    container: container,
    audio: [],
    mini: true,
    autoplay: false,
    theme: '#2563eb',
    loop: 'none',
    order: 'list',
    preload: 'auto',
    volume: 0.7,
    mutex: true,
    lrcType: 0
  });
}

/** 播放音频文件 */
function playAudio(filePath, fileName) {
  if (!player) initPlayer();
  if (!player) { showToast('❌ 播放器未加载'); return; }
  showToast('⏳ 正在加载音频…');

  Promise.all([
    fetch('/api/download?path=' + encodeURIComponent(filePath) + '&format=json', { credentials: 'same-origin' }).then(function(r) { return r.json(); }),
    fetch('/api/audiometa?path=' + encodeURIComponent(filePath), { credentials: 'same-origin' }).then(function(r) { return r.json(); }).catch(function() { return {}; })
  ])
    .then(function(results) {
      const dlData = results[0];
      const meta = results[1];
      const urls = dlData.urls || [];
      if (!urls.length || !urls[0].url) throw new Error('无法获取音频链接');

      const audioUrl = urls[0].url;
      const baseName = fileName.replace(/\.[^.]+$/, '');

      player.list.clear();
      player.list.add({
        name: meta.title || baseName,
        artist: meta.artist || '未知艺术家',
        url: audioUrl,
        cover: meta.cover || '',
        lrc: meta.lyrics || ''
      });
      document.getElementById('player-container').classList.add('show');
      player.play();
      showToast('🎵 开始播放：' + fileName);
    })
    .catch(function(err) {
      showToast('❌ 播放失败：' + err.message);
    });
}

/** 显示 Toast 提示 */
function showToast(msg) {
  let toast = document.getElementById('toast');
  if (toast) toast.remove();

  toast = document.createElement('div');
  toast.id = 'toast';
  toast.className = 'show';
  toast.textContent = msg;
  document.body.appendChild(toast);

  setTimeout(function() {
    toast.classList.remove('show');
    setTimeout(function() { toast.remove(); }, 250);
  }, 2500);
}

// ===== 路径导航与面包屑 =====

function renderBreadcrumbs() {
  const container = document.getElementById('breadcrumbs');
  container.replaceChildren();

  // 1. 根目录节点
  const rootSpan = document.createElement('span');
  if (currentDir === '/') {
    rootSpan.className = 'breadcrumb-current';
    rootSpan.textContent = '根目录';
    container.appendChild(rootSpan);
  } else {
    rootSpan.className = 'breadcrumb-item';
    rootSpan.textContent = '根目录';
    rootSpan.addEventListener('click', function() {
      enterDir('/');
    });
    container.appendChild(rootSpan);
  }

  if (currentDir === '/') return;

  // 2. 子目录逐级解析
  const parts = currentDir.split('/').filter(function(p) { return p !== ''; });
  let accPath = '';
  parts.forEach(function(part, index) {
    // 间隔符
    const sep = document.createElement('span');
    sep.className = 'breadcrumb-separator';
    sep.textContent = ' / ';
    container.appendChild(sep);

    accPath += '/' + part;
    const isLast = index === parts.length - 1;

    const span = document.createElement('span');
    if (isLast) {
      span.className = 'breadcrumb-current';
      span.textContent = part;
      container.appendChild(span);
    } else {
      span.className = 'breadcrumb-item';
      span.textContent = part;
      const targetPath = accPath; // 锁闭包变量
      span.addEventListener('click', function() {
        enterDir(targetPath);
      });
      container.appendChild(span);
    }
  });
}

function enterDir(dir) {
  currentDir = dir;
  loadFiles(dir);
}

// ===== 文件列表加载 =====

function loadFiles(dir) {
  const targetDir = dir || currentDir;
  const listEl = document.getElementById('file-list');
  const statusEl = document.getElementById('status');
  const countEl = document.getElementById('file-count');
  const refreshBtn = document.getElementById('btn-refresh');

  // 渲染面包屑
  renderBreadcrumbs();

  // 重置状态
  listEl.replaceChildren();
  statusEl.style.display = 'block';
  statusEl.replaceChildren();

  const spinner = document.createElement('div');
  spinner.className = 'spinner';
  const tip = document.createElement('div');
  tip.textContent = '正在读取文件列表…';
  statusEl.appendChild(spinner);
  statusEl.appendChild(tip);

  countEl.textContent = '读取中…';
  refreshBtn.disabled = true;

  fetch('/api/files?dir=' + encodeURIComponent(targetDir), { credentials: 'same-origin' })
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      const files = data.list || [];
      statusEl.style.display = 'none';
      countEl.textContent = '当前目录共 ' + files.length + ' 个条目';

      // 1. 如果不是根目录，加入返回上一级条目
      if (targetDir !== '/') {
        listEl.appendChild(buildParentItem());
      }

      if (files.length === 0 && targetDir === '/') {
        statusEl.style.display = 'block';
        statusEl.replaceChildren();
        const empty = document.createElement('div');
        empty.textContent = '网盘根目录为空';
        statusEl.appendChild(empty);
        return;
      }

      // 2. 依次排序：文件夹排前面，文件排后面
      files.sort(function(a, b) {
        return b.isdir - a.isdir;
      });

      // 3. 构建并渲染条目
      files.forEach(function(file) {
        listEl.appendChild(buildFileItem(file));
      });
    })
    .catch(function(err) {
      statusEl.style.display = 'block';
      statusEl.replaceChildren();
      const errDiv = document.createElement('div');
      errDiv.className = 'error-msg';
      errDiv.textContent = '⚠️ 列表读取失败，请检查网盘 Cookie 状态';
      statusEl.appendChild(errDiv);
      console.warn('[网盘] 文件读取失败: ' + err.message);
    })
    .finally(function() {
      refreshBtn.disabled = false;
    });
}

/** 安全构建返回上一级条目 */
function buildParentItem() {
  const item = document.createElement('div');
  item.className = 'file-item';
  item.setAttribute('role', 'listitem');

  const iconEl = document.createElement('div');
  iconEl.className = 'file-icon';
  iconEl.textContent = '📁';
  item.appendChild(iconEl);

  const infoEl = document.createElement('div');
  infoEl.className = 'file-info';

  const nameEl = document.createElement('div');
  nameEl.className = 'file-name';
  nameEl.textContent = '..';
  infoEl.appendChild(nameEl);

  const metaEl = document.createElement('div');
  metaEl.className = 'file-meta';
  metaEl.textContent = '返回上一级目录';
  infoEl.appendChild(metaEl);

  item.appendChild(infoEl);

  // 计算父级目录
  const parts = currentDir.split('/').filter(function(p) { return p !== ''; });
  parts.pop();
  const parentPath = '/' + parts.join('/');

  item.addEventListener('click', function() {
    enterDir(parentPath);
  });

  return item;
}

/** 安全构建单个文件/文件夹条目 */
function buildFileItem(file) {
  const isDir = file.isdir === 1;
  const item = document.createElement('div');
  item.className = 'file-item';
  item.setAttribute('role', 'listitem');

  // 图标
  const iconEl = document.createElement('div');
  iconEl.className = 'file-icon';
  iconEl.textContent = fileIcon(file.server_filename || '', isDir);
  item.appendChild(iconEl);

  // 文件名及元数据
  const infoEl = document.createElement('div');
  infoEl.className = 'file-info';

  const nameEl = document.createElement('div');
  nameEl.className = 'file-name';
  nameEl.textContent = file.server_filename || '未知文件';
  infoEl.appendChild(nameEl);

  const metaEl = document.createElement('div');
  metaEl.className = 'file-meta';
  if (isDir) {
    metaEl.textContent = formatTime(file.local_mtime);
  } else {
    metaEl.textContent = formatSize(file.size) + ' · ' + formatTime(file.local_mtime);
  }
  infoEl.appendChild(metaEl);

  item.appendChild(infoEl);

  // 动作按钮容器
  const actionsEl = document.createElement('div');
  actionsEl.style.display = 'flex';
  actionsEl.style.gap = '8px';

  const actionBtn = document.createElement('button');
  actionBtn.className = 'file-action';
  actionBtn.setAttribute('type', 'button');

  const filePath = file.path || '';
  const fileName = file.server_filename || '';

  if (isDir) {
    actionBtn.textContent = '进入 ➡️';
    actionBtn.setAttribute('aria-label', '进入文件夹 ' + fileName);
    actionBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      enterDir(filePath);
    });
    item.addEventListener('click', function() {
      enterDir(filePath);
    });
    actionsEl.appendChild(actionBtn);
  } else {
    const copyBtn = document.createElement('button');
    copyBtn.className = 'file-action';
    copyBtn.setAttribute('type', 'button');
    copyBtn.textContent = '复制链接 🔗';
    copyBtn.setAttribute('aria-label', '复制链接 ' + fileName);
    copyBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      copyLink(filePath);
    });
    actionsEl.appendChild(copyBtn);

    if (isAudioFile(fileName)) {
      const playBtn = document.createElement('button');
      playBtn.className = 'file-action';
      playBtn.setAttribute('type', 'button');
      playBtn.textContent = '播放 🎵';
      playBtn.setAttribute('aria-label', '播放 ' + fileName);
      playBtn.addEventListener('click', function(e) {
        e.stopPropagation();
        playAudio(filePath, fileName);
      });
      actionsEl.appendChild(playBtn);
      item.addEventListener('click', function() {
        playAudio(filePath, fileName);
      });
    } else {
      item.addEventListener('click', function() {
        downloadFile(filePath, fileName);
      });
    }

    actionBtn.textContent = '下载 ⬇';
    actionBtn.setAttribute('aria-label', '下载 ' + fileName);
    actionBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      downloadFile(filePath, fileName);
    });
    actionsEl.appendChild(actionBtn);
  }

  item.appendChild(actionsEl);
  return item;
}

// ===== 文件下载 =====

function downloadFile(filePath, fileName) {
  if (!filePath) {
    showToast('⚠️ 路径解析失败');
    return;
  }

  showToast('⏳ 正在生成安全直链…');

  fetch('/api/download?path=' + encodeURIComponent(filePath) + '&format=json', { credentials: 'same-origin' })
    .then(function(resp) {
      if (!resp.ok) throw new Error('HTTP ' + resp.status);
      return resp.json();
    })
    .then(function(data) {
      const urls = data.urls || [];
      if (!urls.length || !urls[0].url) {
        throw new Error('签名直链为空');
      }
      const downloadUrl = urls[0].url;

      // 百度直链为跨域地址，a.download 属性对跨域无效
      // 使用 window.open 新窗口打开直链，浏览器会自动触发下载
      window.open(downloadUrl, '_blank', 'noopener,noreferrer');

      showToast('✅ 唤起下载：' + fileName);
    })
    .catch(function(err) {
      showToast('❌ 下载失败：' + err.message);
      console.warn('[网盘] 直链解析失败: ' + err.message);
    });
}

function copyLink(filePath) {
  const url = window.location.origin + '/api/download?path=' + encodeURIComponent(filePath);
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).then(function() {
      showToast('✅ 链接已复制到剪贴板');
    }).catch(function(err) {
      showToast('❌ 复制失败');
      console.warn('复制链接失败', err);
    });
  } else {
    const input = document.createElement('input');
    input.value = url;
    document.body.appendChild(input);
    input.select();
    try {
      document.execCommand('copy');
      showToast('✅ 链接已复制到剪贴板');
    } catch (err) {
      showToast('❌ 复制失败');
    }
    document.body.removeChild(input);
  }
}

// ===== 初始化 =====
document.addEventListener('DOMContentLoaded', function() {
  initPlayer();
  loadFiles();
  document.getElementById('btn-refresh').addEventListener('click', function() {
    loadFiles(currentDir);
  });
});
