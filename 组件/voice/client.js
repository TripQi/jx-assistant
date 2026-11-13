/* 组件：语音（朗读与听写） —— 组件/voice/client.js
 *
 * 分工（2026-10-07 定稿，用户口径）：
 *
 * 1) 朗读（TTS）：**后端实现**。前端 POST /api/voice/tts（文本+语速+音色）→
 *    后端直连微软 Edge「大声朗读」通道（speech.platform.bing.com 的 readaloud，
 *    无密钥、免配置，与 Edge/WebView2 内置朗读同一套服务；协议移植自聊天服务
 *    chat-server 的 Node 实现）→ mp3 流回 → blob 化 <audio> 播放。音色清单来自
 *    /api/voice/voices（Edge 神经音色：晓晓/云希/Aria…，中文排前）。朗读不再用
 *    浏览器的 speechSynthesis——那在远程手机端上部分浏览器没有，旧版连朗读
 *    设置区都不渲染。
 *
 * 2) 听写（STT）两档（设置页对用户只分「本地/云端」，不点名模型与厂商）：
 *    a. 云端语音识别模型（默认）：浏览器自带的 SpeechRecognition——各浏览器接入
 *       的服务商不同（Edge/WebView2、Chrome、国产浏览器各异）且依赖其在线服务
 *       可达，能力随浏览器版本波动（失败处置指引见 dictationFailMsg）；环境没有
 *       该能力时（部分远程手机端浏览器、Electron 壳）自动落到本地模型。
 *    b. 本地语音识别模型（需手动选择，voiceasr.go 通道）：本机/桌面走 WebSocket
 *       流式——说话实时出灰字、停顿约 2 秒由离线模型精修落成黑字（复用浏览器
 *       引擎同款两段式体验）；远程 https 页面开不了 ws，自动回退整段识别。
 *       音频不出本机/服务器；要求 技能\_runtime 带 sherpa-onnx 且 data\asr-models
 *       模型文件齐备，未就绪时设置页切回云端。
 *
 * 3) 快捷键：默认鼠标侧键，可自定义键盘单键/组合键、甚至鼠标左右键同按
 *    （不允许单独的左键或右键）；触发方式默认**点按切换**（按一下开始、再按一下
 *    结束），设置页可切成长按输入。
 *    主/副键帽框与录制器用主程序 HK 的共用控件（app-hotkeys.js），并经
 *    JX.registerHotkey 登记——设置·快捷键 总表里是同一份数据，两边改互相同步。
 *    仅在窗口聚焦时激活（失焦自动松手）。主输入框的占位灰字会像发送快捷键
 *    一样带上语音快捷键提示（app-core 的 sendKeyHint 追加 window.JXVoiceHint，
 *    子输入框由本组件自己拼）。远程端没有桌面级全局快捷键，占位提示自动省略。
 *    触发判定＝**边沿触发**（2026-10-10 修）：组合必须由"本次按下的那个键"补齐，
 *    另有多路兜底清理（失焦 / 页面隐藏 / 重新聚焦 / 指针取消 / 光标离开文档 /
 *    长时间无输入的残留心跳）。防的是"mouseup 丢失后残留把普通点击误判成快捷键"
 *    ——曾表现为点「＋→文件上传」却启动听写，见
 *    语音误触发排查（点上传文件启动听写）2026-10-10.md。
 *
 * 全程零配置（不需要任何密钥）。按钮 DOM 在 web/index.html（位置与样式不变），
 * 默认带 .hidden——按钮属于本组件，组件关掉（设置 → 组件）或没加载到页面
 * = 按钮整组不出现。热开关 dispose 完整撤销（识别停止、朗读停止、钩子注销、
 * 设置页消失），不需要刷新页面。
 */
