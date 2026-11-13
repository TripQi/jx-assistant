# Windows 上用 Word COM 读取 .doc 老文档 —— 完整方法与避坑清单

> 适用：Windows 机器，本机装有 Microsoft Word。无需 antiword / LibreOffice / textract 等任何外部工具。
> 核心思路：**让 Word 自己打开自己的格式**。python-docx 只能读 .docx（zip/OOXML），
> 打不开老式 .doc（OLE2 二进制）——拿 .doc 喂 python-docx 必报
> `Package not found`，这不是环境坏了，是库根本不支持。

## 0. 一条命令验证环境（先跑这个）

```powershell
python -c "import win32com.client as w; app=w.DispatchEx('Word.Application'); print('OK', app.Name, app.Version); app.Quit()"
```

输出 `OK Microsoft Word 16.0` 即全链路可用。报错对照下面的故障表。

## 1. 环境前提（就三条）

1. Windows + 本机安装了 Microsoft Word（WPS 的备选方案见文末）；
2. Python ≥ 3.8（任意来源均可）；
3. `python -m pip install pywin32`（装进**你实际运行的那个解释器**）。

## 2. 最小可用代码：读 .doc 正文

```python
import win32com.client as w

word = w.DispatchEx("Word.Application")   # DispatchEx=独立实例；别用 Dispatch，会粘住用户已开的 Word
word.Visible = False                       # 不弹界面
word.DisplayAlerts = 0                     # 关一切弹窗，没这条弹框会卡死脚本
try:
    doc = word.Documents.Open(
        r"D:\path\含中文也没问题\记录清单.doc",
        ReadOnly=True,            # 只读打开，不动原文件、不产生 ~$ 锁文件
        ConfirmConversions=False, # 跳过"格式转换"询问框
    )
    print(doc.Content.Text)                          # 全文，\r 是段落分隔符
    print("页数:", doc.ComputeStatistics(2))          # 2 = wdStatisticPages
    print("段落:", doc.Paragraphs.Count)
    print("表格:", doc.Tables.Count)
    doc.Close(False)                        # False = 关闭但不保存
finally:
    word.Quit()                             # 必须退出，否则 WINWORD.EXE 进程残留
```

## 3. .doc → .docx 转换（转完就能用 python-docx 纯库继续处理）

```python
doc = word.Documents.Open(src, ReadOnly=True, ConfirmConversions=False)
doc.SaveAs2(dst, FileFormat=16)   # 16=docx；0=老式doc；17=pdf
doc.Close(False)
```

## 4. 故障对照表（别的 AI 常见翻车点）

| 症状 | 真实原因 | 解法 |
|---|---|---|
| `ModuleNotFoundError: win32com` | 解释器没装 pywin32，或装到了另一个 Python 里 | 用**同一个解释器**执行 `python -m pip install pywin32` |
| `'python' 不是内部或外部命令` | 新装 Windows 只有商店占位符 | 用解释器完整路径运行，别依赖 PATH |
| Dispatch / Open 卡住不动、超时 | 有残留的 WINWORD.EXE 僵尸进程，或 Word 弹了对话框在等人点 | 先 `taskkill /f /im WINWORD.EXE` 清场再跑；代码里 `DisplayAlerts=0` |
| 打开文件就挂起 | 受保护视图 / 宏警告 / 修复提示弹窗挡住 | 同上：`DisplayAlerts=0` + `ReadOnly=True` + `ConfirmConversions=False` |
| 报"文件已损坏"，但文件好好的 | 遍历目录时把 `~$xxx.docx` 锁文件当真文档打开了 | 遍历时跳过 `~$` 开头的文件（Word 隐藏锁文件） |
| `Package not found at 'xxx.doc'` | 拿 .doc 喂 python-docx | .doc 只能走 COM；先按第 3 节转 .docx |
| 中文路径乱码 / 找不到文件 | 路径经 shell/命令行转手被转码 | 路径直接写在 Python 源码里（UTF-8 保存），不经 shell 传参 |
| `ActiveX 部件不能创建对象 (0x800A01AD / 429)` | 没装 Word，或 COM 注册损坏 | 装好 Office；或修复安装（setup /repair）；WPS 可试 `KWPS.Application` |
| 越跑越慢、内存上涨 | 每个 Close/Quit 没成对执行，进程泄漏 | try-finally 保证 `Close(False)`+`Quit()`；批量文件**复用同一个 Word 实例**，全部处理完再 Quit 一次 |
| 沙箱/容器里 COM 创建失败 | 运行环境隔离了 COM 或没有桌面会话 | COM 需要真实 Windows 用户会话；纯服务器/沙箱场景换 LibreOffice `soffice --convert-to docx` 路线 |

## 5. 批量处理模板（一个实例干完全部再退）

```python
import glob, os, win32com.client as w

files = [f for f in glob.glob(r"D:\目录\**\*.doc", recursive=True)
         if not os.path.basename(f).startswith("~$")]   # 跳过锁文件

word = w.DispatchEx("Word.Application")
word.Visible = False
word.DisplayAlerts = 0
try:
    for f in files:
        doc = word.Documents.Open(f, ReadOnly=True, ConfirmConversions=False)
        try:
            print(os.path.basename(f), "->", doc.Content.Text[:60].replace("\r", " "))
        finally:
            doc.Close(False)
finally:
    word.Quit()
```

## 6. 没装 Word 的机器：备选

- 装了 WPS 的：把 `Word.Application` 换成 `KWPS.Application`，其余代码不变；
- 什么都没有的：装 LibreOffice，命令行转换：
  `soffice --headless --convert-to docx "文件.doc" --outdir 输出目录`
  （保真度低于 Word 本体，复杂表格/修订可能走样）。

## 7. 实测参照（2026-10-16，Windows 10 + Microsoft 365 Word 16.0）

```
DispatchEx("Word.Application") → OK Microsoft Word 16.0
打开 2021 年交接材料 记录清单.doc（79 KB，OLE2 老格式）：
  页数: 2  段落: 332  表格: 1  字数约: 1700
  正文预览: 两化融合体系记录清单 序号 对应记录 文件编号 保管部门 保存时间 ...
同批 11 个 .docx 由 python-docx 直读，全部 OK。
```
