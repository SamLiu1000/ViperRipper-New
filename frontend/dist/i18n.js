/* ViperRipper New — 轻量 i18n
 * 设计：中文原文就是键。zh 模式原样返回（零开销）；en 模式按短语表替换。
 * 覆盖三处文案来源：静态 HTML（启动时遍历一次）、innerHTML/textContent 注入
 * （拦 setter）、属性（title/placeholder 等）。
 * 切换语言 = 写 localStorage + 重新加载窗口（状态简单、不会出现半翻译）。
 */
(function () {
  'use strict';

  var KEY = 'vp.lang';
  var lang = 'en'; // 默认英文；用户点过切换则以 localStorage 为准
  try {
    var saved = localStorage.getItem(KEY);
    if (saved === 'en' || saved === 'zh') lang = saved;
  } catch (e) {}

  /* ---------------- 短语表：中文 → English ----------------
   * 顺序无关，运行时按"长词优先"匹配；同一短语只写一次。
   */
  var EN = {
    // ===== 菜单 / 工具栏 =====
    '文件': 'File',
    '选项': 'Options',
    '帮助': 'Help',
    '添加链接': 'Add links',
    '导入数据 (JSON)': 'Import data (JSON)',
    '导出全部数据 (JSON)': 'Export all data (JSON)',
    '打开下载目录': 'Open download folder',
    '退出': 'Exit',
    '设置': 'Settings',
    '规则管理': 'Rule manager',
    '关于': 'About',
    '代理': 'Proxy',
    '代理开关：开启后新开始的下载使用「论坛」页填写的代理地址；进行中的连接需停止后重新开始才会切换':
      'Proxy switch: new downloads use the proxy set on the Forum tab; running tasks must be stopped and restarted to switch',
    '搜索收藏（标题、楼层内容…）': 'Search (title, post content…)',
    '导入历史': 'Import history',
    '编辑': 'Edit',
    '删除选中': 'Delete selected',
    '导出选中链接': 'Export selected links',
    '下载列表': 'Downloads',
    '图片列表': 'Images',
    '日志': 'Log',
    '开始下载': 'Start',
    '全部暂停': 'Pause all',
    '暂停': 'Pause',
    '继续下载': 'Resume',
    '停止': 'Stop',
    '添加链接…': 'Add links…',

    // ===== 下载列表表头 / 列 =====
    '标题': 'Title',
    '楼层号': 'Post #',
    '进度': 'Progress',
    '状态': 'Status',
    '添加日期': 'Added',
    '图床': 'Host',
    '链接': 'URL',
    '楼层数': 'Posts',
    '图片名': 'File name',
    '图片链接': 'Image URL',
    '大小': 'Size',
    '下载进度': 'Progress',
    '序号': '#',
    '未知图床': 'Unknown host',
    '尚未下载': 'Not downloaded yet',
    '保存到': 'Saved to',

    // ===== 状态 =====
    '未开始': 'Not started',
    '下载中': 'Downloading',
    '已暂停': 'Paused',
    '已完成': 'Done',
    '已下载': 'Downloaded',
    '已下载 / 下载中': 'Downloaded / Downloading',
    '重试中': 'Retrying',
    '进行中': 'Running',
    '失败': 'Failed',
    '成功': 'OK',
    '已加载': 'Loaded',
    '正在下载': 'Downloading',
    '个链接失效': 'links dead',
    '磁盘：统计中…': 'Disk: measuring…',
    '磁盘：': 'Disk: ',
    '个文件 · ': 'files · ',

    // ===== 列表操作 / 右键菜单 =====
    '查看楼层 / 选图': 'View posts / pick images',
    '重命名': 'Rename',
    '更改下载目录': 'Change download folder',
    '打开文件夹': 'Open folder',
    '在浏览器打开': 'Open in browser',
    '复制': 'Copy',
    '复制条目链接': 'Copy item link',
    '复制本地路径': 'Copy local path',
    '复制标题': 'Copy title',
    '复制页面链接': 'Copy page link',
    '复制图片直链': 'Copy image URL',
    '复制楼层链接': 'Copy post link',
    '复制名称': 'Copy name',
    '复制链接地址': 'Copy link address',
    '复制全部链接': 'Copy all links',
    '标记颜色': 'Mark color',
    '清除标记': 'Clear mark',
    '删除条目': 'Delete item',
    '删除主题': 'Delete theme',
    '删除主题（连同楼层）': 'Delete theme (with posts)',
    '清除解析缓存': 'Clear resolve cache',
    '重载预览图': 'Reload preview',
    '清除缓存': 'Clear cache',
    '清除已完成': 'Clear completed',
    '清除失败': 'Clear failed',
    '清除全部': 'Clear all',
    '清除': 'Clear',
    '重试失败项': 'Retry failed',
    '剪切': 'Cut',
    '粘贴': 'Paste',
    '全选': 'Select all',
    '取消全选': 'Unselect all',
    '红色': 'Red',
    '橙色': 'Orange',
    '黄色': 'Yellow',
    '绿色': 'Green',
    '蓝色': 'Blue',
    '紫色': 'Purple',

    // ===== 楼层选择器 =====
    '选择楼层': 'Select posts',
    '共 ': 'Total ',
    ' 层 · 有图 ': ' posts · with images ',
    ' 层': ' posts',
    '层': 'posts',
    '跳转': 'Go to',
    '楼层号': 'Post #',
    '范围勾选': 'Range select',
    '从': 'From',
    '到': 'To',
    '勾选': 'Select',
    '全选有图': 'Select all with images',
    '反选': 'Invert',
    '已选 ': 'Selected ',
    ' 层 / ': ' posts / ',
    ' 图': ' images',
    '已加载 0 层…': 'Loaded 0 posts…',
    '加载更多': 'Load more',
    '加载全部': 'Load all',
    '加载完成，共 ': 'Finished, ',
    '楼层': 'Post',
    '悬停或勾选楼层查看缩略图': 'Hover or select a post to see thumbnails',
    '无图楼层': 'Post without images',
    '确定下载': 'Download selected',
    '取消': 'Cancel',
    '正在获取楼层…': 'Loading posts…',
    '获取失败：': 'Failed to load: ',
    '楼层加载失败：': 'Post loading failed: ',
    '该主题还没有已加入的楼层': 'No posts added to this theme yet',
    '主题 #': 'Theme #',
    ' — 选择楼层': ' — select posts',
    '（无图楼层）': ' (no images)',
    '缓存已清除，正在重新抓取…': 'Cache cleared, refetching…',
    '楼层缓存已清除': 'Post cache cleared',
    '确定清除该主题的楼层缓存吗？下次打开楼层选择器将重新抓取。':
      'Clear the post cache of this theme? It will be fetched again next time.',
    '删除已缓存的楼层列表，下次打开重新抓取': 'Delete the cached post list; it will be fetched again next time',
    '这是单帖，没有可选楼层。要下载整个主题，请添加主题链接。':
      'This is a single post with no selectable posts. To download the whole theme, add the theme link.',
    '楼层链接已复制': 'Post link copied',
    '该楼层没有名称': 'This post has no name',
    '名称已复制：': 'Name copied: ',
    '展开 / 收起预览': 'Expand / collapse preview',
    '选中一个条目查看磁盘统计': 'Select an item to see disk usage',

    // ===== 导入 / 导出 =====
    '导入历史': 'Import history',
    '清空记录': 'Clear history',
    '复制选中链接': 'Copy selected links',
    '点击选中，Ctrl 多选，Shift 范围选；右键行可复制链接地址':
      'Click to select, Ctrl for multi-select, Shift for a range; right-click a row to copy its link',
    '还没有导入历史': 'No import history yet',
    '已加入下载列表（点击选择楼层）': '— added to downloads (click to pick posts)',
    '已加入 Download': 'Added to Downloads',
    '没有可识别的链接': 'No recognizable links',
    '单帖': 'Single post',
    '多楼层主题': 'Multi-post theme',
    '重复忽略': 'Duplicates skipped',
    '正在导入 ': 'Importing ',
    ' 条链接…': ' links…',
    '（还有 ': ' (',
    ' 条排队）': ' queued)',
    '条：': 'links: ',
    '成功 · ': 'OK · ',
    '多楼层 · ': 'multi-post · ',
    '重复 · ': 'duplicates · ',
    '导入历史保存失败：': 'Failed to save import history: ',
    '导入失败：': 'Import failed: ',
    '选中条目没有可用链接': 'Selected items have no usable links',
    '未选中任何行': 'No rows selected',
    '已复制 ': 'Copied ',
    ' 条链接': ' links',
    '已复制：': 'Copied: ',
    '复制失败：': 'Copy failed: ',
    '普通连接': 'single post',

    // ===== 规则管理 =====
    '规则以 JSON 存放，保存即热加载；同一图床可保存多条规则，勾选启用':
      'Rules are stored as JSON and hot-reloaded on save; multiple rules per host are allowed',
    '重载': 'Reload',
    '规则目录': 'Rules folder',
    '默认': 'Default',
    '搜索 名称 / 域名 / 备注…': 'Search name / domain / note…',
    '全部来源': 'All sources',
    '内置': 'Built-in',
    '原版导入': 'Imported',
    '自定义': 'Custom',
    '只看启用': 'Enabled only',
    '只看勾选状态的规则': 'Show enabled rules only',
    '按来源筛选': 'Filter by source',
    '已选 ': 'Selected ',
    ' 条': ' rules',
    '启用': 'Enable',
    '停用': 'Disable',
    '取消选择': 'Clear selection',
    '删除规则': 'Delete rule',
    '名称': 'Name',
    '匹配域名': 'Domains',
    '来源': 'Source',
    '备注': 'Note',
    '解析步骤': 'Resolve steps',
    '添加步骤': 'Add step',
    '新建规则': 'New rule',
    '导入规则 (JSON)': 'Import rule (JSON)',
    '导出当前规则': 'Export rule',
    '保存并热加载': 'Save & reload',
    '测试：': 'Test: ',
    '粘贴一个真实的图片页地址': 'Paste a real image page URL',
    '缩略图链接（可选；{thumb_url} 规则必填）': 'Thumbnail URL (optional; required for {thumb_url} rules)',
    '测试': 'Test',
    '等待测试…': 'Waiting for test…',
    '名称和匹配域名必填': 'Name and domains are required',
    '已保存并热加载': 'Saved and hot-reloaded',
    '已新建规则': 'Rule created',
    '规则已重新加载': 'Rules reloaded',
    '重载失败：': 'Reload failed: ',
    '体检': 'Check',
    '体检中…': 'Checking…',
    '体检完成：': 'Check finished: ',
    ' 个错误，': ' errors, ',
    ' 个提醒': ' warnings',
    '全部通过': 'All passed',
    '错误项会导致规则跑不通，建议先处理。': 'Errors will break the rule — fix them first.',
    '体检失败：': 'Check failed: ',
    '规则目录已切换：': 'Rules folder switched: ',
    '（复制 ': ' (copied ',
    '，跳过 ': ', skipped ',
    '，共 ': ', total ',
    ' 条）': ' rules)',
    '要把当前目录里的规则复制到新目录吗？\n': 'Copy the rules from the current folder to the new one?\n',
    '\n\n确定 = 复制已有规则过去\n取消 = 只切换目录（新目录会补齐内置规则）':
      '\n\nOK = copy existing rules\nCancel = switch folder only (built-in rules are added)',
    '回到默认目录 data/hosts？\n（新目录里已存在的规则不会被删除）':
      'Return to the default folder data/hosts?\n(Rules already in the new folder are kept)',
    '切换失败：': 'Switch failed: ',
    '选择目录失败：': 'Folder selection failed: ',
    '先选择要删除的规则': 'Select a rule to delete first',
    '删除选中的 ': 'Delete selected ',
    ' 条规则？\n\n自定义规则删掉就没了；内置规则下次启动会自动补回来。':
      ' rules?\n\nCustom rules are gone for good; built-in rules are restored on next start.',
    '已删除规则': 'Rule deleted',
    '已保存生效：': 'Saved: ',
    '已启用': 'enabled',
    '已停用': 'disabled',
    '批量操作失败：': 'Bulk operation failed: ',
    '先在列表中选择要导出的规则': 'Select a rule to export first',
    '已导入规则': 'Rule imported',
    'JSON 解析失败': 'Invalid JSON',
    '测试成功': 'Test passed',
    '测试失败：': 'Test failed: ',
    '正在测试': 'Testing',
    '（缩略图 ': ' (thumbnail ',
    '没有匹配的规则': 'No matching rule',
    '支持的图床': 'Supported hosts',
    '当前支持 ': 'Currently ',
    ' 个图床（': ' hosts (',
    ' 条规则，同域名的多条规则合并显示）:\n': ' rules; rules with the same domain are merged):\n',

    // ===== 设置 =====
    '常规': 'General',
    '下载': 'Download',
    '论坛': 'Forum',
    '扩展': 'Extension',
    '每次添加时跳转': 'Jump on add',
    '论坛 vr: 按钮添加成功后，把本程序窗口提到最前': 'Forum vr: bring this window to the front after the vr: button adds a link',
    '添加后播放提示音': 'Play sound on add',
    '论坛 vr: 按钮添加成功后响一声，与下面的完成提示音是同一个声音': 'Forum vr: beep after the vr: button adds a link; same sound as the completion chime below',
    '文件处理': 'Files',
    '已存在图片': 'Existing images',
    '跳过（推荐，用于续传）': 'Skip (recommended, keeps resumes working)',
    '覆盖': 'Overwrite',
    '重命名保留': 'Rename and keep',
    '目录组织': 'Folder layout',
    '每帖一个文件夹（多图册楼层建子文件夹）': 'One folder per post (subfolders for multi-album posts)',
    '全部平铺': 'Flat',
    '按论坛板块分类': 'Group by forum section',
    '预览': 'Preview',
    '显示图片预览栏': 'Show image preview pane',
    '图片预览表头': 'Preview column in list',
    '开启后下载列表行高加倍，最左侧多一列“预览”，直接显示每个条目的首图缩略': 'Doubles the download-list row height and adds a Preview column on the left showing each item\'s first image thumbnail',
    '其他': 'Other',
    '下载中关窗前先确认': 'Confirm before closing while downloading',
    '勾选后：还有任务在下载时，点 × 或 文件 → 退出 都会先弹确认框（收进任务栏 / 强制退出 / 取消）；取消勾选则直接退出、不提示':
      'When checked, closing or File → Exit while downloading asks first (minimize / force quit / cancel); unchecked closes immediately',
    '任务完成提示音': 'Sound when finished',
    '试听': 'Preview',
    '任务完成后自动关机': 'Shut down when finished',
    '所有任务完成后倒计时 60 秒关机；命令行执行 shutdown /a 可取消':
      'Shuts down 60 seconds after all tasks finish; cancel with shutdown /a',
    '下载目录': 'Download folder',
    '临时目录': 'Temp folder',
    '留空 = 直接下到目标文件夹': 'Empty = download straight to the target folder',
    '浏览': 'Browse',
    '并发': 'Concurrency',
    '同时下载的条目数': 'Items downloading at once',
    '多个收藏项同时下载的上限': 'Maximum number of items downloading simultaneously',
    '每图床并发连接': 'Connections per host',
    '重试次数': 'Retry count',
    '连接超时 (秒)': 'Timeout (seconds)',
    '抓取原始文件名': 'Fetch original file names',
    '开启：优先使用原始文件名（含帖内顺序）；抓取不到的图才退化为 001.jpg 编号':
      'On: use original file names (in post order); fall back to 001.jpg numbering when unavailable',
    '自动获取楼层标题': 'Fetch post titles',
    '用于多图册楼层的文件夹命名': 'Used for folder names of multi-album posts',
    '浏览器扩展桥接': 'Browser extension bridge',
    '供「规则录制器」扩展把生成的规则保存进本程序': 'Lets the Rule Recorder extension save rules into this app',
    '端口': 'Port',
    '默认 47821；填 0 表示自动顺延，被占用时实际端口见下方状态':
      'Default 47821; 0 means auto-increment. The actual port is shown below.',
    '当前状态': 'Status',
    '读取中…': 'Loading…',
    '刷新': 'Refresh',
    '关闭后浏览器扩展无法连接本程序，但下载、解析等功能不受影响。':
      'When off, the browser extension cannot connect, but downloading and resolving still work.',
    '论坛域名': 'Forum domain',
    '镜像域名': 'Mirror domains',
    '主域名连不上时依次尝试，用于登录/感谢/帖子链接；接口（vr.php）不受影响':
      'Tried in order when the main domain fails, for login/thanks/post links; the vr.php API is unaffected',
    '请求限速 (次/秒)': 'Rate limit (req/s)',
    '对论坛 API 的请求频率，过高易触发封禁': 'Request rate to the forum API; too high may trigger a ban',
    '代理地址': 'Proxy address',
    '论坛或图床被墙时填写本地代理端口': 'Local proxy port for blocked forums or hosts',
    '启用代理': 'Enable proxy',
    '勾选后使用上面的地址；不勾则自动检测系统代理。新开始的下载生效（菜单栏也有同一个开关）':
      'Use the address above; otherwise the system proxy is detected. Applies to new downloads (same switch in the menu bar)',
    '账号': 'Account',
    '下载后自动感谢楼主': 'Auto-thank the poster after downloading',
    '需已登录；同一帖子只感谢一次（失败会写日志）':
      'Requires login; each post is thanked once (failures are logged)',
    '用户名': 'Username',
    '论坛用户名': 'Forum username',
    '密码': 'Password',
    '论坛密码': 'Forum password',
    '明文保存在本机数据库；只用下面“网页登录”时可以不填':
      'Stored in plain text in the local database; not needed if you only use Web login',
    '启动时自动登录': 'Log in on startup',
    '立即登录': 'Log in now',
    '登录': 'Log in',
    '注销': 'Log out',
    '清除本程序的论坛登录状态': 'Clear the forum login state of this app',
    '网页登录（备用）': 'Web login (fallback)',
    '网页登录': 'Web login',
    '打开网页登录': 'Open web login',
    '登录时请勾选 Remember Me，否则关闭窗口后会掉线':
      'Tick “Remember Me” when logging in, otherwise the session drops after closing',
    '保存': 'Save',
    '设置已保存': 'Settings saved',
    '保存失败：': 'Save failed: ',
    '请先填写用户名和密码': 'Enter username and password first',
    '登录中…': 'Logging in…',
    '已打开…': 'Opened…',
    '未登录（登录窗口已关闭或超时，可重试）': 'Not logged in (window closed or timed out — try again)',
    '已打开网页登录窗口，请在窗口里完成登录…': 'Web login window opened — finish logging in there…',
    '打开失败：': 'Failed to open: ',
    '已注销论坛登录': 'Logged out',
    '注销失败：': 'Logout failed: ',
    '已登录': 'Logged in',
    '（用户名未识别）': ' (username not recognized)',
    '未登录': 'Not logged in',
    '选择下载目录': 'Choose download folder',
    '选择临时目录': 'Choose temp folder',
    '下载目录已更改为 ': 'Download folder changed to ',
    '代理已开启：新开始的下载生效': 'Proxy on: applies to new downloads',
    '代理已关闭：新开始的下载生效': 'Proxy off: applies to new downloads',
    '需要更新程序后才支持清除缓存': 'Update the app to use cache clearing',
    '已清除解析缓存': 'Resolve cache cleared',
    '该主题还没有已加入的楼层': 'No posts added yet',
    '清除失败：': 'Clear failed: ',
    '保存失败：': 'Save failed: ',

    // ===== 关窗 / 人机验证 =====
    '还有任务在下载': 'Tasks are still downloading',
    '还有任务正在下载，直接退出会中断它们。要怎么处理？':
      'Tasks are still downloading; quitting now will interrupt them. What do you want to do?',
    '最小化到任务栏继续': 'Minimize to taskbar and continue',
    '强制退出': 'Force quit',
    '需要人机验证': 'Human check required',
    '该图床要求确认真人身份才能继续。点“打开验证窗口”，在里面点完确认后关闭窗口，之前失败的图片会自动重新下载。':
      'This host requires a human check. Click “Open verify window”, confirm inside, then close it — failed images download again automatically.',
    '打开验证窗口': 'Open verify window',
    '稍后': 'Later',
    '已打开验证窗口：点完“确认是人类”后关闭窗口即可':
      'Verify window opened — click “I am human” and close it',
    '已通过': 'Passed',
    '验证，正在重新下载之前失败的图片': 'verification, retrying failed images',
    '确认是人类': 'I am human',
    '人机验证': 'Human check',

    // ===== 其它提示 =====
    '没有可打开的文件夹（条目还没有目录信息）': 'No folder to open (this item has no folder yet)',
    '已打开：': 'Opened: ',
    '文件夹不存在，无法打开': 'Folder does not exist',
    '打开文件夹失败：': 'Failed to open folder: ',
    '打开文件夹': 'Open folder',
    '下载任务已完成': 'Downloads finished',
    '全部任务已完成：60 秒后自动关机': 'All tasks finished: shutting down in 60 seconds',
    '先选中一个条目': 'Select an item first',
    '没有可识别的链接': 'No recognizable links',
    '删除选中的条目': 'Delete selected items',
    '删除选中/导出选中': 'Delete / export selected',
    '确定删除这个主题？\n\n它下面的 ': 'Delete this theme?\n\nIts ',
    ' 个楼层会一起删除（已下载的文件不会动）。': ' posts will be deleted too (downloaded files are kept).',
    '确定删除这个条目？': 'Delete this item?',
    '已删除主题及其 ': 'Deleted theme and its ',
    ' 个楼层': ' posts',
    '已删除主题': 'Theme deleted',
    '删除失败：': 'Delete failed: ',
    '已标记该主题': 'Theme marked',
    '已清除标记': 'Mark cleared',
    '标记失败：': 'Marking failed: ',
    '已标记 ': 'Marked ',
    ' 项': ' items',
    '已清除 ': 'Cleared ',
    ' 项的标记': ' marks',
    '确定删除 ': 'Delete ',
    ' 个条目？': ' items?',
    '\n\n其中 ': '\n\n ',
    ' 个主题会连同其下 ': ' themes and their ',
    ' 个楼层一起删除': ' posts will be deleted together',
    '\n另有 ': '\nAlso ',
    ' 个楼层/条目直接删除': ' posts/items will be deleted',
    '（已下载的文件不会动）。': ' (downloaded files are kept).',
    '（含 ': ' (including ',
    ' 个楼层）': ' posts)',
    '已删除主题': 'Theme deleted',
    '编辑模式：勾选要删除的条目（表头可全选）': 'Edit mode: tick the items to delete (header selects all)',
    '已退出编辑模式': 'Exited edit mode',
    '已显示图片预览栏': 'Preview pane shown',
    '已隐藏图片预览栏': 'Preview pane hidden',
    '已复制：': 'Copied: ',
    '版本: v': 'Version: v',
    'Go + Wails 重写版': 'Go + Wails rewrite',

    // ===== 规则编辑器步骤字段 =====
    '源文本，如': 'Source text, e.g. ',
    '在这段文本里找，如': 'Search in this text, e.g. ',
    '正则（取第 1 个捕获组）': 'Regex (first capture group)',
    '正则': 'Regex',
    '没匹配到时的兜底': 'Fallback when nothing matches',
    '兜底值，如': 'Fallback value, e.g. ',
    '替换为': 'Replace with',
    '字段': 'Field',
    '值': 'Value',
    '步骤': 'Step',
    '解释': 'Note',

    // ===== 补充：高频片段（按长词优先命中，这里兜底短词）=====
    '从暂停的进度接着下载': 'Resume from where it paused',
    '全选当前列表': 'Select all in this list',
    '加载更多/加载全部': 'Load more / load all',
    '加载完成，共 ': 'Loaded: ',
    '勾选状态（点行即勾选）': 'selection (click a row to toggle)',
    '区间选': 'range select',
    '合并成 ': 'merged into ',
    '同时仍写进日志列表': 'and also written to the log',
    '在线状态）、': 'online status), ',
    '失败：': 'Failed: ',
    '已下载的文件不会动）。': '(downloaded files are kept).',
    '已关闭（扩展无法连接）': 'Off (extension cannot connect)',
    '已启用，取消勾选即停用': 'Enabled — untick to disable',
    '已导出到 ': 'Exported to ',
    '已打开验证窗口：点完': 'Verify window opened — click ',
    '已选 ': 'Selected ',
    '已选': 'Selected ',
    '当前支持 ': 'Currently ',
    '成功 · ': 'OK · ',
    '打开图片直链': 'Open image URL',
    '打开楼层链接': 'Open post link',
    '打开页面链接': 'Open page link',
    '拖拽调整此栏高度': 'Drag to resize this pane',
    '扫描': 'Scan',
    '无图床信息，恒在最后': 'no host info — always last',
    '显示': 'Show',
    ' / 共 ': ' / Total ',
    '普通模式': 'Normal mode',
    '暂停该主题正在下载的楼层': 'Pause the posts downloading in this theme',
    '更多': 'more',
    '有楼层正在下载': 'posts are downloading',
    '条链接': 'links',
    '版本信息': 'Version info',
    '状态分组': 'status group',
    '用户名未识别）': '(username not recognized)',
    '用户标记颜色': 'user mark color',
    '留空': 'Leave empty',
    '确定': 'OK',
    '确定删除自定义规则': 'Delete custom rule',
    '确定删除这个主题？': 'Delete this theme?',
    '编号': 'No.',
    '编辑模式的行高亮': 'row highlight in edit mode',
    '缩略图链接（可选': 'Thumbnail URL (optional',
    '缩略图': 'Thumbnail',
    '自定义规则删掉就没了': 'Custom rules cannot be recovered',
    '菜单': 'Menu',
    '规则必填）': 'required)',
    '规则目录：': 'Rules folder: ',
    '解析失败': 'Resolve failed',
    '记住': 'Remember',
    '该操作不可恢复。': 'This cannot be undone.',
    '读取失败：': 'Failed to read: ',
    '跳过': 'Skip',
    '运行中 · ': 'Running · ',
    '还有 ': 'and ',
    '这个服务只绑定': 'This service binds only to ',
    '退出主题管理': 'Exit theme view',
    '退回普通单击': 'falls back to a plain click',
    '选中并编辑': 'select and edit',
    '选中条目没有正在下载的': 'Selected items are not downloading',
    '选中条目都已在下载中': 'Selected items are already downloading',
    '选中的帖子优先': 'Selected post first',
    '逗号分隔，如 m1': 'comma separated, e.g. m1',
    '那一次）没有声音': 'that time) is silent',
    '重复 · ': 'duplicates · ',
    '需已登录': 'Requires login',
    '需要人机验证的域名': 'Domains that need a human check',
    '默认 47821': 'default 47821',
    '项': 'items',
    '条': 'rules',
    '层…': 'posts…',
    '层': 'posts',
    // 2 字基础词——先命中长词，这里避免单字替换把词拆坏
    '图片': 'image',
    '条目': 'item',
    '主题': 'theme',
    '楼层': 'post',
    '规则': 'rule',
    '设置': 'settings',
    '进度': 'progress',
    '状态': 'status',
    '标题': 'title',
    '链接': 'link',
    '文件': 'file',
    '目录': 'folder',
    '账号': 'account',
    '密码': 'password',
    '端口': 'port',
    '代理': 'proxy',
    '日志': 'log',
    '编辑': 'edit',
    '删除': 'delete',
    '复制': 'copy',
    '保存': 'save',
    '取消': 'cancel',
    '确认': 'confirm',
    '错误': 'error',
    '提醒': 'warning',
    '分钟': 'minutes',
    '实时': 'live',
    '失效': 'dead',
    '乱序': 'out of order',
    '增减': 'add or remove',
    '处理': 'handle',
    '因此': 'so ',
    '只有': 'only ',
    '另有': 'plus ',
    '其中': 'of which ',
    '可选': 'optional',
    '无图': 'no image',
    '图': 'images',
    '一次': 'once',
    '受影响的': 'affected ',
    '不受影响': 'unaffected',
    // ===== 长句（整句精确匹配，优先于逐词替换）=====
    '保存后文件夹会同步改成这个名字（含你保留的序号后缀）；正在下载的条目在停止/完成后自动归位。':
      'The folder is renamed to match when you save (including any suffix you keep); items still downloading move into place once stopped or finished.',
    '队列里正在下载的任务全部下完时响一声（点“试听”可立即测试）':
      'Play a sound when everything in the queue finishes (click “Preview” to test)',
    '下载中的 .tmp 文件放这里；和下载目录不在同一个盘时会多一次复制':
      'Holds .tmp files while downloading; using a different drive than the download folder costs one extra copy',
    '（本机回环地址），外网无法访问，也不上传任何数据。':
      ' (a loopback address): unreachable from outside, and nothing is uploaded.',
    '它对外提供三个接口：': 'It exposes three endpoints: ',
    '（用规则试解析一张图并回报真实分辨率）、':
      ' (resolve one image with a rule and report its real resolution), ',
    '（把规则写入规则目录并热加载）。': ' (write a rule into the rules folder and hot-reload it).',
    '（千层列表采用虚拟渲染，滚动流畅）':
      ' (thousand-post lists are virtualised and scroll smoothly)',
    '直接使用主页标题': 'Use the page title',
    '粘贴帖子或楼层链接，每行一个，自动识别': 'Paste post or post links, one per line — detected automatically',
    '重新读取规则目录（手工放入/修改 JSON 后点它）':
      'Reload the rules folder (use after adding or editing JSON files by hand)',
    '静态检查所有规则的语法/变量/步骤（不联网）':
      'Statically check the syntax, variables and steps of all rules (offline)',
    '选择规则 JSON 的存放目录（会记住，下次启动沿用）':
      'Choose where rule JSON files live (remembered for next start)',
    '回到程序默认的 data/hosts': 'Return to the default data/hosts',
    '点击=选中并编辑；Ctrl+点击=增减；Shift+点击=区间选；Ctrl+A=全选当前列表；Esc=取消选择':
      'Click = select and edit; Ctrl+click = toggle; Shift+click = range; Ctrl+A = select all; Esc = clear selection',
    '逗号分隔，如 imgbox.com, www.imgbox.com': 'Comma separated, e.g. imgbox.com, www.imgbox.com',
    'host:port，如 127.0.0.1:10809': 'host:port, e.g. 127.0.0.1:10809',
    '勾选后，选中的楼层直接用楼层名称命名条目和文件夹（不加楼层号、不建子目录）。适合只下载其中一层的情况':
      'When ticked, the selected posts are named after the post title (no post number, no subfolder) — handy when you only want one post.',
    '共 ': 'Total ',
    ' 层 · 有图 ': ' posts · with images ',
    '一步': 'step',
    // ===== 补充整句（动态拼接出来的长句）=====
    '个楼层会一起删除': 'posts will be deleted together',
    '条规则，同域名的多条规则合并显示）:\n': 'rules, same-domain rules are merged):\n',
    '条规则？\n\n自定义规则删掉就没了；内置规则下次启动会自动补回来。':
      ' rules?\n\nCustom rules are gone for good; built-in rules are restored on the next start.',
    '项（右键可批量开始/暂停/移除）': 'items (right-click to start/pause/remove in bulk)',
    'URL，如 {page_url}': 'URL, e.g. {page_url}',
    'XPath 选择器': 'XPath selector',
    'cookie 名': 'cookie name',
    '在那段文本里找，如 {body}': 'Search in that text, e.g. {body}',
    '\n\n确定 = 复制已有规则过去\n取消 = 只切换目录（新目录会补齐内置规则）':
      '\n\nOK = copy the existing rules\nCancel = switch the folder only (built-in rules are added)',
    '”）删除后本次运行生效，下次启动会自动恢复。确定删除？':
      '”) — the deletion lasts this session and it comes back on next start. Delete?',
    '删除该主题，它下面的 ': 'Delete this theme and its ',
    '回到默认目录 data/hosts？\n（新目录里已存在的规则不会被删除）':
      'Return to the default data/hosts?\n(Rules already in the new folder are kept)',
    '已启用但未启动（端口可能都被占用，看日志）':
      'Enabled but not started (ports may all be taken — check the log)',
    '确定删除这个主题？\n\n它下面的 ': 'Delete this theme?\n\nIts ',
    '要把当前目录里的规则复制到新目录吗？\n': 'Copy the rules from the current folder to the new one?\n',
    '（保存即热加载，勾选/取消立即生效）':
      ' (saved and hot-reloaded; ticking takes effect immediately)',
    // ===== 规则管理补齐 =====
    '(无)': '(none)',
    '（仅本机）': ' (local only)',
    '已删除 ': 'Deleted ',
    '已启用 ': 'Enabled ',
    '已停用 ': 'Disabled ',
    '，失败 ': ', failed ',
  };

  /* 短语键按长度降序，保证"删除主题"先于"删除"命中 */
  var KEYS = Object.keys(EN).sort(function (a, b) { return b.length - a.length; });

  var cache = new Map();
  var CJK = /[\u4e00-\u9fff]/;

  /* 翻译一段"纯文本"（不含标签）：整句 → 逐词 → 残留中文则整段保持原样 */
  function translateText(s) {
    if (!CJK.test(s)) return s;
    if (Object.prototype.hasOwnProperty.call(EN, s)) return EN[s];

    var out = s;
    for (var i = 0; i < KEYS.length; i++) {
      var k = KEYS[i];
      if (out.indexOf(k) !== -1) out = out.split(k).join(EN[k]);
    }
    // 替换后还有中文 ⇒ 这段里混着用户数据（中文备注/标题/路径）或没覆盖的句子，
    // 整段保持原样，既不破坏数据，也不会出现半中半英
    return CJK.test(out) ? s : out;
  }

  /* HTML 片段：只翻"标签之间的文本段"和常见属性值，逐段判定。
     这样一条中文备注不会被逐词拆开，而表头、按钮这类模板文案照样能翻。 */
  var TAG_TEXT = />([^<>]+)</g;
  var ATTR_RE = /\b(title|placeholder|aria-label|alt)="([^"]*)"/g;

  function translateHTML(s) {
    var out = s.replace(TAG_TEXT, function (m, text) { return '>' + translateText(text) + '<'; });
    // 末尾/开头没有闭合标签包住的文本段（例如 '<i ...></i>暂停'）
    out = out.replace(/>([^<>]+)$/, function (m, text) { return '>' + translateText(text); });
    out = out.replace(/^([^<>]+)</, function (m, text) { return translateText(text) + '<'; });
    return out.replace(ATTR_RE, function (m, name, val) { return name + '="' + translateText(val) + '"'; });
  }

  function L(s) {
    if (lang !== 'en' || typeof s !== 'string' || !s) return s;
    if (!CJK.test(s)) return s;
    var hit = cache.get(s);
    if (hit !== undefined) return hit;

    var out = s.indexOf('<') === -1 ? translateText(s) : translateHTML(s);
    if (cache.size > 4000) cache.clear();
    cache.set(s, out);
    return out;
  }

  /* 把静态 DOM 里的文本节点与常见属性翻一遍（只在 en 模式做一次） */
  function translateStatic(root) {
    if (lang !== 'en') return;
    var walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, null);
    var nodes = [];
    while (walker.nextNode()) nodes.push(walker.currentNode);
    nodes.forEach(function (n) {
      var t = n.nodeValue;
      if (t && CJK.test(t)) {
        var v = L(t);
        if (v !== t) n.nodeValue = v;
      }
    });
    var attrs = ['title', 'placeholder', 'aria-label', 'alt'];
    root.querySelectorAll('*').forEach(function (el) {
      attrs.forEach(function (a) {
        var v = el.getAttribute && el.getAttribute(a);
        if (v && CJK.test(v)) {
          var nv = L(v);
          if (nv !== v) el.setAttribute(a, nv);
        }
      });
    });
    if (document.title && CJK.test(document.title)) document.title = L(document.title);
  }

  /* 拦截 innerHTML / textContent / title，让运行时渲染的文案也走翻译 */
  function patchSetters() {
    if (lang !== 'en') return;
    try {
      var ih = Object.getOwnPropertyDescriptor(Element.prototype, 'innerHTML');
      if (ih && ih.set) {
        Object.defineProperty(Element.prototype, 'innerHTML', {
          configurable: true,
          get: ih.get,
          set: function (v) { ih.set.call(this, typeof v === 'string' ? L(v) : v); }
        });
      }
      var tc = Object.getOwnPropertyDescriptor(Node.prototype, 'textContent');
      if (tc && tc.set) {
        Object.defineProperty(Node.prototype, 'textContent', {
          configurable: true,
          get: tc.get,
          set: function (v) { tc.set.call(this, typeof v === 'string' ? L(v) : v); }
        });
      }
      var tt = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'title');
      if (tt && tt.set) {
        Object.defineProperty(HTMLElement.prototype, 'title', {
          configurable: true,
          get: tt.get,
          set: function (v) { tt.set.call(this, typeof v === 'string' ? L(v) : v); }
        });
      }
    } catch (e) { /* 拦截失败就只翻静态部分，不影响功能 */ }
  }

  function setLang(next) {
    try { localStorage.setItem(KEY, next); } catch (e) {}
    location.reload();
  }

  /* confirm / alert 由浏览器弹，文案也要翻 */
  function patchDialogs() {
    if (lang !== 'en') return;
    var c = window.confirm, al = window.alert;
    window.confirm = function (msg) { return c.call(window, typeof msg === 'string' ? L(msg) : msg); };
    window.alert = function (msg) { return al.call(window, typeof msg === 'string' ? L(msg) : msg); };
  }

  function wireButton() {
    var btn = document.getElementById('gv-langbtn');
    if (!btn) return;
    btn.textContent = lang === 'en' ? '中文' : 'EN';
    btn.addEventListener('click', function () { setLang(lang === 'en' ? 'zh' : 'en'); });
  }

  window.I18N = {
    lang: lang,
    dict: EN,
    L: L,
    t: L,
    setLang: setLang,
    toggle: function () { setLang(lang === 'en' ? 'zh' : 'en'); }
  
};

  /* 启动：先拦 setter，再翻静态 DOM（此时 body 已解析，app.js 还没跑） */
  patchSetters();
  patchDialogs();
  translateStatic(document.body);
  wireButton();
  document.documentElement.lang = lang === 'en' ? 'en' : 'zh-CN';
})();