(function () {
  if (!window.CX) return;

  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;

  let active = false;    // 组件是否激活：dispose 后立刻失效，兜住还在飞的异步回调
  let msgAction = null;  // 已注册的消息操作行钩子（注销用）
  let ctxMenuFn = null;  // 已注册的右键菜单贡献项（注销用）
  const busHooks = [];   // 已挂的事件总线回调 [{ev, fn}]（注销用）
  let sendHook = null;   // 已注册的发送前钩子（可等待；注销用）

  /* ---------- 偏好（localStorage，命名空间 cx:voice） ---------- */

  const pref = {
    get(k, d) {
      try { const v = localStorage.getItem('cx:voice:' + k); return v === null ? d : JSON.parse(v); }
      catch (e) { return d; } // 取值失败（禁用/坏数据）一律回默认
    },
    set(k, v) { try { localStorage.setItem('cx:voice:' + k, JSON.stringify(v)); } catch (e) { /* 隐私模式忽略 */ } },
  };

  // 朗读语速（0.5–3，默认 1.2；后端折成 SSML 的百分比语速）
  function spokenRate() {
    const v = Number(pref.get('rate', 1.2));
    return isFinite(v) ? Math.min(3, Math.max(0.5, v)) : 1.2;
  }
  // 单次听写时长上限（秒）
  function maxDictateSec() {
    const v = Number(pref.get('maxSec', 240));
    return isFinite(v) && v >= 30 ? Math.min(600, v) : 240;
  }

  const ICON = {
    speak: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5L6 9H2v6h4l5 4V5z"/><path d="M15.5 8.5a5 5 0 0 1 0 7"/><path d="M18.5 5.5a9 9 0 0 1 0 13"/></svg>',
    stop: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5L6 9H2v6h4l5 4V5z"/><line x1="16" y1="9" x2="22" y2="15"/><line x1="22" y1="9" x2="16" y2="15"/></svg>',
  };

  function apiErrMsg(res, text) {
    try {
      const j = JSON.parse(text);
      if (j && j.error) return j.error;
    } catch (e) { /* 非 JSON 正文 */ }
    return 'HTTP ' + res.status;
  }

  /* ================= 语音快捷键（主副双快捷键） =================
     表示：**备选数组**——shortcutAlternatives() 返回 [[键…], …]，每个元素是一种
     可触发的组合（组合内全按住才触发，任一组合触发即可）。
     · 默认＝**主副双快捷键，永久并存**：鼠标侧键（主）+ Alt 键（副），两者都
       可触发——有侧键的用侧键，没有的自然用 Alt。
     · 用户自定义（设置页录制）：单组合，元素为 'ctrl'|'alt'|'shift'|'meta' |
       单个字母/数字 | 'f1'..'f12' | 'space' | 'mouse-side'（侧键，前后两枚都算）
       | 'mouse-side-1'（侧键1·后退位）| 'mouse-side-2'（侧键2·前进位）
       | 'mouse-left' | 'mouse-right'。
     · 清空（设置页「清空」按钮）：存空数组＝停用快捷键（只点按钮听写）。
     · 独按修饰键（如默认副键 Alt）：按住期间又按了别的键（用户在按 Alt+X
       组合）→ 解除不触发；点按模式在**独按松开**时触发，避免任何 Alt 组合误触。
     · 校验：不允许只剩单独的 mouse-left 或单独的 mouse-right（废掉正常点击/菜单）。
     · 模式：tap=点按切换（**默认**）；hold=长按输入（按住说话、松开/失焦结束）。
     仅窗口聚焦时激活；失焦自动清空按键状态并结束长按会话。纯字母/数字单键在
     输入框聚焦时不触发（字符要留给打字），组合键不受此限。 */

  const KEY_NAMES = {
    ctrl: 'Ctrl', alt: 'Alt', shift: 'Shift', meta: 'Win', space: '空格',
    'mouse-side': '鼠标侧键', 'mouse-side-1': '鼠标侧键1', 'mouse-side-2': '鼠标侧键2',
    'mouse-left': '鼠标左键', 'mouse-right': '鼠标右键',
  };

  // 配置键名 vs 实际按下的键名：'mouse-side' 两枚侧键都算（旧配置不变），
  // 配了具体某一枚（mouse-side-1=后退位 / mouse-side-2=前进位）就只认那一枚——
  // 与独立输入法（voicetype 的 vtKeyMatches）同一套语义。
  function keyMatches(cfg, actual) {
    if (cfg === actual) return true;
    if (cfg === 'mouse-side') return actual === 'mouse-side-1' || actual === 'mouse-side-2';
    return false;
  }

  function isModKey(k) { return k === 'ctrl' || k === 'alt' || k === 'shift' || k === 'meta'; }

  // 主/副两个独立快捷键槽（设置页各有一个键帽框，可分别录制）：
  // 槽值＝null（跟默认：主=鼠标侧键，副=Alt）| 数组（自定义组合）| 空数组（停用）。
  // 旧版单快捷键偏好（cx:voice:shortcut）一次性迁移：非空→主槽；空（清空过）→两槽停用。
  function shortcutSlot(which) {
    if (!pref.get('scMigrated', false)) {
      const old = pref.get('shortcut', null);
      if (Array.isArray(old)) {
        pref.set('scMain', old.length ? old : []);
        pref.set('scSub', old.length ? null : []);
      }
      pref.set('scMigrated', true);
    }
    const v = pref.get(which === 'main' ? 'scMain' : 'scSub', null);
    if (Array.isArray(v)) return v.length ? v : [];
    return which === 'main' ? ['mouse-side'] : ['alt'];
  }
  function setShortcutSlot(which, v) {
    pref.set(which === 'main' ? 'scMain' : 'scSub', v);
  }
  // 触发备选＝两个槽里没被停用的那些
  function shortcutAlternatives() {
    return [shortcutSlot('main'), shortcutSlot('sub')].filter((a) => a.length > 0);
  }
  function voiceShortcutMode() {
    return pref.get('shortcutMode', 'tap') === 'hold' ? 'hold' : 'tap';
  }
  // 触发方式的产品默认＝**点按切换**（按一下开始、再按一下结束）；长按输入是可选项。
  // 独立语音输入法的触发方式也跟这里走（主程序把它写进 voicetype.json 联动文件），
  // 所以默认只在主程序这一处定。
  // 存量归一（一次性）：早期版本没记"用户显式选过"，机器上可能留着一份从没主动选过
  // 的 hold（旧版默认或验证脚本遗留），表现为"默认竟然是长按"。没有显式选择标记的
  // hold 一律回到默认点按；用户之后在设置页主动选长按会打标记，不再被迁移。
  function migrateShortcutModeDefault() {
    if (pref.get('shortcutModeSet', false)) return;
    if (pref.get('shortcutMode', null) === 'hold') pref.set('shortcutMode', 'tap');
  }
  function shortcutLabel() {
    const alts = shortcutAlternatives();
    if (!alts.length) return '无（已清空）';
    return alts.map((alt) => alt.map((k) => KEY_NAMES[k] || k.toUpperCase()).join('+')).join('或');
  }
  function validShortcut(keys) {
    if (!Array.isArray(keys) || keys.length === 0) return false;
    if (keys.length === 1 && (keys[0] === 'mouse-left' || keys[0] === 'mouse-right')) return false;
    return keys.every((k) => KEY_NAMES[k] !== undefined || /^[a-z0-9]$/.test(k) || /^f([1-9]|1[0-2])$/.test(k));
  }

  // 事件 → 规范化键名
  function normKey(e) {
    const k = e.key;
    if (k === 'Control') return 'ctrl';
    if (k === 'Alt') return 'alt';
    if (k === 'Shift') return 'shift';
    if (k === 'Meta') return 'meta';
    const lk = String(k).toLowerCase();
    if (/^[a-z0-9]$/.test(lk)) return lk;
    if (/^f([1-9]|1[0-2])$/.test(lk)) return lk;
    if (lk === ' ') return 'space';
    return null; // 其他键不收（避免录出系统级/生僻组合）
  }
  function normMouse(e) {
    // button 3=XBUTTON1（侧键1·后退位）4=XBUTTON2（侧键2·前进位），分开记名；
    // 配置里写 'mouse-side' 的经 keyMatches 两枚都算
    return { 0: 'mouse-left', 2: 'mouse-right', 3: 'mouse-side-1', 4: 'mouse-side-2' }[e.button] || null;
  }
  function isEditable(t) {
    return !!(t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable));
  }

  // 全局按键状态与触发（监听装在 document/window 上，dispose 摘除）
  const heldKey = new Set();   // 当前按着的键盘键
  const heldMouse = new Set(); // 当前按着的鼠标键
  let holdSession = false;     // 长按会话进行中
  let firedLatch = false;      // 点按已触发，等快捷键全部松开后复位
  let voiceHK = null;          // 本组件在 HK（app-hotkeys.js）登记的快捷键定义
  const voiceCtl = { main: null, sub: null }; // 各视图的听写控制器（快捷键驱动它们）
  let onResize = null;         // 设置页尺寸联动（旧版曾用于远程窄屏隐藏按钮，已撤）
  // 残留兜底（2026-10-10）：距最近一次真实输入超过 STALE_MS 而按键状态还挂着，
  // 视为"mouseup/keyup 丢了"的残留，清掉——别让它把后续普通点击误判成快捷键。
  let lastInputAt = 0;         // 最近一次按键/鼠标事件时刻
  let staleTimer = null;       // 残留清理心跳
  const STALE_MS = 20000;

  /* ---------- 听写引擎选择：本地（默认）｜云端（浏览器内置识别） ----------
   * pref.engine：xasr（默认·本地）| browser（云端，需手动选）。
   * 对用户只分本地/云端两档：云端由浏览器内置识别能力提供，不同浏览器接入的
   * 服务商不同且依赖其在线服务；选了云端但环境没有 SpeechRecognition 时回退本地。
   * 历史值 'auto'（按环境自动挑浏览器/本地）随本次改版一并归一到本地默认。 */
  function dictationEngine() {
    const p = String(pref.get('engine', 'xasr'));
    return (p === 'browser' && SR) ? 'browser' : 'xasr';
  }
  function dictationIsLocal() { return dictationEngine() !== 'browser'; }

  /* 本地通道起不来时的可执行提示：缺 worker（组件文件不完整）、缺运行时依赖
   * （sherpa-onnx 未装进共享便携运行时）、缺模型文件（组件/voice/models/ 未就绪）
   * 三类是产品侧可预期的失败，直接把「切云端」这条出路告诉用户；其他错误原样显示。 */
  function voiceLocalHint(err) {
    const s = String(err || '');
    if (/asr_worker\.py|sherpa|模型文件不完整|asr-models|便携运行时未装配/.test(s)) {
      return '本地语音识别未就绪（缺运行时或模型文件）：请在 设置 · 语音 把「听写引擎」切到云端语音识别模型；若刚升级过本程序，先重开程序再试';
    }
    return '';
  }

  /* ---------- 本地引擎的采集与上传（16k 单声道 PCM → WAV base64） ---------- */
  function voicePcmToWavB64(floats, sr) {
    const len = floats.length;
    const buf = new ArrayBuffer(44 + len * 2);
    const v = new DataView(buf);
    const wstr = (o, s) => { for (let i = 0; i < s.length; i++) v.setUint8(o + i, s.charCodeAt(i)); };
    wstr(0, 'RIFF'); v.setUint32(4, 36 + len * 2, true); wstr(8, 'WAVE');
    wstr(12, 'fmt '); v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
    v.setUint32(24, sr, true); v.setUint32(28, sr * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
    wstr(36, 'data'); v.setUint32(40, len * 2, true);
    for (let i = 0; i < len; i++) {
      const s = Math.max(-1, Math.min(1, floats[i]));
      v.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
    }
    let bin = '';
    const u8 = new Uint8Array(buf);
    for (let i = 0; i < u8.length; i += 0x8000) bin += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000));
    return btoa(bin);
  }

  async function voiceASRTranscribe(model, device, wavB64) {
    const res = await fetch('/api/c/voice/asr/transcribe', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cmd: 'transcribe', model, device, wav_b64: wavB64 }),
    });
    if (!res.ok) throw new Error(apiErrMsg(res, await res.text().catch(() => '')));
    return res.json();
  }

  function activeVoiceCtl() {
    const ae = document.activeElement;
    if (voiceCtl.sub && ae && ae.id === 'sub-input') return voiceCtl.sub;
    return voiceCtl.main || voiceCtl.sub || null;
  }

  function altHeld(alt) { // 一个备选组合是否被完整按住
    for (const k of alt) {
      if (!heldKey.has(k) && !heldMouse.has(k) && !heldMouseKeyMatches(k)) return false;
    }
    return true;
  }
  // 配置键名（可能是泛指的 'mouse-side'）是否被当前按着的具体侧键满足
  function heldMouseKeyMatches(k) {
    for (const m of heldMouse) if (keyMatches(k, m)) return true;
    return false;
  }

  // 独按修饰键状态（如默认备选里的 Alt）：按住期间按了别的键就解除，
  // 防止用户按 Alt+X 组合时误触语音
  let armedSolo = null; // { key, started }（started=hold 模式已开说话）

  function cancelSolo() {
    if (!armedSolo) return;
    if (armedSolo.started) {
      holdSession = false;
      const ctl = activeVoiceCtl();
      if (ctl && ctl.active()) ctl.stop();
    }
    armedSolo = null;
  }

  function shortcutFire(action, key) { // action: 'press' | 'release'；key＝本次按下的那个键
    if (!active || (window.HK && HK.isRecording()) || !document.hasFocus()) return; // 主程序录制快捷键时暂停本引擎
    lastInputAt = Date.now(); // 刷新"最近一次真实输入"时刻（残留清理心跳用）
    const alts = shortcutAlternatives();
    const mode = voiceShortcutMode();
    const fire = (what) => { // what: 'toggle' | 'start' | 'stop'
      const ctl = activeVoiceCtl();
      if (!ctl) return;
      if (what === 'toggle') ctl.toggle();
      else if (what === 'start') { if (!ctl.active()) ctl.start(); }
      else if (ctl.active()) ctl.stop();
    };

    if (action === 'press') {
      // 1) 非独按修饰键的备选：完整按住即触发
      for (const alt of alts) {
        if (alt.length === 1 && isModKey(alt[0])) continue; // 独按修饰键走 2)
        if (!altHeld(alt)) continue;
        // 边沿触发（2026-10-10 修）：本次按下的键必须**属于**该组合。
        // 否则"按下没配到 mouseup"的残留（默认主快捷键＝鼠标侧键；弹原生文件对话框、
        // 光标移出窗口松手、WebView2 不派发 window blur 等都会造成）会把之后任何一次
        // 普通鼠标点击都补成"组合成立"→ 点「上传文件」却开了听写。详见
        // 语音误触发排查（点上传文件启动听写）2026-10-10.md。
        // 归属判定用 keyMatches：配 'mouse-side' 时侧键 1/2 都算该组合的成员。
        if (key && !alt.some((k) => keyMatches(k, key))) continue;
        if (mode === 'hold') {
          if (holdSession) return;
          const c0 = activeVoiceCtl(); if (c0 && c0.active()) return;
          holdSession = true;
          fire('start');
        } else if (!firedLatch) {
          firedLatch = true; // 一次完整按下只触发一回，全松开后复位
          fire('toggle');
        }
        return;
      }
      // 2) 独按修饰键：按住先布防；点按模式在独按松开时触发（见 release）
      for (const alt of alts) {
        // 同样要求"就是这次按下了这个修饰键"才布防（残留的修饰键不再布防）
        if (alt.length === 1 && isModKey(alt[0]) && altHeld(alt) && alt[0] === key) {
          if (!armedSolo) {
            armedSolo = { key: alt[0], started: false };
            if (mode === 'hold') {
              const c0 = activeVoiceCtl(); if (c0 && c0.active()) return;
              holdSession = true;
              armedSolo.started = true;
              fire('start');
            }
          }
          return;
        }
      }
      return;
    }

    // release
    if (armedSolo) {
      const k = armedSolo.key;
      if (!heldKey.has(k) && !heldMouse.has(k)) { // 独按松开
        if (armedSolo.started) {
          holdSession = false;
          fire('stop');
        } else if (mode === 'tap') {
          fire('toggle');
        }
        armedSolo = null;
        firedLatch = false;
        return;
      }
    }
    if (holdSession) {
      const anyHeld = alts.some((a) => altHeld(a)) || (armedSolo && armedSolo.started);
      if (!anyHeld) {
        holdSession = false;
        fire('stop');
      }
    }
    if (!alts.some((a) => altHeld(a)) && !armedSolo) firedLatch = false;
  }

  let scKeyDown = null, scKeyUp = null, scMouseDown = null, scMouseUp = null, scContextMenu = null, scBlur = null;
  let scVisibility = null, scFocus = null, scPointerCancel = null, scMouseLeave = null;

  // 残留清理心跳：长时间没有输入却还挂着按键状态，就清（长按说话与听写进行中豁免）
  function startStaleTimer() {
    if (staleTimer) return;
    staleTimer = setInterval(() => {
      if (!active || holdSession) return;
      const ctl = activeVoiceCtl();
      if (ctl && ctl.active()) return;
      if (!heldKey.size && !heldMouse.size && !armedSolo) return;
      if (Date.now() - lastInputAt < STALE_MS) return;
      clearShortcutState();
    }, 5000);
  }

  function clearShortcutState() {
    heldKey.clear();
    heldMouse.clear();
    firedLatch = false;
    cancelSolo();
    if (holdSession) {
      holdSession = false;
      const ctl = activeVoiceCtl();
      if (ctl && ctl.active()) ctl.stop();
    }
  }

  function installShortcutEngine() {
    scKeyDown = (e) => {
      if (!active || (window.HK && HK.isRecording())) return; // 主程序录制快捷键时暂停本引擎
      const k = normKey(e);
      if (!k) return;
      // 纯字母/数字单键：目标是输入框时不触发也不拦截（字符留给打字）
      if (k.length === 1 && /^[a-z0-9]$/.test(k) && isEditable(e.target)) {
        heldKey.delete(k);
        return;
      }
      if (shortcutAlternatives().some((a) => a.includes(k))) e.preventDefault();
      if (e.repeat) return;
      if (armedSolo && k !== armedSolo.key) cancelSolo(); // 独按期间按了别的键：解除
      heldKey.add(k);
      shortcutFire('press', k);
    };
    scKeyUp = (e) => {
      if (!active) return;
      const k = normKey(e);
      if (k) {
        // keyup 也拦一道（keydown 已拦）：Windows 上 Alt 松开会触发系统菜单/工具栏
        // 焦点转移等默认行为，防焦点被带离输入框（2026-10-12）
        if (shortcutAlternatives().some((a) => a.includes(k))) e.preventDefault();
        heldKey.delete(k);
      }
      shortcutFire('release');
    };
    scMouseDown = (e) => {
      if (!active || (window.HK && HK.isRecording())) return;
      const m = normMouse(e);
      if (!m) return;
      // 侧键在快捷键里时拦掉默认的后退/前进导航（配 'mouse-side' 时两枚都拦，
      // 只配某一枚时另一枚放行——原生前进/后退照常）
      if (shortcutAlternatives().some((a) => a.some((k) => keyMatches(k, m)))) e.preventDefault();
      if (armedSolo && m !== armedSolo.key) cancelSolo();
      heldMouse.add(m);
      shortcutFire('press', m);
    };
    scMouseUp = (e) => {
      if (!active) return;
      const m = normMouse(e);
      if (m) heldMouse.delete(m);
      shortcutFire('release');
    };
    // 快捷键含鼠标右键（左右同按）时，按住期间压掉右键菜单
    scContextMenu = (e) => {
      if (!active) return;
      if (shortcutAlternatives().some((a) => a.includes('mouse-right')) &&
          heldMouse.has('mouse-left') && heldMouse.has('mouse-right')) {
        e.preventDefault();
      }
    };
    scBlur = () => clearShortcutState();
    document.addEventListener('keydown', scKeyDown);
    document.addEventListener('keyup', scKeyUp);
    document.addEventListener('mousedown', scMouseDown);
    document.addEventListener('mouseup', scMouseUp);
    document.addEventListener('contextmenu', scContextMenu);
    window.addEventListener('blur', scBlur);
    // —— 兜底清理（2026-10-10 修：光靠 blur 不够）——
    // 现场是"mouseup 丢失 + 窗口没失焦/WebView2 不派发 blur"，残留会永久留下。
    // 页面隐藏、重新聚焦、指针取消、光标离开文档，各清一次；另有残留心跳。
    scVisibility = () => { if (document.hidden) clearShortcutState(); };
    scFocus = () => clearShortcutState();
    scPointerCancel = () => clearShortcutState();
    scMouseLeave = () => { // 光标离开文档后，按着的鼠标键不可能再收到 mouseup
      if (!heldMouse.size) return;
      heldMouse.clear();
      shortcutFire('release');
    };
    document.addEventListener('visibilitychange', scVisibility);
    window.addEventListener('focus', scFocus);
    document.addEventListener('pointercancel', scPointerCancel, true);
    document.addEventListener('mouseleave', scMouseLeave);
    startStaleTimer();
  }

  function removeShortcutEngine() {
    if (scKeyDown) document.removeEventListener('keydown', scKeyDown);
    if (scKeyUp) document.removeEventListener('keyup', scKeyUp);
    if (scMouseDown) document.removeEventListener('mousedown', scMouseDown);
    if (scMouseUp) document.removeEventListener('mouseup', scMouseUp);
    if (scContextMenu) document.removeEventListener('contextmenu', scContextMenu);
    if (scBlur) window.removeEventListener('blur', scBlur);
    if (scVisibility) document.removeEventListener('visibilitychange', scVisibility);
    if (scFocus) window.removeEventListener('focus', scFocus);
    if (scPointerCancel) document.removeEventListener('pointercancel', scPointerCancel, true);
    if (scMouseLeave) document.removeEventListener('mouseleave', scMouseLeave);
    if (staleTimer) { clearInterval(staleTimer); staleTimer = null; }
    if (onResize) window.removeEventListener('resize', onResize);
    scKeyDown = scKeyUp = scMouseDown = scMouseUp = scContextMenu = scBlur = onResize = null;
    scVisibility = scFocus = scPointerCancel = scMouseLeave = null;
    clearShortcutState();
  }

  /* ---------- 占位灰字：语音快捷键提示（主输入框经基座 sendKeyHint 追加
     window.JXVoiceHint；子输入框由本组件拼接，dispose 时还原） ---------- */
  let subBasePH = null; // 子输入框原始占位文案

  function voiceHintText() {
    if (window.JX_REMOTE) return ''; // 远程端无全局快捷键（那是桌面端的钩子），不占位提示
    if (!shortcutAlternatives().length) return ''; // 已清空：占位提示不带语音段
    const mode = voiceShortcutMode();
    return '；语音输入：' + shortcutLabel() + (mode === 'hold' ? '（按住说话）' : '（点按）');
  }
  function refreshShortcutPlaceholders() {
    if (!active) return;
    window.JXVoiceHint = voiceHintText();
    if (typeof updateSendKeyHint === 'function') updateSendKeyHint();
    const sub = document.getElementById('sub-input');
    if (sub) {
      // 取原始文案：组件激活若发生在执行中（setStreamUI 已追加插话/排队段），
      // 从存货 dataset.idlePh 取，别把执行段错记成原始文案导致还原后多出一段
      if (subBasePH === null) subBasePH = (sub.dataset.runPh && sub.dataset.idlePh) || sub.placeholder || '向子对话发消息…';
      // 执行中（runPh 标记）占位符带着插话/排队段：刷新快捷键提示时同样带上
      sub.placeholder = subBasePH + voiceHintText() + (sub.dataset.runPh ? RUN_HINT_SEGMENT : '');
    }
  }
  function restoreSubPlaceholder() {
    const sub = document.getElementById('sub-input');
    if (sub && subBasePH !== null) sub.placeholder = subBasePH;
  }

  /* ================= 一、朗读（TTS，后端 Edge 通道，免配置） ================= */

  /** 去掉会被念出来的 markdown 语法噪声（与 dsh-speech 的 cleanForSpeech 同款）。 */
  function cleanForSpeech(src) {
    let s = String(src || '');
    s = s.replace(/(```|~~~)[\s\S]*?(```|~~~)/g, ' ');
    s = s.replace(/`([^`]*)`/g, '$1');
    s = s.replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1');
    s = s.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1');
    s = s.replace(/^#{1,6}\s+/gm, '');
    s = s.replace(/^\s*[-*+]\s+/gm, '');
    s = s.replace(/^\s*\d+\.\s+/gm, '');
    s = s.replace(/^\s*>\s?/gm, '');
    s = s.replace(/\|/g, ' ');
    s = s.replace(/[*_~]{1,3}/g, '');
    s = s.replace(/\n{2,}/g, '。').replace(/\n/g, ' ');
    return s.replace(/\s{2,}/g, ' ').trim();
  }

  let playingBtn = null; // 正在朗读的那条消息的按钮（单飞：同时只有一条在播）
  let playingSeq = null; // 正在朗读的消息 seq：后台事件会触发主界面就地翻新（重建消息行、按钮 DOM 换新），重挂按钮时靠它恢复「停止」态
  let lastOpenSid = null; // 上次 session:open 的会话 id：同 id 的事件驱动翻新（refreshCurrent 重开同一会话）不是用户切换，不该停朗读——远程/多会话并行时 report_ready、done 等事件密集，曾把长文朗读在首块后就安静砍掉

  let playURL = null;    // blob URL（播完/停止即 revoke）
  let playEl = null;     // 长寿命 <audio> 元素：自动播放策略按元素解锁——首块在点击
                         // 手势内播放过一次后，后续块换 src 续播不再需要新手势。曾每块
                         // new Audio：手机浏览器第二块起 play() 被拒（NotAllowedError），
                         // 表现为"只读第一句就停"。stopAudio 只停播不丢弃元素。
  let ttsAbort = null;   // 分段朗读在途请求的中止器（停止时连同预取一起砍）
  let ttsStopChain = null; // 分段播放链的停摆钩子（ttsPlay 内部赋值）
  let ttsGen = 0;        // 播放链代数：连点「停止→再点朗读」时新旧链的按钮与 seq 完全
                         // 相同（同一条消息），旧链迟到的 settle 靠它辨认——否则会把
                         // 新链刚挂上的「停止」态摘掉（朗读在播、按钮却显示「朗读」，
                         // 用户再点一次即把在播链当「再点停止」砍掉，观感就是朗读中断）

  function restBtn(btn) {
    btn.classList.remove('playing');
    btn.innerHTML = ICON.speak + '朗读';
    btn.title = '朗读这条消息';
  }

  function stopAudio() {
    if (ttsAbort) { try { ttsAbort.abort(); } catch (e) { /* 已中止 */ } ttsAbort = null; }
    if (ttsStopChain) { try { ttsStopChain(); } catch (e) { /* 已停 */ } ttsStopChain = null; }
    if (playEl) {
      playEl.onended = playEl.onerror = null;
      playEl.pause();
      // 不丢弃元素：保住自动播放解锁态（见上 playEl 声明处注释），只卸掉源释放解码
      playEl.removeAttribute('src');
      try { playEl.load(); } catch (e) { /* 个别环境对无源 load 抛错，忽略 */ }
    }
    if (playURL) { URL.revokeObjectURL(playURL); playURL = null; }
  }

  function stopSpeak() {
    stopAudio();
    if (playingBtn) { restBtn(playingBtn); playingBtn = null; }
    playingSeq = null;
  }

  /** 朗读文本切块：句末标点切大块、软标点收尾小块；首块刻意更短（尽快出声）。
   * 后端 /api/voice/tts 虽是流式响应，但浏览器没有对 POST 渐进播放的通用手段
   * （旧实现 res.blob() 攒齐全文才开播——长消息要等微软合成完整篇才出声），
   * 故改为按句分段合成 + 顺序播放 + 预取：首块约 0.5–1.5s 出声，后续块在
   * 上一块播放期间已在下载。 */
  function splitSpeechChunks(text) {
    const parts = [];
    let buf = '';
    for (const ch of text) {
      buf += ch;
      const hard = '。！？!?；;\n'.indexOf(ch) >= 0;
      const soft = '，,、：:）)'.indexOf(ch) >= 0;
      const first = parts.length === 0;
      const max = first ? 80 : 180;
      const min = first ? 18 : 60;
      if ((hard && buf.length >= min) || (soft && buf.length >= max) || buf.length >= 240) {
        const t = buf.trim();
        if (t) parts.push(t);
        buf = '';
      }
    }
    const tail = buf.trim();
    if (tail) parts.push(tail);
    return parts.length ? parts : [text.trim()];
  }

  function ttsFetchBlob(text, signal) {
    return fetch('/api/voice/tts', {
      method: 'POST', signal,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text, rate: spokenRate(), voice: pref.get('edgeVoice', '') }),
    }).then(async (res) => {
      if (!res.ok) throw new Error(apiErrMsg(res, await res.text().catch(() => '')));
      return res.blob();
    });
  }

  /** 后端合成并播放一段文本（分段流式）：返回 Promise，全部播完/出错/被停时落定
   * （单飞：先停上一条）。播放队列预取窗口 3 块：块 i 在播时，i+1/i+2 已在合成。 */
  function ttsPlay(text) {
    stopAudio();
    ttsAbort = new AbortController();
    const signal = ttsAbort.signal;
    const chunks = splitSpeechChunks(text);
    const blobs = chunks.map(() => null);
    let idx = 0, stopped = false;
    const prefetch = () => {
      for (let i = idx; i < Math.min(idx + 3, chunks.length); i++) {
        if (!blobs[i]) blobs[i] = ttsFetchBlob(chunks[i], signal);
      }
    };
    prefetch();
    return new Promise((resolve, reject) => {
      const playChunk = () => {
        if (stopped) return;
        if (idx >= chunks.length) { stopAudio(); resolve(); return; }
        const i = idx;
        blobs[i].then((blob) => {
          if (stopped || signal.aborted) return;
          playURL = URL.createObjectURL(blob);
          if (!playEl) playEl = new Audio(); // 长寿命元素：只建一次，后续块换 src 续播
          playEl.src = playURL;
          playEl.onended = () => {
            URL.revokeObjectURL(playURL); playURL = null;
            idx += 1;
            prefetch();
            playChunk();
          };
          // 收尾回调先验本链（与上一行 blobs[i].then 同款守卫）：旧链被停/被顶替后
          // 其 play() 会因 stopAudio 的 pause() 以 AbortError 迟到 reject——此刻新链
          // 往往已启动，无守卫的 stopAudio() 会误杀新链（abort 其预取、整条安静砍掉），
          // 表现为「点朗读键没声音」，桌面端鼠标连点停止→再点朗读（几百毫秒内）最易踩中。
          const gone = () => stopped || signal.aborted;
          playEl.onerror = () => { if (gone()) return; stopAudio(); reject(new Error('播放失败')); };
          playEl.play().catch((e) => { if (gone()) return; stopAudio(); reject(e); });
        }).catch((e) => {
          if (signal.aborted || stopped) return;
          stopAudio();
          reject(i === 0 ? e : new Error('朗读中断：' + (e && e.message ? e.message : e)));
        });
      };
      ttsStopChain = () => { stopped = true; resolve(); }; // 被停/被顶替：安静落定（按钮由 stopSpeak 复位）
      playChunk();
    });
  }

  /** 朗读一条消息：按钮态驱动 + 后端合成播放。 */
  async function speakTextBackend(text, btn) {
    const mySeq = btn.dataset && btn.dataset.seq != null ? Number(btn.dataset.seq) : null;
    const gen = ++ttsGen; // 本链代数：settle 只认它（见 ttsGen 声明处注释）
    playingBtn = btn;
    playingSeq = mySeq;
    btn.classList.add('playing');
    btn.innerHTML = ICON.stop + '停止';
    btn.title = '停止朗读';
    // 收尾复位：本链仍是最新链才动——被 stopSpeak（切会话/再点停止）或新的朗读
    // 顶替后不得碰；播放期间翻新把按钮迁到新 DOM（playingBtn≠btn 但
    // playingSeq===mySeq）仍复位那个新按钮（翻新不改代数，正常覆盖）
    const settle = () => {
      if (gen !== ttsGen) return;
      if (playingBtn) restBtn(playingBtn);
      playingBtn = null;
      playingSeq = null;
    };
    try {
      await ttsPlay(text);
      settle();
    } catch (e) {
      settle();
      voiceToast('朗读失败：' + (e && e.message ? e.message : e));
    }
  }

  function speakText(text, btn) {
    if (playingBtn === btn) { stopSpeak(); return; } // 再点一次 = 停止
    stopSpeak(); // 单飞：新的朗读立即停掉上一条
    speakTextBackend(text, btn);
  }

  /** 朗读任意文本（无按钮态；右键菜单等外部入口）：与消息按钮同一条链路
   *（同分块播放器、同音色语速），失败走组件 toast。 */
  async function speakPlain(text) {
    playingBtn = null;
    playingSeq = null; // 无按钮入口（右键菜单）：不关联任何消息，翻新重挂的按钮不得误恢复「停止」态
    try {
      await ttsPlay(text);
    } catch (e) {
      voiceToast('朗读失败：' + (e && e.message ? e.message : e));
    }
  }

  // 消息操作行钩子：给每条助手消息挂「朗读」（钩子可注销，热关闭后新渲染的行不会再有）
  function onMessageAction(row, m) {
    if (!active || m.role !== 'assistant') return;
    const text = cleanForSpeech(m.content);
    if (!text) return;
    const btn = document.createElement('button');
    btn.className = 'act-copy act-tts';
    if (m.seq != null) btn.dataset.seq = m.seq;
    if (m.seq != null && m.seq === playingSeq) {
      // 就地翻新重建了消息行：这条消息正在朗读中，新按钮接上「停止」态
      //（playingBtn 指向的旧按钮已随 DOM 换掉，不接上就没法停/态对不上）
      playingBtn = btn;
      btn.classList.add('playing');
      btn.innerHTML = ICON.stop + '停止';
      btn.title = '停止朗读';
    } else {
      btn.innerHTML = ICON.speak + '朗读';
      btn.title = '朗读这条消息';
    }
    btn.onclick = () => speakText(text, btn);
    row.appendChild(btn);
  }

  /* ================= 二、听写（语音输入，浏览器 SpeechRecognition） =================
     以下为本组件一直以来的实现，原样保留（原属基础架构 web/app-chat.js，组件化时
     迁入）。四个层次：
     1) 连接：点麦克风先进「连接中」——按钮转圈，不算开始说话。连上（onstart）才
        变蓝进入录音态；VOICE_CONNECT_TIMEOUT 之内连不上就退避重试，重试
        VOICE_MAX_RETRY 次仍连不上才弹报错。首次连接往往比后续慢（要跟云端服务
        建链），所以这里的等待给得比"点了没反应"的看门狗宽，且**不打断正在建立
        的连接**。
     2) 识别：浏览器原生 Web Speech（WebView2/Edge/Chrome——微软的识别服务）。
        音频由浏览器直接发给云端识别服务，链路抖动、静音收尾、引擎自行结束都很
        常见 → 录音态掉线同样退避重连。
     3) 灰字：转写先落在叠层上显示为灰字——它不在输入框的值里，所以不可编辑、
        不可选中；静置 VOICE_SETTLE_MS（2.5 秒）后落成黑色正文写进输入框，此后
        与手打文字一样可编辑。断线收尾时把已有灰字立即落定，避免白说。
     4) 节流：单次听写最长 maxDictateSec()（默认 240 秒），最后 9 秒在麦克风按钮
        上出倒计时徽标。 */

  const voiceOn = { main: false, sub: false };
  const voiceSess = { main: null, sub: null };

  // 桌面壳判定（runDesktop 导航的 URL 带 shell=desktop，app-core 据此给 <html>
  // 挂 desktop-shell 类）与 WebView2/Edge 运行时版本（UA 的 Edg/ 段）。听写走
  // WebView2 内置的浏览器识别，该能力随运行时版本波动（微软 153 系运行时有官方
  // 回归、152 正常），失败提示里带上版本，在出问题的机器上好对号入座。
  function desktopShell() {
    return document.documentElement.classList.contains('desktop-shell');
  }
  function edgeRuntimeVer() {
    const m = navigator.userAgent.match(/Edg\w*\/([\d.]+)/);
    return m ? m[1] : '';
  }
  function dictationFailMsg(err) {
    const code = err ? '（' + err + '）' : '';
    if (desktopShell()) {
      const v = edgeRuntimeVer();
      return '语音识别连接失败' + code + '：桌面壳的听写依赖 WebView2 运行时内置的识别通道，'
        + '部分运行时版本存在故障' + (v ? '（当前 ' + v + '）' : '')
        + '。可更新 WebView2 运行时后重试，或以 -browser 参数启动、用 Edge 浏览器打开本页使用语音';
    }
    return '语音识别连接失败（连续 ' + VOICE_MAX_RETRY + ' 次）：浏览器可能不支持或识别服务不可达，建议换用 Microsoft Edge 浏览器' + code;
  }

  const VOICE_MAX_RETRY = 3;          // 连续连不上/没结果的重试上限（任一次拿到文字即清零）
  const VOICE_RETRY_BASE = 400;       // 首次重试等待（毫秒），此后线性退避
  const VOICE_RETRY_STEP = 400;       // 每多一次重试多等这么久
  const VOICE_RETRY_MAX = 1600;       // 单次等待上限（毫秒）
  const VOICE_CONNECT_TIMEOUT = 5000; // 连接阶段等待上限（毫秒）：超时才判这次失败
  const VOICE_SETTLE_MS = 2500;       // 灰字静置多久落成正文
  const VOICE_SWEEP_MS = 400;         // 定稿巡检间隔
  const VOICE_STOP_GRACE = 1200;      // 用户按停后等引擎吐完最后一句的兜底时长（毫秒）
  const VOICE_TAIL_WAIT_MS = 8000;    // 按停后等在途整段识别的上限（毫秒），超了才收尾

  /**
   * 把「上一段已有文字」与「新到文字」拼起来：中文之间不加空格（语音转写的中文
   * 分段常常只是停顿，插空格会切碎句子），英文/数字单词之间保留一个空格。
   */
  function voiceJoin(prev, next) {
    if (prev === '') return next;
    const tail = prev.charAt(prev.length - 1), head = next.charAt(0);
    return (/[A-Za-z0-9]/.test(tail) && /[A-Za-z0-9]/.test(head)) ? prev + ' ' + next : prev + next;
  }

  /**
   * 灰色暂定层：把还没定稿的转写文字叠在输入框上显示为灰字。
   * 它 pointer-events:none 且不进输入框的值 → 不可编辑、不可选中、不吃点击；
   * 已定稿正文用透明占位一起排版，保证灰字正好接在正文末尾（换行位置一致）。
   */
  function voiceShadow(inp, tail) {
    const card = inp.closest ? inp.closest('.composer-card') : null;
    if (!card) return;
    let sh = null;
    for (const child of card.children) { if (child.classList.contains('voice-shadow')) { sh = child; break; } }
    if (!sh) {
      sh = document.createElement('div');
      sh.classList.add('voice-shadow', 'hidden');
      card.appendChild(sh);
    }
    if (!tail) {
      sh.classList.add('hidden');
      sh.textContent = '';
      if (inp.classList) inp.classList.remove('voice-dictating');
      return;
    }
    if (getComputedStyle(card).position === 'static') card.style.position = 'relative';
    const cs = getComputedStyle(inp);
    // 相对卡片定位（灰字层挂在卡片上）：用视口坐标相减，不依赖 offsetParent——
    // 输入框后来包进了 composer-input-wrap（position:relative，见标记胶囊背衬层），
    // offsetLeft/Top 的参照变成了 wrapper，会差出附件区/引用胶囊条的高度
    const r = inp.getBoundingClientRect(), cr = card.getBoundingClientRect();
    sh.style.left = (r.left - cr.left) + 'px';
    sh.style.top = (r.top - cr.top) + 'px';
    sh.style.width = r.width + 'px';
    sh.style.height = r.height + 'px';
    ['font-size', 'font-family', 'font-weight', 'line-height', 'letter-spacing',
      'padding', 'box-sizing', 'white-space', 'word-break', 'overflow-wrap', 'text-align']
      .forEach((k) => { sh.style[k] = cs[k]; });
    // 输入框内容超出可视高度而滚动时，灰字层跟着上移，保持对齐
    sh.style.transform = 'translateY(-' + (inp.scrollTop || 0) + 'px)';
    if (inp.classList) inp.classList.add('voice-dictating'); // 灰字在时隐掉 placeholder，避免两层字叠在一起
    sh.textContent = '';
    const head = document.createElement('span');
    head.classList.add('voice-shadow-head');
    head.textContent = inp.value;            // 透明占位：与正文同排版，灰字接在其后
    const rest = document.createElement('span');
    rest.classList.add('voice-shadow-tail');
    rest.textContent = tail;
    sh.append(head, rest);
    sh.classList.remove('hidden');
  }

  /** 组件卸载时连节点一起摘掉灰字层（正常运行期只隐藏，留着复用）。 */
  function removeVoiceShadow(inp) {
    if (inp.classList) inp.classList.remove('voice-dictating');
    const card = inp && inp.closest ? inp.closest('.composer-card') : null;
    if (!card) return;
    for (const child of Array.from(card.children)) {
      if (child.classList.contains('voice-shadow')) child.remove();
    }
  }

  /** 一次性轻提示（语音连不上/朗读失败等），浮在输入区上方，不打断输入。 */
  function voiceToast(text) {
    const el = document.createElement('div');
    el.classList.add('voice-toast');
    el.textContent = text;
    document.body.appendChild(el);
    requestAnimationFrame(() => el.classList.add('show'));
    setTimeout(() => {
      el.classList.remove('show');
      setTimeout(() => { if (el.remove) el.remove(); }, 300);
    }, 6000);
  }

  /** 把一段文字落进输入框正文（程序化写入不触发 input 事件：手动落草稿）。
   *  落框顺带打 jxVoice 标记：发送时 app-chat.js 读它随消息上报 voiceUsed，
   *  后端置位会话粘性标记——用过一次语音，环境快照从此每轮带同音错字提示。
   *  输入框被清空（发送清框 / 用户手动删光）时标记随之撤销（见 init 的监听）。 */
  function commitText(view, text) {
    if (!text) return;
    const inp = view.input;
    inp.value = voiceJoin(inp.value, text);
    inp.dataset.jxVoice = '1';
    autoGrow(inp);
    if (view === mainView) persistMainDraftLS(); else persistSubDraftLS();
  }

  function bindVoiceButton(view) {
    const btn = viewEl(view, 'btn-voice');
    if (!btn) return;
    const key = view.key;
    // 一次听写的会话状态（重连全程复用同一份，只换识别实例）
    const sess = voiceSess[key] = {
      rec: null,          // 当前识别实例：每次重试换新实例（onend 之后不能可靠复用）
      retries: 0,         // 本次听写内连续「没结果」的次数
      active: false,      // 会话是否还在（true 期间按钮是连接中/录音态、灰字层工作）
      live: false,        // 当前实例是否已连上（onstart 之后为 true）
      done: false,        // 已收尾：用户点了停止、超时、或重试次数用尽
      stopping: false,    // 用户已按停：等引擎吐完最后一句就收尾，不再重试
      segs: new Map(),    // 未定稿的转写：槽位索引 → { text, final, changedAt }
      committed: new Map(), // 已落进正文的槽位：索引 → 已落定文字（引擎重报时按前缀剥离）
      maxIdx: -1,         // 引擎当前报到的槽位（比它小说明引擎重置了结果列表）
      startedAt: 0,       // 本次听写开始时刻（时长上限与倒计时用）
      count: null,        // 倒计时徽标元素
      spin: null,         // 连接中转圈元素
      restartTimer: null, // 退避重试定时器
      connectTimer: null, // 连接阶段超时定时器
      stopTimer: null,    // 按停后的兜底收尾定时器
      hintTimer: null,    // 提示文案的清除定时器
      sweepTimer: null,   // 定稿巡检定时器
      teardown: null,     // 组件卸载时的收尾（下方赋值）
    };
    let lastTail = null, lastHead = null; // 灰字层上次渲染的内容（避免每 400ms 重排）
    if (getComputedStyle(btn).position === 'static') btn.style.position = 'relative';

    const clearT = (name) => { if (sess[name]) { clearTimeout(sess[name]); sess[name] = null; } };
    // 提示文案：同一时刻只留一条；ms 为空 = 常驻到下一次提示
    const setHint = (text, ms) => {
      clearT('hintTimer');
      view.hint.textContent = text;
      if (ms) sess.hintTimer = setTimeout(() => { if (view.hint.textContent === text) view.hint.textContent = ''; }, ms);
    };
    // 进度提示（连接中/重连中）：这类文案由我们写、也由我们清；用标记区分，
    // 免得把别的提示（报错等）误清掉
    const setProgressHint = (text) => { sess.progressHint = true; setHint(text); };
    const clearProgressHint = () => { if (sess.progressHint) { sess.progressHint = false; setHint(''); } };
    // 按钮三态：空闲（白）/ 连接中（转圈）/ 录音中（蓝色脉冲）
    const setSpin = (on) => {
      if (on && !sess.spin) {
        sess.spin = document.createElement('span');
        sess.spin.classList.add('voice-spinner');
        btn.appendChild(sess.spin);
        btn.classList.add('connecting');
      } else if (!on && sess.spin) {
        sess.spin.remove();
        sess.spin = null;
        btn.classList.remove('connecting');
      }
    };
    // 倒计时徽标：最后 9 秒显示秒数，换数字时弹一下
    const setCount = (n) => {
      if (n === null) {
        if (sess.count) sess.count.remove();
        sess.count = null;
        return;
      }
      if (!sess.count) {
        sess.count = document.createElement('span');
        sess.count.classList.add('voice-count');
        btn.appendChild(sess.count);
      }
      if (sess.count.textContent !== String(n)) {
        sess.count.textContent = String(n);
        sess.count.classList.remove('pop');
        void sess.count.offsetWidth; // 触发重排，让动画能重放
        sess.count.classList.add('pop');
      }
    };
    // 灰字层渲染（tail 为空即隐藏）
    const renderShadow = () => {
      const inp = view.input;
      let tail = '';
      for (const i of Array.from(sess.segs.keys()).sort((a, b) => a - b)) tail += sess.segs.get(i).text;
      if (tail === lastTail && inp.value === lastHead) return;
      lastTail = tail;
      lastHead = inp.value;
      voiceShadow(inp, tail);
    };
    // 把指定槽位定稿：写进输入框正文（黑字），从灰字层移除
    const commitSegs = (idxs) => {
      let text = '';
      for (const i of idxs.slice().sort((a, b) => a - b)) {
        const seg = sess.segs.get(i);
        if (!seg) continue;
        sess.segs.delete(i);
        sess.committed.set(i, (sess.committed.get(i) || '') + seg.text);
        text += seg.text;
      }
      if (text !== '') commitText(view, text);
      renderShadow();
    };
    // 全部定稿（断线收尾、用户按停、引擎重置结果列表时用：先说出来的先落定，不丢字）
    const commitAll = () => {
      if (sess.segs.size > 0) commitSegs(Array.from(sess.segs.keys()));
      else renderShadow();
    };
    // 收尾：定稿残留灰字、停掉实例与定时器、按钮回空闲、灰字层收起
    const finish = (msg, toast) => {
      const rec = sess.rec;
      sess.progressHint = false;
      sess.active = false;
      sess.live = false;
      sess.done = true;
      sess.stopping = false;
      sess.rec = null;
      clearT('restartTimer');
      clearT('connectTimer');
      clearT('stopTimer');
      if (sess.sweepTimer) { clearInterval(sess.sweepTimer); sess.sweepTimer = null; }
      commitAll();
      voiceOn[key] = false;
      btn.classList.remove('on');
      setSpin(false);
      setCount(null);
      try { if (rec && rec.isLocal) rec.abort(); else if (rec) rec.stop(); } catch (e) { /* 已结束 */ }
      // 收尾必须把常驻提示收掉（录音中…/正在识别…）：否则它会挂在输入框下面
      // 不消失（用户报过：整段说完、消息都发出去了还显示「已出 N 句」）
      if (msg) setHint(msg, 4000); else setHint('');
      if (toast) voiceToast(toast);
      // 焦点归还（2026-10-12 修）：点按钮开启听写后焦点一直挂在麦克风按钮上，
      // 按 Alt 停止后按回车，激活的是按钮（反复开关语音）而不是发送。收尾时
      // 若焦点还停在本按钮或游离在 body 上（开启语音前没点过输入框），把光标
      // 还给输入框并放到末尾——回车即可发送。焦点在别处（如设置页）时不打扰。
      const ae = document.activeElement;
      if (ae === btn || ae === document.body) {
        try {
          view.input.focus();
          view.input.setSelectionRange(view.input.value.length, view.input.value.length);
        } catch (e) { /* 非 text 控件忽略 */ }
      }
    };
    // 用户按停（含时长到顶）：给引擎一点时间把最后一句吐完，1.2 秒兜底收尾；
    // 等待期按钮转圈 +「正在识别…」，让用户知道识别还在跑（这一秒原先没有任何反馈）。
    // 本地引擎例外：按句即时出字，stop() 等尾段精修的 done 回包后收尾。
    const stopByUser = () => {
      if (sess.stopping || sess.done) return;
      sess.stopping = true;
      clearT('restartTimer');
      clearT('connectTimer');
      const rec = sess.rec;
      const local = !!(rec && rec.isLocal);
      // 收尾这段「等最后一句 / 等离线精修」的空白期给个转圈：一秒左右没反馈时
      // 用户分不清是还在识别、还是根本没录到（按钮由蓝色脉冲换成转圈 + 一行字）
      btn.classList.remove('on');
      setSpin(true);
      setProgressHint('正在识别…');
      try { if (rec) rec.stop(); } catch (e) { /* 已结束 */ }
      if (local) return; // 本地引擎按句即时出字：stop() 里直接收尾，尾段稍后落
      clearT('stopTimer');
      sess.stopTimer = setTimeout(() => { finish(''); }, VOICE_STOP_GRACE);
    };
    // 退避重试；连续 VOICE_MAX_RETRY 次都没结果就放弃并弹报错。幂等——已有重试在
    // 排队时直接忽略（同一轮故障常同时给 onerror 与 onend 两个回调）。
    const scheduleRetry = () => {
      if (!sess.active || sess.done || sess.stopping || sess.restartTimer) return;
      if (sess.retries >= VOICE_MAX_RETRY) {
        // 带上最后一次的错误码（network / no-speech / aborted / language-not-supported…）；
        // 桌面壳与浏览器形态给不同指引（桌面壳指向 WebView2 运行时，见 dictationFailMsg）
        finish('语音识别已停止', dictationFailMsg(sess.lastErr));
        return;
      }
      const wait = Math.min(VOICE_RETRY_BASE + sess.retries * VOICE_RETRY_STEP, VOICE_RETRY_MAX);
      sess.retries += 1;
      sess.live = false;
      btn.classList.remove('on');
      setSpin(true); // 等待重试期间也转圈：让用户看到「还在连」，不是又白了一下
      setProgressHint('语音识别连接不上，正在重连（' + sess.retries + '/' + VOICE_MAX_RETRY + '）…');
      sess.restartTimer = setTimeout(() => { sess.restartTimer = null; startOnce(); }, wait);
    };
    // 定稿巡检：说话中不落字（引擎持续在改），停下约 2.5 秒才落成正文；
    // 顺带管倒计时与时长上限
    const sweep = () => {
      if (!sess.active) return;
      const t = Date.now();
      const ready = [];
      for (const [i, seg] of sess.segs) {
        if (t - seg.changedAt >= VOICE_SETTLE_MS) ready.push(i);
      }
      // 本地引擎的灰字是草稿（定稿时会整段替换），不参与 2.5 秒静置落黑
      if (ready.length > 0 && !dictationIsLocal()) commitSegs(ready); else renderShadow();
      const remain = maxDictateSec() * 1000 - (t - sess.startedAt);
      if (remain <= 0) { stopByUser(); return; }
      setCount(remain <= 9000 ? Math.ceil(remain / 1000) : null);
    };
    const wsHint = () => '录音中·说话出灰字草稿，停顿约 2 秒定稿成正文；点麦克风结束';
    /* ---------- 本地引擎：伪流式（离线模型两档停顿），手机/电脑同一实现 ----------
     * 流式模型已删（163MB 太重且终稿质量逊于离线）。用离线模型 + 双停顿阈值
     * 复刻流式体验：
     *   短停顿（~0.2s）→ 该短语快速识别 → 灰字草稿（「听到了」的即时反馈，
     *                     多个短语可叠在灰字层）
     *   长停顿（尾随静音满 2s）/按钮结束 → 自上次定稿的全部
 *                     音频整段重识别（静音垫限量 1s，近似停顿中点切分）
     *                     → 黑字定稿，替换灰字草稿（全上下文更准、标点完整）
     * 全部走 HTTP POST（桌面/远程同一条路，远程经中继隧道天然可达）；
     * 桌面/远程体验完全一致。VAD：噪声底自适应，阈值=max(0.008, 底×3.5)。
     * ── 本地模型身份（仅注释记录，用户端文案一律只说「本地语音识别模型」）──
     * 小米 X-ASR 离线版（Zipformer2 transducer，中英双语、原生标点，int8 约
     * 168MB，评估对比定标），随组件目录分发、跑共享便携运行时 sherpa-onnx：
     *   models/xasr/  https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-x-asr-zipformer-transducer-zh-en-punct-int8-2026-06-03.tar.bz2 */
    const makeLocalRec = (stream) => {
      const ctx = new AudioContext({ sampleRate: 16000 });
      const src = ctx.createMediaStreamSource(stream);
      const node = ctx.createScriptProcessor(4096, 1, 1);
      const SPEECH_MIN_MS = 350;    // 短于此视为噪声毛刺，丢弃
      const DRAFT_PAUSE_MS = 200;   // 短停顿：出灰字草稿（该短语快速识别）
      const FINAL_PAUSE_MS = 2000;  // 长停顿：定稿（自上次定稿的全部音频重识别）
      let seg = [];                 // 当前短语的帧
      let speechMs = 0, silenceMs = 0, segSpeechMs = 0;
      let noiseFloor = 0.004;       // 环境噪声底（RMS，自适应）
      let sinceFinal = [];          // 自上次定稿的全部语音帧（短语间静音间隙限量收录）
      let sinceFinalMs = 0;         // 定稿段内的有声总时长
      let finalSilenceMs = 0;       // 尾随静音计时：满 2 秒才定稿（真停顿，非说话时长）
      let padMs = 0;                // 定稿段内已收录的静音垫（间隙+尾部，上限 1 秒）
      let ended = false, segCount = 0, failCount = 0;
      let draftEpoch = 0;          // 草稿代际：定稿时 +1，在途草稿的迟到应答按过期丢弃
      let level = 0;               // 实时音量 0..1（驱动按钮内波形条）
      let bars = null, barRaf = 0, barVol = 0; // B 款动效：按钮内波形（demo 选型）
                                   // （防「定稿已在路上、旧草稿又把重复文字塞回灰字层」的竞态）
      let pendingPost = 0;          // 在途识别（草稿+定稿）：按停收尾要等它清零
      const draftSlots = [];        // 灰字层里草稿占用的槽位号（定稿时撤下）
      const teardownMic = () => {
        if (ended) return true;
        ended = true;
        try { node.disconnect(); } catch (e) { /* 已断 */ }
        try { stream.getTracks().forEach((t) => t.stop()); } catch (e) { /* 已停 */ }
        removeBars();
        return false;
      };
      // B 款动效：录音态把图标位换成 5 根白色音量条（真音量驱动，demo 选型；
      // 蓝底白条为显式配色，不依赖 svg 的 currentColor——老内核浏览器的
      // 「纯蓝无白线」渲染怪癖对录音态天然免疫）
      const mountBars = () => {
        if (bars) return;
        bars = document.createElement('span');
        bars.className = 'voice-bars';
        for (let i = 0; i < 5; i++) bars.appendChild(document.createElement('i'));
        btn.appendChild(bars);
        btn.classList.add('bars-on');
        const tick = () => {
          if (!bars) return;
          barVol += (level - barVol) * 0.25; // 平滑，同 demo
          for (let i = 0; i < 5; i++) {
            const w = Math.sin(Date.now() * 0.012 + i * 1.7) * 0.5 + 0.5;
            bars.children[i].style.height = (3 + barVol * 11 * (0.35 + w * 0.65)) + 'px';
          }
          barRaf = requestAnimationFrame(tick);
        };
        barRaf = requestAnimationFrame(tick);
      };
      const removeBars = () => {
        if (barRaf) { cancelAnimationFrame(barRaf); barRaf = 0; }
        if (bars) { bars.remove(); bars = null; }
        btn.classList.remove('bars-on');
      };
      const joinFrames = (frames) => {
        const total = frames.reduce((n, c) => n + c.length, 0);
        const floats = new Float32Array(total);
        let off = 0;
        for (const c of frames) { floats.set(c, off); off += c.length; }
        return floats;
      };
      const post = (floats) => {
        pendingPost += 1;
        return voiceASRTranscribe(dictationEngine(), 'cpu', voicePcmToWavB64(floats, 16000))
          .finally(() => { pendingPost -= 1; });
      };
      // 灰字草稿：短停顿时该短语的快速识别（即时反馈「听到了」）
      const draftSeg = () => {
        const frames = seg;
        const spoken = segSpeechMs;
        seg = []; segSpeechMs = 0;
        if (spoken < SPEECH_MIN_MS || frames.length * 256 < 1600) return;
        const ep = draftEpoch;
        post(joinFrames(frames)).then((d) => {
          if (!active || sess.done || ep !== draftEpoch) return; // 已被定稿接管：草稿作废
          if (d.text) {
            const idx = sess.segs.size ? Math.max(...Array.from(sess.segs.keys())) + 1 : 0;
            sess.segs.set(idx, { text: d.text, final: false, changedAt: Date.now() });
            draftSlots.push(idx);
            sess.retries = 0;
            renderShadow();
          }
        }).catch(() => { /* 草稿失败不打扰：定稿还会再识别一遍 */ });
      };
      // 定稿：自上次定稿的全部音频整段重识别（全上下文，更准），替换灰字草稿
      // 撤下全部草稿灰字（定稿专用）
      const clearDrafts = () => {
        for (const i of draftSlots) { sess.segs.delete(i); sess.committed.delete(i); }
        draftSlots.length = 0;
        renderShadow();
      };
      // 定稿：自上次定稿的全部音频整段重识别（全上下文，更准），替换灰字草稿。
      // 无缝衔接的关键：灰字要等定稿文字到手的那一刻才撤（同一同步块里撤灰
      // 字、落黑字，一帧完成替换）——先撤后等会让内容「消失几秒」，像丢了。
      const finalizeSeg = () => {
        draftEpoch += 1;             // 在途草稿全部作废：本段音频由定稿接管（含尾巴短语）
        seg = []; segSpeechMs = 0; speechMs = 0; silenceMs = 0;
        const frames = sinceFinal;
        const spoken = sinceFinalMs;
        sinceFinal = []; sinceFinalMs = 0; finalSilenceMs = 0; padMs = 0;
        if (spoken < SPEECH_MIN_MS || frames.length * 256 < 1600) { clearDrafts(); return; }
        post(joinFrames(frames)).then((d) => {
          if (!active || sess.done) return; // 组件卸载/会话已收尾：迟到的定稿不落（防污染下一条草稿；灰字已由 finish 的 commitAll 兜底）
          if (d.text) {
            clearDrafts(); // 黑字到手此刻才撤灰字，同一帧完成替换
            commitText(view, d.text + (/[。！？.!?\s]$/.test(d.text) ? '' : ' '));
            segCount += 1;
          }
          // 识别为空：保留灰字草稿（有内容总比空白强）
        }).catch((e) => {
          if (!active || sess.done) return;
          failCount += 1;
          if (failCount === 1) voiceToast(voiceLocalHint(e && e.message) || '有一句识别失败（网络/服务波动），继续说即可');
          // 识别失败：保留灰字草稿，内容不消失；下轮定稿或 finish 的 commitAll 接手
        });
      };

      node.onaudioprocess = (ev) => {
        if (ended) return;
        const f = ev.inputBuffer.getChannelData(0);
        let sum = 0;
        for (let i = 0; i < f.length; i++) sum += f[i] * f[i];
        const rms = Math.sqrt(sum / f.length);
        level = Math.min(1, rms * 7); // 波形条驱动（demo 同标定）
        const frameMs = (f.length / ctx.sampleRate) * 1000;
        const isSpeech = rms > Math.max(0.008, noiseFloor * 3.5);
        if (!isSpeech) noiseFloor = Math.min(0.02, noiseFloor * 0.98 + rms * 0.02);
        if (isSpeech) {
          const f32 = new Float32Array(f);
          seg.push(f32); sinceFinal.push(f32);
          speechMs += frameMs; segSpeechMs += frameMs; sinceFinalMs += frameMs;
          silenceMs = 0; finalSilenceMs = 0;
        } else if (speechMs > 0) {
          silenceMs += frameMs; finalSilenceMs += frameMs;
          // 间隙/尾部静音限量入定稿段（垫上限 1s，近似「在停顿中点切分」：
          // 真停顿触发时尾部垫≈1s；不足 2s 的呼吸停顿全量保留不分段）
          if (sinceFinalMs > 0 && padMs < 1000) {
            sinceFinal.push(new Float32Array(f));
            padMs += frameMs;
          }
        }
        if (segSpeechMs >= SPEECH_MIN_MS && silenceMs >= DRAFT_PAUSE_MS) draftSeg();
        // 定稿=真停顿（尾随静音满 2 秒）。不再做 SEG_MAX=18s 连续说话强切：常在语句
        // 中途拦腰打断（体验差），去掉后由全局 maxDictateSec 上限与手动点停兜底；
        // 每次真停顿定稿时计时自然归零、重新累计
        if (sinceFinalMs >= SPEECH_MIN_MS && finalSilenceMs >= FINAL_PAUSE_MS) {
          finalizeSeg();
          speechMs = 0; silenceMs = 0;
        }
      };
      src.connect(node);
      node.connect(ctx.destination); // 某些引擎要求节点入图才会回调
      const stopAll = () => {
        if (teardownMic()) return 'ended';
        try { ctx.close(); } catch (e) { /* 已关 */ }
        return 'ok';
      };
      // 收尾等待：尾段定稿若还在途中，等它回来再下结论（防「刚按停就报
      // 未识别到、一秒后文字才落」的误报）；等在途清零或超上限。
      const settleThen = (cb, capMs) => {
        const t0 = Date.now();
        const tick = () => {
          if (pendingPost === 0 || Date.now() - t0 >= capMs) { cb(); return; }
          setTimeout(tick, 120);
        };
        tick();
      };
      const api = {
        isLocal: true,
        mountBars,
        // 正常按停：尾段定稿（等在途识别回来）再收尾
        stop() {
          if (stopAll() === 'ended') return;
          finalizeSeg();
          settleThen(() => {
            if (sess.done || sess.rec !== api) return; // 已收尾或已开始新一轮：这次结论作废
            finish(segCount > 0 ? '' : '未识别到语音内容');
          }, VOICE_TAIL_WAIT_MS);
        },
        // 放弃（发送前暂停/组件卸载）：停麦即止。已在灰字层的草稿由
        // pauseForSend 的 commitAll 同步落黑（那是已识别的真文字，随消息发出）
        abort() { teardownMic(); },
      };
      return api;
    };

    const startLocal = () => {
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        finish('当前页面无法访问麦克风（需 https 或本机地址）');
        return;
      }
      navigator.mediaDevices.getUserMedia({
        audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
      }).then((stream) => {
        if (!sess.active || sess.done) { // 会话已被收掉（快速点按）：直接放掉麦克风
          stream.getTracks().forEach((t) => t.stop());
          return;
        }
        sess.rec = makeLocalRec(stream);
        sess.live = true;
        clearT('connectTimer');
        setSpin(false);
        btn.classList.add('on');
        sess.rec.mountBars();
        setHint(wsHint(), 0);
      }).catch((e) => {
        const denied = e && (e.name === 'NotAllowedError' || e.name === 'PermissionDeniedError');
        finish(desktopShell() && !denied
          ? '麦克风不可用：请检查 Windows 设置 → 隐私 → 麦克风'
          : (denied ? '麦克风未授权：请在浏览器里允许麦克风权限' : '麦克风不可用：' + (e && e.message ? e.message : e)));
      });
    };

    const startOnce = () => {
      if (!sess.active || sess.done) return;
      if (dictationIsLocal()) { startLocal(); return; }
      let rec;
      try { rec = new SR(); } catch (e) { scheduleRetry(); return; }
      sess.rec = rec;
      sess.live = false;
      sess.maxIdx = -1;        // 新实例的结果列表从 0 重新计
      sess.committed.clear();  // 槽位索引跟着换了一套，清掉旧的落定记忆
      rec.lang = 'zh-CN';
      rec.interimResults = true; // 边说边出灰字
      rec.continuous = true;
      rec.onstart = () => {
        // 连上了：撤掉连接超时、转圈换蓝色。**不在这里清零重试计数**——引擎连上
        // 就断的「蓝一下就白」正是要重连的场景，清零会让它无限重连、麦克风一直
        // 占着；真正的清零在 onresult（拿到文字 = 这条链路确实通）。
        if (sess.rec !== rec) return;
        clearT('connectTimer');
        sess.live = true;
        setSpin(false);
        btn.classList.add('on');
        clearProgressHint();
      };
      rec.onresult = (e) => {
        if (sess.rec !== rec || !sess.active || sess.done) return;
        if (e.resultIndex < sess.maxIdx) commitAll(); // 引擎重置了结果列表：先落定已有的
        for (let i = e.resultIndex; i < e.results.length; i++) {
          const item = e.results[i];
          if (!item || item.length === 0) continue;
          let text = item[0].transcript || '';
          if (text === '') continue;
          // 这个槽位已经落定过（引擎把整句重报一遍）：只取新增部分，避免重复落字
          const done = sess.committed.get(i) || '';
          if (done !== '') {
            if (text === done) continue;
            if (text.indexOf(done) === 0) text = text.slice(done.length);
            if (text === '') continue;
          }
          const prev = sess.segs.get(i);
          if (prev && prev.final) continue; // 已定稿的槽位不再被 interim 覆盖
          const final = item[0].isFinal === true;
          if (!prev || prev.text !== text) sess.segs.set(i, { text, final, changedAt: Date.now() });
          sess.maxIdx = Math.max(sess.maxIdx, i);
        }
        sess.retries = 0; // 有转写进来 = 链路是通的，重试预算重新计
        clearProgressHint();
        renderShadow();
      };
      rec.onend = () => {
        if (sess.rec !== rec || !sess.active || sess.done) return;
        sess.rec = null;
        if (sess.stopping) { finish(''); return; } // 用户已按停：正常收尾
        commitAll(); // 断在半句上：先说出来的先落定，别白说
        scheduleRetry();
      };
      rec.onerror = (e) => {
        if (sess.rec !== rec || !sess.active || sess.done) return;
        const code = (e && e.error) || '';
        sess.lastErr = code; // 记下现场：3 次重试都失败时把它写进浮层提示
        // 只有重试也救不回来的错误直接收场；network / no-speech / aborted 等
        // 一律按瞬时故障处理，交给重试机制自愈
        if (code === 'not-allowed' || code === 'service-not-allowed') {
          // 桌面壳里点不出浏览器授权界面：麦克风授权来自启动参数自动放行，
          // 还失败多半是 Windows 系统级麦克风开关（隐私设置）在拦
          finish(desktopShell()
            ? '麦克风未授权：请检查 Windows 设置 → 隐私 → 麦克风（含「允许桌面应用访问麦克风」）'
            : '麦克风未授权：请在浏览器里允许麦克风权限');
          return;
        }
        if (code === 'audio-capture') { finish('没有可用的麦克风（可能被其他程序占用）'); return; }
        commitAll();
        scheduleRetry();
      };
      try {
        rec.start();
      } catch (e) { scheduleRetry(); return; }
      // 连接阶段超时：到点还没 onstart 就按这次失败处理（重试期间按钮继续转圈）
      clearT('connectTimer');
      sess.connectTimer = setTimeout(() => {
        if (sess.rec !== rec || !sess.active || sess.done || sess.live) return;
        sess.rec = null;
        try { rec.stop(); } catch (e) { /* 已结束 */ }
        commitAll();
        scheduleRetry();
      }, VOICE_CONNECT_TIMEOUT);
    };

    btn.onclick = () => {
      if (sess.active) { stopByUser(); return; } // 再点一次 = 结束听写（连接中/录音中都一样）
      sess.active = true;
      sess.done = false;
      sess.stopping = false;
      sess.live = false;
      sess.retries = 0;
      sess.startedAt = Date.now();
      sess.maxIdx = -1;
      sess.segs.clear();
      sess.committed.clear();
      voiceOn[key] = true;
      btn.classList.remove('on');
      setSpin(true); // 先转圈：连上了再变蓝
      setProgressHint(dictationIsLocal() ? '正在打开麦克风…' : '正在连接语音服务…');
      if (sess.sweepTimer) clearInterval(sess.sweepTimer);
      sess.sweepTimer = setInterval(sweep, VOICE_SWEEP_MS);
      // 开启听写即把光标交给输入框（2026-10-12 修）：点按钮开启时焦点此刻正落在
      // 按钮上，不交走的话之后按回车会反复触发本按钮（开关语音）而不是发送；
      // 语音文字的落点本来就是输入框，光标放末尾，说完/打字无缝衔接。
      try {
        view.input.focus();
        view.input.setSelectionRange(view.input.value.length, view.input.value.length);
      } catch (e) { /* 非 text 控件忽略 */ }
      startOnce();
    };

    // 组件卸载：把这一套会话彻底收干净（定时器、识别实例、按钮附加物、灰字层、提示）
    sess.teardown = () => {
      finish('');
      sess.segs.clear();
      sess.committed.clear();
      setHint('');
      removeVoiceShadow(view.input);
      btn.classList.remove('on', 'connecting');
      voiceOn[key] = false;
    };

    // 交给快捷键引擎的控制器（点按=toggle，长按=start/stop；pauseForSend=发送前
    // 收尾并等待落定，见 init 里注册的 JX send 钩子）
    return {
      active: () => sess.active,
      toggle: () => btn.onclick(),
      start: () => { if (!sess.active) btn.onclick(); },
      stop: () => { if (sess.active) btn.onclick(); },
      // 发送前收尾（可等待）：走与用户按停相同的收尾路径——本地引擎定稿尾段
      //（整段重识别，比草稿准）并等在途识别清零，浏览器引擎等引擎吐完最后一句
      //（finish 会把灰字/最后定稿落进输入框）。会话真正落定（done）后才放行，
      // 随说的内容随这条消息一起发出，不再「先发出、文字迟到」。VOICE_TAIL_WAIT_MS
      // 兜底：等不到就按已有内容落定硬收尾（迟到结果由 finalizeSeg 的 done 检查
      // 丢弃，不污染下一条草稿）。
      pauseForSend: () => {
        if (!sess.active) return Promise.resolve();
        if (!sess.done) stopByUser();
        return new Promise((resolve) => {
          const t0 = Date.now();
          const tick = () => {
            if (sess.done) { resolve(); return; }
            if (Date.now() - t0 >= VOICE_TAIL_WAIT_MS) { finish(''); resolve(); return; }
            setTimeout(tick, 120);
          };
          tick();
        });
      },
    };
  }

  /* ================= 三、设置区（独立设置页「语音」） ================= */

  function mountSettings(el) {
    el.innerHTML = '';

    const note = (text) => {
      const p = document.createElement('p');
      p.className = 'sp-note';
      p.textContent = text;
      el.appendChild(p);
    };
    const group = (title) => {
      const g = document.createElement('div');
      g.className = 'sp-group';
      g.textContent = title;
      el.appendChild(g);
    };
    const row = (label, ctrl) => {
      const r = document.createElement('div');
      r.className = 'sp-row';
      const b = document.createElement('span');
      b.className = 'sp-label';
      b.textContent = label;
      r.appendChild(b);
      r.appendChild(ctrl);
      el.appendChild(r);
      return r;
    };
    const btn = (text, title) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'sp-btn';
      b.textContent = text;
      if (title) b.title = title;
      return b;
    };

    /* ----- 朗读（后端 Edge 通道，免配置） ----- */
    group('朗读（后端微软 Edge 通道，免配置）');
    note('朗读由后端直连微软 Edge「大声朗读」接口（无密钥），任何浏览器、远程手机端行为一致。');

    // 语速：滑块 + 当前倍率（后端折成 SSML 百分比语速）
    const rate = document.createElement('input');
    rate.type = 'range';
    rate.min = '0.5'; rate.max = '3'; rate.step = '0.1';
    rate.value = String(spokenRate());
    rate.className = 'sp-range';
    const rateVal = document.createElement('span');
    rateVal.className = 'sp-value';
    rateVal.textContent = rate.value + '×';
    rate.oninput = () => { rateVal.textContent = rate.value + '×'; pref.set('rate', parseFloat(rate.value)); };
    row('朗读语速', rate).appendChild(rateVal);

    // 音色：Edge 神经音色清单（/api/voice/voices，中文排前）
    const sel = document.createElement('select');
    sel.className = 'sp-select';
    const fillVoices = (voices) => {
      sel.innerHTML = '';
      const auto = document.createElement('option');
      auto.value = '';
      auto.textContent = '自动（中文用晓晓）';
      sel.appendChild(auto);
      for (const v of voices) {
        const o = document.createElement('option');
        o.value = v.name;
        o.textContent = v.display;
        sel.appendChild(o);
      }
      sel.value = pref.get('edgeVoice', '');
    };
    fillVoices([]);
    fetch('/api/voice/voices').then(async (res) => {
      if (!res.ok) throw new Error(apiErrMsg(res, await res.text().catch(() => '')));
      const d = await res.json();
      if (!el.isConnected) return; // 设置区已收起：结果作废
      fillVoices(d.voices || []);
    }).catch((e) => {
      const o = document.createElement('option');
      o.value = '';
      o.textContent = '音色清单获取失败（' + (e && e.message ? e.message : e) + '）——点试听可验证服务连通';
      sel.innerHTML = '';
      sel.appendChild(o);
    });
    sel.onchange = () => pref.set('edgeVoice', sel.value);
    row('朗读音色', sel);

    // 试听：播放中变「停止」（走同一条后端通道）
    const test = btn('试听', '用当前语速与音色试听一句');
    let testing = false;
    const setTesting = (on) => { testing = on; test.textContent = on ? '停止' : '试听'; };
    test.onclick = () => {
      if (testing) { stopSpeak(); setTesting(false); return; }
      setTesting(true);
      ttsPlay('你好，这是坚祥数字助理的语音试听。')
        .catch((e) => voiceToast('试听失败：' + (e && e.message ? e.message : e)))
        .then(() => { if (testing && !playEl) setTesting(false); });
    };
    row('试听', test);

    /* ----- 听写 ----- */
    group('听写');
    const sec = document.createElement('input');
    sec.type = 'number';
    sec.min = '30'; sec.max = '600'; sec.step = '10';
    sec.className = 'sp-num';
    sec.value = String(maxDictateSec());
    sec.title = '单次听写的最长时长；到点自动收尾，最后 9 秒按钮上出倒计时';
    sec.onchange = () => {
      const v = Math.round(Number(sec.value));
      const ok = isFinite(v) && v >= 30 && v <= 600 ? v : 240;
      sec.value = String(ok);
      pref.set('maxSec', ok);
    };
    const unit = document.createElement('span');
    unit.className = 'sp-value';
    unit.textContent = '秒（30–600，默认 240）';
    row('单次听写时长上限', sec).appendChild(unit);

    // 听写引擎：本地（默认）/ 云端（浏览器内置识别），不点名厂商
    const engSel = document.createElement('select');
    engSel.className = 'sp-select';
    // 对用户只分「本地/云端」两档，不点名模型与厂商：云端走浏览器内置识别，
    // 不同浏览器接入的服务商本来就不同（Edge/WebView2、Chrome、国产浏览器各异）
    const engOpts = [
      ['xasr', '本地语音识别模型（离线·默认）'],
      ['browser', '云端语音识别模型（当前浏览器内置识别）'],
    ];
    for (const [v, t] of engOpts) {
      const o = document.createElement('option');
      o.value = v; o.textContent = t;
      engSel.appendChild(o);
    }
    engSel.value = String(pref.get('engine', 'xasr')) === 'browser' ? 'browser' : 'xasr';
    engSel.onchange = () => { pref.set('engine', engSel.value); };
    row('听写引擎', engSel);


    note('「本地识别」全程离线（音频不出本机/服务器）：说完一句停顿约 2 秒自动'
      + '落成正文（电脑端另有实时灰字预览）。「云端识别」由当前浏览器内置的识别能力提供（各浏览器'
      + '接入的服务商不同，且依赖其在线服务可达）。默认本地，需要时手动切换；'
      + '首次使用本地识别需加载模型（几秒）。');

    /* ----- 快捷键（主/副键帽框与录制器都是主程序 HK 的共用控件；本组件只交出
       名称/默认值/存储。设置 · 快捷键 总表里是同一份数据，两边改互相同步） ----- */
    group('语音快捷键');

    // 触发方式：点按切换 / 长按输入
    const modeSel = document.createElement('select');
    modeSel.className = 'sp-select';
    const m1 = document.createElement('option');
    m1.value = 'tap'; m1.textContent = '点按切换（默认：按一下开始，再按一下结束）';
    const m2 = document.createElement('option');
    m2.value = 'hold'; m2.textContent = '长按输入（按住说话，松开结束）';
    modeSel.append(m1, m2);
    modeSel.value = voiceShortcutMode();
    modeSel.onchange = () => {
      pref.set('shortcutMode', modeSel.value === 'hold' ? 'hold' : 'tap');
      pref.set('shortcutModeSet', true); // 记下"用户显式选过"：默认归一不再动它
      firedLatch = false;
      refreshShortcutPlaceholders();
    };
    row('触发方式', modeSel);

    // 主/副键帽框：HK.keyCapBox（点击录制、✕ 停用、冲突检测与恢复默认都在 HK）。
    // 老版主程序没有 HK 时给出提示而不是让整页崩掉。
    if (voiceHK && window.HK) {
      row('主快捷键', HK.keyCapBox(voiceHK, 'main'));
      const subWrap = document.createElement('span');
      subWrap.className = 'hk-sub-wrap';
      subWrap.appendChild(HK.keyCapBox(voiceHK, 'sub'));
      const resetBtn = btn('恢复默认', '主=鼠标侧键、副=Alt 键');
      resetBtn.onclick = () => HK.restoreDef(voiceHK);
      subWrap.appendChild(resetBtn);
      row('副快捷键', subWrap);
      note('主＝鼠标侧键（前后两枚都算）、副＝Alt 键，两个框各自点击即可**独立录制**'
        + '自定义组合（键盘单键/组合键、鼠标左右键同按、单个鼠标侧键——录制时按下'
        + '某一枚侧键即记为「鼠标侧键1」（后退位）或「鼠标侧键2」（前进位），'
        + '只认那一枚，另一枚留给浏览器前进/后退；不支持单独的左键或右键；'
        + '框内 ✕ 停用该槽）。「恢复默认」＝主侧键、副 Alt。**注意：独立语音悬浮窗'
        + '只接管右 Alt 与右 Ctrl**——左 Alt、左 Ctrl 留给系统与其它程序（菜单栏、'
        + 'Alt+Tab、Ctrl+组合键都不受影响），所以想用键盘触发听写请按**右边那枚**。'
        + '在 设置 · 快捷键 的'
        + '总表里也能改这一行。桌面版里录的键盘/侧键快捷键在**全电脑任何程序**里'
        + '都有效（全局浮窗听写；组合键须完整按下才触发）。纯字母/数字单键在本窗口'
        + '输入框里打字时不会触发，加修饰键或换功能键即可。');
    } else {
      note('快捷键的录制与统一管理需要新版主程序（未找到快捷键登记通道）。');
    }

    /* ----- 最近语音记录（全局语音输入法落档；识别/注入失败的轮次也在，
       可试听、可复制、可重新识别——说过的话不该因为哪一环失败就丢） ----- */
    group('最近语音记录（全局语音输入法）');
    const histBox = document.createElement('div');
    histBox.className = 'vt-hist';
    el.appendChild(histBox);

    // 试听单飞：一次只播一条，切条自动停前一条
    let histAudio = null;
    const stopHistAudio = () => {
      if (histAudio) { histAudio.pause(); histAudio = null; }
      histBox.querySelectorAll('.vt-hist-op-play').forEach((b) => { b.textContent = '试听'; });
    };

    const histStatus = { injected: ['已输入', 'ok'], copied: ['已复制', 'ok'],
      asr_failed: ['识别失败', 'fail'], empty: ['没听清', 'fail'] };

    const renderHistory = (items) => {
      stopHistAudio();
      histBox.innerHTML = '';
      if (!items || !items.length) {
        const p = document.createElement('p');
        p.className = 'sp-note';
        p.textContent = '还没有记录——用全局语音输入法（上面设置的热键，任何程序里）说一句话就有。'
          + '每条保留原始语音与文字，保留最近 3 条；识别失败、没输入进去的也在，随时可以重新识别或复制。';
        histBox.appendChild(p);
        return;
      }
      items.forEach((it, i) => {
        const [stLabel, stKind] = histStatus[it.status] || [it.status || '未知', ''];
        const rowEl = document.createElement('div');
        rowEl.className = 'vt-hist-item';
        const tm = document.createElement('span');
        tm.className = 'vt-hist-time';
        tm.textContent = (it.time || '').slice(5, 16); // "11-05 12:30"
        const badge = document.createElement('span');
        badge.className = 'vt-hist-badge' + (stKind ? ' ' + stKind : '');
        badge.textContent = stLabel;
        const txt = document.createElement('span');
        txt.className = 'vt-hist-text' + (it.text ? '' : ' none');
        txt.textContent = it.text || '（' + stLabel + '——原始语音在，可重新识别）';
        txt.title = it.text || '';
        rowEl.append(tm, badge, txt);

        const ops = document.createElement('span');
        ops.className = 'vt-hist-ops';
        if (it.audio) {
          const play = btn('试听', '播放这条的原始语音');
          play.className = 'sp-btn vt-hist-op-play';
          play.onclick = () => {
            if (histAudio && histAudio.dataset.i === String(i)) { stopHistAudio(); return; }
            stopHistAudio();
            histAudio = new Audio('/api/voicetype/history/wav?i=' + i);
            histAudio.dataset.i = String(i);
            histAudio.onended = stopHistAudio;
            histAudio.play().catch((e) => voiceToast('试听失败：' + (e && e.message ? e.message : e)));
            play.textContent = '停止';
          };
          ops.appendChild(play);
        }
        if (it.text) {
          const copy = btn('复制', '复制识别文字');
          copy.onclick = () => {
            navigator.clipboard.writeText(it.text).then(
              () => voiceToast('已复制'),
              () => voiceToast('复制失败（浏览器剪贴板不可用）'));
          };
          ops.appendChild(copy);
        }
        if (it.audio) {
          const reid = btn('重新识别', '用本地模型重新识别这条的原始语音');
          reid.onclick = async () => {
            reid.disabled = true;
            reid.textContent = '识别中…';
            try {
              const res = await fetch('/api/voicetype/history/reidentify', {
                method: 'POST', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ i }),
              });
              if (!res.ok) throw new Error(apiErrMsg(res, await res.text().catch(() => '')));
              const d = await res.json();
              if (d.queued) {
                // 输入法在跑：它在做完整链路（含悬浮窗与分发），稍等再取结果
                await new Promise((r) => setTimeout(r, 1200));
                await fetchHistory();
              } else {
                renderHistory(d.items || []);
                if (!d.text) voiceToast('仍没识别出文字');
              }
            } catch (e) {
              voiceToast('重新识别失败：' + (e && e.message ? e.message : e));
              reid.disabled = false;
              reid.textContent = '重新识别';
            }
          };
          ops.appendChild(reid);
        }
        rowEl.appendChild(ops);
        histBox.appendChild(rowEl);
      });
    };

    const fetchHistory = async () => {
      try {
        const res = await fetch('/api/voicetype/history');
        if (!res.ok) throw new Error(apiErrMsg(res, await res.text().catch(() => '')));
        const d = await res.json();
        if (!histBox.isConnected) return; // 设置区已收起：结果作废
        renderHistory(d.items || []);
      } catch (e) {
        const p = document.createElement('p');
        p.className = 'sp-note';
        p.textContent = '记录读取失败：' + (e && e.message ? e.message : e);
        histBox.innerHTML = '';
        histBox.appendChild(p);
      }
    };
    fetchHistory();
  }


  /* ================= 注册 ================= */

  CX.register({
    id: 'voice',

    init() {
      active = true;

      // 先把触发方式归一到默认（点按切换）：必须在下面注册快捷键之前跑——主程序
      // 在注册/HK 变更时把触发方式推进联动文件 voicetype.json，顺序反了就会先把
      // 老的 hold 推给语音输入法。
      migrateShortcutModeDefault();

      // 语音输入按钮（主对话与右栏子对话各一套）：按钮 DOM 在主程序 index.html 里
      //（位置与样式不变），默认带 .hidden——按钮属于本组件，由组件揭开并接管行为；
      // 组件关掉/没加载 = 按钮整组不出现。本地 ASR 落地后所有形态都揭开：浏览器
      // 识别可用的走微软服务，不可用的（远程手机端、Electron 壳）自动落到本地模型。
      for (const view of [mainView, subView]) {
        const b = viewEl(view, 'btn-voice');
        if (!b) continue;
        voiceCtl[view.key] = bindVoiceButton(view);
        b.classList.remove('hidden');
      }

      // 语音标记生命周期：输入框被清空时撤销 jxVoice（发送清框在 send 链路里
      // 已删；这里兜用户手动删光的场景）——之后纯打字的消息不会被误标为语音。
      // jxVoiceBound 防重挂（组件关开重载 init 会再跑一遍）。
      for (const view of [mainView, subView]) {
        const inp = view.input;
        if (!inp || inp.dataset.jxVoiceBound) continue;
        inp.dataset.jxVoiceBound = '1';
        inp.addEventListener('input', () => { if (!inp.value) delete inp.dataset.jxVoice; });
      }

      // 朗读：助手消息操作行的按钮 + 用户真切走会话时停止朗读（后端通道，免配置）。
      // 只按会话 id 变化停：后台事件（done/report_ready/session_updated…）触发的
      // refreshCurrent 会以同一会话 id 重走 openSession，那不是用户切换。
      // message:done（新回合结束）不停朗读——实测实锤的误砍源：正在朗读的会话
      // 跑完汇报轮/收尾流时事件一到就把音频连同预取全砍（首块刚出声即停），
      // 而朗读的是那条消息的完整正文快照，历史消息不可变，没有停的理由。
      msgAction = onMessageAction;
      JX.registerMessageAction(msgAction);
      const onOpen = (s) => {
        const sid = s && s.id;
        if (sid && sid === lastOpenSid) return; // 同会话就地翻新：朗读继续
        lastOpenSid = sid || null;
        stopSpeak();
      };
      busHooks.push({ ev: 'session:open', fn: onOpen });
      for (const h of busHooks) JX.on(h.ev, h.fn);

      // 快捷键引擎 + 输入框占位灰字的语音提示
      installShortcutEngine();
      refreshShortcutPlaceholders();

      // 经主程序通道统一登记（HK）：get/set 直通本组件自己的偏好存储
      //（cx:voice:scMain/scSub），槽位语义与 HK 一致（null=默认 / []=停用 /
      // 数组=自定义），老用户设置零迁移；设置·语音 与 设置·快捷键 两边同步。
      voiceHK = (JX.registerHotkey
        ? JX.registerHotkey({
            id: 'voice.dictation',
            name: '语音听写',
            source: '语音',
            desc: '点按切换或长按说话（触发方式在 设置·语音 改）；组合键须完整按下才触发。'
              + '独立语音浮窗只接管右 Alt、右 Ctrl：左 Alt、左 Ctrl 留给系统（菜单栏、Alt+Tab、Ctrl 组合不受影响）',
            defaults: { main: ['mouse-side'], sub: ['alt'] },
            mouseKeys: true,
            validate: (keys) => (validShortcut(keys) ? null : '不能只用单独的鼠标左键或右键'),
            get: shortcutSlot,
            set: setShortcutSlot,
            onChanged: () => { firedLatch = false; cancelSolo(); refreshShortcutPlaceholders(); },
          })
        : null);

      // 发送前收尾听写（可等待钩子）：send() 在组装消息正文前 await 本钩子，
      // 听写中的尾段识别先落定再放行——随说的内容随这条消息一起发出。main/sub
      // 并行等待（各自内部上限 8 秒，小于钩子层 10 秒总超时，不会被提前放行）
      sendHook = async () => {
        const jobs = [];
        for (const k of ['main', 'sub']) {
          const c = voiceCtl[k];
          if (c && c.pauseForSend) jobs.push(Promise.resolve(c.pauseForSend()));
        }
        await Promise.all(jobs);
      };
      if (JX.hook) JX.hook('send', sendHook);

      // 右键菜单「朗读」条目（JX.registerCtxMenu 组件桥，各菜单用 JX.ctxMenuItems
      // 汇总 —> 桌面壳 app-ctxmenu、左栏会话/工作区、右栏子对话清单全都走这里）：
      // 与消息按钮、设置页试听同一条链路（同分块播放、同音色语速、同单飞停播）。
      // 应用本体不带朗读实现——组件关掉即注销，菜单项随组件消失（宁可不出现，
      // 也不留一个点了没反应的条目）。
      // 取值口径由调用方给（app-core.js 的「右键菜单的朗读取值」）：文字先过
      // cleanForSpeech，剥壳后为空（比如只选了一段代码）就不出条目，避免死点。
      ctxMenuFn = (ctx) => {
        if (!active || !ctx) return [];
        const busy = !!playingBtn || !!ttsStopChain;
        const stop = { label: '停止朗读', fn: () => stopSpeak() };
        const t = ctx.text ? cleanForSpeech(ctx.text) : '';
        if (!t) return busy ? [stop] : []; // 没东西可念：念着时至少给个「停止」
        if (busy) return [stop]; // 单飞：正在念就只给停止，别让人误以为能换一条同时念
        return [{
          label: '朗读',
          fn: () => {
            stopSpeak(); // 单飞：顶掉进行中的朗读（含消息按钮发起的）
            speakPlain(t);
          },
        }];
      };
      JX.registerCtxMenu(ctxMenuFn);

      // 设置区以独立分区页呈现（设置 → 语音，位于「组件」之后）：
      // pane 字段交给 app-settings.js 动态挂导航项与页面容器，组件关掉即撤
      CX.registerSettings({
        id: 'voice',
        title: '语音（朗读与听写）',
        pane: {
          label: '语音',
          icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5L6 9H2v6h4l5 4V5z"/><path d="M15.5 8.5a5 5 0 0 1 0 7"/><path d="M18.5 5.5a9 9 0 0 1 0 13"/></svg>',
        },
        mount: mountSettings,
      });
    },

    dispose() {
      active = false;

      // 快捷键引擎与占位提示收尾（HK 登记一并撤下：快捷键总表里该行随之消失）
      removeShortcutEngine();
      if (JX.unregisterHotkey) JX.unregisterHotkey('voice.dictation');
      voiceHK = null;
      if (sendHook && JX.unhook) JX.unhook('send', sendHook);
      sendHook = null;
      if (ctxMenuFn && JX.unregisterCtxMenu) JX.unregisterCtxMenu(ctxMenuFn);
      ctxMenuFn = null;
      window.JXVoiceHint = '';
      if (typeof updateSendKeyHint === 'function') updateSendKeyHint();
      restoreSubPlaceholder();

      // 听写：会话收尾 + 按钮隐藏（按钮归组件：卸载后整组消失，等下次启用再揭开）
      for (const view of [mainView, subView]) {
        const key = view.key;
        const sess = voiceSess[key];
        if (sess && sess.teardown) sess.teardown();
        voiceSess[key] = null;
        voiceCtl[key] = null;
        voiceOn[key] = false;
        const b = viewEl(view, 'btn-voice');
        if (b) {
          b.classList.add('hidden');
          b.classList.remove('on', 'connecting');
          b.onclick = null;
          if (view.hint) view.hint.textContent = '';
        }
      }

      // 朗读：停止播放、摘掉已渲染的朗读按钮、注销钩子
      stopSpeak();
      document.querySelectorAll('.act-tts').forEach((b) => b.remove());
      if (msgAction && JX.unregisterMessageAction) JX.unregisterMessageAction(msgAction);
      msgAction = null;
      if (JX.off) for (const h of busHooks) JX.off(h.ev, h.fn);
      busHooks.length = 0;

      CX.unregisterSettings('voice');
    },
  });
})();
