# 调研报告：dsh-video-storyboard（看视频插件）核心原理与移植方案

- 调研时间：2026-10-28
- 来源视频：https://b23.tv/eGGNPQ0 （B 站，UP：苍穹晓雷）
- 项目地址：https://github.com/SAOS-3624/dsh-video-storyboard （MIT，v0.3.0，39 star）
- 调研方式：README（中英）+ package.json + dsh.plugin.json + src/asr.js + src/index.js 逐行阅读，未 clone 全量源码

---

## 摘要（先看这四条）

1. **它解决的问题**：纯文本/低成本地"看视频"。把一个视频压成**一张可寻址的宫格图**——9~16 格编号画面、每格时间戳 + 对齐台词 + 底部音频包络条带，一张图看 9-16 帧。DSH 按图计费 384 token/图，逐帧方案同覆盖要 3,072~24,576 token，它省 8~16 倍。
2. **ASR 不带任何模型**（用户最关心的点）：台词层是三级降级——①视频自带字幕轨（ffmpeg 直抽，免费最准）→ ②调 **OpenAI 兼容 `/audio/transcriptions` HTTP 接口**（默认指向云端 SiliconFlow 的 `FunAudioLLM/SenseVoiceSmall`，配置走环境变量或 `~/.dsh/asr.json`，也可指向自建本地端点）→ ③都没有就跳过，分镜照出。**没有任何本地推理代码**，就是一个 fetch 客户端 + ffmpeg 抽 16k wav。→ 我们用自己的语音组件（sherpa-onnx X-ASR）替掉这一层是完全对口的。
3. **零运行时依赖**：Node ≥ 20 + ffmpeg/ffprobe 是全部外部要求。无 npm 包、无图像库（Pillow/sharp 都不要，宫格用纯 Buffer 拼字节 + 内置 5×7 点阵字模）、无字体配置、无模型权重。**这使 Node→Python 重写的移植路线完全可行**——全是纯计算 + ffmpeg 管道。
4. **插件形态**：DSH Cordis 插件（`dsh.plugin.json` + `ctx.tools.register` + `ctx.attachments.saveImage`），注册 3 个工具：`video_storyboard`（概览宫格）/ `video_zoom`（短窗重看）/ `video_probe`（只读容器头，0 token 先探路）。强绑定 DSH 宿主契约，**不能直接跑在我们这**。

---

## 一、管线全景（8 步，每步都是"被真实素材打脸后"的设计）

一次解码多路复用：扫描时只解码到 64×36，切点/密度/音频/台词全部拿到后，最后只为选中的格子各解 1~3 帧。

| # | 步骤 | 做法 | 关键取舍（为什么不用显然的做法） | 对应源码 |
|---|---|---|---|---|
| 1 | 探测 | ffprobe 拿时长/帧率/旋转/音轨/字幕轨 | 手机竖拍旋转元数据要按显示方向算；片尾黑场帧避开 | probe.js |
| 2 | 场景切点检测 | 每帧缩到 64×36 转 HSV，逐像素 \|ΔH\|+\|ΔS\|+\|ΔV\|/3 与上帧比 | 只看亮度会漏"同亮度换色"的切点（fixture 9 切点中 2 个这种；ffmpeg scdet 只找 4/9，它找 9/9） | scan.js |
| 3 | 阈值自适应 | 按**速率**反推：目标≈1.5s/切点，分数下限 12 | `3×p95` 在快剪动画上给 130 → 0 切点，死在最需要多格的内容上 | scan.js |
| 4 | 格位分配 | 密度曲线（切点率+台词字/秒+响度突变三信号）积分等分，吸附真实切点（位移≤理想格长 30%） | 均匀切给"6 秒 20 刀"和"20 秒空镜"同样空间；不设上限的吸附会打乱权重 | density.js |
| 5 | 格数决策 | 画面运动 p90 决定（≥15→12s/格，5-15→20s/格，<5→40s/格），minGap 不超素材粒度（≤中位段长×0.9） | 静态单镜头 16 格=16 张同图；固定 1.2s minGap 会把六个 1s 镜头并成三个 | plan.js / segments.js |
| 6 | 选帧 | 段内 3 候选，比 **Laplacian 方差**取最锐 | 取中点→转场叠影；ffmpeg thumbnail 按直方图→模糊不改直方图，会选糊掉的人员表。且三次 seek 各解 1 帧比 thumbnail 解整段**更快**（9.2s vs 11.3s） | sheet.js |
| 7 | 拼宫格 | 帧直接解成 rgb24→cover 裁剪→纯 Buffer 逐字节拼→内置 5×7 点阵字模烧编号（格高 15%）→rawvideo 喂回 ffmpeg 出 PNG | 零图像库零字体依赖=零平台事故（Windows fontconfig 常坏，drawtext 直接报错） | sheet.js |
| 8 | 音频条带 | ebur128 瞬时响度(100ms/点)+silencedetect 静音，画进**同一张图**底部同时间轴：青柱=响度/灰块=静音/黄刻度=切点 | 单独出音频图多花 384 token 还丢对齐；0 额外 token 只占 9% 像素 | audio.js / sheet.js |

