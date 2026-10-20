/* ViperRipper New — frontend logic, talks to Wails bindings (window.go.main.App). */
(function () {
  'use strict';
  window.__mark = ['iife-start'];
  var App = function () { return window.go.main.App; };
  var app = document.getElementById('gv-app');
  var $ = function (id) { return document.getElementById(id); };

  // surface every frontend error in the Log tab (and the window title for
  // headless debugging) — silent failures are the worst kind
  window.addEventListener('error', function (e) {
    var msg = (e.message || 'unknown') + ' @' + (e.filename || '') + ':' + (e.lineno || 0);
    document.title = 'ERR ' + msg.slice(0, 120);
    try { App().LogFromFrontend('ERROR', msg); } catch (ex) {}
  });
  window.addEventListener('unhandledrejection', function (e) {
    var msg = 'promise: ' + (e.reason && (e.reason.message || e.reason) || 'unknown');
    document.title = 'ERR ' + msg.slice(0, 120);
    try { App().LogFromFrontend('ERROR', msg); } catch (ex) {}
  });
  window.__mark.push('listeners-ok');

  var state = {
    posts: [],            // rows of Download tab
    threads: [],
    sel: [],              // selected post row ids
    selThread: 0,         // selected thread row id
    ctxPost: 0,           // 右键/“更多”指向的帖子行（单条菜单项的目标）
    cfHost: '',           // 需要人机验证的域名
    cfURL: '',            // 触发验证的那条链接（打开的初始页）
    images: {},           // postRowId -> [images]
    curPost: 0,           // post shown in General/Photo/preview
    running: {},          // postRowId -> true
    filter: '',
    sort: { col: -1, dir: 1 },  // 当前视图生效的排序（普通列表 / 主题管理各一份，见下）
    normalSort: { col: -1, dir: 1 },   // 普通下载列表记住的排序
    threadSort: { col: -1, dir: 1 },   // 主题管理（楼层列表）记住的排序
    sortView: 'dl',       // 当前 state.sort 属于哪个视图：'dl' | 'th'
    dlOrderIds: null,     // 上一次渲染的行序（key 列表）：进度变化时不重排，避免行乱跳
    dlOrderSig: '',       // 行序对应的排序/过滤签名
    editMode: false,      // Download 列表批量编辑（勾选删除 Thread/Post）
    editSel: [],          // 'p'+postRowId / 't'+threadRowId
    editAnchor: null,     // 编辑模式 shift 范围勾选的锚点 key
    editRule: null,       // host rule being edited
    floors: [],           // floor picker data
    floorThread: 0,
    floorChecked: {},
    floorTotal: 0,        // 后端已知楼层总数（含尚未放行的）
    previewPost: 0,       // 右侧预览当前显示的是哪个帖子的图
    previewImgId: 0,      // 右侧预览当前显示的图片 id
    followPost: 0,        // 没有选中帖子时，右侧信息栏跟随的这个正在下载的帖子
    viewThread: 0,        // thread row id currently managed (0 = normal list)
    importLog: [],        // [{at, items:[{url,status,note}]}] newest first
    importSel: [],        // selected import-row ids (batchIdx:itemIdx)
    importAnchor: -1,     // shift-range anchor within the import panel
    importQueue: [],      // pending AddLinks batches ({text, count})
    importBusy: false,    // an AddLinks call is in flight
    retrying: {}          // imageId -> 标记时间(ms)，用于 photolist 状态栏显示“重试中”
  };

  function icons() {
    if (window.lucide) window.lucide.createIcons({ attrs: { width: 13, height: 13 } });
  }
  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  // 浮窗提示：右下弹一小条，1.6 秒后淡出；同时仍写进日志列表
  function toast(msg) {
    try { addLogLine('INFO', msg); } catch (e) {}
    try {
      var wrap = $('gv-toasts');
      if (!wrap) return;
      var el = document.createElement('div');
      el.className = 'gv-toast';
      el.textContent = msg;
      wrap.appendChild(el);
      while (wrap.children.length > 3) wrap.removeChild(wrap.firstChild);
      setTimeout(function () {
        el.classList.add('out');
        setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 220);
      }, 1600);
    } catch (e) {}
  }

  // ---------- 任务完成提示音 ----------
  // WebAudio 双音（A5 → D6，上行小三度）+ 淡出包络，听感比单音蜂鸣清楚，
  // 也不会在结尾“咔”一声。两个必须注意的点：
  //  1) WebView2 有自动播放限制，AudioContext 在用户交互前是 suspended 的，
  //     必须在 resume() 完成之后才创建振荡器，否则第一次（往往就是最关键的
  //     那一次）没有声音；
  //  2) 万一音频上下文拿不到、或被策略拒绝，退回系统提示音（后端 MessageBeep），
  //     保证“响了”这件事不依赖 webview 的音频策略。
  var doneAudio = { ctx: null, tried: false };
  function playDoneSound() {
    function fallback() {
      try { if (App().BeepDone) App().BeepDone(); } catch (e) {}
    }
    var AC = window.AudioContext || window.webkitAudioContext;
    if (!AC) { fallback(); return; }
    var ac;
    try { ac = doneAudio.ctx || (doneAudio.ctx = new AC()); } catch (e) { fallback(); return; }
    function chime() {
      try {
        var t0 = ac.currentTime + 0.02;
        [[880, 0], [1174.66, 0.17]].forEach(function (p) {
          var o = ac.createOscillator(), g = ac.createGain();
          var start = t0 + p[1], end = start + 0.34;
          o.type = 'sine';
          o.frequency.value = p[0];
          g.gain.setValueAtTime(0.0001, start);
          g.gain.exponentialRampToValueAtTime(0.25, start + 0.02);
          g.gain.exponentialRampToValueAtTime(0.0001, end);
          o.connect(g); g.connect(ac.destination);
          o.start(start); o.stop(end + 0.03);
        });
      } catch (e) { fallback(); }
    }
    if (ac.state === 'suspended') {
      var pr = null;
      try { pr = ac.resume(); } catch (e) {}
      if (pr && pr.then) pr.then(chime, fallback);
      else chime();
      return;
    }
    if (ac.state === 'closed') { doneAudio.ctx = null; fallback(); return; }
    chime();
  }

  // ---------- status bar ----------
  function statusOf(p) {
    if (p.status === 'downloading') {
      var pct = p.total > 0 ? Math.round((p.done + p.failed) / p.total * 100) : 0;
      return { w: pct, t: pct + '%' };
    }
    if (p.status === 'paused') {
      var pct2 = p.total > 0 ? Math.round((p.done + p.failed) / p.total * 100) : 0;
      // 进度条本身就是百分比，文字只写 Paused，避免重复
      return { w: pct2, t: '已暂停' };
    }
    if (p.status === 'error') return { w: p.total > 0 ? Math.round((p.done) / p.total * 100) : 100, t: p.failed + ' 个链接失效' };
    if (p.status === 'done') return { w: 100, t: '100%' };
    return { w: 0, t: '未开始' };
  }

  // ---------- Download list ----------
  // sort key extractors for the download-list header columns (data-col order)
  var sortKeys = [
    function (p) { return (p.title || '').toLowerCase(); },
    function (p) { return p.postId || 0; },
    function (p) { return p.total > 0 ? p.done / p.total : -1; },
    function (p) { return p.status || ''; },
    function (p) { return p.createdAt || ''; },
    function (p) { return (p.hosts || '').toLowerCase(); }
  ];

  // thread aggregation: floors of this thread that are in Download
  function threadMeta(t) {
    var floors = state.posts.filter(function (p) { return p.threadId === t.threadId; });
    var done = floors.filter(function (p) { return p.status === 'done'; }).length;
    var sumDone = 0, sumTotal = 0;
    floors.forEach(function (p) { sumDone += (p.done || 0) + (p.failed || 0); sumTotal += (p.total || 0); });
    return { floors: floors, joined: floors.length, done: done, sumDone: sumDone, sumTotal: sumTotal };
  }

  // 状态分组（Status 列排序用，普通列表和主题管理视图共用这一套，保证
  // 两处排列一致）。数值越大越靠前（该列默认降序）：
  //   4 = 已完成
  //   3 = 下载中（哪怕进度 0%，也排在已暂停之前）
  //   2 = 已暂停
  //   1 = 全部失败（N 个链接失效）
  //   0 = 未开始（ready / pending）
  // 因此"已下载 / 下载中"始终排在 ready 之前，且下载中恒在 paused 之上。
  function statusRankOfPost(p) {
    var s = p.status;
    if (s === 'done') return 4;
    if (s === 'downloading') return 3;
    if (s === 'paused') return 2;
    if (s === 'error') return 1;
    return 0;
  }
  function progressOfPost(p) {
    return p.total > 0 ? (p.done + p.failed) / p.total : -1;
  }

  function threadRow(t, manage) {
    var m = threadMeta(t);
    var row = document.createElement('div');
    // 管理视图里主题行钉在表头下面（class 决定 position: sticky），
    // 滚动楼层列表时不跟着滚走
    row.className = 'gv-row gv-row-thread' + (manage ? ' gv-pinned' : '');
    row.setAttribute('data-tid', t.id);
    if (t.mark) row.setAttribute('data-mark', t.mark); // 用户标记颜色
    // 进度条显示图片级百分比（所有已加入楼层的已完成图片 / 总图片），
    // 不用进管理列表就能看到下载进度；无图片统计时退回楼层完成度
    var pct;
    if (m.sumTotal > 0) pct = Math.round(m.sumDone / m.sumTotal * 100);
    else pct = m.joined > 0 ? Math.round(m.done / m.joined * 100) : 0;
    // Progress 三段：已完成楼层 / 已选择楼层 / 有图片的楼层总数（无信息时全 0）
    var totalFloors = t.postCount || 0;
    // 状态条只显示五种：未开始 / 下载中 / 已暂停 / 已完成 / N 个链接失效
    var stTxt = '';
    var isDl = false; // 下载中 → 状态条加发光（CSS 动画）
    if (m.joined > 0) {
      var errN = 0, dlN = 0;
      m.floors.forEach(function (p) {
        if (p.status === 'error') errN++;
        else if (p.status === 'downloading') dlN++;
      });
      // 刚点开始、状态还没落库时，用运行表兜底，避免“在跑却不发光”
      var anyRun = m.floors.some(function (p) { return state.running && state.running[p.id]; });
      isDl = dlN > 0 || anyRun;
      if (m.done === m.joined) { stTxt = '已完成'; isDl = false; }
      else if (errN > 0 && m.done + errN === m.joined) { stTxt = errN + ' 个链接失效'; isDl = false; }
      else if (isDl) stTxt = '下载中';
      else stTxt = m.floors.every(function (p) { return p.status === 'ready'; }) ? '未开始' : '已暂停';
    }
    row.innerHTML =
      '<span class="gv-name">' + esc(t.title) + '</span>' +
      '<span class="gv-cnt">—</span>' +
      '<span class="gv-cnt">' + m.done + ' / ' + m.joined + ' / ' + totalFloors + '</span>' +
      '<span class="gv-state' + (isDl ? ' gv-dl' : '') + '"><i style="width:' + pct + '%"></i><span>' + esc(stTxt) + '</span></span>' +
      '<span class="gv-cnt">' + esc((t.createdAt || '').slice(0, 16)) + '</span>' +
      '<span class="gv-cnt">—</span>';
    var act = document.createElement('button');
    if (manage) {
      act.className = 'gv-thread-close';
      act.title = '退出主题管理';
      act.innerHTML = '<i data-lucide="x"></i>';
      act.addEventListener('click', function (e) {
        e.stopPropagation();
        state.viewThread = 0;
        state.sel = [];
        // back to the exact spot of the normal list we left
        renderPosts(dlistScrollMem);
      });
    } else {
      act.className = 'gv-act more';
      act.title = '菜单';
      act.innerHTML = '<i data-lucide="more-horizontal"></i>';
      act.addEventListener('click', function (e) {
        e.stopPropagation();
        state.selThread = t.id;
        var b = act.getBoundingClientRect();
        openThreadCtx(b.left, b.bottom + 4);
      });
    }
    row.appendChild(act);
    row.addEventListener('click', function () {
      if (!manage) {
        dlistScrollMem = $('gv-dlist').scrollTop;
        state.viewThread = t.id;
        state.sel = [];
        renderPosts(0);
      }
    });
    row.addEventListener('contextmenu', function (e) {
      e.preventDefault();
      state.selThread = t.id;
      openThreadCtx(e.clientX, e.clientY);
    });
    return row;
  }

  var dlistScrollMem = 0; // normal-list scroll kept across the thread manage view

  // 下载时 posts:dirty 每 100ms 来一次，而每次重绘都会把整表拆了重建：
  // 鼠标按下时点中的那个 DOM 节点被换掉，click 事件就直接丢了（感觉"点了没反应"）。
  // 所以按下期间只记"待重绘"，等 click 派发完再补一次。
  var dlPress = { down: false, pending: false };
  document.addEventListener('mousedown', function (e) {
    if (e.target && e.target.closest && e.target.closest('#gv-dlist')) dlPress.down = true;
  }, true);
  document.addEventListener('mouseup', function () {
    if (!dlPress.down) return;
    // setTimeout(0)：让紧跟 mouseup 的 click 先派发，再重绘
    setTimeout(function () {
      dlPress.down = false;
      if (dlPress.pending) {
        dlPress.pending = false;
        renderPosts();
      }
    }, 0);
  });
  // 窗口失焦时 mouseup 可能收不到：别让列表一直卡在"按下中"
  window.addEventListener('blur', function () {
    if (!dlPress.down) return;
    dlPress.down = false;
    if (dlPress.pending) { dlPress.pending = false; renderPosts(); }
  });

  // 进度类更新不需要 10fps：把 posts:dirty 触发的重绘合并到 ~250ms 一次
  var postsRenderTimer = null;
  function schedulePostsRender() {
    if (postsRenderTimer) return;
    postsRenderTimer = setTimeout(function () {
      postsRenderTimer = null;
      renderPosts();
    }, 250);
  }

  // syncSortView 普通列表与主题管理视图各记一份排序：
  // 进入/退出主题管理时，把当前视图的排序存回它自己的槽位，再换成另一个
  // 视图上次用的那份。这样同一个表头在两个视图里可以保持不同的排序列。
  function syncSortView(manage) {
    var mode = manage ? 'th' : 'dl';
    if (state.sort) {
      if (state.sortView === 'th') state.threadSort = { col: state.sort.col, dir: state.sort.dir };
      else state.normalSort = { col: state.sort.col, dir: state.sort.dir };
    }
    state.sortView = mode;
    var src = mode === 'th' ? state.threadSort : state.normalSort;
    state.sort = { col: src.col, dir: src.dir };
  }

  // persistSort 把当前排序列/方向写回设置（两个视图各一份），重启后仍然生效。
  // 防抖 400ms，连点表头不会反复写库。
  var sortSaveTimer = null;
  function persistSort() {
    if (!state.sort) return;
    if (state.sortView === 'th') state.threadSort = { col: state.sort.col, dir: state.sort.dir };
    else state.normalSort = { col: state.sort.col, dir: state.sort.dir };
    if (sortSaveTimer) clearTimeout(sortSaveTimer);
    sortSaveTimer = setTimeout(function () {
      sortSaveTimer = null;
      // 先取最新设置再改：不要用启动时那份旧副本把窗口位置等字段覆盖回去
      App().LoadSettings().then(function (cur) {
        cur.sortCol = state.normalSort.col;
        cur.sortDir = state.normalSort.dir;
        cur.threadSortCol = state.threadSort.col;
        cur.threadSortDir = state.threadSort.dir;
        if (state.settings) {
          state.settings.sortCol = cur.sortCol;
          state.settings.sortDir = cur.sortDir;
          state.settings.threadSortCol = cur.threadSortCol;
          state.settings.threadSortDir = cur.threadSortDir;
        }
        return App().SaveSettings(cur);
      }).catch(function () {});
    }, 400);
  }

  // renderPosts 的 forceSort：表头点了排序时一定要真的重排（否则会被
  // "保持行位置"的逻辑吃掉，看起来像点了没反应）。
  function renderPosts(restoreScroll, forceSort) {
    if (dlPress.down) { dlPress.pending = true; return; }
    var list = $('gv-dlist');
    // rebuilding rows resets scrollTop — keep it unless the caller pins a
    // value (entering/leaving the thread manage view swaps the row set)
    var keep = typeof restoreScroll === 'number' ? restoreScroll : list.scrollTop;
    var rows = list.querySelectorAll('.gv-row');
    rows.forEach(function (r) { r.remove(); });
    var q = state.filter;
    var matchQ = function (s) { return !q || (s || '').toLowerCase().indexOf(q) !== -1; };
    // manage view header first; a stale viewThread (thread removed while
    // inside it) resets to the normal list BEFORE tlist is computed, or the
    // other thread rows would be filtered out by the dead id
    var manage = state.viewThread > 0;
    // 两个视图各用各的排序：进入/退出主题管理时在这里切换
    syncSortView(manage);
    if (manage) {
      var mt = state.threads.find(function (x) { return x.id === state.viewThread; });
      if (!mt) {
        // thread removed while inside its manage view — back to the normal
        // list at the spot we left, not the manage view's scroll offset
        state.viewThread = 0; manage = false;
        keep = dlistScrollMem;
        syncSortView(false); // 回到普通列表，排序也换回普通列表那份
      } else {
        list.appendChild(threadRow(mt, true));
        // 管理视图的楼层列表同样尊重表头排序：默认（未点击过表头，col=-1）
        // 按楼层号升序；Title/添加日期/Status 按对应字段和当前方向排；
        // 其余列对楼层无意义，退回楼层号。副键始终是楼层号，保证稳定。
        var mfl = state.posts.filter(function (p) { return p.threadId === mt.threadId; });
        var fcol = state.sort.col, fdir = state.sort.dir;
        mfl.sort(function (a, b) {
          var r = 0;
          if (fcol === 0) {
            // 楼层标题都带楼层号前缀（6F—xxx），字典序会排出 1F/10F/100F/2F 的
            // 乱序；按名称列排序时用楼层号做主键（自然顺序）、标题文本做副键
            r = (a.floorNo || 0) - (b.floorNo || 0);
            if (r === 0) {
              var ta = String(a.title || '').toLowerCase(), tb = String(b.title || '').toLowerCase();
              r = ta < tb ? -1 : (ta > tb ? 1 : 0);
            }
          } else if (fcol === 3) {
            // Status：分组规则与普通列表完全一致（见 statusRankOfPost）：
            // 已完成 / 下载中在未开始（ready）之前，错误夹在中间；
            // 同组内按完成度，最后用楼层号兜底。以前这里直接退回楼层号，
            // 所以管理视图里的状态排列和外面的列表对不上。
            var ra = statusRankOfPost(a), rb = statusRankOfPost(b);
            if (ra !== rb) {
              r = ra < rb ? -1 : 1;
            } else {
              var pa = progressOfPost(a), pb = progressOfPost(b);
              if (pa !== pb) r = pa < pb ? -1 : 1;
            }
          } else if (fcol === 4) {
            var da = String(a.createdAt || ''), db = String(b.createdAt || '');
            r = da < db ? -1 : (da > db ? 1 : 0);
          }
          if (r === 0) r = (a.floorNo || 0) - (b.floorNo || 0);
          return fdir === 1 ? r : -r;
        });
        var rowKeys = ['t' + mt.id], editRows = [list.lastChild];
        // 可见行序：shift 范围选择按屏幕上的顺序取。管理视图以前不设
        // visOrder，selectPost 会退回全局 state.posts 的顺序（跨主题），
        // 于是 shift 多选会选到别的主题的楼层甚至"没反应"。
        state.visOrder = mfl.map(function (p) { return p.id; });
        mfl.forEach(function (p) {
          list.appendChild(postRow(p, false));
          editRows.push(list.lastChild);
          rowKeys.push('p' + p.id);
        });
        // 连接管理视图里同样能批量勾选：连接行（黄色吸顶行）+ 它下面的楼层
        applyEditMode(list, editRows, rowKeys);
        finishDlist();
        list.scrollTop = keep;
        return;
      }
    }    var tlist = state.threads.filter(function (t) {
      return matchQ(t.title + ' ' + t.url);
    });
    var entries = [];
    // floors of threads shown as yellow rows live only inside the thread's
    // manage view — hide them from the normal list (plain posts, which have
    // no thread entry, still show)
    var threadForumIds = {};
    tlist.forEach(function (t) { threadForumIds[t.threadId] = true; });
    state.posts.forEach(function (p) {
      if (threadForumIds[p.threadId]) return;
      if (matchQ(p.title + ' ' + p.postUrl + ' ' + p.hosts)) entries.push({ kind: 'p', at: p.createdAt || '', p: p });
    });
    tlist.forEach(function (t) { entries.push({ kind: 't', at: (t.createdAt || '') + ' 23:59', th: t }); });
    var col = state.sort.col, dir = state.sort.dir;
    if (col === 0 || col === 4) { // title / date sort applies to both kinds
      // 标题按自然顺序排：楼层条目标题带 "6F—" 前缀，纯字典序会排出
      // 1F/10F/100F/2F 的乱序；双方都有楼层号时按数字比，否则按文本
      function titleFloorNo(s) { var m = /^(\d+)F/.exec(s || ''); return m ? +m[1] : null; }
      entries.sort(function (a, b) {
        var va = col === 0 ? ((a.kind === 't' ? a.th.title : a.p.title) || '').toLowerCase() : (a.at || '');
        var vb = col === 0 ? ((b.kind === 't' ? b.th.title : b.p.title) || '').toLowerCase() : (b.at || '');
        if (col === 0) {
          var fa = titleFloorNo(va), fb = titleFloorNo(vb);
          if (fa !== null && fb !== null && fa !== fb) return (fa - fb) * dir;
        }
        return va === vb ? 0 : (va < vb ? -dir : dir);
      });
    } else if (col === 1 || col === 2 || col === 3 || col === 5) {
      // per-column comparators; Thread rows (yellow) join per the user's rules:
      // Post 列 Thread 恒置顶（无楼层 ID）、Progress/Status 按汇总值混排、
      // Hosts 列无值的 Thread 恒在最后
      function progressValOf(en) {
        if (en.kind === 't') { var m = threadMeta(en.th); return m.joined > 0 ? m.done / m.joined : -1; }
        return progressOfPost(en.p);
      }
      // Status 主键：状态分组。刚开始下载（进度 0%）的也属于"进行中"，
      // 必须排在还没开始的（ready）之前——完成百分比区分不了这一点；
      // 下载中（哪怕 0%）也必须排在已暂停之前。Post 行与主题管理视图共用
      // statusRankOfPost，保证两处排列一致。
      function statusRankOf(en) {
        if (en.kind !== 't') return statusRankOfPost(en.p);
        var m2 = threadMeta(en.th);
        if (m2.joined === 0) return 0; // 未开始
        var errN = 0, dlN = 0, pausedN = 0;
        m2.floors.forEach(function (p) {
          if (p.status === 'error') errN++;
          else if (p.status === 'downloading') dlN++;
          else if (p.status === 'paused') pausedN++;
        });
        if (m2.done === m2.joined) return 4;        // 全部完成
        if (dlN > 0) return 3;                      // 有楼层在下载
        if (m2.done + errN === m2.joined) return 1; // 剩下的都死了
        if (pausedN > 0) return 2;                  // 已暂停
        return 0;
      }
      function sortVal(en) {
        if (col === 1) return en.kind === 't' ? null : (en.p.postId || 0);
        // Progress 列统一按图片总数排：Thread 行用已加入楼层的图片总数
        // （与进度条显示的分母一致，而不是 thread 表里整帖的 imgCount 统计），
        // Post 行用本楼层的图片总数
        if (col === 2) {
          if (en.kind === 't') {
            var mm = threadMeta(en.th);
            return mm.joined > 0 ? mm.sumTotal : (en.th.imgCount || 0);
          }
          return en.p.total || 0;
        }
        if (col === 3) return statusRankOf(en); // 状态分组
        return en.kind === 't' ? null : (en.p.hosts || '').toLowerCase();
      }
      entries.sort(function (a, b) {
        var ka = sortVal(a), kb = sortVal(b);
        var aNo = ka === null, bNo = kb === null;
        if (col === 1) { // Thread 恒在最上面
          if (aNo && bNo) return 0;
          if (aNo) return -1;
          if (bNo) return 1;
        } else if (col === 5) { // Thread 无图床信息，恒在最后
          if (aNo && bNo) return 0;
          if (aNo) return 1;
          if (bNo) return -1;
        }
        if (ka === kb) {
          // Status 组内副键：完成度高在前（首次点击方向下组内近似降序）
          if (col === 3) {
            var va = progressValOf(a), vb = progressValOf(b);
            if (va !== vb) return va < vb ? dir : -dir;
          }
          return 0;
        }
        return ka < kb ? -dir : dir;
      });
    } else {
      entries.sort(function (a, b) { return a.at === b.at ? 0 : (a.at < b.at ? 1 : -1); }); // newest first
    }
    // 下载中进度每秒变好几次，跟着实时进度重排会让行在鼠标底下乱跳，
    // 点下去可能正好落到已经换走的条目上。这里保留上一次的行序：只要排序列/
    // 方向/过滤没变、行集合也没变，就沿用旧顺序（内容照常刷新）；用户点表头
    // （forceSort）或增删了行才真正重排。
    var orderKey = function (en) { return en.kind === 't' ? 't' + en.th.id : 'p' + en.p.id; };
    var sig = col + '|' + dir + '|' + (q || '') + '|' + (manage ? 'm' : 'n');
    // 只有"正在下载"时才冻结行序；平时（改名、状态跃迁这些一次性变化）
    // 该重排就重排，行为保持不变。
    var busy = !!(state.running && Object.keys(state.running).length);
    if (busy && !forceSort && state.dlOrderSig === sig && state.dlOrderIds &&
        state.dlOrderIds.length === entries.length) {
      var prevIdx = {};
      state.dlOrderIds.forEach(function (k, i) { prevIdx[k] = i; });
      var sameSet = entries.every(function (en) { return prevIdx[orderKey(en)] !== undefined; });
      if (sameSet) {
        entries.sort(function (a, b) { return prevIdx[orderKey(a)] - prevIdx[orderKey(b)]; });
      } else {
        state.dlOrderIds = entries.map(orderKey);
      }
    } else {
      state.dlOrderIds = entries.map(orderKey);
    }
    state.dlOrderSig = sig;
    // 可见顺序（当前排序渲染出的行序）：shift 范围选择必须跟着这个走，
    // 否则非默认排序时选中的范围和高亮对不上
    state.visOrder = [];
    state.visKeys = []; // 全部可见行（含 Thread），编辑模式 shift 范围勾选用
    var rowKeys = [], editRows = [];
    entries.forEach(function (en) {
      var isT = en.kind === 't';
      if (isT) list.appendChild(threadRow(en.th, false));
      else { list.appendChild(postRow(en.p, true)); state.visOrder.push(en.p.id); }
      rowKeys.push(isT ? 't' + en.th.id : 'p' + en.p.id);
      editRows.push(list.lastChild);
    });
    // 编辑模式：表头全选 + 每行勾选框（连接行与楼层行一视同仁）
    applyEditMode(list, editRows, rowKeys);
    finishDlist();
    list.scrollTop = keep;
  }

  function postRow(p, interactive) {
    var st = statusOf(p);
    var row = document.createElement('div');
    // 编辑模式的行高亮 = 勾选状态（点行即勾选）；普通模式 = 选中集
    var hl = state.editMode ? state.editSel.indexOf('p' + p.id) >= 0 : state.sel.indexOf(p.id) >= 0;
    row.className = 'gv-row' + (hl ? ' sel' : '');
    row.setAttribute('data-id', p.id);
    if (p.mark) row.setAttribute('data-mark', p.mark); // 用户标记颜色
    row.innerHTML =
      '<span class="gv-name">' + esc(p.title) + '</span>' +
      '<span class="gv-cnt">楼层 #' + p.postId + '</span>' +
      '<span class="gv-cnt">' + p.done + ' / ' + p.failed + ' / ' + p.total + '</span>' +
      '<span class="gv-state' + ((p.status === 'downloading' || (state.running && state.running[p.id])) ? ' gv-dl' : '') + '"><i style="width:' + st.w + '%"></i><span>' + esc(st.t) + '</span></span>' +
      '<span class="gv-cnt">' + esc((p.createdAt || '').slice(0, 16)) + '</span>' +
      '<span class="gv-cnt" style="white-space:nowrap;overflow:hidden;text-overflow:ellipsis">' + esc(p.hosts) + '</span>' +
      '<span></span>';
    var more = document.createElement('button');
    more.className = 'gv-act more';
    more.title = '菜单';
    more.innerHTML = '<i data-lucide="more-horizontal"></i>';
    more.addEventListener('click', function (e) {
      e.stopPropagation();
      var b = more.getBoundingClientRect();
      openCtx(b.left, b.bottom + 4, p.id);
    });
    row.appendChild(more);
    row.addEventListener('click', function (e) {
      if (state.editMode) { editRowClick('p' + p.id, e); return; }
      selectPost(p.id, e.ctrlKey, e.shiftKey);
    });
    row.addEventListener('dblclick', function () { openFloorsForPost(p); });
    row.addEventListener('contextmenu', function (e) {
      e.preventDefault();
      if (state.sel.indexOf(p.id) < 0) selectPost(p.id, false);
      // 记住"右键的是哪一行"：查看楼层等单条菜单项以它为准，
      // 不跟着多选数量变灰（以前多选后这一项会被禁用）
      openCtx(e.clientX, e.clientY, p.id);
    });
    return row;
  }

  function finishDlist() {
    icons();
    // header sort indicators
    var hrow = $('gv-dlist').querySelector('.gv-hrow');
    if (hrow) {
      hrow.querySelectorAll('.gv-sort').forEach(function (i) { i.remove(); });
      if (state.sort.col >= 0) {
        var hc = hrow.querySelector('span[data-col="' + state.sort.col + '"]');
        if (hc) {
          var arrow = document.createElement('i');
          arrow.className = 'gv-sort';
          arrow.textContent = state.sort.dir === 1 ? '▲' : '▼';
          hc.appendChild(arrow);
        }
      }
    }
    renderRunBtn();
    if (colApplies.dl) colApplies.dl();
  }

  var selAnchor = -1; // shift-range anchor
  function selectPost(id, ctrl, shift) {
    // shift 范围按屏幕上当前排序的可见行序取，不能用 posts 原始数组序
    var ids = state.visOrder && state.visOrder.length ? state.visOrder : state.posts.map(function (p) { return p.id; });
    if (shift && selAnchor >= 0) {
      var a = ids.indexOf(selAnchor), b = ids.indexOf(id);
      if (a >= 0 && b >= 0) {
        // range select; ctrl+shift unions with the existing selection
        var range = ids.slice(Math.min(a, b), Math.max(a, b) + 1);
        if (ctrl) range.forEach(function (rid) { if (state.sel.indexOf(rid) < 0) state.sel.push(rid); });
        else state.sel = range;
      } else {
        // 锚点不在当前视图里（刚切进主题管理视图、或换过排序/过滤）：
        // 不能什么都不做，否则 shift 点下去像"没反应"——退回普通单击
        state.sel = [id];
        selAnchor = id;
      }
    } else if (ctrl) {
      var i = state.sel.indexOf(id);
      if (i >= 0) state.sel.splice(i, 1); else state.sel.push(id);
      selAnchor = id;
    } else {
      state.sel = [id];
      selAnchor = id;
    }
    state.posts.forEach(function (p) { if (p.id === id && !ctrl && !shift) state.curPost = id; });
    if (!ctrl && !shift && state.sel.length) state.curPost = state.sel[0];
    renderPosts();
    showInfoPanel();
  }

  function selPosts() {
    return state.posts.filter(function (p) { return state.sel.indexOf(p.id) >= 0; });
  }

  // ---------- Log ----------
  function addLogLine(level, msg) {
    var list = $('gv-loglist');
    var row = document.createElement('div');
    row.className = 'gv-logrow';
    row.innerHTML = '<span class="gv-t">' + new Date().toTimeString().slice(0, 8) + '</span>' +
      '<span class="gv-lvl-' + (level === 'ERROR' ? 'e' : level === 'WARN' ? 'w' : 'i') + '">' + esc(level.toUpperCase()) + '</span>' +
      '<span>' + esc(msg) + '</span>';
    list.insertBefore(row, list.firstChild);
    while (list.children.length > 500) list.removeChild(list.lastChild);
  }

  // ---------- Photo list / info strip ----------
  function showInfoPanel() {
    var p = state.posts.find(function (x) { return x.id === state.curPost; });
    var bar = $('gv-ondisk');
    if (!p) {
      if (bar) bar.textContent = '选中一个条目查看磁盘统计';
      $('gv-pho').innerHTML = '';
      return;
    }
    if (App().PromotePostRecover) App().PromotePostRecover(p.id);
    // 用户明确选中了帖子：右侧面板从此只看这个帖子，不再跟随下载
    state.followPost = 0;
    if (bar) bar.textContent = '磁盘：统计中…';
    App().DiskStats(p.id).then(function (ds) {
      if (bar && ds) bar.textContent = '磁盘：' + ds.count + ' 个文件 · ' + fmtSize(ds.bytes || 0);
    }).catch(function () {});
    // 打开帖子：先拉一次快照并把右侧预览自动填上（不点也能看到图）
    refreshImages(p.id, { force: true });
  }
  function fi(k, v, half) {
    return '<div class="gv-fi' + (half ? ' half' : '') + '"><span class="gv-k">' + esc(k) + '</span><span class="gv-v">' + esc(v) + '</span></div>';
  }

  var RETRY_SHOW_MS = 2000; // 重试标记的最短展示时间：秒失败时也能看清“重试中”

  function stTxtOf(im) {
    var t = state.retrying[im.id];
    if (t) {
      // 排队中（pending）就是正在重试；秒失败回到 error 的，短时间里仍显示
      // “重试中”，否则界面上只是一闪而过，看着像点了没反应
      if (im.state === 'pending' || (im.state === 'error' && Date.now() - t < RETRY_SHOW_MS)) {
        return '<span class="gv-st2" style="color:var(--gv-accent)">重试中</span>';
      }
      delete state.retrying[im.id];
    }
    switch (im.state) {
      case 'done': return '<span class="gv-st2" style="color:var(--gv-ok)">done</span>';
      case 'error': return '<span class="gv-st2" style="color:var(--gv-err)">error</span>';
      case 'downloading': return '<span class="gv-st2" style="color:var(--gv-accent)">downloading</span>';
      default: return '<span class="gv-st2" style="color:var(--gv-fg2)">pending</span>';
    }
  }

  // markRetry 把这次要重试的图片先记下来（必须是 RetryFailed 之前的状态，
  // 否则拉到的已经是 pending 了），再真的去重试。
  function markRetry(pid) {
    return App().ListImages(pid).then(function (imgs) {
      var n = 0;
      (imgs || []).forEach(function (im) {
        if (im.state === 'error') { state.retrying[im.id] = Date.now(); n++; }
      });
      if (!n) return null; // 这张专辑没有失败图片，点了也没意义
      return App().RetryFailed(pid).then(function () {
        reloadPhotos(pid, true);
        [700, 1500, 2400].forEach(function (ms) {
          setTimeout(function () { renderPhotoList(true); }, ms);
        });
      });
    });
  }

  // reloadPhotos 拉一次当前选中帖子的图片列表并立即重绘（force 跳过节流）。
  function reloadPhotos(pid, force) {
    return refreshImages(pid, { force: force });
  }

  // ---------- 下载中的快照刷新 / 右侧预览自动切换 ----------
  // images:dirty 只带图片 id，前端按当前帖子重拉一次快照。下载时这个事件
  // 最多每 100ms 一次，这里串行化（同一时刻只有一个请求在飞）：以前每次都
  // 直接并发请求，先发的慢请求后返回时会把新快照覆盖成旧快照，于是右侧
  // 预览"有时切、有时不切"。期间再来事件就补跑一轮，不丢更新。
  // opts.silent：只建立基线（打开帖子时的首次加载），不触发自动切换预览；
  // opts.force：重绘图片列表时跳过节流。
  var imgPoll = { busy: false, again: false };

  function refreshImages(postId, opts) {
    if (!postId) return Promise.resolve();
    var o = opts || {};
    if (imgPoll.busy) { imgPoll.again = true; return Promise.resolve(); }
    imgPoll.busy = true;
    return App().ListImages(postId).then(function (imgs) {
      imgPoll.busy = false;
      // 选中的帖子优先；没选中任何帖子时，跟随正在下载的那个帖子
      if (postId === state.curPost || (!state.curPost && postId === state.followPost)) {
        applyImageSnapshot(postId, imgs || [], o);
      }
      if (imgPoll.again) {
        imgPoll.again = false;
        refreshImages(state.curPost || state.followPost); // 补跑的一轮按"实时"处理
      }
    }, function () {
      // 失败就等下一次事件，不要在这里自旋
      imgPoll.busy = false;
      imgPoll.again = false;
    });
  }

  // applyImageSnapshot 用新快照替换图片缓存，并把这一轮"刚从别的状态变成
  // done"的图推到右侧预览。多线程下载时完成顺序和 seq 顺序不一致，用
  // "列表里最后一张 done"判断会漏掉先下完的高序号图（那正是有时不切的原因），
  // 所以必须看状态跃迁。
  // 没有新完成的图时，如果右侧面板还空着（或停在上一个帖子的图），就自动
  // 挑一张显示——不点也能看到内容。
  function applyImageSnapshot(postId, imgs, opts) {
    var o = opts || {};
    var prev = {};
    (state.images[postId] || []).forEach(function (i) { prev[i.id] = i.state; });
    state.images[postId] = imgs;
    if (postId === state.curPost && $('gv-pho').style.display !== 'none') renderPhotoList(o.force);
    if (app.getAttribute('data-prev') !== 'on' || !$('gv-bigbox')) return;
    var fresh = imgs.filter(function (i) { return i.state === 'done' && prev[i.id] !== 'done'; });
    var latest = fresh.length ? fresh[fresh.length - 1] : null;
    if (latest) {
      if (latest.id !== window.__lastLiveImg) {
        window.__lastLiveImg = latest.id;
        showLivePreview(latest);
      }
      return;
    }
    autoPickPreview(postId, imgs);
  }

  // showLivePreview 自动切换右侧预览（受 1 秒间隔限制）。
  // 下载很快时画面会疯狂闪，这里把间隔内完成的图合并成"最后一张"再显示。
  var LIVE_SWITCH_MS = 1000;
  var liveSwitch = { at: 0, timer: null, pending: null };

  function showLivePreview(im) {
    state.previewImgId = im.id;
    state.previewPost = state.curPost || state.followPost;
    var wait = LIVE_SWITCH_MS - (Date.now() - liveSwitch.at);
    if (wait <= 0) {
      liveSwitch.at = Date.now();
      liveSwitch.pending = null;
      showPreviewImage(im);
      return;
    }
    // 间隔内只保留最新的一张，等时间到了再切（不会排队一张张补播）
    liveSwitch.pending = im;
    if (!liveSwitch.timer) {
      liveSwitch.timer = setTimeout(function () {
        liveSwitch.timer = null;
        var p = liveSwitch.pending;
        liveSwitch.pending = null;
        if (!p || state.previewImgId !== p.id) return; // 期间用户自己点过别的图
        liveSwitch.at = Date.now();
        showPreviewImage(p);
      }, wait);
    }
  }

  // cancelLiveSwitch 取消还没执行的延迟切换（用户手动点图 / 清空面板时用）
  function cancelLiveSwitch() {
    if (liveSwitch.timer) { clearTimeout(liveSwitch.timer); liveSwitch.timer = null; }
    liveSwitch.pending = null;
  }

  // autoPickPreview 右侧面板没东西可看时自动挑一张：优先最后一张已完成的，
  // 没有就挑正在下载的。用户手动点过本帖子的某张图就保持不动。
  function autoPickPreview(postId, imgs) {
    var kept = state.previewPost === postId && state.previewImgId &&
      imgs.some(function (i) { return i.id === state.previewImgId; });
    if (kept) return false;
    var pick = null;
    for (var i = imgs.length - 1; i >= 0; i--) {
      if (imgs[i].state === 'done') { pick = imgs[i]; break; }
    }
    if (!pick) {
      pick = imgs.find(function (x) { return x.state === 'downloading'; }) || null;
    }
    if (!pick) {
      // 换到一个没有图的帖子：别把上一个帖子的图留在面板上
      if (state.previewPost !== postId) clearPreview();
      return false;
    }
    state.previewPost = postId;
    state.previewImgId = pick.id;
    // 只有已完成图的 id 才记进 __lastLiveImg：正在下载的那张等它下完后
    // 还要再刷一次（缩略图 → 磁盘原图）
    window.__lastLiveImg = pick.state === 'done' ? pick.id : 0;
    showPreviewImage(pick);
    return true;
  }

  // clearPreview 清空右侧预览（没有可显示的图时用）
  function clearPreview() {
    var box = $('gv-bigbox');
    var el = box.querySelector('img');
    if (el) el.remove();
    var ph = $('gv-bigph');
    if (ph) ph.style.display = '';
    var cap = $('gv-bigcap');
    if (cap) cap.style.display = 'none';
    var badge = $('gv-bigbadge');
    if (badge) badge.style.display = 'none';
    var info = $('gv-imginfo');
    if (info) info.innerHTML = '';
    state.previewPost = 0;
    state.previewImgId = 0;
    // 取消还没执行的延迟切换，免得清空后又蹦出一张
    cancelLiveSwitch();
  }

  var progMap = {};

  // 单图下载进度：只更新对应行的细进度条和百分比，不整表重绘
  function applyProgress(list) {
    var followPost = 0;
    (list || []).forEach(function (p) {
      progMap[p.id] = p;
      if (!followPost && p.postRowId) followPost = p.postRowId;
      var row = document.querySelector('.gv-prow[data-imgid="' + p.id + '"]');
      if (!row) return;
      var bar = row.querySelector('.gv-pfill');
      var label = row.querySelector('.gv-pct');
      var pct = p.total > 0 ? Math.min(100, Math.round(p.done / p.total * 100)) : 0;
      if (bar) bar.style.width = pct + '%';
      if (label) label.textContent = p.total > 0 ? (pct + '%') : fmtSize(p.done);
    });
    // 没有选中帖子时，右侧信息栏自动跟随正在下载的帖子。
    // 同时下多个帖子时 img:progress 每批的顺序是随机的，跟着"本批第一个"
    // 会让跟随目标每 100ms 换一次、面板疯狂闪；所以只有当前跟的帖子
    // 已经不在下载了，才允许换目标。
    if (!state.curPost && followPost && followPost !== state.followPost) {
      if (!state.followPost || !state.running[state.followPost]) {
        state.followPost = followPost;
        refreshImages(followPost);
      }
    }
  }

  // ---------- 图片列表：表头排序 ----------
  // 列 0 缩略图（不可排）、1 序号、2 图片名、3 页面链接、4 大小、5 下载进度、6 状态
  var photoSort = (function () {
    try {
      var v = JSON.parse(localStorage.getItem('gvs-pho'));
      if (v && typeof v.col === 'number' && (v.dir === 1 || v.dir === -1)) return v;
    } catch (e) {}
    return { col: 1, dir: 1 }; // 默认按 # 升序（和原来看起来一致）
  })();

  function photoPct(im) {
    if (im.state === 'done') return 100;
    var pr = progMap[im.id];
    if (im.state === 'downloading' && pr && pr.total > 0) return Math.round(pr.done / pr.total * 100);
    return -1; // 未知/未开始，排在已知进度之后
  }

  function photoSortVal(im, col) {
    switch (col) {
      case 1: return im.seq || 0;
      case 2: return (im.originalName || ('' + (im.seq || 0))).toLowerCase();
      case 3: return (im.pageUrl || '').toLowerCase();
      case 4: return im.fileSize || 0;
      case 5: return photoPct(im);
      case 6: {
        var s = im.state;
        if (s === 'done') return 3;
        if (s === 'downloading') return 2;
        if (s === 'pending') return 1;
        return 0; // error
      }
      default: return im.seq || 0;
    }
  }

  function sortPhotoImgs(imgs) {
    var col = photoSort.col, dir = photoSort.dir;
    var arr = imgs.slice();
    arr.sort(function (a, b) {
      var va = photoSortVal(a, col), vb = photoSortVal(b, col);
      var r;
      if (typeof va === 'string' || typeof vb === 'string') {
        r = String(va) < String(vb) ? -1 : (String(va) > String(vb) ? 1 : 0);
      } else {
        r = va - vb;
      }
      if (r === 0) r = (a.seq || 0) - (b.seq || 0); // 同值时按序号，保证稳定
      return dir === 1 ? r : -r;
    });
    return arr;
  }

  function setPhotoSort(col) {
    if (photoSort.col === col) {
      photoSort.dir = -photoSort.dir;
    } else {
      photoSort.col = col;
      // 大小/进度/状态：第一次点先给"大的在前"，其余列先给升序
      photoSort.dir = (col === 4 || col === 5 || col === 6) ? -1 : 1;
    }
    try { localStorage.setItem('gvs-pho', JSON.stringify(photoSort)); } catch (e) {}
    photoOrderForce = true; // 用户明确点了表头，这次必须真的重排
    renderPhotoList(true);
  }

  // 行序稳定：下载中进度/状态每 300ms 就变，如果每次都按实时值重排，
  // 行会一直上下乱跳（点了也点不中）。只有"图片集合变了"或"用户点表头"才重排，
  // 其它时候沿用上一次的行序，和下载列表的处理方式一致。
  var photoOrder = [];
  var photoSetSig = '';
  var photoOrderForce = false;

  function arrangePhotoRows(imgs) {
    var sorted = sortPhotoImgs(imgs);
    var sig = sorted.map(function (im) { return im.id; }).sort(function (a, b) { return a - b; }).join(',');
    if (photoOrderForce || !photoOrder.length || sig !== photoSetSig) {
      photoOrderForce = false;
      photoSetSig = sig;
      photoOrder = sorted.map(function (im) { return im.id; });
      return sorted;
    }
    var byId = {};
    sorted.forEach(function (im) { byId[im.id] = im; });
    var out = [];
    photoOrder.forEach(function (id) { if (byId[id]) out.push(byId[id]); });
    if (out.length !== sorted.length) return sorted; // 兜底：集合对不上时重新排
    return out;
  }

  // 表头的排序箭头（▲/▼），跟下载列表用同一套样式
  function updatePhotoSortMark() {
    var head = document.querySelector('.gv-phead');
    if (!head) return;
    head.querySelectorAll('.gv-sort').forEach(function (i) { i.remove(); });
    if (photoSort.col < 0) return;
    var cell = head.querySelector('span[data-col="' + photoSort.col + '"]');
    if (!cell) return;
    var arrow = document.createElement('i');
    arrow.className = 'gv-sort';
    arrow.textContent = photoSort.dir === 1 ? '▲' : '▼';
    cell.appendChild(arrow);
  }

  var lastPhoRender = 0;
  function renderPhotoList(force) {
    var now = Date.now();
    if (!force && now - lastPhoRender < 300) return; // images:dirty fires up to 10x/s while downloading
    lastPhoRender = now;
    var p = state.posts.find(function (x) { return x.id === state.curPost; });
    var box = $('gv-pho');
    var imgs = state.images[p ? p.id : 0] || [];
    // 进度表只保留当前帖子里的图片，避免越攒越多
    if (imgs.length) {
      var keep = {};
      imgs.forEach(function (i) { keep[i.id] = 1; });
      Object.keys(progMap).forEach(function (k) { if (!keep[k]) delete progMap[k]; });
    }
    box.innerHTML = arrangePhotoRows(imgs).map(function (im) {
      var name = im.originalName || ('#' + im.seq);
      var pr = progMap[im.id];
      var pct = 0, pctTxt = '—';
      if (im.state === 'done') {
        pct = 100; pctTxt = '100%';
      } else if (im.state === 'downloading' && pr) {
        pct = pr.total > 0 ? Math.min(100, Math.round(pr.done / pr.total * 100)) : 0;
        pctTxt = pr.total > 0 ? pct + '%' : fmtSize(pr.done);
      } else if (im.state === 'error') {
        pctTxt = '—';
      }
      return '<div class="gv-prow" data-imgid="' + im.id + '">' +
        '<div class="gv-th2" style="background-image:url(\'/thumbs/' + im.id + '\')"></div>' +
        '<span class="gv-cnt">' + im.seq + '</span>' +
        '<span class="gv-fn">' + esc(name) + '</span>' +
        '<span class="gv-url" title="' + esc(im.pageUrl || '') + '">' + esc(im.pageUrl || '') + '</span>' +
        '<span class="gv-cnt">' + (im.fileSize > 0 ? fmtSize(im.fileSize) : '—') + '</span>' +
        '<span class="gv-pw"><span class="gv-ptrack"><span class="gv-pfill" style="width:' + pct + '%"></span></span>' +
        '<span class="gv-pct">' + pctTxt + '</span></span>' +
        stTxtOf(im) + '<span></span></div>';
    }).join('');
    box.querySelectorAll('.gv-prow').forEach(function (r) {
      r.addEventListener('click', function () {
        box.querySelectorAll('.gv-prow').forEach(function (x) { x.classList.remove('on'); });
        r.classList.add('on');
        var id = +r.getAttribute('data-imgid');
        var img = imgs.find(function (i) { return i.id === id; });
        if (img) { cancelLiveSwitch(); showPreviewImage(img); }
      });
      r.addEventListener('contextmenu', function (e) {
        e.preventDefault();
        var id = +r.getAttribute('data-imgid');
        var img = null;
        for (var i = 0; i < imgs.length; i++) { if (imgs[i].id === id) { img = imgs[i]; break; } }
        if (img) openPicCtx(e.clientX, e.clientY, img);
      });
    });
    // 行是整批重绘的：列宽/列序要在新行上重新套一遍，排序箭头也要跟着更新
    if (colApplies['pho']) colApplies['pho']();
    updatePhotoSortMark();
  }

  // ---------- photo row context menu ----------
  var picTarget = null;
  function openPicCtx(x, y, im) {
    picTarget = im;
    var ctx = $('gv-pictx');
    ctx.classList.add('open');
    ctx.style.left = Math.min(x, window.innerWidth - 190) + 'px';
    ctx.style.top = Math.min(y, window.innerHeight - 150) + 'px';
    ctx.querySelectorAll('button').forEach(function (b) {
      var act = b.getAttribute('data-act');
      b.disabled = act.indexOf('direct') >= 0 && !im.directUrl;
    });
  }
  function closePicCtx() { $('gv-pictx').classList.remove('open'); }
  $('gv-pictx').addEventListener('click', function (e) {
    var b = e.target.closest('button');
    if (!b || b.disabled || !picTarget) return;
    var act = b.getAttribute('data-act');
    var url = act.indexOf('page') >= 0 ? picTarget.pageUrl : picTarget.directUrl;
    closePicCtx();
    if (!url) return;
    if (act.indexOf('copy') === 0) {
      App().WriteClipboard(url).then(function () { toast('已复制：' + url); })
        .catch(function (err) { toast('复制失败：' + err); });
    } else {
      App().OpenBrowser(url);
    }
  });
  document.addEventListener('click', function (e) { if (!e.target.closest('#gv-pictx')) closePicCtx(); });

  function fmtSize(n) {
    if (n > 1048576) return (n / 1048576).toFixed(1) + ' MB';
    if (n > 1024) return (n / 1024).toFixed(0) + ' KB';
    return n + ' B';
  }

  // ---------- preview panel ----------
  function showPreviewImage(im) {
    var box = $('gv-bigbox');
    var ph = $('gv-bigph');
    // 记住当前显示的是哪个帖子的哪张图：自动挑选逻辑靠它判断面板是否还空着
    state.previewPost = state.curPost || state.followPost;
    state.previewImgId = im.id;
    // finished images are served straight from disk; otherwise fall back
    // to the cached thumbnail endpoint, then the direct URL
    var url = im.state === 'done' && im.filePath ? '/local/' + im.id : (im.directUrl || ('/thumbs/' + im.id));
    var existing = box.querySelector('img');
    if (existing) existing.remove();
    if (url) {
      var el = document.createElement('img');
      el.src = url;
      el.draggable = false; // 浏览器原生图片拖拽会干扰点击放大
      el.onerror = function () { el.remove(); ph.style.display = ''; };
      ph.style.display = 'none';
      box.insertBefore(el, box.firstChild);
    } else {
      ph.style.display = '';
    }
    var cap = $('gv-bigcap');
    cap.style.display = '';
    cap.textContent = (im.originalName || '#' + im.seq) + (im.fileSize > 0 ? ' · ' + fmtSize(im.fileSize) : '') + (im.host ? ' · ' + im.host : '');
    var badge = $('gv-bigbadge');
    if (badge) {
      // only surface states that need attention; done/pending are obvious
      // from the photo itself and the info panel
      if (im.state === 'done' || im.state === 'pending') {
        badge.style.display = 'none';
      } else {
        badge.style.display = '';
        badge.textContent = im.state;
        badge.className = 'gv-badge ' + (im.state === 'error' ? 'err' : 'dl');
      }
    }
    $('gv-imginfo').innerHTML =
      '<div class="gv-mtitle">' + esc(im.originalName || '#' + im.seq) + '</div>' +
      '<div class="gv-mgrid">' +
      '<div class="gv-mi"><span>大小</span><b>' + (im.fileSize > 0 ? fmtSize(im.fileSize) : '—') + '</b></div>' +
      '<div class="gv-mi"><span>图床</span><b>' + esc(im.host || '—') + '</b></div>' +
      '<div class="gv-mi"><span>状态</span><b style="color:var(--gv-' + (im.state === 'done' ? 'ok' : im.state === 'error' ? 'err' : 'fg2') + ')">' + esc(im.state) + '</b></div>' +
      '<div class="gv-mi"><span>序号</span><b>#' + im.seq + '</b></div>' +
      '<div class="gv-mi wide"><span>保存到</span><b>' + esc(im.filePath || '尚未下载') + '</b></div>' +
      '</div>';
  }

  // ---------- fullscreen lightbox (wheel zoom anchored at cursor + drag pan) ----------
  var lb = { open: false, scale: 1, tx: 0, ty: 0, drag: null };
  function lbApply() {
    $('gv-lbimg').style.transform = 'translate(' + lb.tx + 'px,' + lb.ty + 'px) scale(' + lb.scale + ')';
  }
  function lbReset() {
    lb.scale = 1; lb.tx = 0; lb.ty = 0;
    lbApply();
  }
  function openLightbox(src) {
    var box = $('gv-lightbox'), img = $('gv-lbimg');
    img.src = src;
    box.classList.add('open');
    lb.open = true;
    lbReset();
  }
  function closeLightbox() {
    $('gv-lightbox').classList.remove('open');
    lb.open = false;
    lb.drag = null;
  }
  function bindLightbox() {
    var box = $('gv-lightbox'), img = $('gv-lbimg');
    // open from the preview panel: click on the actual image element
    $('gv-bigbox').addEventListener('click', function (e) {
      if (e.target.tagName === 'IMG' && e.target.src) {
        e.stopPropagation();
        openLightbox(e.target.src);
      }
    });
    // wheel: zoom keeping the point under the cursor stationary.
    // screen point m maps to image-local p = (m - t) / s; after scaling to
    // s' the same p must stay under m, so t' = m - p * s'.
    box.addEventListener('wheel', function (e) {
      if (!lb.open) return;
      e.preventDefault();
      var factor = e.deltaY < 0 ? 1.2 : 1 / 1.2;
      var ns = Math.min(12, Math.max(0.1, lb.scale * factor));
      var mx = e.clientX, my = e.clientY;
      // the img is flex-centered in the overlay, so its layout origin sits at
      // (rect.left - tx, rect.top - ty) on screen — the anchor math must use
      // coords relative to that origin or the image drifts as you zoom
      var r = img.getBoundingClientRect();
      var ox = r.left - lb.tx, oy = r.top - lb.ty;
      lb.tx = (mx - ox) - (mx - ox - lb.tx) * (ns / lb.scale);
      lb.ty = (my - oy) - (my - oy - lb.ty) * (ns / lb.scale);
      lb.scale = ns;
      if (lb.scale === 1) lbReset();
      else lbApply();
    }, { passive: false });
    // drag to pan
    img.addEventListener('mousedown', function (e) {
      if (e.button !== 0) return;
      e.preventDefault();
      lb.drag = { x: e.clientX - lb.tx, y: e.clientY - lb.ty };
      box.classList.add('dragging');
    });
    window.addEventListener('mousemove', function (e) {
      if (!lb.drag) return;
      lb.tx = e.clientX - lb.drag.x;
      lb.ty = e.clientY - lb.drag.y;
      lbApply();
    });
    window.addEventListener('mouseup', function () {
      if (!lb.drag) return;
      lb.drag = null;
      box.classList.remove('dragging');
    });
    // double-click toggles between fit and 2x (anchored at the cursor)
    img.addEventListener('dblclick', function (e) {
      e.preventDefault();
      if (lb.scale !== 1) { lbReset(); return; }
      var ns = 2;
      var r = img.getBoundingClientRect();
      var ox = r.left - lb.tx, oy = r.top - lb.ty;
      lb.tx = (e.clientX - ox) - (e.clientX - ox - lb.tx) * (ns / lb.scale);
      lb.ty = (e.clientY - oy) - (e.clientY - oy - lb.ty) * (ns / lb.scale);
      lb.scale = ns;
      lbApply();
    });
    box.addEventListener('click', function (e) { if (e.target === box) closeLightbox(); });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && lb.open) closeLightbox();
    });
  }

  // ---------- context menu ----------
  function openCtx(x, y, postId) {
    var ctx = $('gv-ctx');
    ctx.classList.add('open');
    ctx.removeAttribute('data-thread');
    ctx.removeAttribute('data-thread-mode');
    // 右键/点“更多”的那一行：单条菜单项（查看楼层、重命名…）以它为目标，
    // 这样即使同时勾选了多行，针对当前行的那一项也不会被禁用
    state.ctxPost = postId || 0;
    ctx.style.left = Math.min(x, window.innerWidth - 320) + 'px';
    ctx.style.top = Math.min(y, window.innerHeight - 260) + 'px';
    // 恢复全部按钮：线程右键（openThreadCtx）会按白名单隐藏一批按钮，
    // 菜单是同一个 DOM，不恢复的话帖子右键里它们仍然是隐藏状态
    ctx.querySelectorAll('button[data-act]').forEach(function (b) {
      if (b.closest('.gv-sub')) return;
      b.style.display = '';
    });
    ctx.querySelectorAll('.gv-msep').forEach(function (sp) { sp.style.display = ''; });
    // 帖子右键：删除的是这条下载连接（含它的图片记录）
    var delBtn = $('gv-ctxdel');
    if (delBtn) {
      delBtn.innerHTML = '<i data-lucide="trash-2"></i>删除条目';
      delBtn.title = '删除选中的条目';
    }
    var detailBtn = ctx.querySelector('[data-act="detail"]');
    var n = state.sel.length;
    var detailTarget = state.ctxPost
      ? state.posts.find(function (x) { return x.id === state.ctxPost; })
      : (n === 1 ? selPosts()[0] : null);
    // 只要有一个明确的目标行就可用：主题行不存在的情况由 openFloorsForPost
    // 自己查一次并给出提示，不再把菜单项灰掉（灰掉时用户只会觉得"点了没反应"）
    detailBtn.disabled = !detailTarget;
    detailBtn.title = detailTarget ? '' : '先选中一个条目';
    // 已在下载的条目"开始下载"无意义，没在下载的"停止"也无意义
    var sel = selPosts();
    var runningCnt = sel.filter(function (p) { return state.running[p.id]; }).length;
    var dlBtn = ctx.querySelector('[data-act="dl"]');
    var pauseBtn = ctx.querySelector('[data-act="pause"]');
    dlBtn.disabled = n > 0 && runningCnt === sel.length;
    dlBtn.title = dlBtn.disabled ? '选中条目都已在下载中' : '';
    pauseBtn.disabled = n > 0 && runningCnt === 0;
    pauseBtn.title = pauseBtn.disabled ? '选中条目没有正在下载的' : '';
    // 暂停中的条目重新开始 = 继续下载（从已有进度接着来）
    var pausedCnt = sel.filter(function (p) { return p.status === 'paused'; }).length;
    var resumed = n > 0 && pausedCnt === sel.length;
    dlBtn.innerHTML = '<i data-lucide="play"></i>' + (resumed ? '继续下载' : '开始下载');
    icons();
  }
  function closeCtx() { $('gv-ctx').classList.remove('open'); }

  function openThreadCtx(x, y) {
    // reuse the same menu; thread actions are detail(=选择楼层) / folder / open / copy / del
    var ctx = $('gv-ctx');
    ctx.classList.add('open');
    ctx.setAttribute('data-thread', '1');
    state.ctxPost = 0; // 主题行右键：目标由 selThread 决定，别沿用上一次的帖子
    ctx.style.left = Math.min(x, window.innerWidth - 320) + 'px';
    ctx.style.top = Math.min(y, window.innerHeight - 260) + 'px';
    ctx.querySelectorAll('button[data-act]').forEach(function (b) {
      var a = b.getAttribute('data-act');
      // buttons inside a submenu (复制 → 复制帖子链接/路径/标题) follow their
      // parent item and must not be filtered, or the submenu shows up empty
      if (b.closest('.gv-sub')) return;
      var allowed = a === 'dl' || a === 'pause' || a === 'retry' || a === 'cleancache' || a === 'detail' || a === 'rename' || a === 'folder' || a === 'open' || a === 'copy' || a === 'mark' || a === 'del';
      b.style.display = allowed ? '' : 'none';
    });
    ctx.querySelectorAll('.gv-msep').forEach(function (s, i, all) { s.style.display = i === 0 ? 'none' : ''; });
    // 主条目的"开始下载"按当前状态变成上下文按钮：
    //   有楼层正在下载 → 暂停；都没在跑但存在暂停中的楼层 → 继续下载；否则开始下载。
    // 单独的"暂停"项不再显示（已合并进这一个按钮），避免两处操作打架。
    var t = state.threads.find(function (x) { return x.id === state.selThread; });
    var floors = t ? threadMeta(t).floors : [];
    var runningCnt = floors.filter(function (p) {
      return state.running[p.id] || p.status === 'downloading';
    }).length;
    var pausedCnt = floors.filter(function (p) { return p.status === 'paused'; }).length;
    var dlBtn = ctx.querySelector('[data-act="dl"]');
    if (dlBtn) {
      dlBtn.disabled = false;
      if (runningCnt > 0) {
        ctx.setAttribute('data-thread-mode', 'pause');
        dlBtn.innerHTML = '<i data-lucide="pause"></i>暂停';
        dlBtn.title = '暂停该主题正在下载的楼层';
      } else if (pausedCnt > 0) {
        ctx.setAttribute('data-thread-mode', 'start');
        dlBtn.innerHTML = '<i data-lucide="play"></i>继续下载';
        dlBtn.title = '从暂停的进度接着下载';
      } else {
        ctx.setAttribute('data-thread-mode', 'start');
        dlBtn.innerHTML = '<i data-lucide="play"></i>开始下载';
        dlBtn.title = '';
      }
    }
    var pauseBtn = ctx.querySelector('[data-act="pause"]');
    if (pauseBtn) pauseBtn.style.display = 'none';
    // 主条目删除 = 删连接：连同它下面的楼层一起删除（不再只摘掉黄色行）
    var tDel = $('gv-ctxdel');
    if (tDel) {
      tDel.innerHTML = '<i data-lucide="trash-2"></i>删除主题（连同楼层）';
      tDel.title = '删除该主题，它下面的 ' + floors.length + ' 个楼层会一起删除';
    }
    icons();
  }

  $('gv-ctx').addEventListener('click', function (e) {
    var b = e.target.closest('button');
    if (!b) return;
    var act = b.getAttribute('data-act');
    closeCtx();
    handleCtx(act);
  });
  document.addEventListener('click', function (e) { if (!e.target.closest('#gv-ctx')) closeCtx(); });

  // ---------- 用户标记颜色 ----------
  // 右键 → 标记颜色：给选中的楼层 / 普通条目，或者右键的那条 threads 连接
  // 打一个颜色标记（行左侧 3px 色条）。color 为空 = 清除标记。纯视觉，不参与排序。
  function applyMark(isThread, color) {
    if (isThread) {
      var tid = state.selThread;
      if (!tid) return;
      App().MarkThread(tid, color).then(function () {
        var t = state.threads.find(function (x) { return x.id === tid; });
        if (t) t.mark = color;
        renderPosts();
        toast(color ? '已标记该主题' : '已清除标记');
      }).catch(function (e) { toast('标记失败：' + e); });
      return;
    }
    var ids = selPosts().map(function (p) { return p.id; });
    // 编辑模式下以勾选集合为准（和"删除选中/导出选中"同一批条目）
    if (state.editMode) {
      var k = editSelKeys();
      if (k.pids.length) ids = k.pids;
    }
    if (!ids.length) return;
    App().MarkPosts(ids, color).then(function () {
      // 本地同步一份，免得等下一轮刷新才变色
      state.posts.forEach(function (p) { if (ids.indexOf(p.id) >= 0) p.mark = color; });
      renderPosts();
      toast(color ? ('已标记 ' + ids.length + ' 项') : ('已清除 ' + ids.length + ' 项的标记'));
    }).catch(function (e) { toast('标记失败：' + e); });
  }

  function handleCtx(act) {
    var isThread = $('gv-ctx').getAttribute('data-thread') === '1';
    // 主题的"开始下载"是上下文按钮：data-thread-mode=pause 时等价于点"暂停"
    var threadMode = $('gv-ctx').getAttribute('data-thread-mode') || 'start';
    $('gv-ctx').removeAttribute('data-thread');
    $('gv-ctx').removeAttribute('data-thread-mode');
    var sel = selPosts();
    switch (act) {
      case 'dl':
        if (isThread) {
          var t0 = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t0) {
            if (threadMode === 'pause') {
              App().StopPosts(threadMeta(t0).floors.map(function (p) { return p.id; }));
              break;
            }
            var ids0 = threadMeta(t0).floors.filter(function (p) { return p.status !== 'done'; }).map(function (p) { return p.id; });
            if (ids0.length) App().StartPosts(ids0);
          }
          break;
        }
        if (sel.length) App().StartPosts(sel.map(function (p) { return p.id; }));
        break;
      case 'pause':
        if (isThread) {
          var t1 = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t1) App().StopPosts(threadMeta(t1).floors.map(function (p) { return p.id; }));
          break;
        }
        App().StopPosts(sel.map(function (p) { return p.id; }));
        break;
      case 'retry':
        if (isThread) {
          var t2r = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t2r) threadMeta(t2r).floors.forEach(function (p) {
            if (p.status === 'error') markRetry(p.id);
          });
          break;
        }
        sel.forEach(function (p) { markRetry(p.id); });
        break;
      case 'cleancache':
        if (!App().ClearResolveCache) { toast('需要更新程序后才支持清除缓存'); break; }
        if (isThread) {
          // 线程行：清理该主题下所有已加入楼层的解析缓存
          var t3 = state.threads.find(function (x) { return x.id === state.selThread; });
          var fl3 = t3 ? threadMeta(t3).floors.map(function (p) { return p.id; }) : [];
          if (!fl3.length) { toast('该主题还没有已加入的楼层'); break; }
          App().ClearResolveCache(fl3).then(function (msg) {
            toast(msg || '已清除解析缓存');
          }).catch(function (err) { toast('清除失败：' + err); });
          break;
        }
        if (!sel.length) break;
        App().ClearResolveCache(sel.map(function (p) { return p.id; })).then(function (msg) {
          toast(msg || '已清除解析缓存');
        }).catch(function (err) { toast('清除失败：' + err); });
        break;
      case 'detail':
        if (isThread) {
          var t2 = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t2) openFloorPicker(t2);
        } else if (state.ctxPost) {
          // 以右键/点“更多”的那一行为准（多选时也照样能查看它所在的楼层）
          var cp = state.posts.find(function (x) { return x.id === state.ctxPost; });
          if (cp) openFloorsForPost(cp);
        } else if (sel.length === 1) {
          openFloorsForPost(sel[0]);
        }
        break;
      case 'rename':
        if (isThread) {
          var tr = state.threads.find(function (x) { return x.id === state.selThread; });
          if (tr) openRenameThread(tr);
          break;
        }
        if (sel.length === 1) openRename(sel[0]);
        break;
      case 'move':
        if (sel.length === 1) {
          App().PickDownloadDir(I18N.L('更改下载目录')).then(function (dir) {
            if (dir) { App().SetPostDir(sel[0].id, dir); toast('下载目录已更改为 ' + dir); refreshAll(); }
          });
        }
        break;
      case 'folder':
        function openFolderAt(d) {
          if (!d) { toast('没有可打开的文件夹（条目还没有目录信息）'); return; }
          App().OpenPath(d).then(function (opened) {
            if (opened) toast('已打开：' + opened);
            else toast('文件夹不存在，无法打开');
          });
        }
        if (isThread) {
          if (App().ThreadDir) App().ThreadDir(state.selThread).then(openFolderAt).catch(function (e) { toast('打开文件夹失败：' + e); });
        } else if (sel.length >= 1) {
          App().PostDir(sel[0].id).then(openFolderAt).catch(function (e) { toast('打开文件夹失败：' + e); });
        }
        break;
      case 'open':
        if (isThread) {
          var t3 = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t3) App().OpenBrowser(t3.url);
        } else if (sel.length === 1) {
          App().OpenBrowser(sel[0].postUrl);
        }
        break;
      case 'copy-link':
        if (isThread) {
          var t4 = state.threads.find(function (x) { return x.id === state.selThread; });
          if (t4) App().WriteClipboard(t4.url);
        } else if (sel.length === 1) App().WriteClipboard(sel[0].postUrl);
        break;
      case 'copy-path':
        if (sel.length >= 1) App().PostDir(sel[0].id).then(function (d) { App().WriteClipboard(d); });
        break;
      case 'copy-title':
        if (sel.length >= 1) App().WriteClipboard(sel[0].title);
        break;
      case 'mark-red': case 'mark-orange': case 'mark-yellow':
      case 'mark-green': case 'mark-blue': case 'mark-purple': case 'mark-none':
        applyMark(isThread, act === 'mark-none' ? '' : act.slice(5));
        break;
      case 'del':
        if (isThread) {
          // 删连接：黄色行和它下面的楼层一起删（以前只删 thread 行，
          // 楼层失去归属就散落到列表里变成普通帖子）
          var tDel = state.threads.find(function (x) { return x.id === state.selThread; });
          var flN = tDel ? threadMeta(tDel).floors.length : 0;
          var qmsg = flN > 0
            ? '确定删除这个主题？\n\n它下面的 ' + flN + ' 个楼层会一起删除（已下载的文件不会动）。'
            : '确定删除这个条目？';
          if (!confirm(qmsg)) break;
          App().DeleteThread(state.selThread).then(function () {
            if (state.viewThread === state.selThread) state.viewThread = 0;
            state.selThread = 0;
            toast(flN > 0 ? '已删除主题及其 ' + flN + ' 个楼层' : '已删除主题');
            refreshAll();
          }).catch(function (err) { toast('删除失败：' + err); });
        } else if (sel.length) {
          App().DeletePosts(sel.map(function (p) { return p.id; })).then(function () { state.sel = []; refreshAll(); });
        }
        break;
      case 'clear-done': App().ClearPosts('done').then(refreshAll); break;
      case 'clear-err': App().ClearPosts('error').then(refreshAll); break;
      case 'clear-all': App().ClearPosts('all').then(refreshAll); break;
    }
  }

  function openRenameThread(t) {
    $('gv-renname').value = t.title;
    $('gv-renmask').classList.add('open');
    $('gv-renname').focus();
    $('gv-renok').onclick = function () {
      var v = $('gv-renname').value.trim();
      if (v) App().RenameThread(t.id, v).then(function (msg) { $('gv-renmask').classList.remove('open'); refreshAll(); if (msg) toast(msg); });
    };
  }

  function openRename(p) {
    $('gv-renname').value = p.title;
    $('gv-renmask').classList.add('open');
    $('gv-renname').focus();
    $('gv-renok').onclick = function () {
      var v = $('gv-renname').value.trim();
      if (v) App().RenamePost(p.id, v).then(function (msg) { $('gv-renmask').classList.remove('open'); refreshAll(); if (msg) toast(msg); });
    };
  }

  // ---------- floor picker (virtual rendering) ----------
  function openFloorsForPost(p) {
    // post rows belong to a thread; open that thread's picker
    if (!p.threadId) return;
    App().ListThreads().then(function (ts) {
      var t = (ts || []).find(function (x) { return x.threadId === p.threadId; });
      if (t) { openFloorPicker(t); return; }
      // plain post (no thread entry): silently creating one here would
      // duplicate the post in the list as a yellow row — tell the user
      // to add the thread link instead
      toast('这是单帖，没有可选楼层。要下载整个主题，请添加主题链接。');
    });
  }

  function openFloorPicker(t, preselectPostId) {
    state.floorThread = t.id;
    state.floorChecked = {};
    // 先清空：后端事件可能比 StartFloorStream() 的 Promise 更早到达，
    // 若在 then 里再赋值会把已经收到的楼层冲掉
    state.floors = [];
    state.floorTotal = 0;
    var ut = $('gv-fusethread');
    if (ut) ut.checked = false;
    state.floorPreselect = preselectPostId || 0;
    $('gv-fmtitle').textContent = (t.title || '主题 #' + t.threadId) + ' — 选择楼层';
    $('gv-flist').innerHTML = '<div style="padding:20px;color:var(--gv-fg2)">正在获取楼层…</div>';
    $('gv-modal').classList.add('open');
    updateFloorLoadUI(false, 0);
    App().StartFloorStream(t.id).then(function (floors) {
      if (state.floorThread !== t.id) return;
      if (mergeFloorRows(floors)) renderFloorsAll();
    }).catch(function (err) {
      $('gv-flist').innerHTML = '<div style="padding:20px;color:var(--gv-err)">获取失败：' + esc(err) + '</div>';
    });
  }

  // mergeFloorRows 把一批楼层行合并进 state.floors（按 postId 去重更新），
  // 返回是否真的有变化。后端按批推送、"加载更多"会补发，重复行不会叠出来。
  function mergeFloorRows(arr) {
    if (!arr || !arr.length) return false;
    arr.forEach(function (f) {
      var ex = state.floors.find(function (x) { return x.postId === f.postId; });
      if (ex) Object.assign(ex, f);
      else state.floors.push(f);
    });
    state.floors.sort(function (a, b) { return (a.no || 0) - (b.no || 0); });
    return true;
  }

  // renderFloorsAll 楼层增量到达后重建虚拟列表并刷新统计；空列表时保留占位。
  function renderFloorsAll() {
    $('gv-totalf').textContent = state.floors.length;
    $('gv-totalp').textContent = state.floors.filter(function (f) { return f.imgs > 0; }).length;
    if (!state.floors.length) {
      $('gv-flist').innerHTML = '<div style="padding:20px;color:var(--gv-fg2)">正在获取楼层…</div>';
      return;
    }
    buildVirtualList();
    updateFloorStats();
  }

  function scrollToFloor(postId) {
    var idx = state.floors.findIndex(function (f) { return f.postId === postId; });
    if (idx > 0) $('gv-flist').scrollTop = idx * ROW_H;
  }

  // updateFloorLoadUI 控制底部"加载更多/加载全部"按钮与进度文案。
  // total 是后端已知的楼层总数（还没放行的部分也计入），用于显示 N/总数。
  function updateFloorLoadUI(done, total) {
    var n = state.floors.length;
    if (total) state.floorTotal = total;
    var show = !done;
    $('gv-fmore').style.display = show ? '' : 'none';
    $('gv-fall').style.display = show ? '' : 'none';
    if (done) {
      $('gv-fload').textContent = '加载完成，共 ' + n + ' 层';
    } else if (state.floorTotal > n) {
      $('gv-fload').textContent = '已加载 ' + n + ' / ' + state.floorTotal + ' 层…';
    } else {
      $('gv-fload').textContent = '已加载 ' + n + ' 层…';
    }
  }

  // onFloorsPage 后端每放行一批（首批 300 层 / 点加载更多 +300）推送一次增量。
  function onFloorsPage(pl) {
    if (pl.rowID !== state.floorThread || !$('gv-modal').classList.contains('open')) return;
    if (pl.error) { toast('楼层加载失败：' + pl.error); updateFloorLoadUI(true); return; }
    var dirty = mergeFloorRows(pl.floors);
    if (pl.texts) {
      Object.keys(pl.texts).forEach(function (k) {
        var f = state.floors.find(function (x) { return x.postId === +k; });
        if (f && pl.texts[k]) { f.name = pl.texts[k]; dirty = true; }
      });
    }
    if (dirty) renderFloorsAll();
    // 预选楼层可能不在首批 300 层里，等它出现再勾选+滚动
    if (state.floorPreselect) {
      var pf = state.floors.find(function (x) { return x.postId === state.floorPreselect; });
      if (pf) {
        if (pf.imgs > 0) state.floorChecked[pf.postId] = true;
        renderFloorsAll();
        scrollToFloor(pf.postId);
        state.floorPreselect = 0;
      }
    }
    updateFloorLoadUI(!!pl.done, pl.total);
  }

  var ROW_H = 52; // 行内含 48px 缩略图（对齐 photolist 的 gv-th2）
  var floorScrollBound = false; // scroll 监听只挂一次（以前每批都挂，越挂越多）
  function buildVirtualList() {
    var flist = $('gv-flist');
    // 增量批次到达会重建列表：先记住滚动位置，否则每次都被拉回顶部，
    // 用户正在点的行被换掉，右键/勾选看起来就"失效"了
    var keep = flist.scrollTop;
    flist.innerHTML = '';
    var h = state.floors.length * ROW_H;
    var spacer = document.createElement('div');
    spacer.style.height = h + 'px';
    spacer.style.position = 'relative';
    flist.appendChild(spacer);
    if (!floorScrollBound) {
      flist.addEventListener('scroll', renderFloorWindow);
      floorScrollBound = true;
    }
    flist.scrollTop = keep;
    renderFloorWindow();
  }

  function renderFloorWindow() {
    var flist = $('gv-flist');
    var spacer = flist.firstChild;
    if (!spacer || !state.floors.length) return;
    var top = flist.scrollTop;
    var vh = flist.clientHeight;
    var start = Math.max(0, Math.floor(top / ROW_H) - 5);
    var end = Math.min(state.floors.length, Math.ceil((top + vh) / ROW_H) + 5);
    flist.querySelectorAll('.gv-flrow').forEach(function (r) { r.remove(); });
    var frag = document.createDocumentFragment();
    for (var i = start; i < end; i++) {
      var f = state.floors[i];
      var row = document.createElement('div');
      row.className = 'gv-flrow' + (f.imgs === 0 ? ' empty' : '') + (state.floorChecked[f.postId] ? ' checked' : '');
      row.style.position = 'absolute';
      row.style.top = (i * ROW_H) + 'px';
      row.style.left = 0; row.style.right = 0;
      row.setAttribute('data-i', i);
      row.innerHTML =
        '<input type="checkbox" style="width:13px;height:13px"' + (state.floorChecked[f.postId] ? ' checked' : '') + (f.imgs === 0 ? ' disabled' : '') + '>' +
        '<div class="gv-th2"' + (f.thumb ? ' style="background-image:url(\'/floorthumb?url=' + encodeURIComponent(f.thumb) + '\')"' : '') + '></div>' +
        '<span class="gv-no">' + f.no + 'F</span>' +
        '<span class="gv-t">' + (f.name ? esc(f.name) : '<span style="color:var(--gv-fg2)">（无图楼层）</span>') + '</span>' +
        '<span class="gv-cnt">' + (f.imgs ? f.imgs + ' 图' : '—') + '</span>' +
        '<span class="gv-st" style="color:var(--gv-fg2)">' + (f.hosts ? esc(f.hosts) : (f.imgs ? '未知图床' : '—')) + '</span>';
      frag.appendChild(row);
    }
    flist.appendChild(frag);
  }

  function floorAt(el) {
    var row = el.closest('.gv-flrow');
    if (!row) return null;
    return state.floors[+row.getAttribute('data-i')];
  }

  // ---------- floor row context menu ----------
  var floorCtxTarget = null;
  $('gv-flist').addEventListener('contextmenu', function (e) {
    var f = floorAt(e.target);
    if (!f) return;
    e.preventDefault();
    floorCtxTarget = f;
    var menu = $('gv-flctx');
    menu.classList.add('open');
    menu.style.left = Math.min(e.clientX, window.innerWidth - 190) + 'px';
    menu.style.top = Math.min(e.clientY, window.innerHeight - 100) + 'px';
    icons();
  });
  document.addEventListener('click', function (e) {
    if (!e.target.closest('#gv-flctx')) $('gv-flctx').classList.remove('open');
  });
  $('gv-flctx').addEventListener('click', function (e) {
    var b = e.target.closest('button');
    if (!b || !floorCtxTarget) return;
    $('gv-flctx').classList.remove('open');
    var f = floorCtxTarget;
    var url = f.url || ('https://' + (state.forumHost || 'vipergirls.to') + '/threads/' + (state.floorThreadId || 0) + '?p=' + f.postId + '&viewfull=1');
    if (b.getAttribute('data-act') === 'open-floor') {
      App().OpenBrowser(url);
    } else if (b.getAttribute('data-act') === 'copy-floor') {
      App().WriteClipboard(url).then(function () { toast('楼层链接已复制'); });
    } else if (b.getAttribute('data-act') === 'copy-name') {
      if (!f.name) { toast('该楼层没有名称'); return; }
      App().WriteClipboard(f.name).then(function () { toast('名称已复制：' + f.name); });
    }
  });

  $('gv-flist').addEventListener('change', function (e) {
    var f = floorAt(e.target);
    if (!f || f.imgs === 0) return;
    state.floorChecked[f.postId] = e.target.checked;
    e.target.closest('.gv-flrow').classList.toggle('checked', e.target.checked);
    if (e.target.checked) showFloorPreview(f);
    updateFloorStats();
  });
  $('gv-flist').addEventListener('click', function (e) {
    var f = floorAt(e.target);
    if (f) showFloorPreview(f);
  });
  function showFloorPreview(f) {
    var ph = $('gv-fpph');
    var img = $('gv-fpimg');
    var existing = img.querySelector('img');
    if (existing) existing.remove();
    if (f.thumb) {
      var el = document.createElement('img');
      el.src = '/floorthumb?url=' + encodeURIComponent(f.thumb);
      el.onerror = function () { el.remove(); ph.style.display = ''; };
      ph.style.display = 'none';
      img.insertBefore(el, img.firstChild);
    } else { ph.style.display = ''; }
    var cap = $('gv-fpcap');
    cap.style.display = '';
    cap.textContent = f.no + 'F · ' + (f.name || '无图楼层');
    $('gv-fpmeta').innerHTML = '<b>' + f.imgs + ' 图</b>' + (f.hosts ? ' · ' + esc(f.hosts) : '');
  }
  function updateFloorStats() {
    var n = 0, imgs = 0;
    state.floors.forEach(function (f) {
      if (state.floorChecked[f.postId]) { n++; imgs += f.imgs; }
    });
    $('gv-selcount').textContent = n;
    $('gv-selimgs').textContent = imgs;
  }
  function setRangeFloors(lo, hi, val) {
    state.floors.forEach(function (f) {
      if (f.imgs === 0) return;
      if (f.no >= lo && f.no <= hi) state.floorChecked[f.postId] = val;
    });
    renderFloorWindow();
    updateFloorStats();
  }
  $('gv-jump').addEventListener('input', function () {
    var n = +this.value;
    if (n >= 1) {
      var idx = state.floors.findIndex(function (f) { return f.no >= n; });
      if (idx >= 0) $('gv-flist').scrollTop = idx * ROW_H;
    }
  });
  $('gv-rangeok').addEventListener('click', function () {
    var lo = Math.max(1, +$('gv-rangefrom').value || 1);
    var hi = Math.max(lo, +$('gv-rangeto').value || 1e9);
    setRangeFloors(lo, hi, true);
  });
  $('gv-all').addEventListener('click', function () { setRangeFloors(1, 1e9, true); });
  $('gv-none').addEventListener('click', function () { setRangeFloors(1, 1e9, false); });
  $('gv-inv').addEventListener('click', function () {
    state.floors.forEach(function (f) {
      if (f.imgs === 0) return;
      state.floorChecked[f.postId] = !state.floorChecked[f.postId];
    });
    renderFloorWindow();
    updateFloorStats();
  });
  $('gv-fcancel').addEventListener('click', function () { $('gv-modal').classList.remove('open'); });
  $('gv-mclose').addEventListener('click', function () { $('gv-modal').classList.remove('open'); });
  $('gv-modal').addEventListener('click', function (e) { if (e.target === $('gv-modal')) $('gv-modal').classList.remove('open'); });
  $('gv-fmore').addEventListener('click', function () { App().FetchMoreFloors(state.floorThread, false); });
  $('gv-fall').addEventListener('click', function () { App().FetchMoreFloors(state.floorThread, true); });
  $('gv-fclear').addEventListener('click', function () {
    if (!confirm('确定清除该主题的楼层缓存吗？下次打开楼层选择器将重新抓取。')) return;
    var rowId = state.floorThread;
    App().ClearFloorCache(rowId).then(function () {
      if (state.floorThread !== rowId) return;
      state.floors = [];
      state.floorChecked = {};
      state.floorTotal = 0;
      buildVirtualList();
      $('gv-totalf').textContent = '0';
      $('gv-totalp').textContent = '0';
      $('gv-fload').textContent = '缓存已清除，正在重新抓取…';
      toast('楼层缓存已清除');
      // 用清空后的 job 重新拉取（后台分批抓，floors:page 会增量补上）
      App().StartFloorStream(rowId).then(function (floors) {
        if (state.floorThread !== rowId) return;
        if (mergeFloorRows(floors)) renderFloorsAll();
      });
    });
  });
  $('gv-fok').addEventListener('click', function () {
    var ids = Object.keys(state.floorChecked).filter(function (k) { return state.floorChecked[k]; }).map(Number);
    if (!ids.length) return;
    App().ConfirmFloors(state.floorThread, ids, $('gv-fusethread').checked).then(function (msg) {
      $('gv-modal').classList.remove('open');
      toast(msg);
      // jump straight into the thread's manage view so the newly added
      // floors appear inside it
      state.viewThread = state.floorThread;
      state.sel = [];
      state.selThread = 0;
      refreshAll();
      switchTab('dl');
    });
  });

  // ---------- tabs / menus / search ----------
  function switchTab(tab) {
    app.querySelectorAll('[data-mtab]').forEach(function (x) { x.classList.toggle('on', x.getAttribute('data-mtab') === tab); });
    app.setAttribute('data-tab', tab);
  }
  app.querySelectorAll('[data-mtab]').forEach(function (b) {
    b.addEventListener('click', function () { switchTab(b.getAttribute('data-mtab')); });
  });
  // bottom-strip tabs: Photo list (left) / Log (right) — one fills the strip
  app.querySelectorAll('[data-itab]').forEach(function (b) {
    b.addEventListener('click', function () {
      app.querySelectorAll('[data-itab]').forEach(function (x) { x.classList.toggle('on', x === b); });
      var pho = b.getAttribute('data-itab') === 'pho';
      $('gv-phowrap').style.display = pho ? '' : 'none';
      document.querySelector('.gv-logpane').style.display = pho ? 'none' : '';
    });
  });
  app.querySelectorAll('.gv-drop > span').forEach(function (sp) {
    sp.addEventListener('click', function (e) {
      e.stopPropagation();
      var d = sp.parentElement;
      app.querySelectorAll('.gv-drop').forEach(function (x) { if (x !== d) x.classList.remove('open'); });
      d.classList.toggle('open');
    });
  });
  document.addEventListener('click', function (e) {
    if (!e.target.closest('.gv-drop')) app.querySelectorAll('.gv-drop').forEach(function (x) { x.classList.remove('open'); });
  });

  $('gv-search').addEventListener('input', function () {
    state.filter = this.value.toLowerCase();
    renderPosts();
  });

  // add links
  var addmask = $('gv-addmask');
  $('gv-openadd').addEventListener('click', function () { addmask.classList.add('open'); $('gv-addtext').focus(); });
  $('gv-addclose').addEventListener('click', function () { addmask.classList.remove('open'); });
  addmask.addEventListener('click', function (e) { if (e.target === addmask) addmask.classList.remove('open'); });
  $('gv-m-add').addEventListener('click', function () { addmask.classList.add('open'); $('gv-addtext').focus(); });
  $('gv-addscan').addEventListener('click', function () {
    var text = $('gv-addtext').value;
    var count = text.split(/\r?\n/).filter(function (l) { return l.trim(); }).length;
    if (!count) return;
    // hand the batch to the background queue right away — the dialog closes
    // immediately and progress shows up next to the 导入历史 button
    $('gv-addtext').value = '';
    addmask.classList.remove('open');
    state.importQueue.push({ text: text, count: count });
    runImportQueue();
  });

  // import log panel
  var impmask = $('gv-impmask');
  $('gv-importbtn').addEventListener('click', function () {
    renderImportPanel();
    impmask.classList.add('open');
  });
  $('gv-impclose').addEventListener('click', function () { impmask.classList.remove('open'); });
  impmask.addEventListener('click', function (e) { if (e.target === impmask) impmask.classList.remove('open'); });
  $('gv-impcopy').addEventListener('click', function () {
    var urls = selectedImportURLs();
    if (!urls.length) { toast('未选中任何行'); return; }
    App().WriteClipboard(urls.join('\n')).then(function () { toast('已复制 ' + urls.length + ' 条链接'); });
  });
  $('gv-impclear').addEventListener('click', function () {
    state.importLog = [];
    state.importSel = [];
    state.importAnchor = -1;
    renderImportPanel();
    App().SaveImportLog([]).catch(function () {});
  });

  // close-while-downloading confirm
  var closemask = $('gv-closemask');
  $('gv-closemin').addEventListener('click', function () {
    closemask.classList.remove('open');
    window.runtime.WindowMinimise();
  });
  $('gv-closequit').addEventListener('click', function () {
    closemask.classList.remove('open');
    // 强制退出：直接走真正退出，不再被 beforeClose 拦一次
    App().ForceQuit();
  });
  $('gv-closecancel').addEventListener('click', function () { closemask.classList.remove('open'); });
  closemask.addEventListener('click', function (e) { if (e.target === closemask) closemask.classList.remove('open'); });

  // rename modal close
  $('gv-renclose').addEventListener('click', function () { $('gv-renmask').classList.remove('open'); });
  $('gv-renmask').addEventListener('click', function (e) { if (e.target === $('gv-renmask')) $('gv-renmask').classList.remove('open'); });

  // ---------- settings ----------
  var setmask = $('gv-setmask');
  $('gv-m-set').addEventListener('click', function () {
    App().LoadSettings().then(function (s) { fillSettings(s); setmask.classList.add('open'); });
  });
  $('gv-setcancel').addEventListener('click', function () { setmask.classList.remove('open'); });
  setmask.addEventListener('click', function (e) { if (e.target === setmask) setmask.classList.remove('open'); });
  app.querySelectorAll('.gv-setside [data-spage]').forEach(function (b) {
    b.addEventListener('click', function () {
      app.querySelectorAll('.gv-setside [data-spage]').forEach(function (x) { x.classList.toggle('on', x === b); });
      app.querySelectorAll('.gv-setpage').forEach(function (pg) { pg.classList.toggle('on', pg.getAttribute('data-spage') === b.getAttribute('data-spage')); });
    });
  });
  function fillSettings(s) {
    $('gv-s-existing').value = s.existingFiles || 'skip';
    $('gv-s-perfolder').value = s.perPostFolder ? '1' : '0';
    $('gv-s-forumsort').checked = !!s.sortByForum;
    $('gv-s-preview').checked = !!s.showPreview;
    $('gv-s-tray').checked = !!s.minimizeTray;
    $('gv-s-sound').checked = !!s.soundOnDone;
    $('gv-s-shutdown').checked = !!s.shutdownOnDone;
    $('gv-s-dlndir').value = s.downloadDir || '';
    $('gv-s-tempdir').value = s.tempDir || '';
    $('gv-s-posts').value = s.postsConcurrent;
    $('gv-s-hostconc').value = s.hostConcurrent;
    $('gv-s-retries').value = s.maxRetries;
    $('gv-s-timeout').value = s.timeoutSec;
    $('gv-s-origname').checked = !!s.fetchOrigName;
    $('gv-s-floor').checked = !!s.fetchFloorTitle;
    $('gv-s-forum').value = s.forumDomain || '';
    $('gv-s-mirrors').value = s.mirrors || '';
    $('gv-s-rate').value = s.rateLimit;
    $('gv-s-proxy').value = s.proxy || '';
    $('gv-s-proxyuse').checked = !!s.proxyEnabled;
    setToggle('gv-s-proxyon', !!s.proxyEnabled);
    $('gv-s-thank').checked = !!s.autoThank;
    $('gv-s-user').value = s.username || '';
    $('gv-s-pass').value = s.password || '';
    $('gv-s-autologin').checked = !!s.autoLogin;
    $('gv-s-exton').checked = s.extBridgeEnabled !== false;
    $('gv-s-extport').value = (s.extBridgePort === undefined || s.extBridgePort === null) ? 47821 : s.extBridgePort;
    refreshLoginState();
    refreshExtState();
  }
  function collectSettings() {
    return {
      existingFiles: $('gv-s-existing').value,
      perPostFolder: $('gv-s-perfolder').value === '1',
      sortByForum: $('gv-s-forumsort').checked,
      showPreview: $('gv-s-preview').checked,
      minimizeTray: $('gv-s-tray').checked,
      soundOnDone: $('gv-s-sound').checked,
      shutdownOnDone: $('gv-s-shutdown').checked,
      downloadDir: $('gv-s-dlndir').value,
      tempDir: $('gv-s-tempdir').value.trim(),
      postsConcurrent: +$('gv-s-posts').value || 2,
      hostConcurrent: +$('gv-s-hostconc').value || 2,
      maxRetries: +$('gv-s-retries').value || 3,
      timeoutSec: +$('gv-s-timeout').value || 10,
      fetchOrigName: $('gv-s-origname').checked,
      fetchFloorTitle: $('gv-s-floor').checked,
      forumDomain: $('gv-s-forum').value,
      mirrors: $('gv-s-mirrors').value,
      rateLimit: +$('gv-s-rate').value || 2,
      proxy: $('gv-s-proxy').value.trim(),
      proxyEnabled: $('gv-s-proxyuse').checked,
      autoThank: $('gv-s-thank').checked,
      username: $('gv-s-user').value,
      password: $('gv-s-pass').value,
      autoLogin: $('gv-s-autologin').checked,
      extBridgeEnabled: $('gv-s-exton').checked,
      extBridgePort: Math.max(0, Math.min(65535, parseInt($('gv-s-extport').value, 10) || 0)),
      // 列表排序是表头点出来的，不在设置界面里；保存设置时要原样带回去，
      // 否则会被重置成默认（重启后排序丢失）。普通列表 / 主题管理各一份。
      sortCol: (state.normalSort || state.sort).col,
      sortDir: (state.normalSort || state.sort).dir,
      threadSortCol: (state.threadSort || state.sort).col,
      threadSortDir: (state.threadSort || state.sort).dir
    };
  }

  // 读取桥接服务当前状态（设置 → 扩展）
  function refreshExtState() {
    var el = $('gv-extstate');
    if (!el) return;
    App().ExtBridgeStatus().then(function (st) {
      if (!st) { el.textContent = '—'; return; }
      if (!st.enabled) {
        el.textContent = '已关闭（扩展无法连接）';
        el.style.color = '#b45309';
      } else if (st.running && st.url) {
        el.textContent = '运行中 · ' + st.url + '（仅本机）';
        el.style.color = '#15803d';
      } else {
        el.textContent = '已启用但未启动（端口可能都被占用，看日志）';
        el.style.color = '#b45309';
      }
    }).catch(function (e) { el.textContent = '读取失败：' + e; });
  }
  $('gv-setok').addEventListener('click', function () {
    var payload = collectSettings();
    App().SaveSettings(payload).then(function () {
      setmask.classList.remove('open');
      setToggle('gv-s-proxyon', !!payload.proxyEnabled);
      state.settings = payload;      // 提示音/预览等即时项跟着新设置走
      app.setAttribute('data-prev', payload.showPreview ? 'on' : 'off');
      toast('设置已保存');
      refreshExtState();
    }).catch(function (err) { toast('保存失败：' + err); });
  });
  $('gv-extcheck').addEventListener('click', function () { refreshExtState(); });

  $('gv-s-browse').addEventListener('click', function () {
    App().PickDownloadDir(I18N.L('选择下载目录')).then(function (dir) { if (dir) $('gv-s-dlndir').value = dir; });
  });
  // “试听”按钮：不管开关是否勾选都能听一下效果（此时窗口处在用户交互中，
  // AudioContext 的自动播放限制不会被触发）
  var soundTestBtn = $('gv-s-soundtest');
  if (soundTestBtn) soundTestBtn.addEventListener('click', function () { playDoneSound(); });
  $('gv-s-tempbrowse').addEventListener('click', function () {
    App().PickDownloadDir(I18N.L('选择临时目录')).then(function (dir) { if (dir) $('gv-s-tempdir').value = dir; });
  });
  function setToggle(id, on) {
    var t = $(id);
    if (t) t.setAttribute('aria-pressed', on ? 'true' : 'false');
  }
  function isOn(id) {
    var t = $(id);
    return !!t && t.getAttribute('aria-pressed') === 'true';
  }
  // proxy toggle in the menubar: flips immediately and saves right away;
  // effect on downloads follows the rule in its tooltip (new starts use the
  // new setting, in-flight posts keep their start-time snapshot)
  var proxyToggle = $('gv-s-proxyon');
  if (proxyToggle) {
    App().LoadSettings().then(function (s) { setToggle('gv-s-proxyon', !!s.proxyEnabled); }).catch(function () {});
    proxyToggle.addEventListener('click', function () {
      var next = !isOn('gv-s-proxyon');
      setToggle('gv-s-proxyon', next);
      App().LoadSettings().then(function (s) {
        s.proxyEnabled = next;
        return App().SaveSettings(s);
      }).then(function () {
        toast(next ? '代理已开启：新开始的下载生效' : '代理已关闭：新开始的下载生效');
      }).catch(function (err) {
        setToggle('gv-s-proxyon', !next);
        toast('保存失败：' + err);
      });
    });
  }
  $('gv-s-login').addEventListener('click', function () {
    if (!$('gv-s-user').value.trim() || !$('gv-s-pass').value) {
      $('gv-loginstate').textContent = '请先填写用户名和密码';
      return;
    }
    $('gv-loginstate').textContent = '登录中…';
    App().LoginNow($('gv-s-user').value, $('gv-s-pass').value).then(function () {
      refreshLoginState();
    }).catch(function (err) { $('gv-loginstate').textContent = '失败：' + err; });
  });
  // 网页登录：登录窗口是独立进程，登录成功后主程序通过事件通知；
  // 这里再轮询一段时间，既能在成功时立刻刷出用户名，也能在窗口被关掉
  // 但没登上时给出明确提示（以前会一直停在"已打开…"，看着像卡住）。
  var webLoginWatch = null;
  function watchWebLogin() {
    if (webLoginWatch) clearInterval(webLoginWatch);
    var ticks = 0;
    webLoginWatch = setInterval(function () {
      ticks++;
      App().LoginState().then(function (st) {
        if (st && st.loggedIn) {
          renderLoginState(true, st.username);
          clearInterval(webLoginWatch);
          webLoginWatch = null;
        } else if (ticks >= 90) { // 3 分钟
          clearInterval(webLoginWatch);
          webLoginWatch = null;
          $('gv-loginstate').textContent = '未登录（登录窗口已关闭或超时，可重试）';
        }
      }).catch(function () {});
    }, 2000);
  }
  $('gv-s-weblogin').addEventListener('click', function () {
    $('gv-loginstate').textContent = '已打开网页登录窗口，请在窗口里完成登录…';
    App().OpenWebLogin().then(function (msg) {
      if (msg) { $('gv-loginstate').textContent = '打开失败：' + msg; return; }
      watchWebLogin();
    }).catch(function (err) { $('gv-loginstate').textContent = '打开失败：' + err; });
  });
  $('gv-s-logout').addEventListener('click', function () {
    App().LogoutNow().then(function () {
      renderLoginState(false, '');
      toast('已注销论坛登录');
    }).catch(function (err) { toast('注销失败：' + err); });
  });
  window.runtime.EventsOn('login:changed', function (st) {
    renderLoginState(st && st.loggedIn, st && st.username);
  });
  function renderLoginState(ok, name) {
    var txt = ok
      ? '<span style="color:#2e9e44;font-weight:bold">✔</span> 已登录' + (name ? '（' + esc(name) + '）' : '（用户名未识别）')
      : '未登录';
    $('gv-loginstate').innerHTML = txt;
  }
  function refreshLoginState() {
    App().LoginState().then(function (st) {
      renderLoginState(st && st.loggedIn, st && st.username);
    }).catch(function () { renderLoginState(false, ''); });
  }

  // ---------- host manager ----------
  var hostmask = $('gv-hostmask');
  $('gv-m-hosts').addEventListener('click', function () { openHostManager(); });
  function openHostManager() {
    hostmask.classList.add('open');
    refreshHostsDir();
    App().ListHosts().then(function (rules) {
      state.hostRules = rules || [];
      renderHostList();
    });
  }

  function refreshHostsDir() {
    App().HostsDir().then(function (d) {
      var el = $('gv-hostdirtext');
      if (el) el.textContent = I18N.L('规则目录：') + (d || 'data/hosts') + I18N.L('（保存即热加载，勾选/取消立即生效）');
      if (el) el.title = d || '';
    }).catch(function () {});
  }

  function switchHostsDir(dir, askMigrate) {
    var doSwitch = function (migrate) {
      return App().SetHostsDir(dir, migrate).then(function (r) {
        toast('规则目录已切换：' + r.dir + '（复制 ' + r.copied + '，跳过 ' + r.skipped + '，共 ' + r.rules + ' 条）');
        openHostManager();
      });
    };
    if (askMigrate && dir) {
      var migrate = confirm('要把当前目录里的规则复制到新目录吗？\n' + dir +
        '\n\n确定 = 复制已有规则过去\n取消 = 只切换目录（新目录会补齐内置规则）');
      return doSwitch(migrate);
    }
    return doSwitch(false);
  }

  $('gv-hostdir').addEventListener('click', function () {
    App().PickHostsDir().then(function (dir) {
      if (!dir) return;
      switchHostsDir(dir, true).catch(function (e) { toast('切换失败：' + e); });
    }).catch(function (e) { toast('选择目录失败：' + e); });
  });

  $('gv-hostreload').addEventListener('click', function () {
    App().HostReload().then(function () {
      toast('规则已重新加载');
      openHostManager();
    }).catch(function (e) { toast('重载失败：' + e); });
  });

  $('gv-hostlint').addEventListener('click', function () {
    var out = $('gv-testout');
    out.textContent = '体检中…';
    App().HostLint().then(function (issues) {
      issues = issues || [];
      var errs = issues.filter(function (i) { return i.level === 'error'; });
      var warns = issues.filter(function (i) { return i.level !== 'error'; });
      var lines = issues.map(function (i) {
        return '[' + i.level + '] ' + i.rule + (i.step ? ' 步骤' + i.step : '') + '：' + i.message;
      });
      out.innerHTML = '<b>体检完成：' + errs.length + ' 个错误，' + warns.length + ' 个提醒</b>\n' +
        (lines.length ? lines.map(esc).join('\n') : '全部通过') +
        (errs.length ? '\n\n<span class="bad">错误项会导致规则跑不通，建议先处理。</span>' : '');
    }).catch(function (e) { out.textContent = '体检失败：' + e; });
  });

  $('gv-hostdireset').addEventListener('click', function () {
    if (!confirm('回到默认目录 data/hosts？\n（新目录里已存在的规则不会被删除）')) return;
    switchHostsDir('', false).catch(function (e) { toast('切换失败：' + e); });
  });
  $('gv-hostclose').addEventListener('click', function () { hostmask.classList.remove('open'); });
  hostmask.addEventListener('click', function (e) { if (e.target === hostmask) hostmask.classList.remove('open'); });

  // ---------- 规则列表：列 + 搜索 + 排序（183 条之后全靠这张表） ----------
  var hostSort = { key: 'added', desc: true };
  state.hostSel = [];     // 多选中的规则 id（按文件名，不是按名字）
  state.hostAnchor = -1;  // Shift 区间选的起点行号
  var HOST_COLS = [
    { k: '', t: '' },
    { k: 'name', t: '名称' },
    { k: 'domain', t: '匹配域名' },
    { k: 'origin', t: '来源' },
    { k: 'steps', t: '步骤' },
    { k: 'added', t: '添加日期' },
    { k: '', t: '备注' }
  ];
  function hostOrigin(h) { return h.origin || (h.builtin ? 'builtin' : 'custom'); }
  function hostOriginText(h) {
    var o = hostOrigin(h);
    return o === 'builtin' ? '内置' : o === 'imported' ? '原版导入' : '自定义';
  }
  function hostRows() {
    var q = (($('gv-hfilter') && $('gv-hfilter').value) || '').trim().toLowerCase();
    var type = $('gv-hftype') ? $('gv-hftype').value : 'all';
    var onlyOn = $('gv-hfon') ? $('gv-hfon').checked : false;
    var arr = (state.hostRules || []).filter(function (h) {
      if (type !== 'all' && hostOrigin(h) !== type) return false;
      if (onlyOn && !h.enabled) return false;
      if (!q) return true;
      return [h.name || '', (h.domains || []).join(' '), h.note || '', h.id || ''].join(' ').toLowerCase().indexOf(q) >= 0;
    });
    var dir = hostSort.desc ? -1 : 1;
    arr.sort(function (a, b) {
      var r;
      switch (hostSort.key) {
        case 'name': r = String(a.name || '').localeCompare(String(b.name || '')); break;
        case 'domain': r = String((a.domains || [])[0] || '').localeCompare(String((b.domains || [])[0] || '')); break;
        case 'origin': r = hostOriginText(a).localeCompare(hostOriginText(b)); break;
        case 'steps': r = (a.steps || []).length - (b.steps || []).length; break;
        default: r = String(a.added || '').localeCompare(String(b.added || ''));
      }
      if (r === 0) r = String(a.name || '').localeCompare(String(b.name || ''));
      return dir * r;
    });
    return arr;
  }
  function renderHostList() {
    var tbl = $('gv-htable');
    if (!tbl) return;
    var rows = hostRows();
    var cnt = $('gv-hcount');
    if (cnt) cnt.textContent = '显示 ' + rows.length + ' / 共 ' + (state.hostRules || []).length + ' 条';
    var head = '<div class="gv-hhdr"><span></span>' + HOST_COLS.slice(1).map(function (c) {
      if (!c.k) return '<span>' + c.t + '</span>';
      var on = hostSort.key === c.k;
      var mark = on ? (hostSort.desc ? ' ▼' : ' ▲') : '';
      return '<span data-sort="' + c.k + '" class="' + (on ? 'on' : '') + '">' + c.t + mark + '</span>';
    }).join('') + '</div>';
    var body = rows.map(function (h) {
      var rid = h.id || h.name;
      return '<div class="gv-hitem' + (hostSelHas(rid) ? ' sel' : '') + (state.editRule === rid ? ' on' : '') + '" data-id="' + esc(rid) + '" title="' + esc(h.id || h.name) + '">' +
        '<input type="checkbox"' + (h.enabled ? ' checked' : '') + ' style="width:12px;height:12px" title="' + (h.enabled ? '已启用，取消勾选即停用' : '已停用') + '">' +
        '<span class="gv-hn">' + esc(h.name) + '</span>' +
        '<span class="gv-hdom" title="' + esc((h.domains || []).join(', ')) + '">' + esc((h.domains || []).join(', ')) + '</span>' +
        '<span class="gv-tag ' + hostOrigin(h) + '">' + hostOriginText(h) + '</span>' +
        '<span class="gv-hnum">' + ((h.steps || []).length) + '</span>' +
        '<span class="gv-hdate">' + esc(h.added || '-') + '</span>' +
        '<span class="gv-hnote" title="' + esc(h.note || '') + '">' + esc(h.note || '') + '</span>' +
        '</div>';
    }).join('');
    var keepScroll = tbl.scrollTop; // 重绘会让列表跳回顶部，多选时尤其糟
    tbl.innerHTML = head + (body || '<div style="padding:10px;font-size:11px;color:var(--gv-fg2)">没有匹配的规则</div>');
    tbl.scrollTop = keepScroll;

    tbl.querySelectorAll('.gv-hhdr span[data-sort]').forEach(function (sp) {
      sp.addEventListener('click', function () {
        var k = sp.getAttribute('data-sort');
        if (hostSort.key === k) hostSort.desc = !hostSort.desc;
        else { hostSort.key = k; hostSort.desc = (k === 'added'); }
        renderHostList();
      });
    });
    tbl.querySelectorAll('.gv-hitem').forEach(function (d) {
      var id = d.getAttribute('data-id');
      var h = (state.hostRules || []).filter(function (x) { return (x.id || x.name) === id; })[0];
      if (!h) return;
      // 勾选框沿用 click 语义：点它就是单条启停“即存即生效”
      d.addEventListener('click', function (e) {
        if (e.target.tagName !== 'INPUT') return;
        e.stopPropagation();
        var want = !h.enabled;
        // 按文件(id)下发：同名时自定义会遮蔽内置，按名字勾选会误伤另一条
        App().HostToggle(h.id || h.name, want).then(function () {
          // 勾选即写盘：JSON 里的 enabled 被改写并热加载，无需另外保存
          toast('已保存生效：' + h.name + (want ? ' 已启用' : ' 已停用'));
          openHostManager();
        }).catch(function (err) { toast('保存失败：' + err); });
      });
      // 多选：和导入面板一致的 mousedown 语义（ctrl/shift 用起来才跟手，
      // 且只改行样式不整表重绘，不然列表在指针底下重建会飘）
      d.addEventListener('mousedown', function (e) {
        if (e.button !== 0 || e.target.tagName === 'INPUT') return;
        e.preventDefault();
        var rows = hostRows(), idx = -1;
        for (var i = 0; i < rows.length; i++) {
          if ((rows[i].id || rows[i].name) === id) { idx = i; break; }
        }
        if (e.shiftKey && state.hostAnchor >= 0) {
          var a = Math.min(state.hostAnchor, idx), b = Math.max(state.hostAnchor, idx);
          if (!e.ctrlKey) state.hostSel = [];
          for (var j = a; j <= b && j < rows.length; j++) hostSelAdd(rows[j].id || rows[j].name);
        } else if (e.ctrlKey || e.metaKey) {
          hostSelToggle(id);
        } else {
          state.hostSel = [id];
        }
        state.hostAnchor = idx;
        hostPaintSel(tbl);
        loadHostEditor(h);
      });
    });
    hostPaintSel(tbl);
    icons();
  }
  // ---------- 多选：Ctrl 增减 / Shift 区间 / Ctrl+A 全选 / Esc 清空 ----------
  function hostSelHas(id) { return (state.hostSel || []).indexOf(id) >= 0; }
  function hostSelAdd(id) { if (!hostSelHas(id)) state.hostSel.push(id); }
  function hostSelToggle(id) {
    var i = (state.hostSel || []).indexOf(id);
    if (i >= 0) state.hostSel.splice(i, 1); else state.hostSel.push(id);
  }
  function hostPaintSel(tbl) {
    var box = tbl || $('gv-htable');
    if (box) {
      box.querySelectorAll('.gv-hitem').forEach(function (r) {
        r.classList.toggle('sel', hostSelHas(r.getAttribute('data-id')));
      });
    }
    var n = (state.hostSel || []).length;
    var bulk = $('gv-hbulk'), sc = $('gv-hselcount');
    if (bulk) bulk.classList.toggle('on', n > 0);
    if (sc) sc.textContent = n;
  }
  function hostBulkToggle(on) {
    var ids = (state.hostSel || []).slice();
    if (!ids.length) return;
    App().HostToggleMany(ids, on).then(function (r) {
      toast((on ? '已启用 ' : '已停用 ') + r.ok + ' 条' + (r.failed ? '，失败 ' + r.failed + ' 条' : ''));
      openHostManager();
    }).catch(function (err) { toast('批量操作失败：' + err); });
  }
  function hostBulkDelete() {
    var ids = (state.hostSel || []).slice();
    if (!ids.length) return;
    if (!confirm('删除选中的 ' + ids.length + ' 条规则？\n\n自定义规则删掉就没了；内置规则下次启动会自动补回来。')) return;
    App().HostDeleteMany(ids).then(function (r) {
      toast('已删除 ' + r.ok + ' 条' + (r.failed ? '，失败 ' + r.failed + ' 条' : ''));
      state.hostSel = [];
      state.editRule = null;
      openHostManager();
    }).catch(function (err) { toast('删除失败：' + err); });
  }
  // 工具栏：输入即筛，不用点按钮
  (function () {
    var fq = $('gv-hfilter'), ft = $('gv-hftype'), fon = $('gv-hfon');
    if (fq) fq.addEventListener('input', function () { renderHostList(); });
    if (ft) ft.addEventListener('change', function () { renderHostList(); });
    if (fon) fon.addEventListener('change', function () { renderHostList(); });
    // 批量操作条：选了才出来
    var on = $('gv-hselon'), off = $('gv-hseloff'), none = $('gv-hselnone'), del = $('gv-hseldel');
    if (on) on.addEventListener('click', function () { hostBulkToggle(true); });
    if (off) off.addEventListener('click', function () { hostBulkToggle(false); });
    if (none) none.addEventListener('click', function () { state.hostSel = []; renderHostList(); });
    if (del) del.addEventListener('click', hostBulkDelete);
  })();
  // 快捷键：面板打开且焦点不在输入框里时才生效
  document.addEventListener('keydown', function (e) {
    if (!hostmask || !hostmask.classList.contains('open')) return;
    var tag = ((e.target && e.target.tagName) || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return;
    if ((e.ctrlKey || e.metaKey) && String(e.key).toLowerCase() === 'a') {
      e.preventDefault();
      state.hostSel = hostRows().map(function (x) { return x.id || x.name; });
      renderHostList();
      return;
    }
    if (e.key === 'Escape' && (state.hostSel || []).length) {
      state.hostSel = [];
      renderHostList();
    }
  });
  function editedRule() {
    return (state.hostRules || []).filter(function (h) {
      return (h.id || h.name) === state.editRule;
    })[0] || null;
  }

  function loadHostEditor(h) {
    state.editRule = h.id || h.name;
    $('gv-hname').value = h.name;
    $('gv-hdom').value = (h.domains || []).join(', ');
    $('gv-hnote').value = h.note || '';
    state.editSteps = JSON.parse(JSON.stringify(h.steps || []));
    renderSteps();
    renderHostList();
    $('gv-testout').textContent = '等待测试…';
  }
  function renderSteps() {
    var box = $('gv-steps');
    box.innerHTML = '';
    (state.editSteps || []).forEach(function (st, i) {
      var d = document.createElement('div');
      d.className = 'gv-step';
      var sel = '<span class="gv-sno">' + (i + 1) + '</span>' +
        '<select class="gv-sact">' +
        ['get', 'post', 'set-cookie', 'extract', 'search', 'regex', 'set'].map(function (a) {
          return '<option value="' + a + '"' + (st.action === a ? ' selected' : '') + '>' + a + '</option>';
        }).join('') + '</select>';
      var extra = '';
      switch (st.action) {
        case 'get': case 'post':
          extra = '<input data-k="url" value="' + esc(st.url || '') + '" placeholder="URL，如 {page_url}">';
          break;
        case 'extract':
          extra = '<select class="gv-sfld"><option value="image_url"' + (st.field === 'image_url' ? ' selected' : '') + '>image_url</option><option value="filename"' + (st.field === 'filename' ? ' selected' : '') + '>filename</option></select>' +
            '<input data-k="selector" value="' + esc(st.selector || '') + '" placeholder="XPath 选择器">' +
            '<input data-k="attr" value="' + esc(st.attr || '') + '" placeholder="src/alt/title/text" style="max-width:90px">' +
            '<input data-k="default" value="' + esc(st.default || '') + '" placeholder="兜底值，如 {url_name}" style="max-width:110px">';
          break;
        case 'search':
          extra = '<select class="gv-sfld"><option value="image_url"' + (st.field === 'image_url' ? ' selected' : '') + '>image_url</option><option value="filename"' + (st.field === 'filename' ? ' selected' : '') + '>filename</option></select>' +
            '<input data-k="url" value="' + esc(st.url || '') + '" placeholder="在那段文本里找，如 {body}" style="max-width:110px">' +
            '<input data-k="selector" value="' + esc(st.selector || '') + '" placeholder="正则（取第 1 个捕获组）">' +
            '<input data-k="default" value="' + esc(st.default || '') + '" placeholder="没匹配到时的兜底" style="max-width:120px">';
          break;
        case 'regex':
          extra = '<select class="gv-sfld"><option value="image_url"' + (st.field === 'image_url' ? ' selected' : '') + '>image_url</option><option value="page_url"' + (st.field === 'page_url' ? ' selected' : '') + '>page_url</option><option value="filename"' + (st.field === 'filename' ? ' selected' : '') + '>filename</option></select>' +
            '<input data-k="url" value="' + esc(st.url || '') + '" placeholder="源文本，如 {thumb_url}/{body}" style="max-width:120px">' +
            '<input data-k="selector" value="' + esc(st.selector || '') + '" placeholder="正则">' +
            '<input data-k="replace" value="' + esc(st.replace || '') + '" placeholder="替换为">';
          break;
        case 'set-cookie':
          extra = '<input data-k="name" value="' + esc(st.name || '') + '" placeholder="cookie 名">' +
            '<input data-k="value" value="' + esc(st.value || '') + '" placeholder="值">';
          break;
        case 'set':
          extra = '<input data-k="field" value="' + esc(st.field || '') + '" placeholder="字段">' +
            '<input data-k="value" value="' + esc(st.value || '') + '" placeholder="值">';
          break;
      }
      d.innerHTML = sel + extra + '<button class="gv-stepx" title="删除"><i data-lucide="x"></i></button>';
      d.querySelector('.gv-sact').addEventListener('change', function () {
        st.action = this.value;
        Object.keys(st).forEach(function (k) { if (!['action'].includes(k)) delete st[k]; });
        renderSteps();
      });
      d.querySelectorAll('[data-k]').forEach(function (inp) {
        inp.addEventListener('change', function () { st[inp.getAttribute('data-k')] = inp.value; });
      });
      var fld = d.querySelector('.gv-sfld');
      if (fld) fld.addEventListener('change', function () { st.field = fld.value; });
      d.querySelector('.gv-stepx').addEventListener('click', function () {
        state.editSteps.splice(i, 1);
        renderSteps();
      });
      box.appendChild(d);
    });
    icons();
  }
  $('gv-stepadd').addEventListener('click', function () {
    if (!state.editSteps) state.editSteps = [];
    state.editSteps.push({ action: 'get', url: '{page_url}' });
    renderSteps();
  });
  $('gv-hostnew').addEventListener('click', function () {
    state.editRule = null;
    $('gv-hname').value = '';
    $('gv-hdom').value = '';
    $('gv-hnote').value = '';
    state.editSteps = [{ action: 'get', url: '{page_url}' }, { action: 'extract', field: 'image_url', selector: '', attr: 'src' }];
    renderSteps();
  });
  $('gv-hostdelete').addEventListener('click', function () {
    var id = state.editRule;
    var target = (state.hostRules || []).filter(function (h) { return (h.id || h.name) === id; })[0];
    if (!target) { $('gv-testout').textContent = '先选择要删除的规则'; return; }
    var name = target.name;
    var msg = target.builtin
      ? '“' + name + '”（内置，“' + id + '”）删除后本次运行生效，下次启动会自动恢复。确定删除？'
      : '确定删除自定义规则 “' + name + '”（“' + id + '”）？该操作不可恢复。';
    if (!confirm(msg)) return;
    App().HostDelete(id).then(function () {
      state.editRule = null;
      $('gv-hname').value = ''; $('gv-hdom').value = ''; $('gv-hnote').value = '';
      state.editSteps = [];
      renderSteps();
      toast('已删除规则 ' + name);
      openHostManager();
    }).catch(function (e) {
      $('gv-testout').textContent = '删除失败：' + e;
    });
  });

  $('gv-hostsave').addEventListener('click', function () {
    var domains = $('gv-hdom').value.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
    if (!$('gv-hname').value.trim() || !domains.length) { $('gv-testout').textContent = '名称和匹配域名必填'; return; }
    var cur = editedRule();
    var rule = {
      id: state.editRule || '',              // 有 id = 写回同一条规则文件，而不是新建 custom
      name: $('gv-hname').value.trim(),
      domains: domains,
      note: $('gv-hnote').value,
      steps: state.editSteps || [],
      enabled: cur ? !!cur.enabled : true   // 沿用当前勾选状态，保存不该把它改回启用
    };
    App().HostSave(rule).then(function () {
      $('gv-testout').innerHTML = cur
        ? '<b>✔ 已保存并热加载</b>（' + esc(state.editRule) + '，' + (rule.enabled ? '启用' : '已停用') + '）'
        : '<b>✔ 已新建规则</b>（' + esc(rule.name) + '.custom.json）';
      openHostManager();
    }).catch(function (err) { $('gv-testout').textContent = '保存失败：' + err; });
  });
  $('gv-hostdelete') /* created dynamically below */;
  $('gv-testrun').addEventListener('click', function () {
    var url = $('gv-testurl').value.trim();
    if (!url) return;
    var thumbEl = $('gv-testthumb');
    var thumb = (thumbEl && thumbEl.value || '').trim();
    var name = $('gv-hname').value.trim();
    var out = $('gv-testout');
    // save the editor content first so we test what we see
    var domains = $('gv-hdom').value.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
    var curR = editedRule();
    var rule = {
      id: state.editRule || '',
      name: name, domains: domains, note: $('gv-hnote').value,
      steps: state.editSteps || [], enabled: curR ? !!curR.enabled : true
    };
    out.textContent = '正在测试 ' + url + (thumb ? '（缩略图 ' + thumb + '）' : '') + ' …';
    App().HostSave(rule).then(function () {
      return App().HostTestByName(name, url, thumb);
    }).then(function (res) {
      if (res.ok) {
        out.innerHTML = '<b>✔ 测试成功</b>\nimage_url: ' + esc(res.imageUrl) + '\nfilename: ' + esc(res.filename || '(无)') + '\n\n' +
          (res.trace || []).map(esc).join('\n');
      } else {
        out.innerHTML = '<span class="bad">✘ 测试失败：' + esc(res.error) + '</span>\n\n' +
          (res.trace || []).map(esc).join('\n');
      }
    }).catch(function (err) { out.textContent = '测试失败：' + err; });
  });
  $('gv-hostimport').addEventListener('click', function () {
    var inp = document.createElement('input');
    inp.type = 'file'; inp.accept = '.json';
    inp.onchange = function () {
      var f = inp.files[0];
      if (!f) return;
      var fr = new FileReader();
      fr.onload = function () {
        try {
          var rule = JSON.parse(fr.result);
          App().HostSave(rule).then(function () { toast('已导入规则 ' + rule.name); openHostManager(); });
        } catch (e) { toast('JSON 解析失败'); }
      };
      fr.readAsText(f);
    };
    inp.click();
  });
  $('gv-hostexport').addEventListener('click', function () {
    var name = $('gv-hname').value.trim();
    var h = (state.hostRules || []).find(function (x) { return x.name === name; });
    if (!h) { toast('先在列表中选择要导出的规则'); return; }
    var blob = new Blob([JSON.stringify({ name: h.name, domains: h.domains, note: h.note, steps: h.steps }, null, 2)], { type: 'application/json' });
    var a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = h.name + '.json';
    a.click();
  });

  // ---------- top buttons ----------
  $('gv-m-export').addEventListener('click', function () {
    App().ExportData().then(function (p) { if (p) toast('已导出到 ' + p); });
  });
  $('gv-m-import').addEventListener('click', function () {
    App().ImportData().then(function (msg) { if (msg) { toast(msg); refreshAll(); loadImportLog(); } });
  });
  $('gv-m-opendir').addEventListener('click', function () {
    App().LoadSettings().then(function (s) { App().OpenPath(s.downloadDir); });
  });
  $('gv-m-quit').addEventListener('click', function () { App().Quit(); });
  $('gv-m-about').addEventListener('click', function () {
    var body = $('gv-aboutbody');
    body.style.display = 'none';
    body.textContent = '';
    $('gv-aboutmask').classList.add('open');
    icons();
  });
  function aboutBody() {
    var body = $('gv-aboutbody');
    body.style.display = 'block';
    return body;
  }
  $('gv-aboutver').addEventListener('click', function () {
    App().Version().then(function (v) {
      aboutBody().textContent = 'ViperRipper New\n版本: v' + v + '\nGo + Wails 重写版';
      icons();
    });
  });
  $('gv-abouthosts').addEventListener('click', function () {
    App().ListHosts().then(function (rules) {
      // 同一域名往往有多条规则（主规则 + og:image/源码搜索等兜底），按域名
      // 合并去重显示，否则支持列表会一下子多出很多重复条目
      var doms = {};
      (rules || []).forEach(function (r) {
        (r.domains || []).forEach(function (d) { doms[d] = true; });
      });
      var names = Object.keys(doms).sort(function (a, b) { return a.localeCompare(b); });
      var ruleN = (rules || []).length;
      var body = aboutBody();
      body.textContent = '当前支持 ' + names.length + ' 个图床（' + ruleN + ' 条规则，同域名的多条规则合并显示）:\n' + names.join('\n');
      icons();
    });
  });
  $('gv-aboutclose').addEventListener('click', function () { $('gv-aboutmask').classList.remove('open'); });
  $('gv-aboutmask').addEventListener('click', function (e) { if (e.target === this) this.classList.remove('open'); });

  // ---------- run button ----------
  function renderRunBtn() {
    var btn = $('gv-run');
    var running = Object.keys(state.running).length > 0;
    btn.innerHTML = running
      ? '<i data-lucide="square"></i><span>全部暂停</span>'
      : '<i data-lucide="play"></i><span>开始下载</span>';
    var hint = $('gv-selhint');
    if (hint) hint.textContent = state.sel.length > 1 ? '已选 ' + state.sel.length + ' 项（右键可批量开始/暂停/移除）' : '';
    icons();
  }
  $('gv-run').addEventListener('click', function () {
    var running = Object.keys(state.running).length > 0;
    if (running) {
      // with a multi-selection, stop only the selected running posts
      var selRun = selPosts().filter(function (p) { return state.running[p.id]; });
      if (selRun.length) App().StopPosts(selRun.map(function (p) { return p.id; }));
      else App().StopAll();
    } else {
      var sel = selPosts();
      if (sel.length) App().StartPosts(sel.map(function (p) { return p.id; }));
      else App().StartAll();
    }
  });

  // ---------- editable context menu (cut/copy/paste) ----------
  // WebView2 blocks the native menu in production; build our own so
  // right-click paste works in the add-links textarea and every input.
  var edMenu = document.createElement('div');
  edMenu.className = 'gv-menu';
  edMenu.style.zIndex = 200;
  edMenu.innerHTML = '<button data-ed="cut">剪切</button>' +
    '<button data-ed="copy">复制</button>' +
    '<button data-ed="paste">粘贴</button>' +
    '<button data-ed="selall">全选</button>';
  app.querySelector('.gv-main').appendChild(edMenu);
  var edTarget = null;
  document.addEventListener('contextmenu', function (e) {
    var t = e.target;
    if (t && (t.tagName === 'TEXTAREA' || (t.tagName === 'INPUT' && !/^checkbox|radio|number$/i.test(t.type || '')))) {
      e.preventDefault();
      edTarget = t;
      if (!t.id) t.id = 'gv-edtmp';
      edMenu.classList.add('open');
      var x = Math.min(e.clientX, window.innerWidth - 130);
      var y = Math.min(e.clientY, window.innerHeight - 130);
      edMenu.style.left = x + 'px';
      edMenu.style.top = y + 'px';
    } else if (!e.target.closest('#gv-ctx')) {
      edMenu.classList.remove('open');
    }
  });
  document.addEventListener('click', function (e) {
    if (!e.target.closest('.gv-menu') || e.target.closest('#gv-ctx')) edMenu.classList.remove('open');
  });
  edMenu.addEventListener('click', function (e) {
    var b = e.target.closest('button');
    if (!b || !edTarget) return;
    edMenu.classList.remove('open');
    var act = b.getAttribute('data-ed');
    var el = edTarget;
    el.focus();
    if (act === 'selall') { el.select(); return; }
    if (act === 'copy' || act === 'cut') {
      var s0 = el.selectionStart, s1 = el.selectionEnd;
      if (s0 === s1) { el.select(); }
      document.execCommand(act);
      return;
    }
    if (act === 'paste') {
      var fn = window.runtime && window.runtime.ClipboardGetText;
      if (!fn) return;
      fn().then(function (text) {
        if (!text) return;
        var s0 = el.selectionStart || 0, s1 = el.selectionEnd || 0;
        el.value = el.value.slice(0, s0) + text + el.value.slice(s1);
        el.selectionStart = el.selectionEnd = s0 + text.length;
        el.dispatchEvent(new Event('input', { bubbles: true }));
      });
    }
  });

  // ---------- column headers: desktop-standard boundary hit-testing ----------
  // mirrors the original JavaFX TableView behaviour: hovering a header
  // boundary (±5px) turns the cursor into col-resize and dragging there
  // resizes the LEFT column only (others slide); dragging a header cell
  // further than 6px reorders it with a ghost + insertion indicator.
  var colApplies = {};
  // opts（可选）：header/row 选择器、flexCol（哪一列吃剩余宽度，默认最后一列）。
  // 下载列表用默认值，右侧图片列表传 {header:'.gv-phead', row:'.gv-prow', flexCol:2}。
  function makeColsResizable(listEl, key, onSort, opts) {
    var opt = opts || {};
    var headerSel = opt.header || '.gv-hrow';
    var rowSel = opt.row || '.gv-row';
    var flexCol = (typeof opt.flexCol === 'number') ? opt.flexCol : -1;
    var hrow = listEl.querySelector(headerSel);
    if (!hrow) return;
    var cols = gridCells(hrow).length;
    function load(k, fallback) {
      try {
        var v = JSON.parse(localStorage.getItem(k));
        if (Array.isArray(v) && v.length === cols) return v;
      } catch (e) {}
      return fallback;
    }
    var widths = load('gvw-' + key, opt.defaults || gridCells(hrow).map(function (c) { return Math.max(40, c.offsetWidth || 120); }));
    var order = load('gvo-' + key, [...Array(cols).keys()]);
    if (order.slice().sort().join() !== [...Array(cols).keys()].join()) order = [...Array(cols).keys()];

    // 编辑模式的勾选框绝对定位、不占 grid 列：列宽/排序系统必须无视它，
    // 否则 children 索引偏移会把 order/宽度套错列
    function gridCells(container) {
      return [...container.children].filter(function (el) {
        return !(el.classList && el.classList.contains('gv-ckbox'));
      });
    }

    function apply() {
      var tpl = order.map(function (ci, pos) {
        var isFlex = flexCol >= 0 ? (ci === flexCol) : (pos === cols - 1);
        return isFlex ? 'minmax(60px, 1fr)' : widths[ci] + 'px';
      });
      hrow.style.gridTemplateColumns = tpl.join(' ');
      var hcells = gridCells(hrow);
      order.forEach(function (ci, pos) { hcells[ci].style.order = pos; });
      listEl.querySelectorAll(rowSel).forEach(function (r) {
        r.style.gridTemplateColumns = hrow.style.gridTemplateColumns;
        var cells = gridCells(r);
        order.forEach(function (ci, pos) { cells[ci].style.order = pos; });
      });
    }
    function save() {
      try {
        localStorage.setItem('gvw-' + key, JSON.stringify(widths));
        localStorage.setItem('gvo-' + key, JSON.stringify(order));
      } catch (e2) {}
    }
    function visualCells() {
      var cs = gridCells(hrow);
      return order.map(function (c) { return cs[c]; });
    }

    // floating ghost header label + full-height indicator line (JavaFX style)
    // 这两个是绝对定位的浮层，容器必须是定位元素（图片列表面板默认不是）
    try {
      if (getComputedStyle(listEl).position === 'static') listEl.style.position = 'relative';
    } catch (e0) {}
    var dropline = document.createElement('div');
    dropline.className = 'gv-dropline';
    var ghost = document.createElement('div');
    ghost.className = 'gv-ghost';
    listEl.appendChild(dropline);
    listEl.appendChild(ghost);
    var dragOffsetX = 0;
    function lineAt(clientX) {
      var cr = listEl.getBoundingClientRect();
      dropline.style.left = (clientX - cr.left) + 'px';
      dropline.style.display = 'block';
    }
    function lineAtRel(relX) {
      dropline.style.left = relX + 'px';
      dropline.style.display = 'block';
    }
    function ghostAt(clientX, ci) {
      var cr = listEl.getBoundingClientRect();
      var hr = hrow.getBoundingClientRect();
      ghost.style.top = (hr.top - cr.top) + 'px';
      ghost.style.left = (clientX - cr.left - dragOffsetX) + 'px';
      ghost.style.width = Math.max(40, widths[ci]) + 'px';
      ghost.style.display = 'flex';
      ghost.textContent = gridCells(hrow)[ci].textContent.trim();
    }
    function hideIndicators() {
      dropline.style.display = 'none';
      ghost.style.display = 'none';
    }
    function dropPos(x) {
      var cells = visualCells();
      var last = cells.length - 1;
      // 末尾是空白填充列时，不允许把别的列拖到它后面（会让内容跑到填充列之外）
      if (cells[last] && cells[last].classList.contains('gv-ph')) last -= 1;
      if (last < 0) last = 0;
      var drop = last;
      for (var pos = 0; pos <= last; pos++) {
        var r2 = cells[pos].getBoundingClientRect();
        if (x < r2.left + r2.width / 2) { drop = pos; break; }
      }
      return drop;
    }
    function dropLineRelX(x) {
      // line on the target column's left edge (right edge past its
      // midpoint); hidden while hovering the dragged column itself
      var cells = visualCells();
      var drop = dropPos(x);
      var from = cells.indexOf(cellRef.cell);
      if (drop === from) return null;
      var r2 = cells[drop].getBoundingClientRect();
      var cr = listEl.getBoundingClientRect();
      var before = x < r2.left + r2.width / 2;
      if (!before && drop < cells.length - 1) {
        return cells[drop + 1].getBoundingClientRect().left - cr.left;
      }
      return r2.left - cr.left;
    }
    var cellRef = { cell: null };

    // boundary hit test: visual index of the column LEFT of the hovered
    // boundary, or -1; last column's right edge excluded
    function hitBoundary(x) {
      var cells = visualCells();
      for (var pos = 0; pos < cols - 1; pos++) {
        var r2 = cells[pos].getBoundingClientRect();
        if (Math.abs(x - r2.right) <= 5) return pos;
      }
      return -1;
    }

    var bndCell = null;
    function setBndCell(cell) {
      if (bndCell === cell) return;
      if (bndCell) bndCell.classList.remove('gv-bnd-line');
      bndCell = cell;
      if (bndCell) bndCell.classList.add('gv-bnd-line');
    }
    hrow.addEventListener('mousemove', function (e) {
      if (hrow._mode) return;
      var b = hitBoundary(e.clientX);
      hrow.classList.toggle('gv-bnd', b >= 0);
      setBndCell(b >= 0 ? gridCells(hrow)[order[b]] : null);
    });
    hrow.addEventListener('mouseleave', function () {
      if (!hrow._mode) { hrow.classList.remove('gv-bnd'); setBndCell(null); }
    });

    hrow.addEventListener('mousedown', function (e) {
      if (e.button !== 0) return;
      var b = hitBoundary(e.clientX);
      if (b >= 0) {
        // ---- resize: left column changes, others slide ----
        e.preventDefault();
        hrow._mode = 'resize';
        hrow.classList.add('gv-resizing');
        var leftCi = order[b];
        var startX = e.clientX;
        var startW = widths[leftCi];
        lineAt(e.clientX);
        function mvR(ev) {
          widths[leftCi] = Math.max(40, startW + (ev.clientX - startX));
          lineAt(ev.clientX);
          apply();
        }
        function upR() {
          document.removeEventListener('mousemove', mvR);
          document.removeEventListener('mouseup', upR);
          hideIndicators();
          hrow._mode = null;
          hrow.classList.remove('gv-resizing');
          save();
        }
        document.addEventListener('mousemove', mvR);
        document.addEventListener('mouseup', upR);
        return;
      }
      // ---- reorder: press a header cell, move > 6px to pick it up ----
      var cell = e.target.closest('span');
      if (!cell || cell.parentElement !== hrow) return;
      if (cell.classList.contains('gv-ph')) return; // spacer column is inert
      e.preventDefault();
      var ci = Array.prototype.indexOf.call(gridCells(hrow), cell);
      cellRef.cell = cell;
      var startX = e.clientX;
      var dragging = false;
      dragOffsetX = e.clientX - cell.getBoundingClientRect().left;
      function mvO(ev) {
        if (!dragging && Math.abs(ev.clientX - startX) > 6) {
          dragging = true;
          hrow._mode = 'reorder';
          hrow.classList.add('gv-reordering');
          ghostAt(ev.clientX, ci);
        }
        if (!dragging) return;
        ghostAt(ev.clientX, ci);
        var lx = dropLineRelX(ev.clientX);
        if (lx == null) dropline.style.display = 'none';
        else lineAtRel(lx);
      }
      function upO(ev) {
        document.removeEventListener('mousemove', mvO);
        document.removeEventListener('mouseup', upO);
        hideIndicators();
        hrow._mode = null;
        hrow.classList.remove('gv-reordering');
        if (!dragging) {
          // plain click on a header cell → toggle sort (data-col survives reorders)
          if (onSort) onSort(+cell.getAttribute('data-col') || 0);
          return;
        }
        var drop = dropPos(ev.clientX);
        var from = order.indexOf(ci);
        order.splice(from, 1);
        if (drop > from) drop--;
        order.splice(drop, 0, ci);
        save();
        apply();
      }
      document.addEventListener('mousemove', mvO);
      document.addEventListener('mouseup', upO);
    });

    apply();
    colApplies[key] = apply;
  }

  // ---------- collapsibles ----------
  $('gv-infotoggle').addEventListener('click', function () {
    app.setAttribute('data-info', app.getAttribute('data-info') === 'off' ? 'on' : 'off');
  });
  // drag the info strip's top edge to resize its height (persists)
  (function () {
    var rz = $('gv-inforz'), ib = $('gv-ibody'), info = $('gv-info'), dlist = $('gv-dlist');
    if (!rz || !ib) return;
    // 信息栏浮在列表上方（absolute），调整高度不会推动列表本身。
    // 滚动余量（padding-bottom）只增不减：减小滚动高度会让浏览器
    // 回收滚动位置（clamp），把行顶走；缩小时先挂账，等用户主动
    // 滚离底部时再悄悄收掉（收的时候算好量，不会触发 clamp）。
    var lastPad = 0;
    function syncPad(allowShrink) {
      if (!dlist || !info) return;
      var want = info.offsetHeight;
      if (want >= lastPad) { // 变大 / 初始化：安全，直接同步
        dlist.style.paddingBottom = want + 'px';
        lastPad = want;
        return;
      }
      if (!allowShrink) return; // 变小：拖动中先不动，行保持钉住
      var cut = lastPad - want;
      var maxNow = dlist.scrollHeight - dlist.clientHeight;
      if (dlist.scrollTop <= maxNow - cut) { // 收缩后也夹不住当前滚动位置
        dlist.style.paddingBottom = want + 'px';
        lastPad = want;
      }
    }
    // 用户滚动时若还有挂账的多余余量，随时找机会收掉
    if (dlist) dlist.addEventListener('scroll', function () { syncPad(true); });
    try {
      var saved = parseInt(localStorage.getItem('gv-infoh'), 10);
      if (saved >= 80) ib.style.height = saved + 'px';
    } catch (e) {}
    requestAnimationFrame(function () { syncPad(false); });
    rz.addEventListener('mousedown', function (e) {
      if (e.button !== 0) return;
      e.preventDefault();
      var startY = e.clientY, startH = ib.offsetHeight;
      app.setAttribute('data-info', 'on');
      function mv(ev) {
        var h = startH + (startY - ev.clientY); // pull up → taller
        h = Math.max(80, Math.min(window.innerHeight * 0.7, h));
        ib.style.height = h + 'px';
        syncPad(false);
      }
      function up() {
        document.removeEventListener('mousemove', mv);
        document.removeEventListener('mouseup', up);
        syncPad(false);
        try { localStorage.setItem('gv-infoh', String(ib.offsetHeight)); } catch (e2) {}
      }
      document.addEventListener('mousemove', mv);
      document.addEventListener('mouseup', up);
    });
    // 收起/展开按钮：用户明确想改变可见区域，允许一次性收缩
    $('gv-infotoggle').addEventListener('click', function () { setTimeout(function () { syncPad(true); }, 0); });
  })();
  $('gv-rhandle').addEventListener('click', function () {
    var on = app.getAttribute('data-prev') === 'off'; // 反一下：当前关 → 开
    app.setAttribute('data-prev', on ? 'on' : 'off');
    var ic = $('gv-rhandle').querySelector('i,svg');
    if (ic) ic.style.transform = on ? '' : 'rotate(180deg)';
    // 刚展开预览栏时把当前帖子的图补上（否则会是一块空白）
    if (on && state.curPost) autoPickPreview(state.curPost, state.images[state.curPost] || []);
    // 快捷按钮也要记住，否则重启又回到设置里的值
    App().LoadSettings().then(function (s) {
      s.showPreview = on;
      return App().SaveSettings(s);
    }).then(function () {
      if (state.settings) state.settings.showPreview = on;
      var cb = $('gv-s-preview');
      if (cb) cb.checked = on;
      toast(on ? '已显示图片预览栏' : '已隐藏图片预览栏');
    }).catch(function () {});
  });
  app.setAttribute('data-prev', 'on');

  // applyEditMode 给当前列表装上编辑模式的勾选框：表头全选 + 每行一个。
  // rows 与 keys 同序（keys[i] 对应 rows[i]），key 形如 'p12'（楼层行）/
  // 't3'（连接行）。普通列表和连接的管理视图共用这一份，两处的连接行、
  // 楼层行都能勾选、删除、导出链接。
  function applyEditMode(list, rows, keys) {
    state.visKeys = keys.slice();
    list.classList.toggle('editing', state.editMode);
    var hrow0 = list.querySelector('.gv-hrow');
    if (hrow0) {
      var oldHck = hrow0.querySelector('.gv-ckbox');
      if (oldHck) oldHck.remove();
      if (state.editMode) {
        var allCk = document.createElement('input');
        allCk.type = 'checkbox';
        allCk.className = 'gv-ckbox';
        var picked = keys.filter(function (k) { return state.editSel.indexOf(k) >= 0; }).length;
        allCk.checked = keys.length > 0 && picked === keys.length;
        allCk.indeterminate = picked > 0 && picked < keys.length;
        allCk.title = allCk.checked ? '取消全选' : '全选';
        allCk.addEventListener('click', function (e) {
          e.stopPropagation();
          if (allCk.checked) {
            keys.forEach(function (k) { if (state.editSel.indexOf(k) < 0) state.editSel.push(k); });
          } else {
            // 只取消当前视图里的条目，别的视图的勾选留着
            state.editSel = state.editSel.filter(function (k) { return keys.indexOf(k) < 0; });
          }
          renderPosts();
          updateEditDelBtn();
        });
        hrow0.insertBefore(allCk, hrow0.firstChild);
      }
    }
    if (!state.editMode) return;
    rows.forEach(function (row, i) {
      var key = keys[i];
      if (!row || !key) return;
      row.setAttribute('data-tkey', key); // 点击代理靠它认行
      // 编辑模式：行高亮跟随勾选状态（点行即勾选，不必瞄准小框）
      row.classList.toggle('sel', state.editSel.indexOf(key) >= 0);
      var ck = document.createElement('input');
      ck.type = 'checkbox';
      ck.className = 'gv-ckbox';
      ck.checked = state.editSel.indexOf(key) >= 0;
      ck.addEventListener('click', function (e) {
        e.stopPropagation();
        var i2 = state.editSel.indexOf(key);
        if (ck.checked && i2 < 0) state.editSel.push(key);
        if (!ck.checked && i2 >= 0) state.editSel.splice(i2, 1);
        row.classList.toggle('sel', ck.checked);
        updateEditDelBtn();
      });
      row.insertBefore(ck, row.firstChild);
    });
  }

  // 编辑模式点行 = 切换勾选：统一在容器上代理（捕获阶段先跑并截断，
  // 这样连接行不会顺带进入管理视图、楼层行也不会触发选中）
  $('gv-dlist').addEventListener('click', function (e) {
    if (!state.editMode) return;
    if (e.target.closest('.gv-ckbox')) return;
    if (e.target.closest('.gv-act') || e.target.closest('.gv-thread-close')) return;
    var row = e.target.closest('.gv-row');
    if (!row) return;
    var key = row.getAttribute('data-tkey');
    if (!key) return;
    e.stopPropagation();
    editRowClick(key, e);
  }, true);

  // ---------- Download 列表批量编辑（勾选删除/导出链接） ----------
  // 编辑模式点行：普通点击=切换勾选，Ctrl+点击=切换（不碰其它行），
  // Shift+点击=把锚点到当前行的整段勾上。行高亮跟随勾选。
  function editRowClick(key, e) {
    var has = state.editSel.indexOf(key) >= 0;
    if (e.shiftKey && state.editAnchor) {
      var a = state.visKeys.indexOf(state.editAnchor), b = state.visKeys.indexOf(key);
      if (a >= 0 && b >= 0) {
        for (var i = Math.min(a, b); i <= Math.max(a, b); i++) {
          var k2 = state.visKeys[i];
          if (state.editSel.indexOf(k2) < 0) state.editSel.push(k2);
        }
        renderPosts();
        updateEditDelBtn();
        return;
      }
    }
    state.editAnchor = key; // 普通/Ctrl 点击更新范围锚点
    if (has) state.editSel.splice(state.editSel.indexOf(key), 1);
    else state.editSel.push(key);
    renderPosts();
    updateEditDelBtn();
  }
  function updateEditDelBtn() {
    var on = state.editMode && state.editSel.length;
    var del = $('gv-editdel'), exp = $('gv-editexp');
    if (del) del.style.display = on ? '' : 'none';
    if (exp) exp.style.display = on ? '' : 'none';
    // 勾单行时不整表重绘，所以表头全选框的状态要在这里手动同步
    var hck = $('gv-dlist').querySelector('.gv-hrow .gv-ckbox');
    if (hck && state.visKeys.length) {
      var picked = state.visKeys.filter(function (k) { return state.editSel.indexOf(k) >= 0; }).length;
      hck.checked = picked === state.visKeys.length;
      hck.indeterminate = picked > 0 && picked < state.visKeys.length;
    }
  }
  // 编辑模式里勾选的条目 key：'p'+postRowId / 't'+threadRowId
  function editSelKeys() {
    var pids = [], tids = [];
    state.editSel.forEach(function (k) {
      if (k.charAt(0) === 'p') pids.push(+k.slice(1)); else tids.push(+k.slice(1));
    });
    return { pids: pids, tids: tids };
  }
  $('gv-editbtn').addEventListener('click', function () {
    state.editMode = !state.editMode;
    state.editSel = [];
    state.editAnchor = null;
    updateEditDelBtn();
    renderPosts();
    toast(state.editMode ? '编辑模式：勾选要删除的条目（表头可全选）' : '已退出编辑模式');
  });
  $('gv-editdel').addEventListener('click', function () {
    if (!state.editSel.length) return;
    var k = editSelKeys();
    var pids = k.pids, tids = k.tids;
    var n = pids.length + tids.length;
    // 连接（黄色行）是"删连接"：它下面的楼层跟着一起删，不会散落到列表里
    var floorN = 0;
    tids.forEach(function (id) {
      var t = state.threads.find(function (x) { return x.id === id; });
      if (t) floorN += threadMeta(t).floors.length;
    });
    var msg = '确定删除 ' + n + ' 个条目？';
    if (tids.length) msg += '\n\n其中 ' + tids.length + ' 个主题会连同其下 ' + floorN + ' 个楼层一起删除';
    if (pids.length) msg += '\n另有 ' + pids.length + ' 个楼层/条目直接删除';
    if (!confirm(msg + '（已下载的文件不会动）。')) return;
    var jobs = [];
    if (pids.length) jobs.push(App().DeletePosts(pids));
    if (tids.length) jobs.push(App().DeleteThreads(tids));
    Promise.all(jobs).then(function () {
      if (tids.indexOf(state.viewThread) >= 0) state.viewThread = 0;
      state.editSel = [];
      state.sel = [];
      updateEditDelBtn();
      refreshAll();
      toast('已删除 ' + n + ' 项' + (floorN ? '（含 ' + floorN + ' 个楼层）' : ''));
    }).catch(function (err) { toast('删除失败：' + err); });
  });
  // 导出选中条目的链接（主题行=主题链接，帖子行=帖子链接），复制到剪贴板
  $('gv-editexp').addEventListener('click', function () {
    if (!state.editSel.length) return;
    var k = editSelKeys();
    var urls = [];
    state.threads.forEach(function (t) {
      if (k.tids.indexOf(t.id) >= 0 && t.url) urls.push(t.url);
    });
    state.posts.forEach(function (p) {
      if (k.pids.indexOf(p.id) >= 0 && p.postUrl) urls.push(p.postUrl);
    });
    if (!urls.length) { toast('选中条目没有可用链接'); return; }
    App().WriteClipboard(urls.join('\n')).then(function () {
      toast('已复制 ' + urls.length + ' 条链接');
    });
  });

  // ---------- Wails events ----------
  // followRunningPost 没有选中帖子时，让右侧信息栏跟随一个正在下载的帖子。
  // 下载速度很快的图可能来不及产生进度事件，所以不能只靠 img:progress。
  function followRunningPost() {
    if (state.curPost) return;
    // 已经跟随的帖子还在跑就不换目标，避免多个帖子并发时来回跳
    if (state.followPost && state.running[state.followPost]) return;
    var pid = 0;
    Object.keys(state.running || {}).forEach(function (k) {
      if (!pid && state.running[k]) pid = +k;
    });
    if (!pid || pid === state.followPost) return;
    state.followPost = pid;
    refreshImages(pid);
  }

  function bindEvents() {
    if (!window.runtime) return;
    window.runtime.EventsOn('posts:dirty', function (ids) {
      App().RunningPosts().then(function (running) {
        state.running = {};
        (running || []).forEach(function (id) { state.running[id] = true; });
        renderRunBtn();
        followRunningPost();
      });
      App().RefreshPosts(ids).then(function (posts) {
        (posts || []).forEach(function (np) {
          var i = state.posts.findIndex(function (p) { return p.id === np.id; });
          if (i >= 0) state.posts[i] = np;
        });
        // 下载时这个事件每 100ms 一次，整表重建 + 重排既费性能又抢点击，
        // 合并成 ~250ms 一次；按下鼠标时 renderPosts 会自己延后
        schedulePostsRender();
        if (state.sel.some(function (id) { return ids.indexOf(id) >= 0; }) || ids.indexOf(state.curPost) >= 0) showInfoPanel();
      });
    });
    window.runtime.EventsOn('img:progress', applyProgress);
    window.runtime.EventsOn('floors:page', onFloorsPage);

    // 全部任务完成：按需响一声（WebAudio 双音，不用打包音频文件）
    window.runtime.EventsOn('app:finished', function () {
      toast('下载任务已完成');
      if (!state.settings || !state.settings.soundOnDone) return;
      playDoneSound();
    });

    // 自动关机已安排：给个能看见的提示（倒计时 60 秒，shutdown /a 可取消）
    window.runtime.EventsOn('app:shutdown', function (msg) {
      toast(msg || '全部任务已完成：60 秒后自动关机');
    });

    window.runtime.EventsOn('images:dirty', function (ids) {
      // 选中了帖子就只看它；没选中时跟随正在下载的帖子（右侧信息栏不空着）
      var pid = state.curPost || state.followPost;
      if (!pid) return;
      refreshImages(pid);
    });
    window.runtime.EventsOn('log:new', function (lines) {
      (lines || []).forEach(function (l) { addLogLine(l.level, l.msg); });
    });

    // 带楼层链接但主题有多个含图楼层：已加入主题列表，打开楼层选择器并预勾选该楼层
    window.runtime.EventsOn('links:floorpick', function (item) {
      App().ListThreads().then(function (ts) {
        var t = (ts || []).find(function (x) { return x.threadId === item.threadId; });
        if (t) { openFloorPicker(t, item.postId); return; }
        toast('主题 #' + item.threadId + ' 已加入下载列表（点击选择楼层）');
      });
    });

    // 下载中点 × ：后端拦下关闭并要求前端确认（最小化继续 / 强制退出 / 取消）
    window.runtime.EventsOn('app:closebusy', function () {
      $('gv-closemask').classList.add('open');
      // 回执：告诉后端确认框确实弹出来了（否则连续点 × 会被当成弹窗失败而直接退出）
      if (App().NotifyCloseAskShown) App().NotifyCloseAskShown();
    });

    // 人机验证（Cloudflare）：某图床返回“你是人吗”页面时，提示用户开验证窗口
    window.runtime.EventsOn('cf:need', function (pl) {
      if (!pl || !pl.host) return;
      state.cfHost = pl.host;
      state.cfURL = pl.url || '';
      $('gv-cfhost').textContent = pl.host;
      $('gv-cfbar').classList.add('open');
      icons();
    });
    // 验证通过：后端已收下凭证并自动重下失败的图
    window.runtime.EventsOn('cf:passed', function (pl) {
      $('gv-cfbar').classList.remove('open');
      var h = (pl && pl.host) ? pl.host + ' ' : '';
      toast('已通过 ' + h + '验证，正在重新下载之前失败的图片');
    });
    // 提示条上的两个按钮
    var cfOpen = $('gv-cfopen'), cfLater = $('gv-cfclose');
    if (cfOpen) cfOpen.addEventListener('click', function () {
      var host = state.cfHost || '';
      if (!host) return;
      cfOpen.disabled = true;
      App().OpenChallengeWindow(host, state.cfURL || '').then(function (err) {
        cfOpen.disabled = false;
        if (err) toast(err);
        else toast('已打开验证窗口：点完“确认是人类”后关闭窗口即可');
      }).catch(function (err) { cfOpen.disabled = false; toast(String(err)); });
    });
    if (cfLater) cfLater.addEventListener('click', function () {
      $('gv-cfbar').classList.remove('open');
    });
  }

  // ---------- import log ----------
  var impSumTimer = null;
  // background import queue: batches run one at a time so the dialog closes
  // instantly and results stream into the summary strip / import panel
  function runImportQueue() {
    if (state.importBusy) return;
    var job = state.importQueue.shift();
    if (!job) return;
    state.importBusy = true;
    var queued = state.importQueue.reduce(function (n, j) { return n + j.count; }, 0);
    showImportBusy(job.count, queued);
    App().AddLinks(job.text).then(function (res) {
      recordImport(res);
    }, function (err) {
      toast('导入失败：' + err);
      showImportSummary({ posts: 0, threads: 0, dups: 0, failed: job.count, items: [] });
    }).then(function () {
      state.importBusy = false;
      refreshAll();
      runImportQueue();
    });
  }
  function showImportBusy(count, queued) {
    var el = $('gv-impsum');
    if (impSumTimer) { clearTimeout(impSumTimer); impSumTimer = null; }
    el.innerHTML = '<span class="th">正在导入 <b>' + count + '</b> 条链接…'
      + (queued ? '（还有 <b>' + queued + '</b> 条排队）' : '') + '</span>';
    el.classList.add('on');
  }
  function recordImport(res) {
    if (!res || !res.items || !res.items.length) return;
    var d = new Date();
    var at = ('0' + d.getHours()).slice(-2) + ':' + ('0' + d.getMinutes()).slice(-2) + ':' + ('0' + d.getSeconds()).slice(-2);
    state.importLog.unshift({ at: at, items: res.items });
    state.importSel = [];
    state.importAnchor = -1;
    renderImportPanel();
    showImportSummary(res);
    App().SaveImportLog(state.importLog).catch(function (err) { toast('导入历史保存失败：' + err); });
  }
  function showImportSummary(res) {
    var el = $('gv-impsum');
    var parts = [];
    if (res.posts) parts.push('<span class="ok"><b>' + res.posts + '</b> 单帖</span>');
    if (res.threads) parts.push('<span class="th"><b>' + res.threads + '</b> 多楼层主题</span>');
    if (res.dups) parts.push('<span class="dup"><b>' + res.dups + '</b> 重复忽略</span>');
    if (res.failed) parts.push('<span class="fail"><b>' + res.failed + '</b> 失败</span>');
    if (!parts.length) parts.push('<span>没有可识别的链接</span>');
    el.innerHTML = parts.join('');
    el.classList.add('on');
    if (impSumTimer) clearTimeout(impSumTimer);
    impSumTimer = setTimeout(function () { el.classList.remove('on'); impSumTimer = null; }, 60000);
  }
  function renderImportPanel() {
    var list = $('gv-implist');
    if (!list) return;
    list.innerHTML = '';
    if (!state.importLog.length) {
      list.innerHTML = '<div style="padding:24px;color:var(--gv-fg2);text-align:center">还没有导入历史</div>';
      return;
    }
    state.importLog.forEach(function (batch, bi) {
      var sum = { ok: 0, thread: 0, dup: 0, fail: 0 };
      batch.items.forEach(function (it) { sum[it.status] = (sum[it.status] || 0) + 1; });
      var head = document.createElement('div');
      head.className = 'gv-impbatch';
      head.textContent = batch.at + ' — ' + batch.items.length + ' 条：'
        + sum.ok + ' 成功 · ' + sum.thread + ' 多楼层 · ' + sum.dup + ' 重复 · ' + sum.fail + ' 失败';
      list.appendChild(head);
      batch.items.forEach(function (it, ii) {
        var id = bi + ':' + ii;
        var row = document.createElement('div');
        row.className = 'gv-improw' + (state.importSel.indexOf(id) >= 0 ? ' sel' : '');
        row.setAttribute('data-iid', id);
        var noteTxt = statusText(it.status) + (it.note ? ' — ' + it.note : '');
        row.innerHTML = '<span class="gv-impdot ' + it.status + '"></span>'
          + '<span class="gv-impurl" title="' + esc(it.url) + '">' + esc(it.url) + '</span>'
          + '<span class="gv-impnote" title="' + esc(noteTxt) + '">' + esc(noteTxt) + '</span>';
        // selection on mousedown: re-rendering the whole list on click made
        // ctrl/shift feel dead (list rebuilt under the pointer, scroll jumps) —
        // update row classes in place instead
        row.addEventListener('mousedown', function (e) {
          if (e.button !== 0) return;
          e.preventDefault();
          var ids = [];
          state.importLog.forEach(function (b, b2) { b.items.forEach(function (_, i2) { ids.push(b2 + ':' + i2); }); });
          // anchor is a string id ("0:3") — comparing it with >= 0 is always
          // false, so the shift branch never ran; test against -1 instead
          if (e.shiftKey && state.importAnchor !== -1) {
            var a = ids.indexOf(state.importAnchor), b = ids.indexOf(id);
            if (a >= 0 && b >= 0) {
              var range = ids.slice(Math.min(a, b), Math.max(a, b) + 1);
              if (e.ctrlKey) range.forEach(function (rid) { if (state.importSel.indexOf(rid) < 0) state.importSel.push(rid); });
              else state.importSel = range;
            }
          } else if (e.ctrlKey) {
            var i = state.importSel.indexOf(id);
            if (i >= 0) state.importSel.splice(i, 1); else state.importSel.push(id);
            state.importAnchor = id;
          } else {
            state.importSel = [id];
            state.importAnchor = id;
          }
          list.querySelectorAll('.gv-improw').forEach(function (r) {
            r.classList.toggle('sel', state.importSel.indexOf(r.getAttribute('data-iid')) >= 0);
          });
        });
        row.addEventListener('contextmenu', function (e) {
          e.preventDefault();
          if (state.importSel.indexOf(id) < 0) { state.importSel = [id]; state.importAnchor = id; }
          list.querySelectorAll('.gv-improw').forEach(function (r) {
            r.classList.toggle('sel', state.importSel.indexOf(r.getAttribute('data-iid')) >= 0);
          });
          openImpMenu(e.clientX, e.clientY);
        });
        list.appendChild(row);
      });
    });
  }
  function statusText(st) {
    return st === 'ok' ? '成功' : st === 'thread' ? '多楼层主题' : st === 'dup' ? '重复忽略' : '失败';
  }
  function selectedImportURLs() {
    var out = [];
    state.importLog.forEach(function (b, bi) {
      b.items.forEach(function (it, ii) {
        if (state.importSel.indexOf(bi + ':' + ii) >= 0) out.push(it.url);
      });
    });
    return out;
  }
  var impMenu = null;
  function openImpMenu(x, y) {
    if (!impMenu) {
      impMenu = document.createElement('div');
      impMenu.className = 'gv-impmenu';
      impMenu.innerHTML = '<button data-m="copy">复制链接地址</button>'
        + '<button data-m="copyall">复制全部链接</button>';
      impMenu.addEventListener('click', function (e) {
        var m = e.target.getAttribute('data-m');
        impMenu.classList.remove('open');
        if (m === 'copy') {
          var urls = selectedImportURLs();
          if (urls.length) App().WriteClipboard(urls.join('\n')).then(function () { toast('已复制 ' + urls.length + ' 条链接'); });
        } else if (m === 'copyall') {
          var all = [];
          state.importLog.forEach(function (b) { b.items.forEach(function (it) { all.push(it.url); }); });
          if (all.length) App().WriteClipboard(all.join('\n')).then(function () { toast('已复制 ' + all.length + ' 条链接'); });
        }
      });
      document.body.appendChild(impMenu);
    }
    impMenu.classList.add('open');
    impMenu.style.left = Math.min(x, window.innerWidth - 160) + 'px';
    impMenu.style.top = Math.min(y, window.innerHeight - 90) + 'px';
  }
  document.addEventListener('click', function (e) { if (impMenu && !e.target.closest('.gv-impmenu')) impMenu.classList.remove('open'); });

  // ---------- init ----------
  function refreshAll() {
    Promise.all([App().ListPosts(), App().ListThreads(), App().RunningPosts()]).then(function (r) {
      state.posts = r[0] || [];
      state.threads = r[1] || [];
      state.running = {};
      (r[2] || []).forEach(function (id) { state.running[id] = true; });
      renderPosts();
      renderRunBtn();
      if (state.curPost) showInfoPanel();
    });
  }

  function loadImportLog() {
    App().LoadImportLog().then(function (recs) {
      state.importLog = recs || [];
      renderImportPanel();
    }).catch(function () {});
  }

  window.addEventListener('DOMContentLoaded', function () {
    App().LoadSettings().then(function (s) {
      state.settings = s;
      app.setAttribute('data-prev', s.showPreview ? 'on' : 'off');
      // 恢复上次的排序：普通列表、主题管理视图各一份
      function clampCol(v) { return (typeof v === 'number' && v >= -1 && v <= 5) ? v : -1; }
      function clampDir(v) { return (v === 1 || v === -1) ? v : 1; }
      state.normalSort = { col: clampCol(s.sortCol), dir: clampDir(s.sortDir) };
      state.threadSort = { col: clampCol(s.threadSortCol), dir: clampDir(s.threadSortDir) };
      state.sortView = 'dl';
      state.sort = { col: state.normalSort.col, dir: state.normalSort.dir };
      renderPosts(undefined, true); // forceSort：按恢复的排序真的重排
    });
    refreshAll();
    loadImportLog();
    App().ListLogs().then(function (logs) {
      (logs || []).slice().reverse().forEach(function (l) { addLogLine(l.level, l.msg); });
    });
    // persist main-window size (debounced; Go reads the real outer size)
    var winSvTimer = null;
    window.addEventListener('resize', function () {
      if (winSvTimer) clearTimeout(winSvTimer);
      winSvTimer = setTimeout(function () {
        winSvTimer = null;
        try { App().SaveWindowSize(); } catch (e) {}
      }, 600);
    });
    window.__mark.push('dcl-entered');
    // 把界面语言告诉后端：原生文件对话框的标题由后端弹，需要知道用哪种语言
    try { if (window.I18N && App().SetUILang) App().SetUILang(I18N.lang); } catch (e) {}
    bindEvents();
    bindLightbox();
    window.__mark.push('bindEvents-ok');
    icons();
    makeColsResizable($('gv-dlist'), 'dl', function (col) {
      if (state.sort.col === col) state.sort.dir = -state.sort.dir;
      // Progress / Status 列首次点击即"大的在前"（降序），再点反向
      else state.sort = { col: col, dir: (col === 2 || col === 3) ? -1 : 1 };
      // forceSort：用户明确要求重排，不能沿用"保持行位置"的旧顺序
      renderPosts(undefined, true);
      persistSort(); // 排序写进设置，重启后保持
    });
    window.__mark.push('cols-ok');
  });
  // 右侧图片列表的表头：与下载列表同一套（拖边界调宽 / 拖动换列序 / 点击排序）。
  // 末尾留一个空白填充列（索引 7）吃剩余宽度——和上面那张表完全一致，
  // 不能用中间的"图片名"当弹性列，否则拖它右边的边界会变成"压缩弹性列"，
  // 看起来就是"往右拖、内容反而向左跑"。
  makeColsResizable($('gv-phowrap'), 'pho', setPhotoSort, {
    header: '.gv-phead',
    row: '.gv-prow',
    flexCol: 7,
    defaults: [68, 36, 300, 260, 56, 104, 84, 60]
  });
})();