**长片策略**（token 预算的精髓）：
- `video_probe`（0 token）→ 一次 16 格总览（384）→ 只开可疑的 2~3 段（3×384）。前 384 就能决定要不要继续——省钱靠"让模型早停"，不靠压单图。
- 文本清单有预算：每格台词封顶（low 60/med 120/high 400 字符，截断落在句末「。」），超限给 `cellRange + textOnly` 翻页续读（不重出图 0 token，翻页 ~0.8s）。
- CLI 批处理：`--batch 300` 一部 2h 片 → 24 张窗图 + timeline.md。
- 两种读法：结构读法（全片总览，会"剧透"）vs 顺序读法（from/to 窗口）。probe 把两种都列出并标注哪种剧透。
- 缓存：键=(路径,大小,mtime,窗口,阈值策略)，不 hash 文件内容。5 分钟片子 13.4s→3.6s。

**在线视频**（v0.3 新增）：input 直接收 B 站等 yt-dlp 支持的链接。两级取流：扫描用 360p 全片（~7MB/5min），zoom 回原始流逐帧取 1080p 不落盘。需要 yt-dlp + cookies.txt（匿名 480p 且 1080p 不在格式列表里，无降级信号）。

---

## 二、ASR 层逐行分析（src/asr.js，我们对接的关键）

### 2.1 三级降级
```
字幕轨（ffmpeg -map 0:s:0 抽 SRT，免费最准）
  → OpenAI 兼容 /audio/transcriptions（需配置；默认云端 SiliconFlow SenseVoiceSmall）
  → 都没有 → 返回 null，绝不让分镜失败
```

### 2.2 调用细节（就 30 行核心：ffmpeg 抽 16k 单声道 wav → FormData POST → 解析 verbose_json）
- 音频：`ffmpeg -i 视频 -vn -ac 1 -ar 16000 -f wav -`（16k 是 ASR 通用采样率）
- 请求：`POST {baseUrl}/audio/transcriptions`，`model=FunAudioLLM/SenseVoiceSmall`，`response_format=verbose_json`，`language=zh`，Bearer key
- 响应：`json.segments[]`（每段 start/end/text）→ 时间戳 + 片偏移加回全局时间轴
- 配置解析：env（DSH_ASR_BASE_URL/KEY…）> `$DSH_HOME/asr.json` > 内置默认（SiliconFlow）。env 值统一剥首尾引号（setx 引号进注册表的真实事故）。

### 2.3 四个踩坑经验（对接我们的 ASR 时同样适用，必须照搬）
| 坑 | 现象 | 解法 |
|---|---|---|
| 长请求漂移 | 单次 299s 请求时间码整体压早 13s（≈4.5%，停顿被吃掉） | **客户端切 ≤45s 片**，漂移降到 0.02s |
| 切片劈词 | 固定秒数切把词切两半 | 切点吸附**静音中点**（silencedetect 给的） |
| 音乐段幻觉 | 纯音乐吐 29s/9 字假句，按时间重叠贴到 5 个格子 | `时长>8s 且 <0.8 字/秒` 判幻觉丢弃，**丢弃条数在输出里报出**（不静默丢） |
| 专业词必错 | 彗差→绘插、八羽怪→巴与怪 | 用户术语词典（asr.json 的 terms），长词优先单遍替换，报替换次数 |

### 2.4 台词对齐与反馈
- `alignCells`：按时间重叠贴格，一句跨两格两边都保留（宁可重复不让格子没台词）；真断点（相邻句隔≥1s）插内联时间戳 `[10:09.4]`
- 字/秒作为**第三条密度信号**喂回格位分配：话密的地方格短
- `字/秒 < 0.8 且响度 > −18 LUFS` ≈ 音乐驱动片（别指望台词，多给格）

---

## 三、插件宿主契约（为什么不能直接拿来用）

- `dsh.plugin.json`：`entry.inject: ["tools", "attachments"]`
- `src/index.js`：`import('@deepseek-ai/dsh-tools')` 拿 `defineTool`；`apply(ctx)` 注册三工具，返回 disposer
- 图片：`ctx.attachments.saveImage({data: PNG字节, mediaType, name})` → attachment ref → `{type:'image', attachment}` 内容块
- schema 限制：宿主只支持 string/number/…/oneOf + required，**没有 minimum/maximum/pattern**，范围校验必须放 execute 里
- 工具结果文本上限：宿主单次约 24k 字符，超了折进磁盘（模型不会去读）→ 这就是 textOnly 翻页机制的由来

→ 我们没有 Cordis/dsh-tools 宿主，这套入口完全不可用；但**管线本体（src/plan/scan/segments/probe/sheet/audio/asr/cache/pipeline）与宿主零耦合**，可整体移植。

---

## 四、我们的机制对照（已逐文件核实）

| 机制 | 现状 | 与本项目的对接点 |
|---|---|---|
| **技能**（文件类能力的法定形态） | `技能/<名>/SKILL.md + manifest.json + scripts/`，内置便携 Python（技能/_runtime），市场可发，read_skill 清单先行 | Python 重写管线的落点；pdf/ocr 先例=工具（Go 薄壳）→技能运行时跑 Python 脚本 |
| **组件 C 形态**（自包含 worker，voice 是样板） | manifest 声明 workers → 声明式桥挂 `/api/c/<id>/<path>`，json-lines，call 180s 超时/32MB 上限 | 视频理解是文件类能力，AGENT.md 明确文件类走技能不走组件 |
| **插件兼容层** | `插件兼容层/<名>/` + 编译期 `adapter_<名>.go`（Go 半边手写）；`插件/` 放 DSH 原件 | 每个 DSH 插件都要手写 Go 适配器 + 需要 Node 运行时（我们不分发 Node）——路线最贵 |
| **语音组件**（组件/voice） | 本地 X-ASR（小米 Zipformer2 transducer，sherpa-onnx，int8 模型 167MB 已随包），worker 协议 `{"cmd":"transcribe","wav_b64":...}→{ok,text}`，端点 `/api/c/voice/asr/transcribe` | **替代它的 ASR 层**。缺口：现协议只回纯文本无时间戳 |
| **演示记录器**（recorder.go + voiceasr.go） | 已有"VAD 停顿分段 → X-ASR 转写 → SRT"整链路（350ms 起效/3s 尾静音/18s 强切段/能量判据 rms>底噪×3.5） | ASR 时间戳问题的现成参考实现 |
| **图片入上下文** | `read_image` 工具 + 内容寻址附件库；**视觉模型按预算内联 base64（单次最多 8 张）**，纯文本模型降级信封 | 对应它的 saveImage + 384/图计费；我们图数预算更宽，"多帧一张图"依然有效（省上下文、可寻址） |
| **ffmpeg** | **机器无 ffmpeg/ffprobe，运行时也不带** | 移植的最大分发缺口（见方案） |

---

## 五、移植方案对比

### 方案 A：技能形态 + Python 重写管线（推荐）
`技能/user/video-storyboard/`（或先内置）：SKILL.md + scripts/storyboard.py
- 管线 8 步用 Python 重写：全部是纯计算 + ffmpeg 子进程管道，原项目零 npm 依赖证明无算法黑盒；numpy 可用（语音组件已在用）。估算 1,500~2,500 行 Python
- 出图落盘 → `read_image` 看图（对齐我们附件机制）；manifest 文本走工具结果
- 三工具语义映射成一个脚本的三组参数（storyboard/zoom/probe → 子命令），SKILL.md 教模型先 probe 再 overview 再 zoom
- **改动面**：新技能目录 + （可选）asr_worker.py 加时间戳；主程序零改动或近零
- 优点：完全符合"文件类能力走技能"的架构纪律；市场可发布（zip 落盘即装）；离线；迭代不动主程序
- 缺点：重写工作量最大（但一次性，且测试可参照它的 154 个用例）

### 方案 B：C 形态组件（组件/video-storyboard/）
worker(Python) + manifest 挂 `/api/c/video-storyboard/run`
- 同样要重写管线，还多一层组件桥；得到一个独立开关与设置页
- 不推荐：违背"文件类能力走技能"的现行架构决策；没有前端 UI 需求时组件形态没有增量价值

### 方案 C：DSH 兼容插件（插件/ + 插件兼容层/adapter）
- 要解决 Node ≥20 运行时分发（我们没有）+ Cordis 契约模拟（ctx.tools.register/saveImage），或手写 Go 适配器包一层 Node 进程
- 前次调研（20261025）结论一致：DSH 原件"一个都装不了"，值钱的是算法与规则，不是宿主胶水。**仅当想长期跟进 DSH 插件生态时才值得**，单为此一个插件不值

### 共同的前置缺口（无论选哪条）
1. **ffmpeg/ffprobe 分发**：三个选项——
   a. 技能内置二进制（downloader 内置 aria2c 的先例；ffmpeg essentials win-x64 ≈ 80MB，会撑大安装包与市场包）
   b. 首启按需下载到 技能/_runtime/bin（voice models 先例是随包；下载可复用 downloader）
   c. 依赖用户自装并探 PATH（面向非编程用户的产品不推荐）
2. **B 站/在线链接**（二期）：yt-dlp 是纯 Python 包，便携 Python 可 pip 直装（比 dsh 项目用独立二进制更顺）；cookies.txt 登录态问题照旧（Chromium 127+ 加密拿不到，需用户导出）
3. **ASR 时间戳**（下节）

---

## 六、ASR 对接：用我们的语音组件替换它的接口层

它的 ASR 层是**纯客户端**（ffmpeg 抽 wav → HTTP → verbose_json segments），所以替换=在我们这提供"带时间戳的转写"。三条路，按推荐排序：

### 路 1（推荐）：asr_worker.py 增加 `with_ts` 输出
sherpa-onnx 的 `OfflineRecognizerResult` 有 token 级 `timestamps` 属性（transducer 模型支持；X-ASR 是 transducer，模型自带标点——按句末标点聚 token 即得句级 segments）。
- 改动：worker 的 transcribe 加 `with_ts` 参数，回 `{ok, text, tokens, timestamps}`；技能脚本按标点聚合成 segments。约 30~40 行
- **待实测**：X-ASR 模型的 timestamps 是否真有值（本地 组件/voice/models/xasr/test_wavs/ 有 4 个测试 wav，半小时可验证）
- 优点：一次调用拿全文+时间戳，最准最省；听写、演示记录器将来也能受益

### 路 2（零 worker 改动）：静音切片 + 现有 transcribe
ffmpeg silencedetect 拿停顿 → 按停顿边界切片（它本来就要做 ≤45s 静音切片防漂移）→ 每片调现有 `/api/c/voice/asr/transcribe` → 片边界即 segment 时间戳
- worker 零改动；粒度=停顿边界（句级或更长），对"贴格子"够用（格子本来就是秒级~十秒级）
- 与演示记录器的 VAD 分段逻辑同构（350ms/3s/18s 那套常量可直接对齐）
- 技能脚本经 HTTP 调组件端点（run_python 跑在本机可访问 localhost 端点；桥单飞行，逐片串行正好）

### 路 3：给 worker 加独立 `transcribe_segments` 命令
worker 内部自己 VAD 分段+逐段转写+返回 segments。所有调用方共用，但把分段策略焊进了 worker——演示记录器的分段在 Go 侧、听写在 client 侧，三处需求不同，不如路 1 通用。

**照搬它的防坑三件套**（哪条路都要）：≤45s 切片防漂移、静音中点切防劈词、`>8s 且 <0.8 字/秒` 幻觉过滤+报丢弃数。字幕轨优先的路它在做（ffmpeg -map 0:s:0 抽 SRT），我们也照做——有字幕轨时零成本最准。

---

## 七、分期建议

- **一期（MVP，本地文件）**：Python 管线（scan/density/plan/segments/sheet/audio/probe/cache）+ 字幕轨抽取 + ASR 路 1 或 2 + read_image 出图 + SKILL.md。验证价值。
- **二期**：textOnly 翻页、批处理 CLI、术语词典、在线链接（yt-dlp pip 直装）、缓存硬化。
- **三期（视使用频率）**：工具化（video_storyboard/video_zoom/video_probe 三个 Go 工具壳，pdf/ocr 模式）——SKILL.md 是文档技能时模型要多一步 read_skill，高频使用后工具化更顺。

---

## 八、待用户拍板

1. 路线：A 技能（推荐）/ B 组件 / C DSH 兼容插件
2. ffmpeg 分发：内置（包大 ~80MB）/ 首启下载 / 用户自装
3. 一期是否含在线视频链接（建议二期）
